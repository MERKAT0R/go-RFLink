/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"
)

var errWebhookInvalid = errors.New("invalid webhook")

const (
	maxWebhooks           = 8
	defaultHookTimeoutSec = 5
	defaultPollInterval   = 30
	minPollIntervalSec    = 5
	defaultPollMaxBytes   = 512
	maxPollMaxBytes       = 4096
	maxHookURLLen         = 2048
	maxHeaderNameLen      = 64
	maxHeaderValueLen     = 512
	maxHookNameLen        = 64
)

// RuntimeWebhook is stored in runtime.json / WEBHOOKS_JSON.
type RuntimeWebhook struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Enabled          bool              `json:"enabled"`
	Kind             string            `json:"kind"` // poll | push
	URL              string            `json:"url"`
	Method           string            `json:"method"` // GET | POST
	IntervalSec      int               `json:"interval_sec,omitempty"`
	TimeoutSec       int               `json:"timeout_sec,omitempty"`
	MaxResponseBytes int               `json:"max_response_bytes,omitempty"`
	Payload          string            `json:"payload,omitempty"` // push: raw | sumjson
	Headers          map[string]string `json:"headers,omitempty"`
	CreatedAt        string            `json:"created_at,omitempty"`
}

type webhookManager struct {
	app *App

	mu     sync.RWMutex
	hooks  []RuntimeWebhook
	cancel context.CancelFunc
	wg     sync.WaitGroup

	client    *http.Client
	pushSem   chan struct{} // limits concurrent outbound push HTTP calls
	reloadMu  sync.Mutex    // serializes Reload and Stop
	reloadGen uint64
	stopped   bool
}

const maxConcurrentPush = 8

func newWebhookManager(app *App) *webhookManager {
	m := &webhookManager{
		app:     app,
		pushSem: make(chan struct{}, maxConcurrentPush),
		client: &http.Client{
			// Timeout overridden per-request via context.
			Timeout:   0,
			Transport: webhookTransport(),
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 3 {
					return fmt.Errorf("too many redirects")
				}
				*req = *req.WithContext(withWebhookScheme(req.Context(), req.URL.Scheme))
				if err := validateWebhookURL(req.URL.String()); err != nil {
					return err
				}
				if len(via) > 0 && webhookRedirectStripsSecrets(via[0], req) {
					stripWebhookRedirect(req)
				}
				return nil
			},
		},
	}
	return m
}

const webhookUserAgent = "go-rflink-webhook/1.0"

// stripWebhookRedirect drops headers and the body on a hop that must not see
// secrets. User-Agent and Accept are replaced with this client's own values:
// a webhook header of the same name would otherwise be copied onto the new host.
func stripWebhookRedirect(req *http.Request) {
	if req == nil {
		return
	}
	req.Header = make(http.Header)
	req.Header.Set("User-Agent", webhookUserAgent)
	req.Body = http.NoBody
	req.GetBody = nil
	req.ContentLength = 0
}
func webhookRedirectStripsSecrets(from, to *http.Request) bool {
	if from == nil || to == nil || from.URL == nil || to.URL == nil {
		return true
	}
	if !strings.EqualFold(from.URL.Hostname(), to.URL.Hostname()) {
		return true
	}
	fromScheme := strings.ToLower(from.URL.Scheme)
	toScheme := strings.ToLower(to.URL.Scheme)
	if fromScheme == "https" && toScheme != "https" {
		return true
	}
	if fromScheme == toScheme && webhookPort(from.URL) != webhookPort(to.URL) {
		return true
	}
	return false
}

func webhookPort(u *url.URL) string {
	if u == nil {
		return ""
	}
	if port := u.Port(); port != "" {
		return port
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	default:
		return ""
	}
}

func (m *webhookManager) Start(parent context.Context) {
	m.Reload()
}

func (m *webhookManager) Stop() {
	m.reloadMu.Lock()
	m.stopped = true
	cancel := m.cancel
	m.cancel = nil
	m.reloadMu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.wg.Wait()
}

// Reload rebuilds workers from current opts.webhooks (env/runtime applied).
// The lock is not held while workers drain: a poll blocked in serial Write
// must not keep Stop from cancelling the rest of the set.
func (m *webhookManager) Reload() {
	m.reloadMu.Lock()
	if m.stopped {
		m.reloadMu.Unlock()
		return
	}
	m.reloadGen++
	gen := m.reloadGen
	cancel := m.cancel
	m.cancel = nil
	m.reloadMu.Unlock()
	if cancel != nil {
		cancel()
	}

	// Publish the new list before waiting. An in-flight poll keeps the copy
	// it already started, but push must stop using a webhook that was removed
	// or edited. Waiting first left the old list in place for the whole poll,
	// including a serial write that does not return until the driver does.
	m.reloadMu.Lock()
	if m.stopped || gen != m.reloadGen {
		m.reloadMu.Unlock()
		return
	}
	m.app.opts.runtimeMu.RLock()
	hooks := append([]RuntimeWebhook{}, m.app.opts.webhooks...)
	m.app.opts.runtimeMu.RUnlock()
	m.mu.Lock()
	m.hooks = hooks
	m.mu.Unlock()
	m.reloadMu.Unlock()

	m.wg.Wait()

	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	if m.stopped || gen != m.reloadGen {
		return
	}

	parent := context.Background()
	if m.app.ctx != nil {
		parent = m.app.ctx
	}
	ctx, cancel := context.WithCancel(parent)
	m.mu.Lock()
	m.hooks = hooks
	m.cancel = cancel
	m.mu.Unlock()

	nPoll, nPush := 0, 0
	for i := range hooks {
		h := hooks[i]
		if !h.Enabled {
			continue
		}
		switch strings.ToLower(h.Kind) {
		case "poll":
			nPoll++
			m.wg.Add(1)
			go m.runPoll(ctx, h)
		case "push":
			nPush++
		}
	}
	log.Info("webhooks loaded",
		"total", len(hooks),
		"poll_active", nPoll,
		"push_configured", nPush,
	)
}

func (m *webhookManager) Snapshot() []RuntimeWebhook {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]RuntimeWebhook, len(m.hooks))
	copy(out, m.hooks)
	return out
}

func (m *webhookManager) runPoll(ctx context.Context, h RuntimeWebhook) {
	defer m.wg.Done()
	defer func() {
		if rec := recover(); rec != nil {
			log.Error("webhook poll panic recovered", "name", h.Name, "panic", fmt.Sprintf("%v", rec))
		}
	}()

	interval := min(max(h.IntervalSec, minPollIntervalSec), 86400)
	timeout := h.TimeoutSec
	if timeout <= 0 {
		timeout = defaultHookTimeoutSec
	}
	maxBytes := h.MaxResponseBytes
	if maxBytes <= 0 {
		maxBytes = defaultPollMaxBytes
	}
	if maxBytes > maxPollMaxBytes {
		maxBytes = maxPollMaxBytes
	}
	method := strings.ToUpper(h.Method)
	if method == "" {
		method = http.MethodGet
	}

	log.Info("webhook poll started", "name", h.Name, "interval_sec", interval, "method", method)

	// Stagger first tick slightly.
	t := time.NewTimer(time.Duration(interval) * time.Second)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Debug("webhook poll stopped", "name", h.Name)
			return
		case <-t.C:
			m.doPollOnce(ctx, h, method, timeout, maxBytes)
			t.Reset(time.Duration(interval) * time.Second)
		}
	}
}

func (m *webhookManager) doPollOnce(ctx context.Context, h RuntimeWebhook, method string, timeoutSec, maxBytes int) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Error("webhook poll tick panic recovered", "name", h.Name, "panic", fmt.Sprintf("%v", rec))
		}
	}()

	reqCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()
	reqCtx = withWebhookScheme(reqCtx, urlScheme(h.URL))

	if err := validateWebhookURL(h.URL); err != nil {
		log.Warn("webhook poll skipped: bad url", "name", h.Name, "err", err)
		return
	}
	if err := validateWebhookURLResolved(reqCtx, h.URL); err != nil {
		log.Warn("webhook poll blocked after resolve", "name", h.Name, "err", err)
		return
	}

	var body io.Reader
	if method == http.MethodPost {
		body = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, h.URL, body)
	if err != nil {
		log.Warn("webhook poll request build failed", "name", h.Name, "err", err)
		return
	}
	req.Header.Set("User-Agent", webhookUserAgent)
	req.Header.Set("Accept", "text/plain, application/json;q=0.9, */*;q=0.1")
	for k, v := range h.Headers {
		if k != "" && v != "" {
			req.Header.Set(k, v)
		}
	}

	resp, err := m.client.Do(req)
	if err != nil {
		log.Warn("webhook poll failed", "name", h.Name, "err", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	limited := io.LimitReader(resp.Body, int64(maxBytes)+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		log.Warn("webhook poll read failed", "name", h.Name, "err", err)
		return
	}
	if len(data) > maxBytes {
		log.Warn("webhook poll response too large", "name", h.Name, "max_bytes", maxBytes)
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Warn("webhook poll non-2xx", "name", h.Name, "status", resp.StatusCode)
		return
	}

	text := strings.TrimSpace(string(data))
	if text == "" {
		log.Debug("webhook poll empty body", "name", h.Name)
		return
	}
	// First non-empty line only (avoid multi-command abuse).
	if i := strings.IndexAny(text, "\r\n"); i >= 0 {
		text = strings.TrimSpace(text[:i])
	}
	if text == "" {
		return
	}

	res := m.app.ExecuteCommandFrom(text, "webhook", h.Name)
	if res.Status != "ok" {
		log.Warn("webhook poll command not ok", "name", h.Name, "status", res.Status, "error", res.Error)
	}
}

// NotifyRaw delivers a serial line to enabled push webhooks with payload=raw.
func (m *webhookManager) NotifyRaw(entry rawLineEntry) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Error("webhook push raw panic recovered", "panic", fmt.Sprintf("%v", rec))
		}
	}()
	m.mu.RLock()
	hooks := append([]RuntimeWebhook{}, m.hooks...)
	m.mu.RUnlock()

	body := map[string]string{
		"type":    "raw",
		"at":      entry.At,
		"message": entry.Message,
	}
	if entry.Device != "" {
		body["device"] = entry.Device
	}
	payload, _ := json.Marshal(body)
	for _, h := range hooks {
		if !h.Enabled || strings.ToLower(h.Kind) != "push" {
			continue
		}
		if strings.ToLower(h.Payload) != "raw" {
			continue
		}
		m.enqueuePush(h, payload, "application/json")
	}
}

// NotifySumJSON delivers sensor aggregate JSON to push webhooks.
func (m *webhookManager) NotifySumJSON(hwid string, sumJSON []byte) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Error("webhook push sumjson panic recovered", "panic", fmt.Sprintf("%v", rec))
		}
	}()
	m.mu.RLock()
	hooks := append([]RuntimeWebhook{}, m.hooks...)
	m.mu.RUnlock()

	wrapped, _ := json.Marshal(map[string]any{
		"type": "sumjson",
		"hwid": hwid,
		"at":   time.Now().UTC().Format(time.RFC3339Nano),
		"data": json.RawMessage(sumJSON),
	})
	for _, h := range hooks {
		if !h.Enabled || strings.ToLower(h.Kind) != "push" {
			continue
		}
		if strings.ToLower(h.Payload) != "sumjson" {
			continue
		}
		m.enqueuePush(h, wrapped, "application/json")
	}
}

// enqueuePush runs doPush with a concurrency limit; excess pushes are dropped.
func (m *webhookManager) enqueuePush(h RuntimeWebhook, body []byte, contentType string) {
	select {
	case m.pushSem <- struct{}{}:
		go func() {
			defer func() { <-m.pushSem }()
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("webhook push worker panic recovered", "name", h.Name, "panic", fmt.Sprintf("%v", rec))
				}
			}()
			m.doPush(h, body, contentType)
		}()
	default:
		log.Warn("webhook push dropped: concurrency limit", "name", h.Name, "limit", maxConcurrentPush)
	}
}

const maxGETURLLen = 7800 // leave headroom under common 8KiB proxy limits

func (m *webhookManager) doPush(h RuntimeWebhook, body []byte, contentType string) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Error("webhook push panic recovered", "name", h.Name, "panic", fmt.Sprintf("%v", rec))
		}
	}()
	if err := validateWebhookURL(h.URL); err != nil {
		log.Warn("webhook push skipped: bad url", "name", h.Name, "err", err)
		return
	}
	timeout := h.TimeoutSec
	if timeout <= 0 {
		timeout = defaultHookTimeoutSec
	}
	method := strings.ToUpper(h.Method)
	if method == "" {
		method = http.MethodPost
	}
	if method != http.MethodPost && method != http.MethodGet {
		method = http.MethodPost
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	var rdr io.Reader
	u := h.URL
	if method == http.MethodGet {
		expanded, err := expandWebhookGETURL(h.URL, h.Payload, body)
		if err != nil {
			log.Warn("webhook push GET expand failed", "name", h.Name, "err", err)
			return
		}
		if len(expanded) > maxGETURLLen {
			log.Warn("webhook push GET url too long after payload", "name", h.Name, "len", len(expanded), "max", maxGETURLLen)
			return
		}
		if err := validateWebhookURLLimit(stripWebhookPlaceholders(expanded), maxGETURLLen); err != nil {
			log.Warn("webhook push GET url invalid after expand", "name", h.Name, "err", err)
			return
		}
		u = expanded
		rdr = nil
	} else {
		// POST: optional placeholders in URL; body is always JSON payload.
		if webhookURLHasPlaceholder(h.URL) {
			expanded, err := expandWebhookPlaceholders(h.URL, body)
			if err != nil {
				log.Warn("webhook push POST url expand failed", "name", h.Name, "err", err)
				return
			}
			u = expanded
		}
		rdr = bytes.NewReader(body)
	}
	if err := validateWebhookURLResolvedLimit(ctx, u, maxGETURLLen); err != nil {
		log.Warn("webhook push blocked after resolve", "name", h.Name, "err", err)
		return
	}
	ctx = withWebhookScheme(ctx, urlScheme(u))
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		log.Warn("webhook push build failed", "name", h.Name, "err", err)
		return
	}
	req.Header.Set("User-Agent", webhookUserAgent)
	if method != http.MethodGet {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range h.Headers {
		if k != "" && v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := m.client.Do(req)
	if err != nil {
		log.Warn("webhook push failed", "name", h.Name, "err", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Warn("webhook push non-2xx", "name", h.Name, "status", resp.StatusCode)
		return
	}
	log.Debug("webhook push ok", "name", h.Name, "status", resp.StatusCode, "method", method)
}

// webhook placeholders for GET/POST URL templates.
// Longer tokens must be replaced before shorter ones: "{payload}" is a substring of "{{payload}}".
var webhookPlaceholders = []string{
	"{{payload}}",
	"%payload%", "%PAYLOAD%",
	"%raw%", "%RAW%",
	"%json%", "%JSON%",
	"{payload}",
}

func webhookURLHasPlaceholder(u string) bool {
	for _, ph := range webhookPlaceholders {
		if strings.Contains(u, ph) {
			return true
		}
	}
	return false
}

func webhookPlaceholdersLongestFirst() []string {
	phs := append([]string(nil), webhookPlaceholders...)
	sort.Slice(phs, func(i, j int) bool { return len(phs[i]) > len(phs[j]) })
	return phs
}

func stripWebhookPlaceholders(u string) string {
	out := u
	for _, ph := range webhookPlaceholdersLongestFirst() {
		out = strings.ReplaceAll(out, ph, "x")
	}
	return out
}

func expandWebhookPlaceholders(template string, body []byte) (string, error) {
	enc := url.QueryEscape(string(body))
	out := template
	for _, ph := range webhookPlaceholdersLongestFirst() {
		out = strings.ReplaceAll(out, ph, enc)
	}
	if err := webhookExpandedKeepsTarget(template, out); err != nil {
		return "", err
	}
	return out, nil
}

// webhookExpandedKeepsTarget rejects a substitution that changes host or adds a
// path segment. QueryEscape keeps "/" as %2F; a broken replacement (raw braces)
// makes url.URL re-decode that into a real slash.
func webhookExpandedKeepsTarget(template, expanded string) error {
	base, err := url.Parse(stripWebhookPlaceholders(template))
	if err != nil {
		return fmt.Errorf("webhook template: %w", err)
	}
	got, err := url.Parse(expanded)
	if err != nil {
		return fmt.Errorf("webhook url: %w", err)
	}
	if got.User != nil || !strings.EqualFold(base.Scheme, got.Scheme) ||
		!strings.EqualFold(base.Hostname(), got.Hostname()) || base.Port() != got.Port() {
		return fmt.Errorf("webhook url target changed after payload substitution")
	}
	if strings.Count(base.EscapedPath(), "/") != strings.Count(got.EscapedPath(), "/") {
		return fmt.Errorf("webhook url path changed after payload substitution")
	}
	return nil
}

// expandWebhookGETURL substitutes %payload%/%raw%/%json%/{payload} or, if none present,
// appends query param raw= / json= / payload= with the body.
func expandWebhookGETURL(template, payloadKind string, body []byte) (string, error) {
	if webhookURLHasPlaceholder(template) {
		return expandWebhookPlaceholders(template, body)
	}
	u, err := url.Parse(template)
	if err != nil {
		return "", err
	}
	key := "payload"
	switch strings.ToLower(strings.TrimSpace(payloadKind)) {
	case "raw":
		key = "raw"
	case "sumjson", "json":
		key = "json"
	}
	u.RawQuery = appendWebhookQuery(u.RawQuery, key, string(body))
	return u.String(), nil
}

// appendWebhookQuery adds key=value. A query that url.ParseQuery rejects
// (a semicolon, a broken escape) is kept as stored and the new pair is appended.
// Rebuilding it through url.Values drops those parameters with no error.
func appendWebhookQuery(rawQuery, key, value string) string {
	extra := url.Values{}
	extra.Set(key, value)
	enc := extra.Encode()
	if rawQuery == "" {
		return enc
	}
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return rawQuery + "&" + enc
	}
	q.Set(key, value)
	return q.Encode()
}

// --- validation / SSRF ---

func validateWebhookURL(raw string) error {
	return validateWebhookURLLimit(raw, maxHookURLLen)
}

func validateWebhookURLLimit(raw string, maxLen int) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("empty url")
	}
	if maxLen <= 0 {
		maxLen = maxHookURLLen
	}
	if len(raw) > maxLen {
		return fmt.Errorf("url too long")
	}
	// %payload% etc. are not valid percent-encoding — strip before parse.
	u, err := url.Parse(stripWebhookPlaceholders(raw))
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https")
	}
	if u.Host == "" {
		return fmt.Errorf("missing host")
	}
	if u.User != nil {
		return fmt.Errorf("userinfo in url not allowed")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("missing hostname")
	}
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") || lower == "metadata.google.internal" {
		return fmt.Errorf("host not allowed: %s", host)
	}
	// Block cloud metadata hostname patterns
	if strings.Contains(lower, "169.254.169.254") {
		return fmt.Errorf("metadata address not allowed")
	}
	if ip := net.ParseIP(host); ip != nil {
		if err := rejectResolvedIP(u.Scheme, ip); err != nil {
			return err
		}
	} else if u.Scheme == "http" {
		// Allow common LAN / lab TLDs without TLS; public hostnames need https.
		if !isLocalLabHost(lower) {
			return fmt.Errorf("http with hostname requires https (or private IP / .local/.lan/.loc)")
		}
	}
	return nil
}

func rejectDangerousIP(ip net.IP) error {
	if ip.IsLoopback() {
		return fmt.Errorf("loopback not allowed")
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return fmt.Errorf("link-local not allowed")
	}
	if ip.IsMulticast() || ip.IsUnspecified() {
		return fmt.Errorf("invalid ip")
	}
	// AWS/GCP/Azure metadata, including the IPv6 IMDS address (a ULA, so IsPrivate is true).
	if ip.Equal(net.ParseIP("169.254.169.254")) ||
		ip.Equal(net.ParseIP("169.254.170.2")) ||
		ip.Equal(net.ParseIP("fd00:ec2::254")) {
		return fmt.Errorf("metadata address not allowed")
	}
	return nil
}

func rejectResolvedIP(scheme string, ip net.IP) error {
	if err := rejectDangerousIP(ip); err != nil {
		return err
	}
	if strings.EqualFold(scheme, "http") && !ip.IsPrivate() {
		return fmt.Errorf("http only allowed for private IP addresses (use https)")
	}
	return nil
}

type webhookSchemeCtx struct{}

func withWebhookScheme(ctx context.Context, scheme string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, webhookSchemeCtx{}, strings.ToLower(scheme))
}

func urlScheme(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil {
		return ""
	}
	return u.Scheme
}

func webhookSchemeFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	scheme, _ := ctx.Value(webhookSchemeCtx{}).(string)
	return scheme
}

func webhookTransport() *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok || base == nil {
		return &http.Transport{Proxy: nil, DialContext: webhookDialContext}
	}
	tr := base.Clone()
	tr.Proxy = nil
	tr.DialContext = webhookDialContext
	return tr
}

// webhookDialContext connects only to addresses checked in this call.
// A second DNS lookup inside net/http would reopen a rebinding window.
func webhookDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	var last error
	dialer := net.Dialer{Timeout: 10 * time.Second}
	for _, ipa := range ips {
		if err := rejectResolvedIP(webhookSchemeFrom(ctx), ipa.IP); err != nil {
			last = err
			continue
		}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ipa.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		last = err
	}
	if last == nil {
		last = fmt.Errorf("no allowed address for %s", host)
	}
	return nil, last
}

func isLocalLabHost(host string) bool {
	for _, sfx := range []string{".local", ".lan", ".loc", ".internal", ".home", ".intranet", ".test"} {
		if strings.HasSuffix(host, sfx) {
			return true
		}
	}
	return false
}

// validateWebhookURLResolved looks up DNS and rejects dangerous IPs (metadata/loopback).
// Mitigates DNS-rebinding SSRF between config time and request time.
func validateWebhookURLResolved(ctx context.Context, raw string) error {
	return validateWebhookURLResolvedLimit(ctx, raw, maxHookURLLen)
}

func validateWebhookURLResolvedLimit(ctx context.Context, raw string, maxLen int) error {
	if err := validateWebhookURLLimit(raw, maxLen); err != nil {
		return err
	}
	u, err := url.Parse(stripWebhookPlaceholders(strings.TrimSpace(raw)))
	if err != nil {
		return err
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("missing hostname")
	}
	if ip := net.ParseIP(host); ip != nil {
		return rejectResolvedIP(u.Scheme, ip)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	lookupCtx, lookupCancel := context.WithTimeout(ctx, 5*time.Second)
	defer lookupCancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(lookupCtx, host)
	if err != nil {
		return fmt.Errorf("dns lookup: %w", err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("dns lookup: no addresses")
	}
	for _, ipa := range addrs {
		if err := rejectResolvedIP(u.Scheme, ipa.IP); err != nil {
			return fmt.Errorf("resolved %s → %s: %w", host, ipa.IP, err)
		}
	}
	return nil
}

// sanitizeRuntimeWebhooks drops invalid entries and caps the list.
// The result is never nil when the caller already decided runtime owns the key.
func sanitizeRuntimeWebhooks(list []RuntimeWebhook) []RuntimeWebhook {
	out := make([]RuntimeWebhook, 0, len(list))
	for i := range list {
		h := list[i]
		if err := ValidateWebhookConfig(&h); err != nil {
			log.Warn("runtime webhook skipped", "name", h.Name, "err", err)
			continue
		}
		out = append(out, h)
		if len(out) >= maxWebhooks {
			break
		}
	}
	return stabilizeWebhookIDs(out, nil)
}

// ValidateWebhookConfig validates a webhook before save.
func ValidateWebhookConfig(h *RuntimeWebhook) error {
	if h == nil {
		return fmt.Errorf("nil webhook")
	}
	h.Name = strings.TrimSpace(h.Name)
	h.URL = strings.TrimSpace(h.URL)
	h.Kind = strings.ToLower(strings.TrimSpace(h.Kind))
	h.Method = strings.ToUpper(strings.TrimSpace(h.Method))
	h.Payload = strings.ToLower(strings.TrimSpace(h.Payload))

	if h.Name == "" || len(h.Name) > maxHookNameLen {
		return fmt.Errorf("name required (max %d chars)", maxHookNameLen)
	}
	if !reLabel.MatchString(h.Name) {
		return fmt.Errorf("invalid name %q", h.Name)
	}
	switch h.Kind {
	case "poll", "push":
	default:
		return fmt.Errorf("kind must be poll or push")
	}
	if err := validateWebhookURL(h.URL); err != nil {
		return fmt.Errorf("url: %w", err)
	}
	if h.Method == "" {
		if h.Kind == "poll" {
			h.Method = http.MethodGet
		} else {
			h.Method = http.MethodPost
		}
	}
	if h.Method != http.MethodGet && h.Method != http.MethodPost {
		return fmt.Errorf("method must be GET or POST")
	}
	if h.TimeoutSec < 0 || h.TimeoutSec > 60 {
		return fmt.Errorf("timeout_sec must be 0..60")
	}
	if h.Kind == "poll" {
		if h.IntervalSec == 0 {
			h.IntervalSec = defaultPollInterval
		}
		if h.IntervalSec < minPollIntervalSec || h.IntervalSec > 86400 {
			return fmt.Errorf("interval_sec must be %d..86400", minPollIntervalSec)
		}
		if h.MaxResponseBytes == 0 {
			h.MaxResponseBytes = defaultPollMaxBytes
		}
		if h.MaxResponseBytes < 32 || h.MaxResponseBytes > maxPollMaxBytes {
			return fmt.Errorf("max_response_bytes must be 32..%d", maxPollMaxBytes)
		}
	}
	if h.Kind == "push" {
		if h.Payload == "" {
			h.Payload = "sumjson"
		}
		if h.Payload != "raw" && h.Payload != "sumjson" {
			return fmt.Errorf("payload must be raw or sumjson")
		}
	}
	if len(h.Headers) > 16 {
		return fmt.Errorf("too many headers (max 16)")
	}
	for k, v := range h.Headers {
		if len(k) > maxHeaderNameLen || len(v) > maxHeaderValueLen {
			return fmt.Errorf("header name/value too long")
		}
		if strings.ContainsAny(k, "\r\n") || strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("header contains CR/LF")
		}
	}
	return nil
}

var (
	webhookIDMu  sync.Mutex
	webhookIDSeq uint64
)

func newWebhookID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err == nil {
		return hex.EncodeToString(b)
	}
	webhookIDMu.Lock()
	webhookIDSeq++
	n := webhookIDSeq
	webhookIDMu.Unlock()
	return fmt.Sprintf("id-%d-%d", time.Now().UnixNano(), n)
}

// stabilizeWebhookIDs fills empty ids, keeps ids already issued for the same
// name, and gives a duplicate id its own value so update/delete can address it.
// Several hooks may share a name. Each one keeps the id from the same position
// in the previous list, so a later reload does not mint a new id for the second hook.
func stabilizeWebhookIDs(list, prev []RuntimeWebhook) []RuntimeWebhook {
	byName := make(map[string][]string, len(prev))
	for _, h := range prev {
		name := strings.ToLower(strings.TrimSpace(h.Name))
		if name == "" || h.ID == "" {
			continue
		}
		byName[name] = append(byName[name], h.ID)
	}
	used := make(map[string]struct{}, len(list))
	for i := range list {
		name := strings.ToLower(strings.TrimSpace(list[i].Name))
		if ids := byName[name]; len(ids) > 0 {
			list[i].ID = ids[0]
			byName[name] = ids[1:]
		}
		if list[i].ID == "" {
			list[i].ID = newWebhookID()
		}
		if _, ok := used[list[i].ID]; ok {
			log.Warn("webhook duplicate id reassigned", "name", list[i].Name, "id", list[i].ID)
			list[i].ID = newWebhookID()
		}
		used[list[i].ID] = struct{}{}
	}
	return list
}

func webhookIDsChanged(before, after []RuntimeWebhook) bool {
	if len(before) != len(after) {
		return true
	}
	for i := range before {
		if before[i].ID != after[i].ID {
			return true
		}
	}
	return false
}

func parseWebhooksJSON(raw string) []RuntimeWebhook {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var list []RuntimeWebhook
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		log.Warn("WEBHOOKS_JSON parse failed", "err", err)
		return nil
	}
	var out []RuntimeWebhook
	for i := range list {
		h := list[i]
		if err := ValidateWebhookConfig(&h); err != nil {
			log.Warn("WEBHOOKS_JSON skip invalid entry", "name", h.Name, "err", err)
			continue
		}
		out = append(out, h)
		if len(out) >= maxWebhooks {
			break
		}
	}
	return out
}

// mergeWebhookUpdate applies only the JSON keys that were present. An omitted
// enabled flag stays as stored, and an empty headers object keeps secrets.
func mergeWebhookUpdate(existing RuntimeWebhook, raw map[string]json.RawMessage, incoming RuntimeWebhook) (RuntimeWebhook, error) {
	merged := existing
	if _, ok := raw["name"]; ok {
		merged.Name = incoming.Name
	}
	if _, ok := raw["url"]; ok {
		merged.URL = incoming.URL
	}
	if _, ok := raw["kind"]; ok {
		merged.Kind = incoming.Kind
	}
	if _, ok := raw["method"]; ok {
		merged.Method = incoming.Method
	}
	if _, ok := raw["enabled"]; ok {
		merged.Enabled = incoming.Enabled
	}
	if _, ok := raw["interval_sec"]; ok {
		merged.IntervalSec = incoming.IntervalSec
	}
	if _, ok := raw["timeout_sec"]; ok {
		merged.TimeoutSec = incoming.TimeoutSec
	}
	if _, ok := raw["max_response_bytes"]; ok {
		merged.MaxResponseBytes = incoming.MaxResponseBytes
	}
	if _, ok := raw["payload"]; ok {
		merged.Payload = incoming.Payload
	}
	if hdr, ok := raw["headers"]; ok {
		trimmed := bytes.TrimSpace(hdr)
		// {} and null keep stored secrets. Pretty-printed "{ }" is the same
		// empty object; only a non-empty object replaces the saved headers.
		if len(trimmed) > 0 && string(trimmed) != "null" && !emptyJSONObject(trimmed) {
			merged.Headers = incoming.Headers
		}
	}
	merged.ID = existing.ID
	merged.CreatedAt = existing.CreatedAt
	if err := ValidateWebhookConfig(&merged); err != nil {
		return RuntimeWebhook{}, fmt.Errorf("%w: %s", errWebhookInvalid, err.Error())
	}
	return merged, nil
}

// emptyJSONObject reports whether raw is a JSON object with no keys.
// Invalid JSON returns false so the caller can replace headers as before.
func emptyJSONObject(raw []byte) bool {
	if len(raw) == 0 || raw[0] != '{' {
		return false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	return len(m) == 0
}

func webhooksPublicView(list []RuntimeWebhook) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, h := range list {
		hdrNames := make([]string, 0, len(h.Headers))
		for k := range h.Headers {
			hdrNames = append(hdrNames, k)
		}
		out = append(out, map[string]any{
			"id":                 h.ID,
			"name":               h.Name,
			"enabled":            h.Enabled,
			"kind":               h.Kind,
			"url":                h.URL,
			"method":             h.Method,
			"interval_sec":       h.IntervalSec,
			"timeout_sec":        h.TimeoutSec,
			"max_response_bytes": h.MaxResponseBytes,
			"payload":            h.Payload,
			"header_names":       hdrNames, // values never exposed
			"created_at":         h.CreatedAt,
		})
	}
	return out
}
