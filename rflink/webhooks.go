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
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"
)

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

	client   *http.Client
	pushSem  chan struct{} // limits concurrent outbound push HTTP calls
	reloadMu sync.Mutex    // serializes Reload
}

const maxConcurrentPush = 8

func newWebhookManager(app *App) *webhookManager {
	m := &webhookManager{
		app:     app,
		pushSem: make(chan struct{}, maxConcurrentPush),
		client: &http.Client{
			// Timeout overridden per-request via context.
			Timeout: 0,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 3 {
					return fmt.Errorf("too many redirects")
				}
				// Re-validate each hop (SSRF via redirect).
				if err := validateWebhookURL(req.URL.String()); err != nil {
					return err
				}
				return validateWebhookURLResolved(req.URL.String())
			},
		},
	}
	return m
}

func (m *webhookManager) Start(parent context.Context) {
	m.Reload()
}

func (m *webhookManager) Stop() {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.mu.Unlock()
	m.wg.Wait()
}

// Reload rebuilds workers from current opts.webhooks (env/runtime applied).
func (m *webhookManager) Reload() {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()

	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.mu.Unlock()
	m.wg.Wait()

	m.mu.Lock()
	hooks := append([]RuntimeWebhook{}, m.app.opts.webhooks...)
	m.hooks = hooks
	ctx, cancel := context.WithCancel(context.Background())
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

	interval := max(h.IntervalSec, minPollIntervalSec)
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

	if err := validateWebhookURL(h.URL); err != nil {
		log.Warn("webhook poll skipped: bad url", "name", h.Name, "err", err)
		return
	}
	if err := validateWebhookURLResolved(h.URL); err != nil {
		log.Warn("webhook poll blocked after resolve", "name", h.Name, "err", err)
		return
	}

	reqCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	var body io.Reader
	if method == http.MethodPost {
		body = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, h.URL, body)
	if err != nil {
		log.Warn("webhook poll request build failed", "name", h.Name, "err", err)
		return
	}
	req.Header.Set("User-Agent", "go-rflink-webhook/1.0")
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

	log.Info("webhook poll command", "name", h.Name, "command", text)
	res := m.app.ExecuteCommand(text)
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

	payload, _ := json.Marshal(map[string]string{
		"type":    "raw",
		"at":      entry.At,
		"message": entry.Message,
	})
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
		if err := validateWebhookURL(stripWebhookPlaceholders(expanded)); err != nil {
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
	if err := validateWebhookURLResolved(u); err != nil {
		log.Warn("webhook push blocked after resolve", "name", h.Name, "err", err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		log.Warn("webhook push build failed", "name", h.Name, "err", err)
		return
	}
	req.Header.Set("User-Agent", "go-rflink-webhook/1.0")
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
var webhookPlaceholders = []string{
	"%payload%", "%PAYLOAD%",
	"%raw%", "%RAW%",
	"%json%", "%JSON%",
	"{payload}", "{{payload}}",
}

func webhookURLHasPlaceholder(u string) bool {
	for _, ph := range webhookPlaceholders {
		if strings.Contains(u, ph) {
			return true
		}
	}
	return false
}

func stripWebhookPlaceholders(u string) string {
	out := u
	for _, ph := range webhookPlaceholders {
		out = strings.ReplaceAll(out, ph, "x")
	}
	return out
}

func expandWebhookPlaceholders(template string, body []byte) (string, error) {
	enc := url.QueryEscape(string(body))
	out := template
	for _, ph := range webhookPlaceholders {
		out = strings.ReplaceAll(out, ph, enc)
	}
	return out, nil
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
	q := u.Query()
	key := "payload"
	switch strings.ToLower(strings.TrimSpace(payloadKind)) {
	case "raw":
		key = "raw"
	case "sumjson", "json":
		key = "json"
	}
	q.Set(key, string(body))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// --- validation / SSRF ---

func validateWebhookURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("empty url")
	}
	if len(raw) > maxHookURLLen {
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
		if err := rejectDangerousIP(ip); err != nil {
			return err
		}
		if u.Scheme == "http" && !ip.IsPrivate() && !ip.IsLoopback() {
			// loopback already rejected; public HTTP discouraged
			return fmt.Errorf("http only allowed for private IP addresses (use https)")
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
	// AWS/GCP/Azure metadata
	if ip.Equal(net.ParseIP("169.254.169.254")) || ip.Equal(net.ParseIP("169.254.170.2")) {
		return fmt.Errorf("metadata address not allowed")
	}
	return nil
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
func validateWebhookURLResolved(raw string) error {
	if err := validateWebhookURL(raw); err != nil {
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
		return rejectDangerousIP(ip)
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("dns lookup: %w", err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("dns lookup: no addresses")
	}
	for _, ip := range ips {
		if err := rejectDangerousIP(ip); err != nil {
			return fmt.Errorf("resolved %s → %s: %w", host, ip, err)
		}
	}
	return nil
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

func newWebhookID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
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
		if h.ID == "" {
			h.ID = newWebhookID()
		}
		out = append(out, h)
		if len(out) >= maxWebhooks {
			break
		}
	}
	return out
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
