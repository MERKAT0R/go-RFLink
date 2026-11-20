/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func stubClient(status int, body string, called *int) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		if called != nil {
			*called++
		}
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})}
}

func TestWebSocketAuthAndHub(t *testing.T) {
	app, h := newCoverageStack(t)

	t.Run("origin", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/rflink/raw/ws", nil)
		req.Host = "localhost"
		if !wsUpgrader.CheckOrigin(req) {
			t.Fatal("empty origin")
		}
		req.Header.Set("Origin", "https://evil.example")
		if wsUpgrader.CheckOrigin(req) {
			t.Fatal("foreign origin")
		}
		req.Header.Set("Origin", "http://localhost")
		if !wsUpgrader.CheckOrigin(req) {
			t.Fatal("same host")
		}
		req.Header.Set("Origin", "://")
		if wsUpgrader.CheckOrigin(req) {
			t.Fatal("bad origin")
		}
	})

	t.Run("credentials", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/ws?token=readtoken123456", nil)
		if _, _, ok := h.wsCredentials(req); ok {
			t.Fatal("query token")
		}
		req.Header.Set("X-API-Token", "readtoken123456")
		if _, tok, ok := h.wsCredentials(req); !ok || tok == "" {
			t.Fatal("read token")
		}
		req.Header.Del("X-API-Token")
		req.Header.Set("Authorization", "Bearer cmdtoken12345678")
		if _, _, ok := h.wsCredentials(req); ok {
			t.Fatal("command token")
		}
		sess, err := app.sessions.Create("203.0.113.10")
		if err != nil {
			t.Fatal(err)
		}
		req = httptest.NewRequest(http.MethodGet, "/ws", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sess.ID})
		sid, _, ok := h.wsCredentials(req)
		if !ok || sid == "" {
			t.Fatal("session")
		}
		app.sessions.Revoke(sess.ID)
		if h.wsStillAllowed(sess.ID, "") {
			t.Fatal("revoked session")
		}
		if h.wsStillAllowed("", "") {
			t.Fatal("empty")
		}
		if !h.wsStillAllowed("", "readtoken123456") {
			t.Fatal("token still allowed")
		}
		if subtleConstantTimeEq("a", "b") || !subtleConstantTimeEq("same", "same") {
			t.Fatal("constant compare")
		}
	})

	t.Run("method and unauthorized", func(t *testing.T) {
		rec := doReq(h, http.MethodPost, "/api/v1/rflink/raw/ws", "", nil, nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("post %d", rec.Code)
		}
		rec = doReq(h, http.MethodGet, "/api/v1/rflink/raw/ws", "", nil, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("get %d", rec.Code)
		}
	})

	t.Run("upgrade hello", func(t *testing.T) {
		app.publisher.appendRawLine("20;02;PONG;")
		srv := httptest.NewServer(h.server.Handler)
		t.Cleanup(srv.Close)
		u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/rflink/raw/ws"
		hdr := http.Header{}
		hdr.Set("Authorization", "Bearer readtoken123456")
		conn, resp, err := (&websocket.Dialer{HandshakeTimeout: 2 * time.Second}).Dial(u, hdr)
		if err != nil {
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			t.Fatalf("dial: %v status %d", err, status)
		}
		defer func() { _ = conn.Close() }()
		var hello struct {
			Type   string `json:"type"`
			Buffer []struct {
				Message string `json:"message"`
			} `json:"buffer"`
		}
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if err := conn.ReadJSON(&hello); err != nil {
			t.Fatal(err)
		}
		if hello.Type != "hello" || len(hello.Buffer) == 0 || hello.Buffer[0].Message != "20;02;PONG;" {
			t.Fatalf("hello %+v", hello)
		}
	})

	t.Run("hub drop and double unsubscribe", func(t *testing.T) {
		hub := newRawHub()
		ch := hub.subscribe()
		for range 32 {
			hub.broadcast(rawLineEntry{Message: "a"})
		}
		done := make(chan struct{})
		go func() {
			hub.broadcast(rawLineEntry{Message: "overflow"})
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("broadcast blocked")
		}
		hub.unsubscribe(ch)
		hub.unsubscribe(ch)
	})
}

func TestWebhookPollPushAndSanitize(t *testing.T) {
	app, _ := newCoverageStack(t)
	m := app.webhooks
	hook := RuntimeWebhook{Name: "poller", URL: "http://10.1.2.3/hook", Kind: "poll", Method: "GET"}

	t.Run("rejects before dial", func(t *testing.T) {
		calls := 0
		m.client = stubClient(200, "10;PING;", &calls)
		m.doPollOnce(context.Background(), RuntimeWebhook{Name: "p", URL: ""}, http.MethodGet, 2, 100)
		m.doPollOnce(context.Background(), RuntimeWebhook{Name: "p", URL: "http://127.0.0.1/x"}, http.MethodGet, 2, 100)
		if calls != 0 {
			t.Fatalf("dialed %d", calls)
		}
	})

	t.Run("non-2xx empty and too large", func(t *testing.T) {
		calls := 0
		m.client = stubClient(501, "10;PING;", &calls)
		m.doPollOnce(context.Background(), hook, http.MethodGet, 2, 100)
		m.client = stubClient(200, "\n\n", &calls)
		m.doPollOnce(context.Background(), hook, http.MethodGet, 2, 100)
		m.client = stubClient(200, strings.Repeat("A", 40), &calls)
		m.doPollOnce(context.Background(), hook, http.MethodGet, 2, 8)
		port := app.serial.port.(*fakeSerial)
		port.mu.Lock()
		n := len(port.written)
		port.mu.Unlock()
		if n != 0 {
			t.Fatalf("wrote %d", n)
		}
	})

	t.Run("first line only", func(t *testing.T) {
		m.client = stubClient(200, "10;PING;\n10;REBOOT;\n", nil)
		m.doPollOnce(context.Background(), hook, http.MethodGet, 2, 200)
		port := app.serial.port.(*fakeSerial)
		port.mu.Lock()
		defer port.mu.Unlock()
		if len(port.written) != 1 || !strings.Contains(string(port.written[0]), "10;PING;") {
			t.Fatalf("written %#v", port.written)
		}
		if strings.Contains(string(port.written[0]), "REBOOT") {
			t.Fatal("second line")
		}
	})

	t.Run("push and drop", func(t *testing.T) {
		calls := 0
		m.client = stubClient(200, "ok", &calls)
		m.doPush(RuntimeWebhook{Name: "bad", URL: "http://127.0.0.1/x"}, []byte("x"), "text/plain")
		if calls != 0 {
			t.Fatal("push dialed loopback")
		}
		m.doPush(RuntimeWebhook{Name: "ok", URL: "http://10.1.2.3/push", Method: "POST"}, []byte(`{}`), "application/json")
		if calls != 1 {
			t.Fatalf("push calls %d", calls)
		}
		for range maxConcurrentPush {
			m.pushSem <- struct{}{}
		}
		m.enqueuePush(RuntimeWebhook{Name: "drop", URL: "http://10.1.2.3/push"}, []byte("x"), "text/plain")
		if calls != 1 {
			t.Fatal("dropped push still ran")
		}
		for range maxConcurrentPush {
			<-m.pushSem
		}
	})

	t.Run("sanitize validate and view", func(t *testing.T) {
		var list []RuntimeWebhook
		for i := range 9 {
			list = append(list, RuntimeWebhook{
				Name: string(rune('a' + i)), Kind: "push", URL: "http://10.1.2.3/h", Method: "POST", Payload: "raw",
			})
		}
		list = append(list, RuntimeWebhook{Name: "bad name", Kind: "nope", URL: "http://10.1.2.3/h"})
		got := sanitizeRuntimeWebhooks(list)
		if len(got) != maxWebhooks {
			t.Fatalf("kept %d", len(got))
		}
		if err := ValidateWebhookConfig(nil); err == nil {
			t.Fatal("nil")
		}
		bad := RuntimeWebhook{Name: "ok", Kind: "poll", URL: "http://10.1.2.3/h", IntervalSec: 1}
		if err := ValidateWebhookConfig(&bad); err == nil {
			t.Fatal("interval")
		}
		hdr := RuntimeWebhook{Name: "ok", Kind: "push", URL: "http://10.1.2.3/h", Headers: map[string]string{"X": "a\nb"}}
		if err := ValidateWebhookConfig(&hdr); err == nil {
			t.Fatal("crlf")
		}
		expanded, err := expandWebhookGETURL("http://10.1.2.3/h", "raw", []byte("20;01;PING;"))
		if err != nil || !strings.Contains(expanded, "raw=") {
			t.Fatalf("%s %v", expanded, err)
		}
		view := webhooksPublicView([]RuntimeWebhook{{
			Name: "n", Headers: map[string]string{"Authorization": "super-secret-value"},
		}})
		raw, err := json.Marshal(view)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "super-secret-value") {
			t.Fatal("header secret leaked")
		}
		id := newWebhookID()
		if len(id) < 8 {
			t.Fatal(id)
		}
	})

	t.Run("stop then reload", func(t *testing.T) {
		fresh := newWebhookManager(app)
		fresh.Stop()
		fresh.Reload()
		if !fresh.stopped || fresh.cancel != nil {
			t.Fatal("reload after stop")
		}
		fresh.Stop()
	})
}

func TestRuntimeHAAndBackups(t *testing.T) {
	app, _ := newCoverageStack(t)
	rm := app.runtime
	var none *runtimeManager
	if none.consumeHAChanged() {
		t.Fatal("nil manager")
	}
	on := true
	rm.mu.Lock()
	rm.applyLocked()
	rm.file.HADiscovery = &on
	rm.applyLocked()
	prefix := rm.opts.Publish.HADiscoveryPrefix
	rm.mu.Unlock()
	if prefix != "homeassistant" {
		t.Fatalf("prefix %q", prefix)
	}
	if !rm.consumeHAChanged() {
		t.Fatal("ha change")
	}
	if rm.consumeHAChanged() {
		t.Fatal("ha change stuck")
	}

	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := moveFileReplace(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("src remains")
	}
	if err := os.WriteFile(src, []byte("newer"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := moveFileReplace(src, dst); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(dst)
	if err != nil || string(body) != "newer" {
		t.Fatalf("%q %v", body, err)
	}
}

func TestMQTTHelpersHASpecAndStop(t *testing.T) {
	app, h := newCoverageStack(t)
	p := app.publisher

	for _, tc := range []struct{ in, want string }{
		{"mqtt", "mqtt"},
		{"tcp", "mqtt"},
		{"ssl", "tls"},
		{"ws", "ws"},
		{"wss", "wss"},
	} {
		o := &Options{}
		o.Publish.Scheme = tc.in
		o.Publish.Host = "broker.example:1883"
		u, err := buildBrokerURL(o)
		if err != nil || u.Scheme != tc.want || u.Host != "broker.example:1883" {
			t.Fatalf("%s -> %+v %v", tc.in, u, err)
		}
	}
	badURL := &Options{}
	badURL.Publish.Scheme = "mqtt"
	badURL.Publish.Host = "%"
	if _, err := buildBrokerURL(badURL); err == nil {
		t.Fatal("bad host")
	}
	plain := &Options{}
	plain.Publish.Scheme = "mqtt"
	plain.Publish.TLSCAFile = "unused"
	if cfg, err := buildTLSConfig(plain); err != nil || cfg != nil {
		t.Fatalf("plain tls cfg %v %v", cfg, err)
	}
	wss := &Options{}
	wss.Publish.Scheme = "wss"
	if !wss.isTLSScheme() {
		t.Fatal("wss")
	}
	plainScheme := &Options{}
	plainScheme.Publish.Scheme = "mqtt"
	if plainScheme.isTLSScheme() {
		t.Fatal("mqtt tls")
	}
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, mustTestCertPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	tlsOpts := &Options{}
	tlsOpts.Publish.Scheme = "mqtts"
	tlsOpts.Publish.TLSCAFile = caPath
	cfg, err := buildTLSConfig(tlsOpts)
	if err != nil || cfg == nil || cfg.RootCAs == nil {
		t.Fatalf("ca %v %v", cfg, err)
	}
	bad := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(bad, []byte("not a cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	badPEM := &Options{}
	badPEM.Publish.Scheme = "mqtts"
	badPEM.Publish.TLSCAFile = bad
	if _, err := buildTLSConfig(badPEM); err == nil {
		t.Fatal("bad pem")
	}
	missing := &Options{}
	missing.Publish.Scheme = "mqtts"
	missing.Publish.TLSCAFile = filepath.Join(dir, "missing")
	if _, err := buildTLSConfig(missing); err == nil {
		t.Fatal("missing ca")
	}

	if dedupKey("20;AC;Oregon;ID=1;") != dedupKey("20;AD;Oregon;ID=1;") {
		t.Fatal("dedup key")
	}
	p.opts.Publish.DedupWindowMs = 0
	if p.isDuplicate("20;01;Oregon;ID=1;") {
		t.Fatal("window 0")
	}
	p.opts.Publish.DedupWindowMs = 3000
	p.markDedup("20;01;Oregon;ID=1;")
	if !p.isDuplicate("20;02;Oregon;ID=1;") {
		t.Fatal("retransmit")
	}
	if err := validateCmdTopic("rflink", "rflink"); err == nil {
		t.Fatal("same topic")
	}
	if err := validateCmdTopic("rflink/sensors", "rflink"); err == nil {
		t.Fatal("parent")
	}
	if err := validateCmdTopic("rflink", "rflink/send"); err != nil {
		t.Fatal(err)
	}
	if sanitizeTopicSegment("A/B C") != "A_B_C" {
		t.Fatal(sanitizeTopicSegment("A/B C"))
	}
	if p.sensorFieldTopic("A/B", "x-y") != "rflink/A_B/x-y" {
		t.Fatal(p.sensorFieldTopic("A/B", "x-y"))
	}
	sd := &SensorData{Model: "Oregon", Id: "0001", Hwid: "OREGON_0001"}
	p.applyFriendlyName(sd)
	if sd.FriendlyName == "" {
		t.Fatal("friendly")
	}
	if err := p.SubscribeCommands(nil); err != nil {
		t.Fatal(err)
	}
	if err := p.Resubscribe(); err != nil {
		t.Fatal(err)
	}
	if err := p.publishRaw(context.Background(), "rflink/x", "v", 0, false); err == nil {
		t.Fatal("publish without client")
	}
	p.opts.Publish.PendingTTLSec = 60
	p.pending = map[string]pendingSensor{
		"OREGON_0001": {sd: sd, storedAt: time.Now()},
	}
	p.flushPending(context.Background())
	if len(p.pending) != 0 {
		t.Fatal("pending remains")
	}

	temp := resolveHAEntitySpec("temp", "20.0")
	if temp.component != "sensor" || temp.nameSuffix != "Temperature" {
		t.Fatalf("%+v", temp)
	}
	onOff := resolveHAEntitySpec("sw_04", "ON")
	if onOff.component != "binary_sensor" || onOff.offDelay == nil || *onOff.offDelay != 1 {
		t.Fatalf("%+v", onOff)
	}
	up := resolveHAEntitySpec("sw_04", "UP")
	if up.component != "sensor" || up.payloadOn != "" {
		t.Fatalf("%+v", up)
	}
	baro := resolveHAEntitySpec("baro", "1013")
	if baro.nameSuffix != "Baro" {
		t.Fatal(baro.nameSuffix)
	}
	if !hasSwitchButtonFields(map[string]string{"sw_01": "ON"}) {
		t.Fatal("sw")
	}
	p.publishHomeAssistantDiscovery(context.Background(), sd, map[string]string{
		"switch": "1", "cmd": "ON", "sw_01": "ON", "temp": "20.0",
	}, "homeassistant")
	old := Version
	Version = ""
	t.Cleanup(func() { Version = old })
	origin := haOriginInfo()
	if origin.Name != "go-rflink" || origin.SWVersion != "dev" {
		t.Fatalf("%+v", origin)
	}
	if haDefaultEntityID("sensor", "oregon_temp") != "sensor.oregon_temp" {
		t.Fatal(haDefaultEntityID("sensor", "oregon_temp"))
	}
	buttons := parseHAButtons("Kitchen:10;NewKaku;1;1;ON")
	if len(buttons) != 1 || !strings.HasSuffix(buttons[0].Command, ";") {
		t.Fatalf("%+v", buttons)
	}

	app.Stop()
	if h.server == nil {
		t.Fatal("server pointer cleared on the wrapper only")
	}
}

func mustTestCertPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
