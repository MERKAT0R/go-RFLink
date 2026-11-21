/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
)

func newCoverageStack(t *testing.T) (*App, *httpAPIServer) {
	t.Helper()
	dir := t.TempDir()
	opts := &Options{}
	opts.HTTP.Listen = "127.0.0.1:0"
	opts.HTTP.AuthToken = "test-gui-password-16"
	opts.HTTP.GUI = true
	opts.HTTP.CORSOrigins = "https://ha.example"
	opts.HTTP.TrustedProxies = "127.0.0.1"
	opts.Publish.Topic = "rflink"
	opts.Publish.CmdTopic = "rflink/send"
	opts.Publish.HADiscoveryPrefix = "homeassistant"
	opts.Publish.CommandRateLimit = 20
	opts.Publish.CommandMaxLen = 256
	opts.Publish.DedupWindowMs = 3000
	opts.Publish.IgnoreList = "Keeloq"
	opts.Serial.Device = "COM_TEST"
	opts.Serial.Baud = 57600
	opts.Serial.ReadTimeoutSec = 10
	opts.Serial.ReconnectMinDelaySec = 3
	opts.Serial.ReconnectMaxDelaySec = 60
	opts.RuntimeConfigFile = filepath.Join(dir, "runtime.json")
	opts.APIAuthTokens = "reader:readtoken123456:never:read,cmd:cmdtoken12345678:never:command,admin:admintoken123456:never:admin"
	opts.apiTokens = parseAPIAuthTokens(opts.APIAuthTokens, "")
	opts.ignoreSet = map[string]struct{}{"KEELOQ": {}}

	rm, err := newRuntimeManager(opts)
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	t.Cleanup(rm.Stop)

	pub := &Publisher{
		opts:          opts,
		pending:       map[string]pendingSensor{},
		dedupLast:     map[string]time.Time{},
		sensorSeen:    map[string]time.Time{},
		serialRestart: make(chan struct{}, 1),
		rawCap:        8,
		sampleCap:     8,
		startedAt:     time.Now(),
		runCtx:        context.Background(),
	}
	pub.lastSensorAt.Store(time.Time{})
	pub.lastSensorHWID.Store("")
	pub.lastSerialAt.Store(time.Time{})

	ctx, cancel := context.WithCancel(context.Background())
	app := &App{
		opts:        opts,
		publisher:   pub,
		serial:      &SerialReader{port: newFakeSerial(), opts: opts},
		serialCh:    make(chan serialFrame, 4),
		ctx:         ctx,
		cancel:      cancel,
		cmdTokens:   20,
		cmdLastFill: time.Now(),
		runtime:     rm,
		sessions:    newSessionStore(),
	}
	app.webhooks = newWebhookManager(app)

	h, err := startHTTPServer(app)
	if err != nil {
		cancel()
		t.Fatalf("http: %v", err)
	}
	t.Cleanup(func() {
		h.Shutdown()
		cancel()
	})
	return app, h
}

func doReq(h *httpAPIServer, method, path, body string, hdr map[string]string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.RemoteAddr = "127.0.0.1:2345"
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rr := httptest.NewRecorder()
	h.server.Handler.ServeHTTP(rr, req)
	return rr
}

func loginGUI(t *testing.T, h *httpAPIServer) []*http.Cookie {
	t.Helper()
	rr := doReq(h, http.MethodPost, "/api/v1/session", `{"token":"test-gui-password-16"}`, map[string]string{
		"Content-Type": "application/json",
	}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("login %d %s", rr.Code, rr.Body.String())
	}
	return rr.Result().Cookies()
}

func csrfOf(cookies []*http.Cookie) string {
	for _, c := range cookies {
		if c.Name == cookieCSRF {
			return c.Value
		}
	}
	return ""
}

func TestHTTPAPI(t *testing.T) {
	_, h := newCoverageStack(t)

	t.Run("probes", func(t *testing.T) {
		rr := doReq(h, http.MethodGet, "/healthz", "", nil, nil)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "ok") {
			t.Fatalf("healthz %d %s", rr.Code, rr.Body.String())
		}
		if doReq(h, http.MethodPost, "/healthz", "", nil, nil).Code != http.StatusMethodNotAllowed {
			t.Fatal("healthz post")
		}
		rr = doReq(h, http.MethodGet, "/readyz", "", nil, nil)
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("readyz %d", rr.Code)
		}
		h.app.publisher.mqttUp.Store(true)
		rr = doReq(h, http.MethodGet, "/readyz", "", nil, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("readyz up %d", rr.Code)
		}
		if doReq(h, http.MethodPost, "/readyz", "", nil, nil).Code != http.StatusMethodNotAllowed {
			t.Fatal("readyz post")
		}
	})

	t.Run("meta and gui", func(t *testing.T) {
		rr := doReq(h, http.MethodGet, "/api/v1/meta", "", nil, nil)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"authenticated": false`) {
			t.Fatalf("meta %s", rr.Body.String())
		}
		if doReq(h, http.MethodPost, "/api/v1/meta", "", nil, nil).Code != http.StatusMethodNotAllowed {
			t.Fatal("meta post")
		}
		rr = doReq(h, http.MethodGet, "/", "", nil, nil)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "go-rflink") && !strings.Contains(rr.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("gui %d %s", rr.Code, rr.Header().Get("Content-Type"))
		}
		if rr.Header().Get("Content-Security-Policy") == "" {
			t.Fatal("missing csp")
		}
		if doReq(h, http.MethodGet, "/missing", "", nil, nil).Code != http.StatusNotFound {
			t.Fatal("gui miss")
		}
		rr = doReq(h, http.MethodGet, "/api/v1/csrf", "", nil, nil)
		if rr.Code != http.StatusOK || csrfOf(rr.Result().Cookies()) == "" {
			t.Fatalf("csrf %d", rr.Code)
		}
	})

	t.Run("login", func(t *testing.T) {
		if doReq(h, http.MethodPost, "/api/v1/session", `{}`, nil, nil).Code != http.StatusUnsupportedMediaType {
			t.Fatal("login content type")
		}
		rr := doReq(h, http.MethodPost, "/api/v1/session", `{"token":"wrong-password-value"}`, map[string]string{
			"Content-Type": "application/json",
		}, nil)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("bad login %d", rr.Code)
		}
		rr = doReq(h, http.MethodGet, "/api/v1/session", "", nil, nil)
		if !strings.Contains(rr.Body.String(), `"authenticated": false`) {
			t.Fatalf("status %s", rr.Body.String())
		}
	})

	cookies := loginGUI(t, h)
	csrf := csrfOf(cookies)
	authHdr := map[string]string{headerCSRF: csrf, "Content-Type": "application/json"}

	t.Run("session authed", func(t *testing.T) {
		rr := doReq(h, http.MethodGet, "/api/v1/session", "", nil, cookies)
		if !strings.Contains(rr.Body.String(), `"authenticated": true`) {
			t.Fatalf("status %s", rr.Body.String())
		}
		rr = doReq(h, http.MethodGet, "/api/v1/health", "", nil, nil)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("open health %d", rr.Code)
		}
		rr = doReq(h, http.MethodGet, "/api/v1/health", "", nil, cookies)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"version"`) {
			t.Fatalf("health %d %s", rr.Code, rr.Body.String())
		}
		if doReq(h, http.MethodPost, "/api/v1/health", "", authHdr, cookies).Code != http.StatusMethodNotAllowed {
			t.Fatal("health post")
		}
		rr = doReq(h, http.MethodPost, "/api/v1/command", `{"command":"10;PING;"}`, map[string]string{"Content-Type": "application/json"}, cookies)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("csrf %d %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("bearer scopes", func(t *testing.T) {
		rr := doReq(h, http.MethodGet, "/api/v1/health", "", map[string]string{"Authorization": "Bearer readtoken123456"}, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("reader %d", rr.Code)
		}
		rr = doReq(h, http.MethodGet, "/api/v1/health", "", map[string]string{"X-API-Token": "cmdtoken12345678"}, nil)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("cmd scope health %d %s", rr.Code, rr.Body.String())
		}
		rr = doReq(h, http.MethodGet, "/api/v1/tokens", "", map[string]string{"Authorization": "Bearer admintoken123456"}, nil)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("admin bearer tokens %d %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("command and rate", func(t *testing.T) {
		rr := doReq(h, http.MethodPost, "/api/v1/command", `{"command":"10;PING;"}`, map[string]string{
			"Authorization": "Bearer cmdtoken12345678",
			"Content-Type":  "application/json",
		}, nil)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"status": "ok"`) {
			t.Fatalf("cmd %d %s", rr.Code, rr.Body.String())
		}
		rr = doReq(h, http.MethodPost, "/api/v1/command", `{"command":"10;PING;10;REBOOT;"}`, map[string]string{
			"Authorization": "Bearer cmdtoken12345678",
			"Content-Type":  "application/json",
		}, nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("stacked %d %s", rr.Code, rr.Body.String())
		}
		rr = doReq(h, http.MethodPost, "/api/v1/command", `{}`, map[string]string{
			"Authorization": "Bearer cmdtoken12345678",
			"Content-Type":  "application/json",
		}, nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("empty %d", rr.Code)
		}
		h.app.opts.HTTP.ReadOnly = true
		rr = doReq(h, http.MethodPost, "/api/v1/command", `{"command":"10;PING;"}`, map[string]string{
			"Authorization": "Bearer cmdtoken12345678",
			"Content-Type":  "application/json",
		}, nil)
		h.app.opts.HTTP.ReadOnly = false
		if rr.Code != http.StatusForbidden {
			t.Fatalf("readonly %d", rr.Code)
		}
		h.app.cancel()
		rr = doReq(h, http.MethodPost, "/api/v1/command", `{"command":"10;PING;"}`, map[string]string{
			"Authorization": "Bearer cmdtoken12345678",
			"Content-Type":  "application/json",
		}, nil)
		if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), `"status": "stopping"`) {
			t.Fatalf("stopping %d %s", rr.Code, rr.Body.String())
		}
		rr = doReq(h, http.MethodGet, "/api/v1/rate_limit", "", map[string]string{"Authorization": "Bearer readtoken123456"}, nil)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"limit_per_sec"`) {
			t.Fatalf("rate %s", rr.Body.String())
		}
	})

	t.Run("sensors raw serial rediscover", func(t *testing.T) {
		h.app.publisher.handleSerialMessage(context.Background(), "", "20;02;PONG;")
		h.app.publisher.handleSerialMessage(context.Background(), "", "20;01;Oregon;ID=0001;TEMP=00c8;")
		rr := doReq(h, http.MethodGet, "/api/v1/sensors?samples=1", "", nil, cookies)
		if rr.Code != http.StatusOK {
			t.Fatalf("sensors %d", rr.Code)
		}
		rr = doReq(h, http.MethodGet, "/api/v1/rflink/raw", "", nil, cookies)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "PONG") {
			t.Fatalf("raw %s", rr.Body.String())
		}
		rr = doReq(h, http.MethodGet, "/api/v1/serial", "", nil, cookies)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "COM_TEST") {
			t.Fatalf("serial %s", rr.Body.String())
		}
		rr = doReq(h, http.MethodPost, "/api/v1/ha/rediscover", "", authHdr, cookies)
		if rr.Code != http.StatusOK {
			t.Fatalf("rediscover %d %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("config", func(t *testing.T) {
		rr := doReq(h, http.MethodGet, "/api/v1/config", "", nil, cookies)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "runtime_path") {
			t.Fatalf("get config %s", rr.Body.String())
		}
		rr = doReq(h, http.MethodPut, "/api/v1/config", `{"friendly_names":"bad"}`, authHdr, cookies)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("bad names %d", rr.Code)
		}
		rr = doReq(h, http.MethodPut, "/api/v1/config", `{"friendly_names":"OREGON_0001:Kitchen"}`, authHdr, cookies)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Kitchen") {
			t.Fatalf("put %d %s", rr.Code, rr.Body.String())
		}
		rr = doReq(h, http.MethodGet, "/api/v1/config/backups", "", nil, cookies)
		if rr.Code != http.StatusOK {
			t.Fatalf("backups %d", rr.Code)
		}
		rr = doReq(h, http.MethodPost, "/api/v1/config/restore", `{"slot":0}`, authHdr, cookies)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("restore %d", rr.Code)
		}
	})

	t.Run("tokens webhooks sessions", func(t *testing.T) {
		rr := doReq(h, http.MethodPost, "/api/v1/tokens", `{"name":"night","expire":"30d","scopes":["read"]}`, authHdr, cookies)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"token"`) {
			t.Fatalf("create token %d %s", rr.Code, rr.Body.String())
		}
		rr = doReq(h, http.MethodGet, "/api/v1/tokens", "", nil, cookies)
		if !strings.Contains(rr.Body.String(), "night") {
			t.Fatalf("list %s", rr.Body.String())
		}
		rr = doReq(h, http.MethodDelete, "/api/v1/tokens?name=night", "", authHdr, cookies)
		if rr.Code != http.StatusOK {
			t.Fatalf("delete token %d %s", rr.Code, rr.Body.String())
		}

		rr = doReq(h, http.MethodPost, "/api/v1/webhooks", `{"name":"hook","url":"http://192.168.1.50/hook","kind":"push"}`, authHdr, cookies)
		if rr.Code != http.StatusOK {
			t.Fatalf("webhook %d %s", rr.Code, rr.Body.String())
		}
		var created struct {
			Webhook struct {
				ID string `json:"id"`
			} `json:"webhook"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil || created.Webhook.ID == "" {
			t.Fatalf("webhook id: %v %s", err, rr.Body.String())
		}
		rr = doReq(h, http.MethodPut, "/api/v1/webhooks", `{"id":"`+created.Webhook.ID+`","enabled":false}`, authHdr, cookies)
		if rr.Code != http.StatusOK {
			t.Fatalf("webhook put %d %s", rr.Code, rr.Body.String())
		}
		rr = doReq(h, http.MethodGet, "/api/v1/webhooks", "", nil, cookies)
		if rr.Code != http.StatusOK {
			t.Fatalf("webhook list %d", rr.Code)
		}
		rr = doReq(h, http.MethodDelete, "/api/v1/webhooks?id="+created.Webhook.ID, "", authHdr, cookies)
		if rr.Code != http.StatusOK {
			t.Fatalf("webhook del %d %s", rr.Code, rr.Body.String())
		}

		other, err := h.app.sessions.Create("10.1.1.1")
		if err != nil {
			t.Fatal(err)
		}
		rr = doReq(h, http.MethodGet, "/api/v1/sessions", "", nil, cookies)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"current": true`) {
			t.Fatalf("sessions %s", rr.Body.String())
		}
		rr = doReq(h, http.MethodDelete, "/api/v1/sessions", `{"id":"`+other.ID+`"}`, authHdr, cookies)
		if rr.Code != http.StatusOK {
			t.Fatalf("revoke %d %s", rr.Code, rr.Body.String())
		}
		rr = doReq(h, http.MethodDelete, "/api/v1/sessions", `{"all":true}`, authHdr, cookies)
		if rr.Code != http.StatusOK {
			t.Fatalf("revoke all %d", rr.Code)
		}
	})

	t.Run("cors request id logout", func(t *testing.T) {
		rr := doReq(h, http.MethodOptions, "/api/v1/health", "", map[string]string{
			"Origin":       "https://ha.example",
			"X-Request-ID": "abcd1234",
		}, nil)
		if rr.Code != http.StatusNoContent || rr.Header().Get("Access-Control-Allow-Origin") != "https://ha.example" {
			t.Fatalf("cors %d %s", rr.Code, rr.Header().Get("Access-Control-Allow-Origin"))
		}
		if rr.Header().Get(headerRequestID) != "abcd1234" {
			t.Fatalf("rid %s", rr.Header().Get(headerRequestID))
		}
		rr = doReq(h, http.MethodDelete, "/api/v1/session", "", authHdr, cookies)
		if rr.Code != http.StatusOK {
			t.Fatalf("logout %d %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("panic recovered", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/boom", nil)
		h.middlewareRecovery(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic("boom")
		})).ServeHTTP(rr, req)
		if rr.Code != http.StatusInternalServerError || !strings.Contains(rr.Body.String(), "internal_server_error") {
			t.Fatalf("panic %d %s", rr.Code, rr.Body.String())
		}
	})
}
