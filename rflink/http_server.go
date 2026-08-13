/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"
)

const (
	headerRequestID = "X-Request-ID"
	headerRealIP    = "X-Real-IP"
	headerCSRF      = "X-CSRF-Token"
	cookieCSRF      = "gorflink_csrf"
	maxBodyBytes    = 64 << 10 // 64 KiB
)

type ctxKey int

const (
	ctxRequestID ctxKey = iota
	ctxClientIP
	ctxAuthKind
	ctxAPIToken
)

type httpAPIServer struct {
	app    *App
	server *http.Server
	wg     sync.WaitGroup

	corsOrigins []string
	trustedNets []*net.IPNet
	rawHub      *rawHub
	loginLimit  *loginLimiter
}

func startHTTPServer(app *App) (*httpAPIServer, error) {
	addr := strings.TrimSpace(app.opts.HTTP.Listen)
	if addr == "" {
		return nil, fmt.Errorf("HTTP_LISTEN is empty")
	}

	h := &httpAPIServer{app: app, rawHub: newRawHub(), loginLimit: newLoginLimiter()}
	h.corsOrigins = parseCSVList(app.opts.HTTP.CORSOrigins)
	h.trustedNets = parseTrustedProxies(app.opts.HTTP.TrustedProxies)
	app.publisher.SetRawLineHook(func(e rawLineEntry) {
		h.rawHub.broadcast(e)
		if app.webhooks != nil {
			app.webhooks.NotifyRaw(e)
		}
	})

	mux := http.NewServeMux()

	// Probes — no auth, no CSRF
	mux.HandleFunc("/healthz", h.handleHealthz)
	mux.HandleFunc("/readyz", h.handleReadyz)

	mux.HandleFunc("/api/v1/csrf", h.handleCSRF)
	mux.HandleFunc("/api/v1/meta", h.handleMeta)
	mux.HandleFunc("/api/v1/session", h.handleSession) // POST login / DELETE logout

	mux.HandleFunc("/api/v1/health", h.protect(h.handleAPIHealth))
	mux.HandleFunc("/api/v1/sensors", h.protect(h.handleAPISensors))
	mux.HandleFunc("/api/v1/rflink/raw", h.protect(h.handleAPIRaw))
	mux.HandleFunc("/api/v1/rflink/raw/ws", h.handleRawWS)
	mux.HandleFunc("/api/v1/command", h.protect(h.handleAPICommand))
	mux.HandleFunc("/api/v1/ha/rediscover", h.protect(h.handleAPIRediscover))
	mux.HandleFunc("/api/v1/config", h.protect(h.handleAPIConfig))
	mux.HandleFunc("/api/v1/config/restore", h.protect(h.handleConfigRestore))
	mux.HandleFunc("/api/v1/config/backups", h.protect(h.handleConfigBackups))
	mux.HandleFunc("/api/v1/tokens", h.protect(h.handleAPITokens))
	mux.HandleFunc("/api/v1/webhooks", h.protect(h.handleWebhooks))
	mux.HandleFunc("/api/v1/sessions", h.protect(h.handleSessions))
	mux.HandleFunc("/api/v1/serial", h.protect(h.handleSerialInfo))
	mux.HandleFunc("/api/v1/rate_limit", h.protect(h.handleAPIRateLimit))

	// GUI
	if app.opts.HTTP.GUI {
		mux.HandleFunc("/", h.handleGUI)
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			http.Error(w, "GUI disabled (HTTP_GUI=false)", http.StatusNotFound)
		})
	}

	// Middleware chain (outer → inner): Recovery → RequestID → RealIP → AccessLog → SecurityHeaders → CORS → mux
	var handler http.Handler = mux
	handler = h.middlewareCORS(handler)
	handler = h.middlewareSecurityHeaders(handler)
	handler = h.middlewareAccessLog(handler)
	handler = h.middlewareRealIP(handler)
	handler = h.middlewareRequestID(handler)
	handler = h.middlewareRecovery(handler)

	// ReadTimeout/WriteTimeout must be zero: they apply to the whole connection
	// lifetime and would kill long-lived WebSocket feeds after N seconds.
	h.server = &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       0,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MiB
	}

	guiAuth := strings.TrimSpace(app.opts.HTTP.AuthToken) != ""
	apiAuth := len(app.opts.apiTokens) > 0
	if !guiAuth {
		log.Warn("HTTP_AUTH_TOKEN is not set — GUI is locked (no admin/raw/ws); set HTTP_AUTH_TOKEN to unlock")
	} else if len(strings.TrimSpace(app.opts.HTTP.AuthToken)) < 16 {
		log.Warn("HTTP_AUTH_TOKEN is shorter than 16 characters — use a longer secret")
	}
	if isNonLocalListen(addr) && !guiAuth && !apiAuth {
		log.Warn("HTTP listens on non-loopback without credentials — set HTTP_AUTH_TOKEN / API_AUTH_TOKEN or bind 127.0.0.1",
			"addr", addr,
		)
	}
	log.Info("http server enabled",
		"addr", addr,
		"api", true,
		"gui", app.opts.HTTP.GUI,
		"gui_auth", guiAuth,
		"api_tokens", len(app.opts.apiTokens),
		"read_only", app.opts.HTTP.ReadOnly,
		"runtime_config", app.runtime.pathString(),
		"cors_origins", len(h.corsOrigins),
		"csrf", true,
		"websocket_raw", true,
		"session_ttl_hours", 72,
	)

	h.wg.Go(func() {
		if err := h.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server error", "err", err)
		}
	})

	return h, nil
}

func (h *httpAPIServer) Shutdown() {
	if h.server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.server.Shutdown(ctx); err != nil {
		log.Warn("http server shutdown error", "err", err)
	}
	h.wg.Wait()
	log.Info("http server stopped")
}

// protect applies API token auth and, for mutating methods, CSRF when the client
// is browser-like (has CSRF cookie / no Bearer token).
func (h *httpAPIServer) protect(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ok, r2 := h.checkAuth(w, r)
		if !ok {
			return
		}
		r = r2
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			if !h.checkCSRF(w, r) {
				return
			}
		}
		if !h.checkScope(w, r) {
			return
		}
		next(w, r)
	}
}

// checkAuth accepts: valid GUI session cookie OR valid API_AUTH_TOKEN Bearer.
// HTTP_AUTH_TOKEN is NOT accepted here (only on POST /api/v1/session).
// No open mode: unauthenticated requests are always rejected.
func (h *httpAPIServer) checkAuth(w http.ResponseWriter, r *http.Request) (bool, *http.Request) {
	opts := h.app.opts

	if sid := sessionIDFromRequest(r); sid != "" {
		if sess := h.app.sessions.Get(sid); sess != nil {
			ctx := context.WithValue(r.Context(), ctxAuthKind, "session")
			return true, r.WithContext(ctx)
		}
	}

	got := r.Header.Get("X-API-Token")
	if got == "" {
		auth := r.Header.Get("Authorization")
		if len(auth) >= 7 && strings.EqualFold(auth[:7], "bearer ") {
			got = strings.TrimSpace(auth[7:])
		}
	}
	if got != "" {
		if tok, ok := opts.validAPIToken(got); ok {
			ctx := context.WithValue(r.Context(), ctxAuthKind, "api")
			ctx = context.WithValue(ctx, ctxAPIToken, tok)
			return true, r.WithContext(ctx)
		}
	}

	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
	return false, r
}

// checkScope enforces API token scopes. GUI sessions have full access.
func (h *httpAPIServer) checkScope(w http.ResponseWriter, r *http.Request) bool {
	kind, _ := r.Context().Value(ctxAuthKind).(string)
	if kind != "api" {
		return true
	}
	tok, _ := r.Context().Value(ctxAPIToken).(*apiToken)
	if tok == nil {
		return true
	}
	need := scopeForPath(r.Method, r.URL.Path)
	if need == "" {
		return true
	}
	if tok.hasScope(need) {
		return true
	}
	writeJSON(w, http.StatusForbidden, map[string]string{
		"error": "insufficient_scope",
		"need":  need,
	})
	return false
}

func scopeForPath(method, path string) string {
	switch path {
	case "/api/v1/health", "/api/v1/sensors", "/api/v1/rflink/raw",
		"/api/v1/rate_limit", "/api/v1/serial", "/api/v1/config/backups":
		return "read"
	case "/api/v1/config":
		if method == http.MethodGet {
			return "read"
		}
		return "admin"
	case "/api/v1/config/restore":
		return "admin"
	case "/api/v1/command", "/api/v1/ha/rediscover":
		return "command"
	case "/api/v1/tokens", "/api/v1/sessions":
		return "admin" // still requires GUI session via requireGUISession
	default:
		if strings.HasPrefix(path, "/api/v1/") {
			return "read"
		}
		return ""
	}
}

func (h *httpAPIServer) requireGUISession(w http.ResponseWriter, r *http.Request) bool {
	if strings.TrimSpace(h.app.opts.HTTP.AuthToken) == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "gui_auth_not_configured",
			"hint":  "set HTTP_AUTH_TOKEN to enable GUI administration",
		})
		return false
	}
	sid := sessionIDFromRequest(r)
	if sid == "" || h.app.sessions.Get(sid) == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "gui_session_required"})
		return false
	}
	return true
}

// guiLocked reports minimal GUI mode: no HTTP_AUTH_TOKEN configured.
func (h *httpAPIServer) guiLocked() bool {
	return strings.TrimSpace(h.app.opts.HTTP.AuthToken) == ""
}

func (h *httpAPIServer) effectiveReadOnly() bool {
	return h.app.opts.HTTP.ReadOnly || h.guiLocked()
}

// checkCSRF: session-authenticated requests require double-submit CSRF (fail-closed).
// Pure API Bearer clients (no session cookie) skip CSRF.
func (h *httpAPIServer) checkCSRF(w http.ResponseWriter, r *http.Request) bool {
	hasSession := false
	if sid := sessionIDFromRequest(r); sid != "" {
		hasSession = h.app.sessions.Get(sid) != nil
	}
	c, err := r.Cookie(cookieCSRF)
	cookieVal := ""
	if err == nil && c != nil {
		cookieVal = c.Value
	}
	if hasSession {
		if cookieVal == "" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "csrf_failed", "hint": "missing csrf cookie"})
			return false
		}
		header := r.Header.Get(headerCSRF)
		if header == "" {
			header = r.Header.Get("X-XSRF-Token")
		}
		if subtle.ConstantTimeCompare([]byte(header), []byte(cookieVal)) != 1 {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "csrf_failed"})
			return false
		}
		return true
	}
	// No session: API token clients — skip CSRF.
	return true
}

func (h *httpAPIServer) issueCSRF(w http.ResponseWriter, r *http.Request) string {
	return h.issueCSRFForce(w, r, false)
}

func (h *httpAPIServer) issueCSRFForce(w http.ResponseWriter, r *http.Request, forceNew bool) string {
	if !forceNew {
		if c, err := r.Cookie(cookieCSRF); err == nil && c != nil && len(c.Value) >= 32 {
			return c.Value
		}
	}
	tok, err := randomHex(32)
	if err != nil {
		tok = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieCSRF,
		Value:    tok,
		Path:     "/",
		HttpOnly: false, // readable by GUI JS (double-submit)
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
		MaxAge:   86400,
	})
	return tok
}

func (h *httpAPIServer) handleCSRF(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tok := h.issueCSRF(w, r)
	writeJSON(w, http.StatusOK, map[string]string{"csrf_token": tok})
}

// --- middleware ---

func (h *httpAPIServer) middlewareRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hw := &hijackTracker{ResponseWriter: w}
		defer func() {
			if rec := recover(); rec != nil {
				rid := requestIDFrom(r.Context())
				log.Error("http panic recovered",
					"request_id", rid,
					"path", r.URL.Path,
					"method", r.Method,
					"hijacked", hw.hijacked,
					"panic", fmt.Sprintf("%v", rec),
				)
				// Never write to a hijacked connection (causes native crashes on Windows).
				if hw.hijacked {
					return
				}
				defer func() { _ = recover() }()
				writeJSON(hw, http.StatusInternalServerError, map[string]string{
					"error":      "internal_server_error",
					"request_id": rid,
				})
			}
		}()
		next.ServeHTTP(hw, r)
	})
}

// hijackTracker notes WebSocket upgrades so outer middleware never writes afterward.
type hijackTracker struct {
	http.ResponseWriter
	hijacked bool
}

func (h *hijackTracker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := h.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response does not implement http.Hijacker")
	}
	h.hijacked = true
	return hj.Hijack()
}

func (h *hijackTracker) Flush() {
	if f, ok := h.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (h *hijackTracker) Unwrap() http.ResponseWriter {
	return h.ResponseWriter
}

func (h *httpAPIServer) middlewareRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := strings.TrimSpace(r.Header.Get(headerRequestID))
		if rid == "" || !looksLikeRequestID(rid) {
			rid = newUUID()
		}
		if len(rid) > 64 {
			rid = rid[:64]
		}
		w.Header().Set(headerRequestID, rid)
		ctx := context.WithValue(r.Context(), ctxRequestID, rid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func looksLikeRequestID(s string) bool {
	// Accept UUID or opaque tokens from upstream proxies (printable, limited charset).
	if len(s) < 8 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// newUUID returns an RFC 4122 version-4 UUID string.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Extremely unlikely; fall back to time-based opaque id.
		h, _ := randomHex(16)
		return h
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (h *httpAPIServer) middlewareRealIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r, h.trustedNets)
		ctx := context.WithValue(r.Context(), ctxClientIP, ip)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (h *httpAPIServer) middlewareAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)

		path := r.URL.Path
		fields := []any{
			"request_id", requestIDFrom(r.Context()),
			"method", r.Method,
			"path", path,
			"status", rw.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"ip", clientIPFrom(r.Context()),
		}
		switch {
		case rw.hijacked || path == "/api/v1/rflink/raw/ws":
			log.Debug("http websocket", fields...)
		case rw.status >= 500:
			log.Error("http request", fields...)
		case rw.status >= 400:
			log.Info("http request", fields...)
		case isImportantHTTPRequest(r.Method, path):
			log.Info("http request", fields...)
		default:
			log.Debug("http request", fields...)
		}
	})
}

// isImportantHTTPRequest reports paths that should always appear at info when successful.
func isImportantHTTPRequest(method, path string) bool {
	switch path {
	case "/api/v1/command", "/api/v1/ha/rediscover", "/api/v1/session",
		"/api/v1/config/restore", "/api/v1/tokens", "/api/v1/sessions", "/api/v1/webhooks":
		return true
	case "/api/v1/config":
		return method == http.MethodPut || method == http.MethodPatch || method == http.MethodPost
	default:
		return false
	}
}

func (h *httpAPIServer) middlewareSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("X-Frame-Options", "DENY")
		hdr.Set("Referrer-Policy", "no-referrer")
		hdr.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=(), payment=(), usb=(), interest-cohort=()")
		hdr.Set("Cross-Origin-Opener-Policy", "same-origin")
		hdr.Set("Cross-Origin-Resource-Policy", "same-origin")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			hdr.Set("Cache-Control", "no-store")
		}
		// CSP for GUI document; API stays JSON-only.
		if r.URL.Path == "/" {
			hdr.Set("Content-Security-Policy",
				"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'none'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		}
		next.ServeHTTP(w, r)
	})
}

func (h *httpAPIServer) middlewareCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && h.originAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Token, X-CSRF-Token, X-Request-ID")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, OPTIONS")
			w.Header().Set("Access-Control-Max-Age", "600")
			w.Header().Set("Access-Control-Expose-Headers", headerRequestID)
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *httpAPIServer) originAllowed(origin string) bool {
	if len(h.corsOrigins) == 0 {
		return false
	}
	for _, o := range h.corsOrigins {
		if o == "*" || strings.EqualFold(o, origin) {
			return true
		}
	}
	return false
}

type statusRecorder struct {
	http.ResponseWriter
	status   int
	hijacked bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.hijacked {
		return
	}
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.hijacked {
		return 0, http.ErrBodyReadAfterClose
	}
	return s.ResponseWriter.Write(b)
}

// Hijack allows WebSocket upgrades through the access-log middleware.
func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response does not implement http.Hijacker")
	}
	s.hijacked = true
	s.status = http.StatusSwitchingProtocols
	return hj.Hijack()
}

func (s *statusRecorder) Flush() {
	if s.hijacked {
		return
	}
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}

func requestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxRequestID).(string); ok {
		return v
	}
	return ""
}

func clientIPFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxClientIP).(string); ok {
		return v
	}
	return ""
}

func clientIP(r *http.Request, trusted []*net.IPNet) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	remote := net.ParseIP(host)
	if remote == nil || !ipInNets(remote, trusted) {
		return host
	}
	// Only honour forwarded headers from trusted proxies.
	if xri := strings.TrimSpace(r.Header.Get(headerRealIP)); xri != "" {
		if ip := net.ParseIP(xri); ip != nil {
			return xri
		}
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		cand := strings.TrimSpace(parts[0])
		if ip := net.ParseIP(cand); ip != nil {
			return cand
		}
	}
	return host
}

func ipInNets(ip net.IP, nets []*net.IPNet) bool {
	if len(nets) == 0 {
		// No trusted proxies configured → never trust X-Forwarded-*.
		return false
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func parseTrustedProxies(raw string) []*net.IPNet {
	var out []*net.IPNet
	for _, p := range parseCSVList(raw) {
		if !strings.Contains(p, "/") {
			if ip := net.ParseIP(p); ip != nil {
				if ip.To4() != nil {
					p += "/32"
				} else {
					p += "/128"
				}
			}
		}
		_, network, err := net.ParseCIDR(p)
		if err != nil {
			log.Warn("HTTP_TRUSTED_PROXIES: skip invalid entry", "entry", p, "err", err)
			continue
		}
		out = append(out, network)
	}
	return out
}

func parseCSVList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out []string
	for p := range strings.SplitSeq(raw, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func randomHex(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// --- probes ---

func (h *httpAPIServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (h *httpAPIServer) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	snap := h.app.publisher.Health()
	if !snap.MQTTConnected && !snap.SerialConnected {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status":           "not_ready",
			"mqtt_connected":   snap.MQTTConnected,
			"serial_connected": snap.SerialConnected,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":           "ready",
		"mqtt_connected":   snap.MQTTConnected,
		"serial_connected": snap.SerialConnected,
	})
}

// --- API ---

func (h *httpAPIServer) handleAPIHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, h.app.publisher.Health())
}

func (h *httpAPIServer) handleAPISensors(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	includeSamples := r.URL.Query().Get("samples") == "1" || r.URL.Query().Get("samples") == "true"
	resp := map[string]any{
		"last_seen": h.app.publisher.SensorsLastSeen(),
	}
	if includeSamples {
		resp["recent"] = h.app.publisher.RecentSensorSamples()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *httpAPIServer) handleAPIRaw(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"lines": h.app.publisher.RecentRawLines(),
	})
}

func (h *httpAPIServer) handleSession(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		h.sessionLogin(w, r)
	case http.MethodDelete:
		h.sessionLogout(w, r)
	case http.MethodGet:
		h.sessionStatus(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *httpAPIServer) sessionLogin(w http.ResponseWriter, r *http.Request) {
	// Login may have no session yet: CSRF only if cookie already present.
	if c, err := r.Cookie(cookieCSRF); err == nil && c != nil && c.Value != "" {
		header := r.Header.Get(headerCSRF)
		if header == "" {
			header = r.Header.Get("X-XSRF-Token")
		}
		if subtle.ConstantTimeCompare([]byte(header), []byte(c.Value)) != 1 {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "csrf_failed"})
			return
		}
	}
	ip := clientIPFrom(r.Context())
	if ip == "" {
		ip = r.RemoteAddr
	}
	if ok, retry := h.loginLimit.allow(ip); !ok {
		log.Info("session login rate-limited", "ip", ip, "retry_after_sec", int(retry.Seconds()))
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(retry.Seconds())+1))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error":           "too_many_attempts",
			"retry_after_sec": int(retry.Seconds()) + 1,
		})
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	var req struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(body, &req)
	if req.Token == "" {
		req.Token = strings.TrimSpace(string(body))
	}
	want := strings.TrimSpace(h.app.opts.HTTP.AuthToken)
	if want == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "gui_auth_not_configured"})
		return
	}
	if !h.app.opts.guiTokenOK(req.Token) {
		h.loginLimit.fail(ip)
		log.Info("session login failed", "ip", ip)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	h.loginLimit.success(ip)
	sess, err := h.app.sessions.Create(ip)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "session_create_failed"})
		return
	}
	setSessionCookie(w, r, sess)
	// Rotate CSRF after successful login
	csrf := h.issueCSRFForce(w, r, true)
	log.Info("session created",
		"session_id_prefix", sess.ID[:8],
		"ip", ip,
		"expires_at", sess.ExpiresAt.Format(time.RFC3339),
		"active_sessions", h.app.sessions.Count(),
	)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "ok",
		"expires_at": sess.ExpiresAt.Format(time.RFC3339),
		"ttl_hours":  72,
		"csrf_token": csrf,
	})
}

func (h *httpAPIServer) sessionLogout(w http.ResponseWriter, r *http.Request) {
	if !h.checkCSRF(w, r) {
		return
	}
	sid := sessionIDFromRequest(r)
	if sid != "" {
		if h.app.sessions.Revoke(sid) {
			log.Info("session revoked", "session_id_prefix", sid[:min(8, len(sid))], "ip", clientIPFrom(r.Context()))
		}
	}
	clearSessionCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *httpAPIServer) sessionStatus(w http.ResponseWriter, r *http.Request) {
	sid := sessionIDFromRequest(r)
	sess := h.app.sessions.Get(sid)
	if sess == nil {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"expires_at":    sess.ExpiresAt.Format(time.RFC3339),
	})
}
func (h *httpAPIServer) handleMeta(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sid := sessionIDFromRequest(r)
	authed := sid != "" && h.app.sessions.Get(sid) != nil
	guiAuth := strings.TrimSpace(h.app.opts.HTTP.AuthToken) != ""
	writeJSON(w, http.StatusOK, map[string]any{
		"gui_auth_required":   guiAuth,
		"gui_auth_configured": guiAuth,
		"gui_locked":          !guiAuth,
		"api_auth_required":   true, // no open mode
		"authenticated":       authed,
		"read_only":           h.effectiveReadOnly(),
		"http_read_only":      h.app.opts.HTTP.ReadOnly,
		"gui":                 h.app.opts.HTTP.GUI,
		"runtime_path":        h.app.runtime.pathString(),
		"version":             Version,
		"git_sha":             GitSHA,
		"warning": func() string {
			if !guiAuth {
				return "HTTP_AUTH_TOKEN is not set — GUI is locked (read-only, no raw/ws/admin). Set HTTP_AUTH_TOKEN and restart."
			}
			return ""
		}(),
	})
}

func (h *httpAPIServer) handleAPIRateLimit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	lim, rem, unlimited := h.app.CommandRateStatus()
	writeJSON(w, http.StatusOK, map[string]any{
		"unlimited":     unlimited,
		"limit_per_sec": lim,
		"remaining":     rem,
		"read_only":     h.effectiveReadOnly(),
	})
}

func (h *httpAPIServer) handleAPICommand(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.effectiveReadOnly() {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "read_only"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body failed"})
		return
	}
	payload := strings.TrimSpace(string(body))
	if strings.HasPrefix(payload, "{") {
		var obj struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(body, &obj); err == nil && obj.Command != "" {
			payload = obj.Command
		}
	}
	if payload == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty command"})
		return
	}
	res := h.app.ExecuteCommand(payload)
	status := http.StatusOK
	switch res.Status {
	case "rejected", "rate_limited":
		status = http.StatusBadRequest
	case "serial_down", "error":
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, res)
}

func (h *httpAPIServer) handleAPIRediscover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.effectiveReadOnly() {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "read_only"})
		return
	}
	h.app.publisher.ClearHADiscovery()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *httpAPIServer) handleAPIConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.getConfig(w, r)
	case http.MethodPut, http.MethodPatch:
		h.putConfig(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *httpAPIServer) effectiveFriendlyCSV() string {
	o := h.app.opts
	file, src := h.app.runtime.snapshot()
	if src.FriendlyNames == "runtime" && file.FriendlyNames != nil {
		return *file.FriendlyNames
	}
	return o.Publish.FriendlyNames
}

func (h *httpAPIServer) effectiveIgnoreCSV() string {
	o := h.app.opts
	file, src := h.app.runtime.snapshot()
	if src.IgnoreList == "runtime" && file.IgnoreList != nil {
		return *file.IgnoreList
	}
	return o.Publish.IgnoreList
}

func (h *httpAPIServer) effectiveHWIDCSV() string {
	o := h.app.opts
	file, src := h.app.runtime.snapshot()
	if src.HWIDMap == "runtime" && file.HWIDMap != nil {
		return *file.HWIDMap
	}
	return o.Publish.HWIDMap
}

func (h *httpAPIServer) getConfig(w http.ResponseWriter, r *http.Request) {
	_, src := h.app.runtime.snapshot()
	o := h.app.opts
	writeJSON(w, http.StatusOK, map[string]any{
		"friendly_names":      h.effectiveFriendlyCSV(),
		"ignore_list":         h.effectiveIgnoreCSV(),
		"hwid_map":            h.effectiveHWIDCSV(),
		"ha_discovery":        o.Publish.HomeAssistantDiscovery,
		"ha_discovery_prefix": o.Publish.HADiscoveryPrefix,
		"ha_buttons":          o.Publish.HAButtons,
		"ha_switches":         o.Publish.HASwitches,
		"source":              src,
		"runtime_path":        h.app.runtime.pathString(),
		"backups":             h.app.runtime.ListBackups(),
		"note":                "When a key is stored in runtime file it fully overrides env for that key",
	})
}

func (h *httpAPIServer) putConfig(w http.ResponseWriter, r *http.Request) {
	if h.effectiveReadOnly() {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "read_only"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body failed"})
		return
	}
	var req struct {
		FriendlyNames     *string `json:"friendly_names"`
		IgnoreList        *string `json:"ignore_list"`
		HWIDMap           *string `json:"hwid_map"`
		HADiscovery       *bool   `json:"ha_discovery"`
		HADiscoveryPrefix *string `json:"ha_discovery_prefix"`
		HAButtons         *string `json:"ha_buttons"`
		HASwitches        *string `json:"ha_switches"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	patch := RuntimeFile{}
	var fieldErrs []string

	if req.FriendlyNames != nil {
		v := strings.TrimSpace(*req.FriendlyNames)
		if err := ValidateFriendlyNamesCSV(v); err != nil {
			fieldErrs = append(fieldErrs, err.Error())
		} else {
			patch.FriendlyNames = &v
		}
	}
	if req.IgnoreList != nil {
		v := strings.TrimSpace(*req.IgnoreList)
		if err := ValidateIgnoreListCSV(v); err != nil {
			fieldErrs = append(fieldErrs, err.Error())
		} else {
			patch.IgnoreList = &v
		}
	}
	if req.HWIDMap != nil {
		v := strings.TrimSpace(*req.HWIDMap)
		if err := ValidateHWIDMapCSV(v); err != nil {
			fieldErrs = append(fieldErrs, err.Error())
		} else {
			patch.HWIDMap = &v
		}
	}
	if req.HADiscovery != nil {
		patch.HADiscovery = req.HADiscovery
	}
	if req.HADiscoveryPrefix != nil {
		v := strings.TrimSpace(*req.HADiscoveryPrefix)
		if err := ValidateHADiscoveryPrefix(v); err != nil {
			fieldErrs = append(fieldErrs, err.Error())
		} else {
			patch.HADiscoveryPrefix = &v
		}
	}
	if req.HAButtons != nil {
		v := strings.TrimSpace(*req.HAButtons)
		if err := ValidateHAButtonsCSV(v); err != nil {
			fieldErrs = append(fieldErrs, err.Error())
		} else {
			patch.HAButtons = &v
		}
	}
	if req.HASwitches != nil {
		v := strings.TrimSpace(*req.HASwitches)
		if err := ValidateHASwitchesCSV(v); err != nil {
			fieldErrs = append(fieldErrs, err.Error())
		} else {
			patch.HASwitches = &v
		}
	}

	disc := h.app.opts.Publish.HomeAssistantDiscovery
	if req.HADiscovery != nil {
		disc = *req.HADiscovery
	}
	pfx := h.app.opts.Publish.HADiscoveryPrefix
	if req.HADiscoveryPrefix != nil {
		pfx = strings.TrimSpace(*req.HADiscoveryPrefix)
	}
	if disc {
		if pfx == "" {
			pfx = "homeassistant"
			patch.HADiscoveryPrefix = &pfx
		}
	}

	if len(fieldErrs) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":   "validation_failed",
			"details": fieldErrs,
		})
		return
	}

	first, err := h.app.runtime.SaveOverlay(patch)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	log.Info("runtime config updated via API",
		"request_id", requestIDFrom(r.Context()),
		"first_time_keys", first,
		"ip", clientIPFrom(r.Context()),
	)
	_, src2 := h.app.runtime.snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"friendly_names":      h.effectiveFriendlyCSV(),
		"ignore_list":         h.effectiveIgnoreCSV(),
		"hwid_map":            h.effectiveHWIDCSV(),
		"ha_discovery":        h.app.opts.Publish.HomeAssistantDiscovery,
		"ha_discovery_prefix": h.app.opts.Publish.HADiscoveryPrefix,
		"ha_buttons":          h.app.opts.Publish.HAButtons,
		"ha_switches":         h.app.opts.Publish.HASwitches,
		"source":              src2,
		"first_time_keys":     first,
		"warning":             firstTimeWarning(first),
		"runtime_path":        h.app.runtime.pathString(),
		"backups":             h.app.runtime.ListBackups(),
	})
}

func firstTimeWarning(keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	return "These keys are now owned by runtime file (env ignored for them): " + strings.Join(keys, ", ")
}

func (h *httpAPIServer) handleConfigBackups(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"backups": h.app.runtime.ListBackups()})
}

func (h *httpAPIServer) handleConfigRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.effectiveReadOnly() {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "read_only"})
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	var req struct {
		Slot int `json:"slot"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Slot < 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "slot required (1-3)"})
		return
	}
	if err := h.app.runtime.RestoreBackup(req.Slot); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "restored",
		"slot":           req.Slot,
		"friendly_names": h.effectiveFriendlyCSV(),
		"ignore_list":    h.effectiveIgnoreCSV(),
		"source":         func() RuntimeSource { _, s := h.app.runtime.snapshot(); return s }(),
	})
}

func (h *httpAPIServer) handleSessions(w http.ResponseWriter, r *http.Request) {
	if !h.requireGUISession(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		cur := sessionIDFromRequest(r)
		list := h.app.sessions.List()
		for _, item := range list {
			if id, _ := item["id"].(string); id == cur {
				item["current"] = true
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"sessions": list, "count": len(list)})
	case http.MethodDelete:
		body, _ := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
		var req struct {
			ID  string `json:"id"`
			All bool   `json:"all"`
		}
		_ = json.Unmarshal(body, &req)
		all := req.All || r.URL.Query().Get("all") == "1" || strings.EqualFold(r.URL.Query().Get("all"), "true")
		id := strings.TrimSpace(req.ID)
		if id == "" {
			id = strings.TrimSpace(r.URL.Query().Get("id"))
		}
		cur := sessionIDFromRequest(r)
		if all {
			n := h.app.sessions.RevokeAllExcept(cur)
			log.Info("sessions revoked (all others)", "count", n, "by_prefix", cur[:min(8, len(cur))], "ip", clientIPFrom(r.Context()))
			writeJSON(w, http.StatusOK, map[string]any{"status": "revoked", "count": n})
			return
		}
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id or all required"})
			return
		}
		if h.app.sessions.Revoke(id) {
			log.Info("session revoked by admin",
				"target_prefix", id[:min(8, len(id))],
				"by_prefix", cur[:min(8, len(cur))],
				"ip", clientIPFrom(r.Context()),
			)
			if id == cur {
				clearSessionCookie(w, r)
			}
			writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *httpAPIServer) handleSerialInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	o := h.app.opts
	snap := h.app.publisher.Health()
	writeJSON(w, http.StatusOK, map[string]any{
		"device":              o.Serial.Device,
		"baud":                o.Serial.Baud,
		"read_timeout_sec":    o.Serial.ReadTimeoutSec,
		"reconnect_min_sec":   o.Serial.ReconnectMinDelaySec,
		"reconnect_max_sec":   o.Serial.ReconnectMaxDelaySec,
		"watchdog_sec":        o.Serial.WatchdogSec,
		"ping_interval_sec":   o.Serial.PingIntervalSec,
		"ping_timeout_sec":    o.Serial.PingTimeoutSec,
		"ping_fail_threshold": o.Serial.PingFailThreshold,
		"connected":           snap.SerialConnected,
		"last_serial_at":      snap.LastSerialAt,
		"last_pong_at":        snap.LastPongAt,
		"rflink_unresponsive": snap.RFLinkUnresponsive,
		"note":                "SERIAL_* is deploy-time config (env only); change requires restart",
	})
}

func (h *httpAPIServer) handleWebhooks(w http.ResponseWriter, r *http.Request) {
	if !h.requireGUISession(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		file, src := h.app.runtime.snapshot()
		list := h.app.opts.webhooks
		if src.Webhooks == "runtime" {
			list = file.Webhooks
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"webhooks": webhooksPublicView(list),
			"source":   src.Webhooks,
			"max":      maxWebhooks,
		})
	case http.MethodPost:
		h.createWebhook(w, r)
	case http.MethodPut:
		h.updateWebhook(w, r)
	case http.MethodDelete:
		h.deleteWebhook(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *httpAPIServer) loadWebhookList() []RuntimeWebhook {
	file, src := h.app.runtime.snapshot()
	if src.Webhooks == "runtime" {
		return append([]RuntimeWebhook{}, file.Webhooks...)
	}
	return append([]RuntimeWebhook{}, h.app.opts.webhooks...)
}

func (h *httpAPIServer) persistWebhooks(list []RuntimeWebhook) ([]string, error) {
	if list == nil {
		list = []RuntimeWebhook{}
	}
	if len(list) > maxWebhooks {
		return nil, fmt.Errorf("too many webhooks (max %d)", maxWebhooks)
	}
	// SaveOverlay triggers runtime.onChange → webhooks.Reload() (single reload).
	first, err := h.app.runtime.SaveOverlay(RuntimeFile{Webhooks: list})
	if err != nil {
		return nil, err
	}
	return first, nil
}

func (h *httpAPIServer) createWebhook(w http.ResponseWriter, r *http.Request) {
	if h.effectiveReadOnly() {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "read_only"})
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	var req RuntimeWebhook
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if err := ValidateWebhookConfig(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	req.ID = newWebhookID()
	req.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	list := h.loadWebhookList()
	if len(list) >= maxWebhooks {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "max webhooks reached"})
		return
	}
	wantName := strings.ToLower(req.Name)
	for _, existing := range list {
		if strings.ToLower(existing.Name) == wantName {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "name already exists"})
			return
		}
	}
	list = append(list, req)
	first, err := h.persistWebhooks(list)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	log.Info("webhook created", "name", req.Name, "kind", req.Kind, "id_prefix", req.ID[:8])
	writeJSON(w, http.StatusOK, map[string]any{
		"webhook":         webhooksPublicView([]RuntimeWebhook{req})[0],
		"first_time_keys": first,
		"warning":         firstTimeWarning(first),
	})
}

func (h *httpAPIServer) updateWebhook(w http.ResponseWriter, r *http.Request) {
	if h.effectiveReadOnly() {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "read_only"})
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	var req RuntimeWebhook
	if err := json.Unmarshal(body, &req); err != nil || strings.TrimSpace(req.ID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id required"})
		return
	}
	if err := ValidateWebhookConfig(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	list := h.loadWebhookList()
	wantName := strings.ToLower(req.Name)
	for _, existing := range list {
		if existing.ID != req.ID && strings.ToLower(existing.Name) == wantName {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "name already exists"})
			return
		}
	}
	found := false
	for i := range list {
		if list[i].ID == req.ID {
			req.CreatedAt = list[i].CreatedAt
			// Preserve header values if client omitted values (only sent names)
			if len(req.Headers) == 0 && len(list[i].Headers) > 0 {
				req.Headers = list[i].Headers
			}
			list[i] = req
			found = true
			break
		}
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	if _, err := h.persistWebhooks(list); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	log.Info("webhook updated", "name", req.Name, "kind", req.Kind)
	writeJSON(w, http.StatusOK, map[string]any{"webhook": webhooksPublicView([]RuntimeWebhook{req})[0]})
}

func (h *httpAPIServer) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	if h.effectiveReadOnly() {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "read_only"})
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		body, _ := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
		var req struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(body, &req)
		id = strings.TrimSpace(req.ID)
	}
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id required"})
		return
	}
	list := h.loadWebhookList()
	filtered := list[:0]
	for _, hks := range list {
		if hks.ID != id {
			filtered = append(filtered, hks)
		}
	}
	if len(filtered) == len(list) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	if _, err := h.persistWebhooks(filtered); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	log.Info("webhook deleted", "id_prefix", id[:min(8, len(id))])
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *httpAPIServer) handleAPITokens(w http.ResponseWriter, r *http.Request) {
	// Only GUI session may manage API tokens (not machine API tokens).
	if !h.requireGUISession(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.listAPITokens(w)
	case http.MethodPost:
		h.createAPIToken(w, r)
	case http.MethodDelete:
		h.deleteAPIToken(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *httpAPIServer) listAPITokens(w http.ResponseWriter) {
	file, src := h.app.runtime.snapshot()
	var list []map[string]any
	tokens := h.app.opts.apiTokens
	if src.APITokens == "runtime" {
		for _, t := range file.APITokens {
			list = append(list, map[string]any{
				"name": t.Name, "suffix": t.TokenSuffix,
				"expires_at": t.ExpiresAt, "created_at": t.CreatedAt,
				"scopes": t.Scopes,
			})
		}
	} else {
		for _, t := range tokens {
			var exp any
			if t.ExpiresAt != nil {
				exp = t.ExpiresAt.Format(time.RFC3339)
			}
			list = append(list, map[string]any{
				"name": t.Name, "suffix": t.Suffix,
				"expires_at": exp, "created_at": t.CreatedAt.Format(time.RFC3339),
				"from_env": true, "scopes": t.Scopes,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": list, "source": src.APITokens})
}

func (h *httpAPIServer) createAPIToken(w http.ResponseWriter, r *http.Request) {
	if h.effectiveReadOnly() {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "read_only"})
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	var req struct {
		Name   string   `json:"name"`
		Expire string   `json:"expire"` // never | 30m | 24h | RFC3339
		Scopes []string `json:"scopes"` // default: read, command
	}
	if err := json.Unmarshal(body, &req); err != nil || strings.TrimSpace(req.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name required"})
		return
	}
	secret, err := generateAPITokenSecret()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "token_generate_failed"})
		return
	}
	exp := parseTokenExpiry([]string{"", "", req.Expire}, 2)
	var expStr *string
	if exp != nil {
		s := exp.Format(time.RFC3339)
		expStr = &s
	}
	scopes := req.Scopes
	if len(scopes) == 0 {
		scopes = append([]string{}, defaultAPITokenScopes...)
	} else {
		scopes = parseScopeList(strings.Join(scopes, ","))
	}
	suffix := secret[len(secret)-4:]
	entry := RuntimeAPIToken{
		Name:        strings.TrimSpace(req.Name),
		TokenHash:   hashAPIToken(secret, h.app.opts.APITokenPepper),
		TokenSuffix: suffix, ExpiresAt: expStr,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Scopes:    scopes,
	}

	file, src := h.app.runtime.snapshot()
	var list []RuntimeAPIToken
	if src.APITokens == "runtime" {
		list = append([]RuntimeAPIToken{}, file.APITokens...)
	} else {
		// First time: materialize env tokens into runtime, then add new
		for _, t := range h.app.opts.apiTokens {
			var es *string
			if t.ExpiresAt != nil {
				s := t.ExpiresAt.Format(time.RFC3339)
				es = &s
			}
			list = append(list, RuntimeAPIToken{
				Name: t.Name, TokenHash: t.Hash, TokenSuffix: t.Suffix,
				ExpiresAt: es, CreatedAt: t.CreatedAt.Format(time.RFC3339),
				Scopes: t.Scopes,
			})
		}
	}
	wantName := strings.ToLower(entry.Name)
	for _, existing := range list {
		if strings.ToLower(existing.Name) == wantName {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "name already exists"})
			return
		}
	}
	list = append(list, entry)
	first, err := h.app.runtime.SaveOverlay(RuntimeFile{APITokens: list})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	log.Info("api token created", "name", entry.Name, "first_time_keys", first)
	writeJSON(w, http.StatusOK, map[string]any{
		"name":            entry.Name,
		"token":           secret, // shown once
		"suffix":          suffix,
		"expires_at":      expStr,
		"scopes":          scopes,
		"first_time_keys": first,
		"warning":         firstTimeWarning(first),
		"note":            "Store this token now; it will not be shown again",
	})
}

func (h *httpAPIServer) deleteAPIToken(w http.ResponseWriter, r *http.Request) {
	if h.effectiveReadOnly() {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "read_only"})
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		body, _ := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
		var req struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(body, &req)
		name = strings.TrimSpace(req.Name)
	}
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name required"})
		return
	}
	file, src := h.app.runtime.snapshot()
	var list []RuntimeAPIToken
	if src.APITokens == "runtime" {
		list = file.APITokens
	} else {
		for _, t := range h.app.opts.apiTokens {
			var es *string
			if t.ExpiresAt != nil {
				s := t.ExpiresAt.Format(time.RFC3339)
				es = &s
			}
			list = append(list, RuntimeAPIToken{
				Name: t.Name, TokenHash: t.Hash, TokenSuffix: t.Suffix,
				ExpiresAt: es, CreatedAt: t.CreatedAt.Format(time.RFC3339),
				Scopes: t.Scopes,
			})
		}
	}
	filtered := list[:0]
	for _, t := range list {
		if !strings.EqualFold(t.Name, name) {
			filtered = append(filtered, t)
		}
	}
	if _, err := h.app.runtime.SaveOverlay(RuntimeFile{APITokens: filtered}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	log.Info("api token deleted", "name", name)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *httpAPIServer) handleGUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	_ = h.issueCSRF(w, r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(guiHTML))
}

func isNonLocalListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// ":8080" form
		if strings.HasPrefix(addr, ":") {
			return true
		}
		return true
	}
	host = strings.TrimSpace(host)
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip != nil {
		return !ip.IsLoopback()
	}
	return strings.ToLower(host) != "localhost"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// Embedded status UI — token, CSRF, WS raw, pause, filter, command history.
const guiHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1"/>
<title>go-rflink</title>
<style>
:root { --bg:#0f1419; --card:#1a2332; --fg:#e7ecf3; --muted:#8b9bb4; --ok:#3dd68c; --bad:#f07178; --acc:#59c2ff; --track:#0a0e14; }
* { box-sizing:border-box; }
body { margin:0; font:14px/1.45 system-ui,sans-serif; background:var(--bg); color:var(--fg); }
header { padding:16px 20px; border-bottom:1px solid #243044; display:flex; gap:12px; align-items:center; flex-wrap:wrap; }
header h1 { margin:0; font-size:18px; font-weight:600; }
.badge { padding:2px 8px; border-radius:999px; font-size:12px; background:#243044; color:var(--muted); }
.badge.on { background:#143d2c; color:var(--ok); }
.badge.off { background:#3d1a1f; color:var(--bad); }
main { display:grid; grid-template-columns:repeat(auto-fit,minmax(280px,1fr)); gap:16px; padding:16px; }
.card { background:var(--card); border-radius:10px; padding:14px 16px; border:1px solid #243044; }
.card h2 { margin:0 0 10px; font-size:13px; text-transform:uppercase; letter-spacing:.04em; color:var(--muted); }
.row { display:flex; justify-content:space-between; gap:8px; padding:4px 0; border-bottom:1px solid #24304422; }
.row:last-child { border-bottom:0; }
.mono { font-family:ui-monospace,SFMono-Regular,Menlo,monospace; font-size:12px; word-break:break-all; }
input, textarea, button { font:inherit; }
input, textarea { width:100%; background:#0f1419; color:var(--fg); border:1px solid #243044; border-radius:6px; padding:8px 10px; }
button { background:var(--acc); color:#0a1220; border:0; border-radius:6px; padding:8px 14px; cursor:pointer; font-weight:600; }
button.secondary { background:#243044; color:var(--fg); }
button:disabled { opacity:.45; cursor:not-allowed; }
.stack { display:flex; flex-direction:column; gap:8px; }
pre.raw {
  max-height:320px; overflow:auto; background:var(--track); padding:10px; border-radius:6px; margin:0; font-size:11px;
  color:var(--fg); border:1px solid #243044;
  scrollbar-width: thin; scrollbar-color: #3a4a63 var(--track);
}
pre.raw::-webkit-scrollbar { width:10px; height:10px; }
pre.raw::-webkit-scrollbar-track { background:var(--track); border-radius:6px; }
pre.raw::-webkit-scrollbar-thumb { background:#3a4a63; border-radius:6px; border:2px solid var(--track); }
pre.raw::-webkit-scrollbar-thumb:hover { background:#526480; }
.err { color:var(--bad); }
.ok { color:var(--ok); }
table { width:100%; border-collapse:collapse; font-size:12px; }
td, th { text-align:left; padding:4px 6px; border-bottom:1px solid #243044; }
.auth-bar { display:flex; gap:8px; align-items:center; flex:1; min-width:200px; max-width:420px; }
.auth-bar input { flex:1; }
.hint { font-size:12px; color:var(--muted); }
.toolbar { display:flex; gap:8px; flex-wrap:wrap; align-items:center; margin-bottom:8px; }
.iconbtn { padding:4px 8px; font-size:12px; min-width:auto; }
.hist-row { display:flex; gap:6px; align-items:flex-start; padding:4px 0; border-bottom:1px solid #24304433; }
.hist-row code { flex:1; }
.toggle {
  display:inline-flex; align-items:center; gap:10px; cursor:pointer; user-select:none;
  background:#0f1419; border:1px solid #243044; border-radius:999px; padding:4px 12px 4px 4px;
}
.toggle input { position:absolute; opacity:0; width:0; height:0; }
.toggle .knob {
  width:36px; height:20px; border-radius:999px; background:#3a4a63; position:relative; transition:.15s;
}
.toggle .knob::after {
  content:""; position:absolute; top:2px; left:2px; width:16px; height:16px; border-radius:50%;
  background:#e7ecf3; transition:.15s;
}
.toggle input:checked + .knob { background:#1f6f4a; }
.toggle input:checked + .knob::after { left:18px; background:var(--ok); }
.toggle .lbl { font-size:12px; color:var(--muted); }
.toggle input:checked ~ .lbl { color:var(--ok); }
.modal-backdrop {
  display:none; position:fixed; inset:0; background:rgba(0,0,0,.55); z-index:100;
  align-items:center; justify-content:center; padding:16px;
}
.modal-backdrop.show { display:flex; }
.modal {
  background:var(--card); border:1px solid #243044; border-radius:12px; padding:18px 20px;
  max-width:360px; width:100%; box-shadow:0 12px 40px rgba(0,0,0,.4);
}
.modal h3 { margin:0 0 8px; font-size:16px; }
.modal p { margin:0 0 16px; color:var(--muted); font-size:13px; }
.modal-actions { display:flex; gap:8px; justify-content:flex-end; }
.tabs {
  display:flex; gap:6px; padding:8px 16px 0; border-bottom:1px solid #1e2a3a;
  background:var(--bg);
}
.tab {
  background:transparent; border:none; color:var(--muted); padding:10px 14px;
  border-bottom:2px solid transparent; border-radius:0; font-weight:600; font-size:13px;
}
.tab:hover { color:var(--fg); background:transparent; }
.tab.active { color:var(--acc); border-bottom-color:var(--acc); }
.tab-panel { display:none; }
.tab-panel.active { display:contents; }
#tokList .hist-row, #whList .hist-row {
  background:#0f1419; border:1px solid #1e2a3a; border-radius:8px;
  padding:8px 10px; margin:6px 0;
}
#tokList code, #whList code { min-width:7em; }
.pill {
  display:inline-block; font-size:11px; font-family:ui-monospace,Menlo,Consolas,monospace;
  padding:2px 8px; border-radius:999px; border:1px solid #2a3a52; background:#121820;
  color:var(--muted); margin:0 3px 0 0; vertical-align:middle; white-space:nowrap;
}
.pill.kind { color:#9ecbff; border-color:#2a4a6a; background:#0f1a28; }
.pill.method { color:#c4b5fd; border-color:#3a2a5a; background:#16122a; }
.pill.scope { color:#86efac; border-color:#1f4a36; background:#0f1f18; }
.pill.exp { color:#fcd34d; border-color:#4a3a1a; background:#1a160c; }
.pill.payload { color:#fda4af; border-color:#4a2a32; background:#1a1014; }
.pill.suffix { color:var(--fg); }
.pill.url { color:#93c5fd; max-width:280px; overflow:hidden; text-overflow:ellipsis; }
.token-secret-box {
  display:flex; align-items:stretch; gap:8px; margin:8px 0;
  background:#0a1620; border:1px solid var(--acc); border-radius:8px; padding:10px 12px;
  box-shadow:0 0 0 1px rgba(61,139,253,.25), inset 0 0 24px rgba(61,139,253,.06);
}
.token-secret-box code {
  flex:1; word-break:break-all; color:#e7f0ff; font-size:13px; cursor:pointer;
  user-select:all; line-height:1.4;
}
.token-secret-box code:hover { color:#fff; }
.token-secret-box .copybtn {
  flex-shrink:0; align-self:center; padding:6px 10px; font-size:12px;
}
.token-secret-note { font-size:12px; color:var(--muted); margin-bottom:4px; }
.hist-row .pills { display:flex; flex-wrap:wrap; gap:4px; flex:1; align-items:center; }
</style>
</head>
<body>
<header>
  <h1>go-rflink</h1>
  <span id="ver" class="badge">…</span>
  <span id="mqtt" class="badge">MQTT</span>
  <span id="serial" class="badge" style="cursor:pointer" title="Serial info">Serial</span>
  <span id="rflink" class="badge">RFLink</span>
  <span id="uptime" class="badge">uptime</span>
  <span id="rateBadge" class="badge">rate</span>
  <span id="roBadge" class="badge" style="display:none">read-only</span>
  <span id="lockBadge" class="badge off" style="display:none">GUI locked</span>
  <div class="auth-bar" id="authBar" title="Stored in this browser only (localStorage)">
    <input id="token" type="password" placeholder="GUI password (HTTP_AUTH_TOKEN)" autocomplete="off" spellcheck="false"/>
    <button type="button" class="secondary" id="authBtn">Login</button>
  </div>
  <button type="button" class="secondary" id="pauseBtn">Pause</button>
</header>
<nav class="tabs" id="tabNav" aria-label="Sections">
  <button type="button" class="tab active" data-tab="main">Main</button>
  <button type="button" class="tab" data-tab="tokens">API tokens</button>
  <button type="button" class="tab" data-tab="webhooks">Webhooks</button>
</nav>
<div id="lockWarn" class="card" style="display:none;margin:12px 16px;border-color:var(--bad)">
  <strong>GUI locked</strong>
  <p class="hint" style="margin:6px 0 0">HTTP_AUTH_TOKEN is not configured. Admin features, raw serial, WebSocket and API token management are disabled. Set <code>HTTP_AUTH_TOKEN</code> and restart to unlock.</p>
</div>
<main>
<div class="tab-panel active" id="tab-main">
  <section class="card">
    <h2>Health</h2>
    <div id="health" class="mono">loading…</div>
  </section>
  <section class="card">
    <h2>Sensors (last seen)</h2>
    <div id="sensors" class="mono">loading…</div>
  </section>
  <section class="card">
    <h2>Send command</h2>
    <div class="stack">
      <input id="cmd" placeholder="10;PING;" spellcheck="false"/>
      <button id="send">Send</button>
      <button type="button" class="secondary" id="rediscover">HA rediscover</button>
      <div id="cmdOut" class="mono"></div>
      <div class="hint">Rate limit: <span id="rateInfo">…</span></div>
      <div class="toolbar" style="margin-top:8px">
        <h2 style="margin:0;flex:1">Command history</h2>
        <button type="button" class="secondary iconbtn" id="histClear" title="Clear history">Clear</button>
      </div>
      <div id="cmdHist" class="mono"></div>
    </div>
  </section>
  <section class="card">
    <h2>Config (runtime)</h2>
    <div class="stack">
      <label class="hint">Friendly names (HWID:Name,…)</label>
      <textarea id="fn" placeholder="EV1527_0B25F2:Front Door,LACROSSEV4_0002:Bedroom" rows="2" spellcheck="false"></textarea>
      <label class="hint">Ignore list</label>
      <input id="ig" placeholder="Keeloq,NOISE_MODEL,FFFF" spellcheck="false"/>
      <label class="toggle" title="PUBLISH_HOME_ASSISTANT_DISCOVERY">
        <input type="checkbox" id="haDisc"/>
        <span class="knob"></span>
        <span class="lbl">Home Assistant discovery</span>
      </label>
      <label class="hint">HA discovery prefix</label>
      <input id="haPfx" placeholder="homeassistant" spellcheck="false"/>
      <label class="hint">HA buttons (Label:cmd,…)</label>
      <textarea id="haBtn" placeholder="Siren On:10;NewKaku;123456;ON;" rows="2" spellcheck="false"></textarea>
      <label class="hint">HA switches (Label:on:off,…)</label>
      <textarea id="haSw" placeholder="Garden Light:10;NewKaku;AABB;ON;:10;NewKaku;AABB;OFF;" rows="2" spellcheck="false"></textarea>
      <button id="saveCfg" class="secondary">Save config</button>
      <div id="cfgOut" class="mono"></div>
      <div class="hint">Backups (last 3)</div>
      <div id="bakList" class="mono"></div>
      <div class="hint" id="cfgSource"></div>
    </div>
  </section>
  <section class="card">
    <h2>Sessions</h2>
    <div class="stack">
      <div class="toolbar">
        <span class="hint">Active GUI sessions (72h TTL)</span>
        <button type="button" class="secondary iconbtn" id="sessRevokeAll">Revoke others</button>
      </div>
      <div id="sessList" class="mono"></div>
    </div>
  </section>
  </div><!-- /tab-main -->
<div class="tab-panel" id="tab-tokens">
  <section class="card" style="grid-column:1/-1">
    <h2>API tokens</h2>
    <div class="stack">
      <div class="hint">Machine Bearer tokens for automation clients (not the GUI password). Secret is shown <strong>once</strong> on create. Default scopes: <code>read,command</code>.</div>
      <div class="toolbar" style="flex-wrap:wrap">
        <input id="tokName" placeholder="name" style="min-width:140px;flex:1;max-width:200px" spellcheck="false"/>
        <input id="tokExp" placeholder="expire: never | 24h | 30d" style="min-width:160px;flex:1;max-width:220px" spellcheck="false"/>
        <input id="tokScopes" placeholder="scopes: read,command,admin" style="min-width:180px;flex:1;max-width:280px" title="Default: read,command. Add admin for config write." spellcheck="false"/>
        <button type="button" id="tokCreate">Create token</button>
      </div>
      <div id="tokOut" class="mono"></div>
      <div id="tokList" class="mono"></div>
      <div class="hint" id="tokSource"></div>
    </div>
  </section>
</div><!-- /tab-tokens -->
<div class="tab-panel" id="tab-webhooks">
  <section class="card" style="grid-column:1/-1">
    <h2>Webhooks</h2>
<div class="hint">Outbound hooks (app initiates). Poll: pull command text → RFLink. Push: send raw/sumJson. GET URL may use <code>%payload%</code> / <code>%raw%</code> / <code>%json%</code> (or auto <code>?raw=</code>/<code>?json=</code>). Max 8. HTTPS preferred; http for private IP or .local/.lan/.loc.</div>
      <div class="toolbar" style="flex-wrap:wrap">
        <input id="whName" placeholder="name" style="max-width:120px" spellcheck="false"/>
        <select id="whKind" style="background:#0f1419;color:var(--fg);border:1px solid #243044;border-radius:6px;padding:8px">
          <option value="push" selected>push (send data)</option>
          <option value="poll">poll (pull command)</option>
        </select>
        <select id="whMethod" style="background:#0f1419;color:var(--fg);border:1px solid #243044;border-radius:6px;padding:8px">
          <option value="POST" selected>POST</option>
          <option value="GET">GET</option>
        </select>
        <input id="whURL" placeholder="https://example.com/hook" style="min-width:220px;flex:1" spellcheck="false"/>
        <input id="whInterval" placeholder="interval s (poll)" style="max-width:110px" spellcheck="false"/>
        <select id="whPayload" style="background:#0f1419;color:var(--fg);border:1px solid #243044;border-radius:6px;padding:8px">
          <option value="sumjson">sumjson</option>
          <option value="raw">raw</option>
        </select>
        <label class="toggle" title="enabled">
          <input type="checkbox" id="whEnabled" checked/>
          <span class="knob"></span>
          <span class="lbl">enabled</span>
        </label>
        <button type="button" id="whCreate">Add webhook</button>
      </div>
      <div class="hint">Optional header: <code>Name: Value</code> (secret values stored in runtime file, not shown again)</div>
      <input id="whHeader" placeholder="Authorization: Bearer …" spellcheck="false"/>
      <div id="whOut" class="mono"></div>
      <div id="whList" class="mono"></div>
      <div class="hint" id="whSource"></div>
    </div>
  </section>
</div><!-- /tab-webhooks -->

<div class="tab-panel active" id="tab-main-cont">
<section class="card" style="grid-column:1/-1">
    <h2>Recent serial (raw)</h2>
    <div class="toolbar">
      <button type="button" class="secondary" id="rawCopy">Copy</button>
      <button type="button" class="secondary" id="rawDownload">Download</button>
      <input id="rawFilter" placeholder="Filter…" style="max-width:220px" spellcheck="false"/>
      <span id="wsBadge" class="badge">WS …</span>
      <span id="rawMeta" class="hint"></span>
      <span id="rawActionOut" class="mono hint"></span>
    </div>
    <pre class="raw mono" id="raw">loading…</pre>
  </section>
</div><!-- /tab-main-cont -->
</main>
<div class="modal-backdrop" id="confirmModal" role="dialog" aria-modal="true">
  <div class="modal">
    <h3 id="confirmTitle">Confirm</h3>
    <p id="confirmMsg"></p>
    <div class="modal-actions">
      <button type="button" class="secondary" id="confirmCancel">Cancel</button>
      <button type="button" id="confirmOk">OK</button>
    </div>
  </div>
</div>
<script>
const $ = id => document.getElementById(id);
const LS_HIST = 'gorflink_cmd_hist';
const POLL_HEALTH_MS = 5000;
const POLL_SENSORS_MS = 5000;
const POLL_RATE_MS = 2000;
const POLL_CONFIG_MS = 15000;

let csrfToken = '';
let authBlocked = false;
let paused = false;
let readOnly = false;
let meta = {};
let ws = null;
let wsTimer = null;
let sessionOK = false;
let cfgDirty = false;
window.__rawLines = [];
window.__cmdHist = [];

function syncAuthUI() {
  $('authBtn').textContent = sessionOK ? 'Logout' : 'Login';
  $('token').style.display = sessionOK ? 'none' : '';
}

$('authBtn').onclick = async () => {
  if (sessionOK) {
    try {
      await ensureCSRF();
      await fetch('/api/v1/session', { method:'DELETE', credentials:'same-origin',
        headers: csrfToken ? {'X-CSRF-Token': csrfToken} : {} });
    } catch(e) {}
    sessionOK = false;
    authBlocked = true;
    syncAuthUI();
    closeWS();
    $('token').value = '';
    $('health').innerHTML = '<span class="hint">Logged out.</span>';
    return;
  }
  const t = $('token').value.trim();
  if (!t) { $('health').innerHTML = '<span class="err">Enter GUI password</span>'; return; }
  try {
    await ensureCSRF();
    const r = await fetch('/api/v1/session', {
      method:'POST', credentials:'same-origin',
      headers: Object.assign({'Content-Type':'application/json'}, csrfToken ? {'X-CSRF-Token': csrfToken} : {}),
      body: JSON.stringify({token: t})
    });
    const body = await r.json().catch(() => ({}));
    if (!r.ok) {
      if (r.status === 429) throw new Error('Too many attempts — wait '+(body.retry_after_sec||60)+'s');
      throw new Error(body.error || r.statusText);
    }
    if (body.csrf_token) csrfToken = body.csrf_token;
    $('token').value = ''; // never keep password in the field
    sessionOK = true;
    authBlocked = false;
    syncAuthUI();
    bootstrap();
  } catch(e) {
    $('health').innerHTML = '<span class="err">Login failed: '+(e.message||e)+'</span>';
  }
};
$('token').addEventListener('keydown', e => { if (e.key === 'Enter') $('authBtn').click(); });
document.addEventListener('change', e => {
  if (e.target && e.target.id === 'haDisc' && e.target.checked) {
    if (!($('haPfx').value||'').trim()) $('haPfx').value = 'homeassistant';
  }
  if (e.target && ['fn','ig','haDisc','haPfx','haBtn','haSw'].includes(e.target.id)) cfgDirty = true;
});
document.addEventListener('input', e => {
  if (e.target && ['fn','ig','haPfx','haBtn','haSw'].includes(e.target.id)) cfgDirty = true;
});

$('serial').onclick = async () => {
  try {
    let info;
    if (authBlocked || meta.gui_locked) {
      info = { note: 'Login required for full serial details', device: '(auth required)' };
    } else {
      info = await j('/api/v1/serial');
    }
    const rows = [
      ['device', info.device],
      ['baud', info.baud],
      ['connected', info.connected],
      ['last_serial_at', info.last_serial_at],
      ['last_pong_at', info.last_pong_at],
      ['watchdog_sec', info.watchdog_sec],
      ['ping_interval_sec', info.ping_interval_sec],
      ['note', info.note || ''],
    ];
    await confirmDialog('Serial connection', rows.map(([k,v]) => k+': '+(v===undefined||v===null?'':v)).join(String.fromCharCode(10)));
  } catch(e) {
    await confirmDialog('Serial connection', e.message || 'unavailable');
  }
};

$('pauseBtn').onclick = () => {
  paused = !paused;
  $('pauseBtn').textContent = paused ? 'Resume' : 'Pause';
  if (!paused) {
    tickHealth(); tickSensors(); tickRate();
    ensureWS();
  } else {
    closeWS();
  }
};

function cookie(name) {
  const m = document.cookie.match(new RegExp('(?:^|; )' + name.replace(/[$()*+.?[\\\]^{|}]/g,'\\$&') + '=([^;]*)'));
  return m ? decodeURIComponent(m[1]) : '';
}
async function ensureCSRF() {
  csrfToken = cookie('gorflink_csrf');
  if (csrfToken) return;
  try {
    const r = await fetch('/api/v1/csrf', {credentials:'same-origin'});
    const j = await r.json();
    csrfToken = j.csrf_token || cookie('gorflink_csrf') || '';
  } catch (e) {}
}

async function j(url, opts) {
  if (authBlocked) throw Object.assign(new Error('Waiting for Login'), {status:401});
  opts = opts || {};
  opts.credentials = 'same-origin';
  opts.headers = Object.assign({}, opts.headers || {});
  if (csrfToken && opts.method && opts.method !== 'GET') opts.headers['X-CSRF-Token'] = csrfToken;
  const r = await fetch(url, opts);
  const t = await r.text();
  let body; try { body = JSON.parse(t); } catch { body = t; }
  if (r.status === 401) {
    authBlocked = true;
    closeWS();
    throw Object.assign(new Error('Unauthorized — enter API token and press Login'), {status:401, body});
  }
  if (r.status === 403 && body && body.error === 'csrf_failed') {
    csrfToken = ''; await ensureCSRF();
    throw Object.assign(new Error('CSRF failed — retry'), {status:403, body});
  }
  if (r.status === 403 && body && body.error === 'read_only') {
    throw Object.assign(new Error('Server is read-only'), {status:403, body});
  }
  if (!r.ok) throw Object.assign(new Error(r.statusText || 'error'), {status:r.status, body});
  return body;
}

function badge(el, on, label) {
  el.textContent = label;
  el.className = 'badge ' + (on ? 'on' : 'off');
}

function applyReadOnlyUI() {
  const ro = !!readOnly;
  $('roBadge').style.display = ro ? '' : 'none';
  if (ro) $('roBadge').textContent = 'read-only';
  $('send').disabled = ro;
  $('saveCfg').disabled = ro;
  $('cmd').disabled = ro;
  if ($('tokCreate')) $('tokCreate').disabled = ro;
  if ($('rediscover')) $('rediscover').disabled = ro;
  if ($('tokName')) $('tokName').disabled = ro;
  if ($('tokExp')) $('tokExp').disabled = ro;
}

function renderRate(lim, rem, unlimited) {
  if (unlimited) {
    $('rateBadge').textContent = 'rate unlimited';
    $('rateBadge').className = 'badge on';
    $('rateInfo').textContent = 'unlimited';
    return;
  }
  const r = Math.max(0, rem);
  $('rateInfo').textContent = r.toFixed(1) + ' / ' + lim + ' tokens/sec';
  $('rateBadge').textContent = 'rate ' + r.toFixed(1) + '/' + lim;
  $('rateBadge').className = 'badge ' + (r < 1 ? 'off' : 'on');
}

function loadHist() {
  try { window.__cmdHist = JSON.parse(localStorage.getItem(LS_HIST) || '[]'); } catch { window.__cmdHist = []; }
  renderHist();
}
function pushHist(cmd, status) {
  cmd = (cmd || '').trim();
  if (!cmd) return;
  // Dedupe by command text: move existing entry to top instead of cloning.
  window.__cmdHist = (window.__cmdHist || []).filter(h => (h.cmd || '').trim() !== cmd);
  window.__cmdHist.unshift({cmd, status, at: new Date().toISOString()});
  window.__cmdHist = window.__cmdHist.slice(0, 30);
  localStorage.setItem(LS_HIST, JSON.stringify(window.__cmdHist));
  renderHist();
}

function confirmDialog(title, msg) {
  return new Promise(resolve => {
    const backdrop = $('confirmModal');
    const ok = $('confirmOk');
    const cancel = $('confirmCancel');
    if (!backdrop || !ok || !cancel) {
      resolve(window.confirm(title + String.fromCharCode(10) + (msg || '')));
      return;
    }
    $('confirmTitle').textContent = title || 'Confirm';
    $('confirmMsg').textContent = msg || '';
    backdrop.classList.add('show');
    const cleanup = (val) => {
      backdrop.classList.remove('show');
      ok.removeEventListener('click', onOk);
      cancel.removeEventListener('click', onCancel);
      backdrop.removeEventListener('click', onBack);
      document.removeEventListener('keydown', onKey);
      resolve(val);
    };
    const onOk = (e) => { e.preventDefault(); e.stopPropagation(); cleanup(true); };
    const onCancel = (e) => { e.preventDefault(); e.stopPropagation(); cleanup(false); };
    const onBack = (e) => { if (e.target === backdrop) cleanup(false); };
    const onKey = (e) => {
      if (e.key === 'Escape') cleanup(false);
      if (e.key === 'Enter') cleanup(true);
    };
    ok.addEventListener('click', onOk);
    cancel.addEventListener('click', onCancel);
    backdrop.addEventListener('click', onBack);
    document.addEventListener('keydown', onKey);
    setTimeout(() => ok.focus(), 0);
  });
}

$('histClear').onclick = async () => {
  if (!(window.__cmdHist || []).length) return;
  const ok = await confirmDialog('Clear command history?', 'This removes all saved commands from this browser. It cannot be undone.');
  if (!ok) return;
  window.__cmdHist = [];
  localStorage.removeItem(LS_HIST);
  renderHist();
};
function renderHist() {
  const box = $('cmdHist');
  if (!window.__cmdHist.length) { box.innerHTML = '<span class="hint">(empty)</span>'; return; }
  box.innerHTML = window.__cmdHist.map((h, i) =>
    '<div class="hist-row"><code>'+esc(h.cmd)+'</code>'+
    '<span class="hint">'+(h.status||'')+'</span>'+
    '<button type="button" class="secondary iconbtn" data-i="'+i+'" title="Resend">↻</button></div>'
  ).join('');
  box.querySelectorAll('button[data-i]').forEach(btn => {
    btn.onclick = () => {
      if (readOnly || authBlocked) return;
      const item = window.__cmdHist[+btn.getAttribute('data-i')];
      if (item) { $('cmd').value = item.cmd; sendCommand(item.cmd); }
    };
  });
}
function esc(s) {
  return String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
}

function renderRaw() {
  const filter = ($('rawFilter').value || '').trim().toLowerCase();
  let lines = window.__rawLines || [];
  if (filter) lines = lines.filter(l => (l.message||'').toLowerCase().includes(filter) || (l.at||'').toLowerCase().includes(filter));
  const view = lines.slice(-200);
  $('raw').textContent = view.map(l => l.at + '  ' + l.message).join(String.fromCharCode(10)) || '(empty)';
  $('raw').scrollTop = $('raw').scrollHeight;
  $('rawMeta').textContent = (window.__rawLines||[]).length + ' buffered' + (filter ? ' / ' + lines.length + ' matched' : '');
}
$('rawFilter').addEventListener('input', renderRaw);

function closeWS() {
  if (wsTimer) { clearTimeout(wsTimer); wsTimer = null; }
  if (ws) { try { ws.close(); } catch(e){} ws = null; }
  badge($('wsBadge'), false, 'WS off');
}
function ensureWS() {
  if (paused || authBlocked || meta.gui_locked) return;
  if (ws && (ws.readyState === 0 || ws.readyState === 1)) return;
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const url = proto + '//' + location.host + '/api/v1/rflink/raw/ws';
  badge($('wsBadge'), false, 'WS connecting');
  try { ws = new WebSocket(url); } catch(e) { badge($('wsBadge'), false, 'WS error'); scheduleWS(); return; }
  ws.onopen = () => badge($('wsBadge'), true, 'WS live');
  ws.onclose = () => { badge($('wsBadge'), false, 'WS closed'); scheduleWS(); };
  ws.onerror = () => { badge($('wsBadge'), false, 'WS error'); };
  ws.onmessage = (ev) => {
    let msg; try { msg = JSON.parse(ev.data); } catch { return; }
    if (msg.type === 'hello' && Array.isArray(msg.buffer)) {
      window.__rawLines = msg.buffer;
      renderRaw();
    } else if (msg.type === 'line') {
      window.__rawLines.push({at: msg.at, message: msg.message});
      if (window.__rawLines.length > 500) window.__rawLines = window.__rawLines.slice(-400);
      renderRaw();
    }
  };
}
function scheduleWS() {
  if (paused || authBlocked) return;
  if (wsTimer) clearTimeout(wsTimer);
  wsTimer = setTimeout(ensureWS, 3000);
}

async function tickHealth() {
  if (paused || authBlocked) return;
  try {
    const h = await j('/api/v1/health');
    $('ver').textContent = (h.version||'?') + ' @ ' + (h.git_sha||'?');
    badge($('mqtt'), h.mqtt_connected, h.mqtt_connected ? 'MQTT up' : 'MQTT down');
    badge($('serial'), h.serial_connected, h.serial_connected ? 'Serial up' : 'Serial down');
    badge($('rflink'), !h.rflink_unresponsive, h.rflink_unresponsive ? 'RFLink unresponsive' : 'RFLink OK');
    $('uptime').textContent = 'up ' + (h.uptime_sec||0) + 's';
    const rows = [
      ['published', h.published], ['dedup_dropped', h.dedup_dropped], ['serial_stale', h.serial_stale],
      ['last_sensor', (h.last_sensor_hwid||'') + ' ' + (h.last_sensor_at||'')],
      ['last_serial', h.last_serial_at||''], ['last_pong', h.last_pong_at||''], ['ping_fails', h.ping_fails],
    ];
    $('health').innerHTML = rows.map(([k,v]) => '<div class="row"><span>'+k+'</span><span>'+v+'</span></div>').join('');
  } catch(e) {
    if (e.status === 401) $('health').innerHTML = '<span class="err">'+(e.message||'Unauthorized')+'</span>';
    else $('health').innerHTML = '<span class="err">error: '+(e.message||e)+'</span>';
  }
}
async function tickSensors() {
  if (paused || authBlocked) return;
  try {
    const s = await j('/api/v1/sensors?samples=1');
    const ls = s.last_seen || {};
    const keys = Object.keys(ls).sort();
    if (!keys.length) $('sensors').textContent = '(none yet)';
    else {
      let html = '<table><tr><th>HWID</th><th>last seen</th></tr>';
      for (const k of keys) html += '<tr><td class="mono">'+k+'</td><td>'+ls[k]+'</td></tr>';
      $('sensors').innerHTML = html + '</table>';
    }
  } catch(e) {
    if (e.status !== 401) $('sensors').innerHTML = '<span class="err">error: '+(e.message||e)+'</span>';
  }
}
async function tickRate() {
  if (paused || authBlocked) return;
  try {
    const r = await j('/api/v1/rate_limit');
    readOnly = !!r.read_only;
    applyReadOnlyUI();
    renderRate(r.limit_per_sec, r.remaining, r.unlimited);
  } catch(e) {}
}
async function tickConfig() {
  if (paused || authBlocked || meta.gui_locked) return;
  try {
    const c = await j('/api/v1/config');
    if (!cfgDirty) {
      const active = document.activeElement;
      if (active !== $('fn')) $('fn').value = c.friendly_names||'';
      if (active !== $('ig')) $('ig').value = c.ignore_list||'';
      if (active !== $('haPfx')) $('haPfx').value = c.ha_discovery_prefix||'homeassistant';
      if (active !== $('haBtn')) $('haBtn').value = c.ha_buttons||'';
      if (active !== $('haSw')) $('haSw').value = c.ha_switches||'';
      if (active !== $('haDisc')) $('haDisc').checked = !!c.ha_discovery;
    }
    renderBackups(c.backups || []);
    if (c.source) {
      $('cfgSource').textContent = 'source: friendly='+(c.source.friendly_names||'?')+
        ' ignore='+(c.source.ignore_list||'?')+
        ' ha='+(c.source.ha_discovery||'?')+
        ' path='+(c.runtime_path||'');
    }
  } catch(e) {}
  await tickTokens();
  await tickSessions();
  await tickWebhooks();
}

$('sessRevokeAll').onclick = async () => {
  if (authBlocked || meta.gui_locked) return;
  const ok = await confirmDialog('Revoke all other sessions?', 'You stay logged in; every other browser session ends.');
  if (!ok) return;
  try {
    await ensureCSRF();
    const res = await j('/api/v1/sessions?all=1', {method:'DELETE'});
    tickSessions();
    $('sessList').insertAdjacentHTML('beforebegin','');
  } catch(e) {}
};

async function tickSessions() {
  if (paused || authBlocked || meta.gui_locked) return;
  try {
    const res = await j('/api/v1/sessions');
    const list = res.sessions || [];
    if (!list.length) { $('sessList').innerHTML = '<span class="hint">(none)</span>'; return; }
    $('sessList').innerHTML = list.map(s => {
      const cur = s.current ? ' <span class="ok">current</span>' : '';
      return '<div class="hist-row"><code>'+esc(s.id_prefix)+'…</code>'+
        '<span class="hint">'+esc(s.ip||'')+' '+esc(s.expires_at||'')+cur+'</span>'+
        (s.current ? '' : '<button type="button" class="secondary iconbtn" data-sid="'+esc(s.id)+'">Revoke</button>')+
        '</div>';
    }).join('');
    $('sessList').querySelectorAll('button[data-sid]').forEach(btn => {
      btn.onclick = async () => {
        const ok = await confirmDialog('Revoke session?', 'That browser will be logged out.');
        if (!ok) return;
        try {
          await ensureCSRF();
          await j('/api/v1/sessions?id='+encodeURIComponent(btn.getAttribute('data-sid')), {method:'DELETE'});
          tickSessions();
        } catch(e) {}
      };
    });
  } catch(e) {
    if (e.status !== 401) $('sessList').innerHTML = '<span class="hint">—</span>';
  }
}

async function tickTokens() {
  if (paused || authBlocked) return;
  try {
    const res = await j('/api/v1/tokens');
    const list = res.tokens || [];
    $('tokSource').textContent = 'source: '+(res.source||'?');
    if (!list.length) {
      $('tokList').innerHTML = '<span class="hint">(no API tokens)</span>';
      return;
    }
    $('tokList').innerHTML = list.map(t => {
      const exp = t.expires_at || 'never';
      const scopes = (t.scopes && t.scopes.length) ? t.scopes : ['read','command'];
      const scopePills = scopes.map(s => '<span class="pill scope">'+esc(s)+'</span>').join('');
      const env = t.from_env ? '<span class="pill">env</span>' : '';
      return '<div class="hist-row"><code>'+esc(t.name)+'</code>'+
        '<span class="pills">'+
          '<span class="pill suffix" title="token suffix">…'+esc(t.suffix||'')+'</span>'+
          scopePills+
          '<span class="pill exp">exp '+esc(String(exp))+'</span>'+
          env+
        '</span>'+
        '<button type="button" class="secondary iconbtn" data-del="'+esc(t.name)+'" title="Delete">✕</button></div>';
    }).join('');
    $('tokList').querySelectorAll('button[data-del]').forEach(btn => {
      btn.onclick = async () => {
        const name = btn.getAttribute('data-del');
        const ok = await confirmDialog('Delete API token «'+name+'»?', 'Clients using this token will lose access immediately.');
        if (!ok) return;
        try {
          await ensureCSRF();
          await j('/api/v1/tokens?name='+encodeURIComponent(name), {method:'DELETE'});
          $('tokOut').className = 'mono ok';
          $('tokOut').textContent = 'deleted '+name;
          tickTokens();
        } catch(e) {
          $('tokOut').className = 'mono err';
          $('tokOut').textContent = e.message || (e.body && e.body.error) || 'error';
        }
      };
    });
  } catch(e) {
    if (e.status === 401 && e.body && e.body.error === 'gui_session_required') {
      $('tokList').innerHTML = '<span class="hint">Login required to manage API tokens</span>';
    } else if (e.status !== 401) {
      $('tokList').innerHTML = '<span class="err">'+(e.message||e)+'</span>';
    }
  }
}

$('tokCreate').onclick = async () => {
  if (authBlocked || readOnly) {
    $('tokOut').className = 'mono err';
    $('tokOut').textContent = readOnly ? 'read-only' : 'Login required';
    return;
  }
  const name = ($('tokName').value || '').trim();
  if (!name) {
    $('tokOut').className = 'mono err';
    $('tokOut').textContent = 'name required';
    return;
  }
  try {
    await ensureCSRF();
    const res = await j('/api/v1/tokens', {
      method:'POST',
      headers:{'Content-Type':'application/json'},
      body: JSON.stringify({
        name: name,
        expire: ($('tokExp').value||'never').trim(),
        scopes: (($('tokScopes').value||'').trim() || 'read,command').split(/[,+\s]+/).filter(Boolean)
      })
    });
    $('tokName').value = '';
    const secret = res.token || '';
    const note = res.note || 'Store this token now; it will not be shown again';
    const warn = res.warning ? '<div class="hint">'+esc(res.warning)+'</div>' : '';
    $('tokOut').className = '';
    $('tokOut').innerHTML =
      '<div class="token-secret-note ok">Token created — click to copy</div>'+
      '<div class="token-secret-box">'+
        '<code id="tokSecretVal" title="Click to copy">'+esc(secret)+'</code>'+
        '<button type="button" class="secondary copybtn" id="tokCopyBtn" title="Copy">Copy</button>'+
      '</div>'+
      '<div class="hint">'+esc(note)+'</div>'+warn;
    const doCopy = async () => {
      try {
        await navigator.clipboard.writeText(secret);
        $('tokCopyBtn').textContent = 'Copied';
        setTimeout(() => { if ($('tokCopyBtn')) $('tokCopyBtn').textContent = 'Copy'; }, 1500);
      } catch(err) {
        $('tokCopyBtn').textContent = 'Select & Ctrl+C';
      }
    };
    if ($('tokCopyBtn')) $('tokCopyBtn').onclick = doCopy;
    if ($('tokSecretVal')) $('tokSecretVal').onclick = doCopy;
    tickTokens();
  } catch(e) {
    $('tokOut').className = 'mono err';
    $('tokOut').textContent = (e.body && e.body.error) || e.message || 'error';
  }
};



// Client-side mirrors of server validation (server remains authoritative).
function validateRFLinkCmd(cmd) {
  cmd = (cmd || '').trim();
  if (!cmd) return 'empty command';
  if (!cmd.endsWith(';')) cmd += ';';
  if (/[\x00-\x1f]/.test(cmd)) return 'control characters not allowed';
  const body = cmd.slice(0, -1);
  const parts = body.split(';');
  if (parts.length < 2) return 'need node and at least one field (e.g. 10;PING;)';
  if (!/^[0-9]{1,2}$/.test(parts[0])) return 'invalid node (want 10/11/20 etc.)';
  for (let i = 0; i < parts.length; i++) {
    if (!parts[i]) return 'empty field at position '+i;
    if (parts[i].length > 96) return 'field too long at position '+i;
  }
  return '';
}
function validateFriendlyCSV(raw) {
  raw = (raw || '').trim();
  if (!raw) return '';
  for (const part of raw.split(',').map(s => s.trim()).filter(Boolean)) {
    const i = part.indexOf(':');
    if (i < 1) return 'friendly_names: want HWID:Name in "'+part+'"';
    const k = part.slice(0,i).trim(), v = part.slice(i+1).trim();
    if (!/^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$/.test(k)) return 'friendly_names: bad HWID "'+k+'"';
    if (!v || v.length > 64) return 'friendly_names: bad name "'+v+'"';
  }
  return '';
}
function validateIgnoreCSV(raw) {
  raw = (raw || '').trim();
  if (!raw) return '';
  for (const part of raw.split(',').map(s => s.trim()).filter(Boolean)) {
    if (!/^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$/.test(part)) return 'ignore_list: bad token "'+part+'"';
  }
  return '';
}
function validateHAButtonsCSV(raw) {
  raw = (raw || '').trim();
  if (!raw) return '';
  for (const part of raw.split(',').map(s => s.trim()).filter(Boolean)) {
    const i = part.indexOf(':');
    if (i < 1) return 'ha_buttons: want Label:10;CMD; in "'+part+'"';
    const name = part.slice(0,i).trim();
    let cmd = part.slice(i+1).trim();
    if (!name || !cmd) return 'ha_buttons: empty field in "'+part+'"';
    if (!cmd.endsWith(';')) cmd += ';';
    const e = validateRFLinkCmd(cmd);
    if (e) return 'ha_buttons ['+name+']: '+e;
  }
  return '';
}
function validateHASwitchesCSV(raw) {
  raw = (raw || '').trim();
  if (!raw) return '';
  for (const part of raw.split(',').map(s => s.trim()).filter(Boolean)) {
    const bits = part.split(':');
    if (bits.length < 3) return 'ha_switches: want Label:ON_CMD:OFF_CMD in "'+part+'"';
    const name = bits[0].trim();
    let on = bits[1].trim();
    let off = bits.slice(2).join(':').trim();
    if (!name || !on || !off) return 'ha_switches: empty field in "'+part+'"';
    if (!on.endsWith(';')) on += ';';
    if (!off.endsWith(';')) off += ';';
    let e = validateRFLinkCmd(on); if (e) return 'ha_switches ON ['+name+']: '+e;
    e = validateRFLinkCmd(off); if (e) return 'ha_switches OFF ['+name+']: '+e;
  }
  return '';
}

async function sendCommand(cmd) {
  const verr = validateRFLinkCmd(cmd);
  if (verr) {
    $('cmdOut').className = 'mono err';
    $('cmdOut').textContent = JSON.stringify({status:'rejected', error: verr, command: cmd});
    return;
  }
  $('cmdOut').textContent = 'sending…';
  try {
    await ensureCSRF();
    const res = await j('/api/v1/command', {method:'POST', headers:{'Content-Type':'text/plain'}, body: cmd});
    $('cmdOut').className = 'mono ' + (res.status==='ok'?'ok':'err');
    $('cmdOut').textContent = JSON.stringify(res);
    pushHist(res.command || cmd, res.status);
    if (typeof res.rate_limit_remaining === 'number') {
      renderRate(res.rate_limit_per_sec||0, res.rate_limit_remaining, !res.rate_limit_per_sec);
    }
  } catch(e) {
    $('cmdOut').className = 'mono err';
    $('cmdOut').textContent = JSON.stringify(e.body||e.message);
    pushHist(cmd, 'error');
  }
}
$('send').onclick = () => {
  const cmd = $('cmd').value.trim();
  if (!cmd) return;
  if (authBlocked) { $('cmdOut').className='mono err'; $('cmdOut').textContent='Login required'; return; }
  if (readOnly) { $('cmdOut').className='mono err'; $('cmdOut').textContent='read-only'; return; }
  sendCommand(cmd);
};
$('rediscover').onclick = async () => {
  if (authBlocked || readOnly) {
    $('cmdOut').className='mono err';
    $('cmdOut').textContent = readOnly ? 'read-only' : 'Login required';
    return;
  }
  try {
    await ensureCSRF();
    const res = await j('/api/v1/ha/rediscover', {method:'POST'});
    $('cmdOut').className='mono ok';
    $('cmdOut').textContent = JSON.stringify(res);
  } catch(e) {
    $('cmdOut').className='mono err';
    $('cmdOut').textContent = (e.body && e.body.error) || e.message || 'error';
  }
};


$('saveCfg').onclick = async () => {
  if (authBlocked || readOnly) { $('cfgOut').className='mono err'; $('cfgOut').textContent = readOnly?'read-only':'Login required'; return; }
  const errs = [
    validateFriendlyCSV($('fn').value),
    validateIgnoreCSV($('ig').value),
    validateHAButtonsCSV($('haBtn').value),
    validateHASwitchesCSV($('haSw').value),
  ].filter(Boolean);
  if (errs.length) {
    $('cfgOut').className = 'mono err';
    $('cfgOut').textContent = errs.join(String.fromCharCode(10));
    return;
  }
  try {
    await ensureCSRF();
    let pfx = ($('haPfx').value||'').trim();
    if ($('haDisc').checked && !pfx) pfx = 'homeassistant';
    const res = await j('/api/v1/config', { method:'PUT', headers:{'Content-Type':'application/json'},
      body: JSON.stringify({
        friendly_names: $('fn').value,
        ignore_list: $('ig').value,
        ha_discovery: $('haDisc').checked,
        ha_discovery_prefix: pfx,
        ha_buttons: $('haBtn').value,
        ha_switches: $('haSw').value
      }) });
    cfgDirty = false;
    $('cfgOut').className = 'mono ok';
    let msg = 'saved → runtime file';
    if (res.warning) msg += ' | ' + res.warning;
    $('cfgOut').textContent = msg;
    renderBackups(res.backups || []);
  } catch(e) {
    $('cfgOut').className='mono err';
    if (e.body && e.body.details) $('cfgOut').textContent = (e.body.details||[]).join(String.fromCharCode(10));
    else if (e.body && e.body.error) $('cfgOut').textContent = e.body.error;
    else $('cfgOut').textContent = e.message;
  }
};

function renderBackups(list) {
  const el = $('bakList');
  if (!el) return;
  if (!list || !list.length) { el.innerHTML = '<span class="hint">no backups yet</span>'; return; }
  el.innerHTML = list.map(b =>
    '<div class="hist-row"><span class="hint">#'+b.slot+' '+b.mod_time+'</span>'+
    '<button type="button" class="secondary iconbtn" data-slot="'+b.slot+'">Restore</button></div>'
  ).join('');
  el.querySelectorAll('button[data-slot]').forEach(btn => {
    btn.onclick = async () => {
      const ok = await confirmDialog('Restore backup #'+btn.dataset.slot+'?', 'Current runtime.json will be backed up first, then replaced.');
      if (!ok) return;
      try {
        await ensureCSRF();
        const res = await j('/api/v1/config/restore', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({slot:+btn.dataset.slot})});
        $('fn').value = res.friendly_names||'';
        $('ig').value = res.ignore_list||'';
        $('cfgOut').className='mono ok'; $('cfgOut').textContent = 'restored slot '+btn.dataset.slot;
        tickConfig();
      } catch(e) { $('cfgOut').className='mono err'; $('cfgOut').textContent = e.message; }
    };
  });
}


function rawTextFull() {
  const filter = ($('rawFilter').value || '').trim().toLowerCase();
  let lines = window.__rawLines || [];
  if (filter) lines = lines.filter(l => (l.message||'').toLowerCase().includes(filter) || (l.at||'').toLowerCase().includes(filter));
  return lines.map(l => l.at + '  ' + l.message).join(String.fromCharCode(10));
}
function flashRawAction(msg, ok) {
  const el = $('rawActionOut'); el.textContent = msg; el.className = 'mono ' + (ok?'ok':'err');
  setTimeout(() => { if (el.textContent === msg) el.textContent = ''; }, 2000);
}
$('rawCopy').onclick = async () => {
  const t = rawTextFull(); if (!t) { flashRawAction('nothing to copy', false); return; }
  try { await navigator.clipboard.writeText(t); flashRawAction('copied', true); }
  catch { flashRawAction('copy failed', false); }
};
$('rawDownload').onclick = () => {
  const t = rawTextFull(); if (!t) { flashRawAction('nothing to download', false); return; }
  const stamp = new Date().toISOString().replace(/[:.]/g,'-');
  const a = document.createElement('a');
  a.href = URL.createObjectURL(new Blob([t+'\\n'], {type:'text/plain;charset=utf-8'}));
  a.download = 'gorflink-serial-raw-'+stamp+'.txt';
  document.body.appendChild(a); a.click();
  setTimeout(() => { URL.revokeObjectURL(a.href); a.remove(); }, 0);
  flashRawAction('download started', true);
};

async function bootstrap() {
  await ensureCSRF();
  try {
    meta = await fetch('/api/v1/meta', {credentials:'same-origin'}).then(r => r.json());
    readOnly = !!meta.read_only || !!meta.gui_locked;
    sessionOK = !!meta.authenticated && !meta.gui_locked;
    syncAuthUI();
    applyReadOnlyUI();
    if (meta.gui_locked) {
      authBlocked = true;
      $('health').innerHTML = '<span class="err">GUI locked — set HTTP_AUTH_TOKEN</span>';
      closeWS();
    } else if (meta.gui_auth_required && !meta.authenticated) {
      authBlocked = true;
      $('health').innerHTML = '<span class="err">Login required (GUI password)</span>';
      closeWS();
    } else {
      authBlocked = false;
    }
  } catch(e) {}
  if (!authBlocked && !meta.gui_locked) {
    await tickHealth(); await tickSensors(); await tickRate(); await tickConfig();
    ensureWS();
  } else if (!meta.gui_locked) {
    // still show health only after login
  } else {
    // locked: minimal public probes only
    try {
      const hz = await fetch('/healthz').then(r => r.text());
      $('health').innerHTML = '<span class="hint">healthz: '+hz+' (GUI locked)</span>';
    } catch(e) {}
  }
}


async function tickWebhooks() {
  if (paused || authBlocked || meta.gui_locked) return;
  try {
    const res = await j('/api/v1/webhooks');
    $('whSource').textContent = 'source: '+(res.source||'?')+' · max '+(res.max||8);
    const list = res.webhooks || [];
    if (!list.length) { $('whList').innerHTML = '<span class="hint">(no webhooks)</span>'; return; }
    const byId = {};
    list.forEach(w => { byId[w.id] = w; });
    $('whList').innerHTML = list.map(w => {
      const enLbl = w.enabled ? 'on' : 'off';
      const enCls = w.enabled ? 'ok' : 'err';
      let extraPills = '';
      if (w.kind === 'poll') {
        extraPills = '<span class="pill exp">every '+(w.interval_sec||30)+'s</span>';
      } else {
        extraPills = '<span class="pill payload">'+esc(w.payload||'sumjson')+'</span>';
      }
      return '<div class="hist-row">'+
        '<code>'+esc(w.name)+'</code>'+
        '<span class="pills">'+
          '<span class="pill kind">'+esc(w.kind)+'</span>'+
          '<span class="pill method">'+esc(w.method)+'</span>'+
          '<span class="pill url" title="'+esc(w.url)+'">'+esc(w.url)+'</span>'+
          extraPills+
        '</span>'+
        '<button type="button" class="secondary iconbtn" data-wh-toggle="'+esc(w.id)+'" title="Enable/disable"><span class="'+enCls+'">'+enLbl+'</span></button>'+
        '<button type="button" class="secondary iconbtn" data-wh="'+esc(w.id)+'" title="Delete">✕</button></div>';
    }).join('');
    $('whList').querySelectorAll('button[data-wh-toggle]').forEach(btn => {
      btn.onclick = async () => {
        const id = btn.getAttribute('data-wh-toggle');
        const w = byId[id];
        if (!w) return;
        try {
          await ensureCSRF();
          const body = {
            id: w.id, name: w.name, enabled: !w.enabled, kind: w.kind, url: w.url,
            method: w.method, interval_sec: w.interval_sec||30, timeout_sec: w.timeout_sec||5,
            max_response_bytes: w.max_response_bytes||512, payload: w.payload||'sumjson',
            headers: {}
          };
          await j('/api/v1/webhooks', {method:'PUT', headers:{'Content-Type':'application/json'}, body: JSON.stringify(body)});
          $('whOut').className='mono ok';
          $('whOut').textContent = (body.enabled?'enabled':'disabled')+' · '+w.name;
          tickWebhooks();
        } catch(e) {
          $('whOut').className='mono err';
          $('whOut').textContent=(e.body&&e.body.error)||e.message;
        }
      };
    });
    $('whList').querySelectorAll('button[data-wh]').forEach(btn => {
      btn.onclick = async () => {
        const ok = await confirmDialog('Delete webhook?', 'This stops poll/push for that endpoint.');
        if (!ok) return;
        try {
          await ensureCSRF();
          await j('/api/v1/webhooks?id='+encodeURIComponent(btn.getAttribute('data-wh')), {method:'DELETE'});
          $('whOut').className='mono ok'; $('whOut').textContent='deleted';
          tickWebhooks();
        } catch(e) {
          $('whOut').className='mono err';
          $('whOut').textContent=(e.body&&e.body.error)||e.message;
        }
      };
    });
  } catch(e) {
    if (e.status !== 401) $('whList').innerHTML = '<span class="hint">—</span>';
  }
}

$('whCreate').onclick = async () => {
  if (authBlocked || readOnly || meta.gui_locked) {
    $('whOut').className='mono err'; $('whOut').textContent='Login required';
    return;
  }
  const name = ($('whName').value||'').trim();
  const url = ($('whURL').value||'').trim();
  if (!name || !url) {
    $('whOut').className='mono err'; $('whOut').textContent='name and url required';
    return;
  }
  if (!(url.startsWith('https://') || url.startsWith('http://'))) {
    $('whOut').className='mono err'; $('whOut').textContent='url must be http(s)://';
    return;
  }
  const ok = await confirmDialog('Create webhook «'+name+'»?',
    'Poll hooks execute remote text as RFLink commands (same validation/rate-limit). Push hooks send sensor/raw data to your URL. Ensure the endpoint is trusted.');
  if (!ok) return;
  const headers = {};
  const hdr = ($('whHeader').value||'').trim();
  if (hdr) {
    const i = hdr.indexOf(':');
    if (i < 1) { $('whOut').className='mono err'; $('whOut').textContent='header format Name: Value'; return; }
    headers[hdr.slice(0,i).trim()] = hdr.slice(i+1).trim();
  }
  const body = {
    name: name,
    enabled: $('whEnabled').checked,
    kind: $('whKind').value,
    url: url,
    method: $('whMethod').value,
    interval_sec: parseInt($('whInterval').value||'30',10)||30,
    timeout_sec: 5,
    payload: $('whPayload').value,
    headers: headers,
  };
  try {
    await ensureCSRF();
    const res = await j('/api/v1/webhooks', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify(body)});
    $('whOut').className='mono ok';
    $('whOut').textContent = 'created '+(res.webhook&&res.webhook.name)+(res.warning?' · '+res.warning:'');
    $('whName').value=''; $('whURL').value=''; $('whHeader').value='';
    tickWebhooks();
  } catch(e) {
    $('whOut').className='mono err';
    $('whOut').textContent=(e.body&&e.body.error)||e.message;
  }
};


function switchTab(name) {
  document.querySelectorAll('.tab').forEach(b => b.classList.toggle('active', b.dataset.tab === name));
  const panels = {
    main: ['tab-main', 'tab-main-cont'],
    tokens: ['tab-tokens'],
    webhooks: ['tab-webhooks'],
  };
  ['tab-main','tab-main-cont','tab-tokens','tab-webhooks'].forEach(id => {
    const el = $(id);
    if (!el) return;
    const show = (panels[name] || []).includes(id);
    el.classList.toggle('active', show);
  });
}
document.querySelectorAll('.tab').forEach(btn => {
  btn.addEventListener('click', () => switchTab(btn.dataset.tab));
});

(async function init() {
  loadHist();
  await bootstrap();
  setInterval(() => { if (!paused && !authBlocked) tickHealth(); }, POLL_HEALTH_MS);
  setInterval(() => { if (!paused && !authBlocked) tickSensors(); }, POLL_SENSORS_MS);
  setInterval(() => { if (!paused && !authBlocked) tickRate(); }, POLL_RATE_MS);
  setInterval(() => { if (!paused && !authBlocked) tickConfig(); }, POLL_CONFIG_MS);
})();
</script>
</body>
</html>
`
