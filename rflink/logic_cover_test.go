/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/gorilla/websocket"
)

func TestGetOptionsClampsAndRejects(t *testing.T) {
	t.Setenv("PUBLISH_QOS", "9")
	if _, err := GetOptions(); err == nil {
		t.Fatal("qos 9 accepted")
	}
	t.Setenv("PUBLISH_QOS", "0")
	t.Setenv("PUBLISH_LWT_QOS", "0")
	t.Setenv("PUBLISH_TEMPERATURE_UNIT", "kelvin")
	if _, err := GetOptions(); err == nil {
		t.Fatal("bad unit accepted")
	}
	t.Setenv("PUBLISH_TEMPERATURE_UNIT", "C")
	t.Setenv("PUBLISH_KEEPALIVE", "999999")
	t.Setenv("PUBLISH_TOPIC", "rflink/")
	t.Setenv("PUBLISH_CMD_TOPIC", "")
	t.Setenv("PUBLISH_SESSION_EXPIRY", "-5")
	t.Setenv("SERIAL_READ_TIMEOUT_SEC", "999999")
	t.Setenv("SERIAL_RECONNECT_MAX_DELAY_SEC", "999999")
	t.Setenv("PUBLISH_DEDUP_WINDOW_MS", "999999999")
	opts, err := GetOptions()
	if err != nil {
		t.Fatal(err)
	}
	if opts.Publish.KeepAlive != 65535 {
		t.Fatalf("keepalive %d", opts.Publish.KeepAlive)
	}
	if opts.Publish.Topic != "rflink" {
		t.Fatalf("topic %q", opts.Publish.Topic)
	}
	if opts.Publish.CmdTopic != "rflink/send" {
		t.Fatalf("cmd %q", opts.Publish.CmdTopic)
	}
	if opts.Serial.ReadTimeoutSec != 120 {
		t.Fatalf("read timeout %d", opts.Serial.ReadTimeoutSec)
	}
	if opts.Serial.ReconnectMaxDelaySec != 86400 {
		t.Fatalf("backoff %d", opts.Serial.ReconnectMaxDelaySec)
	}
	if opts.Publish.DedupWindowMs != 86400000 {
		t.Fatalf("dedup %d", opts.Publish.DedupWindowMs)
	}
	if opts.mqttKeepAlive() != 65535 || opts.mqttSessionExpiry() != 0 {
		t.Fatal("mqtt duration helpers")
	}
}

func TestValidatorsAndNames(t *testing.T) {
	if err := ValidateFriendlyNamesCSV(""); err != nil {
		t.Fatal(err)
	}
	if err := ValidateFriendlyNamesCSV("BAD:name"); err != nil {
		t.Fatal(err)
	}
	if ValidateFriendlyNamesCSV("no-colon") == nil || ValidateFriendlyNamesCSV("ab/cd:Name") == nil {
		t.Fatal("friendly names accepted junk")
	}
	if ValidateHWIDMapCSV("1:SENSOR") != nil {
		t.Fatal(ValidateHWIDMapCSV("1:SENSOR"))
	}
	if ValidateHWIDMapCSV("only") == nil {
		t.Fatal("hwid map")
	}
	if ValidateIgnoreListCSV("Oregon") != nil || ValidateIgnoreListCSV("a/b") == nil {
		t.Fatal("ignore list")
	}
	if ValidateHAButtonsCSV("Lamp:10;NewKaku;1;1;ON;") != nil {
		t.Fatal(ValidateHAButtonsCSV("Lamp:10;NewKaku;1;1;ON;"))
	}
	if ValidateHAButtonsCSV("Lamp:not a command") == nil {
		t.Fatal("button")
	}
	if ValidateHASwitchesCSV("Lamp:10;NewKaku;1;1;ON:10;NewKaku;1;1;OFF") != nil {
		t.Fatal("switch")
	}
	if ValidateHASwitchesCSV("Lamp:only") == nil {
		t.Fatal("switch shape")
	}
	if ValidateHADiscoveryPrefix("home/assistant") != nil || ValidateHADiscoveryPrefix("bad prefix") == nil {
		t.Fatal("prefix")
	}
	if ValidateBoundedInt("min_interval_ms", 0, 10) != nil || ValidateBoundedInt("min_interval_ms", 10, 10) != nil {
		t.Fatal("bounded int edges")
	}
	if ValidateBoundedInt("min_interval_ms", -1, 10) == nil || ValidateBoundedInt("min_interval_ms", 11, 10) == nil {
		t.Fatal("bounded int accepted an out of range value")
	}
	if ValidateThrottleCSV("") != nil || ValidateThrottleCSV("OREGON_0001:5000, OTHER_2:1") != nil {
		t.Fatal(ValidateThrottleCSV("OREGON_0001:5000, OTHER_2:1"))
	}
	for _, bad := range []string{"nope", "BAD/ID:10", "OREGON_0001:0", "OREGON_0001:86400001", "A:1\nB:2"} {
		if ValidateThrottleCSV(bad) == nil {
			t.Fatalf("throttle accepted %q", bad)
		}
	}
	longThrottle := strings.Repeat("A_1:1,", 65)
	if ValidateThrottleCSV(longThrottle) == nil {
		t.Fatal("throttle accepted too many entries")
	}
	if ValidateCommandDevicesCSV("") != nil || ValidateCommandDevicesCSV(" COM1 , tcp://10.0.0.8:23 ") != nil {
		t.Fatal("command devices")
	}
	if ValidateCommandDevicesCSV("COM1\nCOM2") == nil || ValidateCommandDevicesCSV(strings.Repeat("p,", 17)) == nil || ValidateCommandDevicesCSV(strings.Repeat("x", 257)) == nil {
		t.Fatal("command devices accepted junk")
	}
	if firstTimeWarning(nil) != "" || !strings.Contains(firstTimeWarning([]string{"min_interval_ms"}), "min_interval_ms") {
		t.Fatal(firstTimeWarning([]string{"min_interval_ms"}))
	}
	var noOpts *Options
	if !noOpts.commandDeviceAllow("COM1") || noOpts.commandDevicesConfigured() {
		t.Fatal("nil options filter")
	}
	filter := &Options{}
	filter.Serial.CommandDevices = "com1"
	if !filter.commandDeviceAllow("COM1") || filter.commandDeviceAllow("COM2") || !filter.commandDevicesConfigured() {
		t.Fatal("command device filter")
	}
	if got := filter.serialDeviceStatusTopic(""); got != "/serial/port/status" {
		t.Fatal(got)
	}
	if parseLogLevel("debug") != slog.LevelDebug || parseLogLevel("warning") != slog.LevelWarn || parseLogLevel("error") != slog.LevelError || parseLogLevel("") != slog.LevelInfo {
		t.Fatal("log level")
	}
	if !isNonLocalListen(":8080") || !isNonLocalListen("0.0.0.0:1") || isNonLocalListen("127.0.0.1:80") || isNonLocalListen("localhost:80") {
		t.Fatal("listen scope")
	}
}

func TestSessionStoreAndCookies(t *testing.T) {
	s := newSessionStore()
	var first *session
	for i := range maxSessions {
		sess, err := s.Create("203.0.113.8")
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = sess
		}
	}
	first.CreatedAt = time.Now().Add(-time.Hour)
	if _, err := s.Create("203.0.113.8"); err != nil {
		t.Fatal(err)
	}
	if s.Count() != maxSessions {
		t.Fatalf("count %d", s.Count())
	}
	if s.Get(first.ID) != nil {
		t.Fatal("oldest session was kept")
	}
	cur, err := s.Create("203.0.113.9")
	if err != nil {
		t.Fatal(err)
	}
	cur.ExpiresAt = time.Now().Add(-time.Second)
	if s.Get(cur.ID) != nil {
		t.Fatal("expired session returned")
	}
	live, err := s.Create("203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Revoke(live.ID) || s.Revoke(live.ID) {
		t.Fatal("revoke")
	}
	a, _ := s.Create("1.1.1.1")
	a.CreatedAt = time.Now().Add(time.Hour)
	_, _ = s.Create("1.1.1.2")
	if s.RevokeAllExcept(a.ID) < 1 || s.Count() != 1 {
		t.Fatalf("count after revoke %d", s.Count())
	}
	if len(s.List("")) != 1 {
		t.Fatal("list")
	}
	rr := httptest.NewRecorder()
	setSessionCookie(rr, a, true)
	clearSessionCookie(rr, false)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: a.ID})
	if sessionIDFromRequest(req) != a.ID || sessionIDFromRequest(httptest.NewRequest(http.MethodGet, "/", nil)) != "" {
		t.Fatal("cookie read")
	}
}

func TestCommandRatePingAndShutdown(t *testing.T) {
	opts := &Options{}
	opts.Publish.CommandRateLimit = 1
	opts.Publish.CommandMaxLen = 32
	opts.Serial.PingIntervalSec = 60
	opts.Serial.ReadTimeoutSec = 30
	app := &App{
		opts:        opts,
		publisher:   &Publisher{opts: opts, serialRestart: make(chan struct{}, 1)},
		serial:      &SerialReader{port: newFakeSerial(), opts: opts},
		cmdTokens:   1,
		cmdLastFill: time.Now(),
		ctx:         context.Background(),
	}
	if _, err := opts.NormalizeCommand("10;PING"); err != nil {
		t.Fatal(err)
	}
	if _, err := opts.NormalizeCommand(""); err == nil {
		t.Fatal("empty")
	}
	res := app.ExecuteCommand("10;PING;")
	if res.Status != "ok" {
		t.Fatalf("first %+v", res)
	}
	res = app.ExecuteCommand("10;PING;")
	if res.Status != "rate_limited" {
		t.Fatalf("second %+v", res)
	}
	lim, _, unlimited := app.CommandRateStatus()
	if unlimited || lim != 1 {
		t.Fatalf("limit %d %v", lim, unlimited)
	}
	app.sendPing()
	app.publisher.MarkPingSent()
	app.sendPing()
	app.serial.PauseWrites()
	app.sendPing()
	app.serial.ResumeWrites()
	app.serial = &SerialReader{opts: opts}
	app.sendPing()

	if app.ShutdownGrace() < 25*time.Second {
		t.Fatal("grace")
	}
	if (&App{}).ShutdownGrace() != 25*time.Second {
		t.Fatal("default grace")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	app.wg.Add(1)
	app.runPingLoop(ctx)
	app.wg.Add(1)
	app.ctx = ctx
	app.serial = &SerialReader{port: newFakeSerial(), opts: opts}
	app.runSerialLoop(ctx)

	if classifySerialReadError(nil) != nil || !errors.Is(classifySerialReadError(io.EOF), errSerialStreamClosed) {
		t.Fatal("classify")
	}
	if classifySerialReadError(errors.New("nope")) == nil {
		t.Fatal("generic")
	}
	if err := (&SerialReader{}).Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSensorFieldsAndTokens(t *testing.T) {
	temp := float32(10)
	hum := uint16(40)
	sd := &SensorData{
		Model: "Oregon", Id: "0001", Hwid: "OREGON_0001",
		Temperature: &temp, Humidity: &hum, Bat: "OK", Cmd: "ON", Switch: "02",
		TempRaw: "00c8", Extra: map[string]string{"BARO": "03E8", "empty": ""},
	}
	if sd.String() == "" || sd.TopicID() != "OREGON_0001" {
		t.Fatal(sd.String())
	}
	f := sd.FieldsForPublish(TempUnitF)
	if f["temp"] == "" || f["sw_02"] != "ON" || f["BARO"] != "03E8" {
		t.Fatalf("%v", f)
	}
	if sd.FieldsForPublish(TempUnitRaw)["temp"] != "00c8" {
		t.Fatal("raw")
	}
	if sd.FieldsForPublish(TempUnitC)["hum"] != "40" {
		t.Fatal("c")
	}
	bare := &SensorData{Id: "1", Switch: "1"}
	if bare.TopicID() != "1" || bare.FieldsForPublish("C")["switch"] != "1" {
		t.Fatal("bare")
	}

	opts := &Options{APITokenPepper: "pepper"}
	opts.apiTokens = parseAPIAuthTokens("n:secret-token-value:never:read+admin", "pepper")
	if _, ok := opts.validAPIToken("secret-token-value"); !ok {
		t.Fatal("token")
	}
	if _, ok := opts.validAPIToken("nope"); ok {
		t.Fatal("bad token")
	}
	past := time.Now().Add(-time.Hour)
	opts.apiTokens[0].ExpiresAt = &past
	if _, ok := opts.validAPIToken("secret-token-value"); ok {
		t.Fatal("expired")
	}
	if opts.guiTokenOK("pw") {
		t.Fatal("empty gui password matched")
	}
	opts.HTTP.AuthToken = "pw"
	if !opts.guiTokenOK("pw") || opts.guiTokenOK("other") {
		t.Fatal("gui compare")
	}
	if !opts.apiTokens[0].hasScope("admin") {
		t.Fatal("scope")
	}
	if len(parseScopeList("")) < 1 || len(parseScopeList("read,read,command")) != 2 {
		t.Fatal("scopes")
	}
}

func TestWebhookAndMQTTDialGuards(t *testing.T) {
	if err := rejectDangerousIP(net.ParseIP("127.0.0.1")); err == nil {
		t.Fatal("loopback")
	}
	if err := rejectDangerousIP(net.ParseIP("169.254.169.254")); err == nil {
		t.Fatal("metadata")
	}
	if err := rejectDangerousIP(net.ParseIP("fd00:ec2::254")); err == nil {
		t.Fatal("imds6")
	}
	if err := rejectDangerousIP(net.ParseIP("10.1.1.1")); err != nil {
		t.Fatal(err)
	}
	if !isLocalLabHost("sensor.local") || isLocalLabHost("example.com") {
		t.Fatal("lab host")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := webhookDialContext(ctx, "tcp", "127.0.0.1:9"); err == nil {
		t.Fatal("dialed loopback")
	}
	if err := validateWebhookURLResolved(ctx, "http://127.0.0.1/x"); err == nil {
		t.Fatal("resolved loopback")
	}
	if webhookTransport() == nil {
		t.Fatal("transport")
	}
	_, err := dialMQTT(ctx, autopaho.ClientConfig{}, &url.URL{Scheme: "tcp", Host: "127.0.0.1:1"})
	if err == nil {
		t.Fatal("tcp dial")
	}
	_, err = dialMQTT(ctx, autopaho.ClientConfig{}, &url.URL{Scheme: "tls", Host: "127.0.0.1:1"})
	if err == nil {
		t.Fatal("tls dial")
	}
	_, err = dialMQTTWebsocket(ctx, nil, autopaho.ClientConfig{}, &url.URL{Scheme: "ws", Host: "127.0.0.1:1"})
	if err == nil {
		t.Fatal("ws dial")
	}

	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, msg, err := c.ReadMessage()
		if err != nil {
			return
		}
		_ = c.WriteMessage(websocket.BinaryMessage, msg)
	}))
	defer srv.Close()
	u, err := url.Parse("ws" + srv.URL[len("http"):])
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dialMQTTWebsocket(ctx, nil, autopaho.ClientConfig{}, u)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ws := conn.(*mqttWebsocket)
	if err := ws.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	ws.Lock()
	_, err = ws.Write([]byte("ping"))
	ws.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	n, err := ws.Read(buf)
	if err != nil || string(buf[:n]) != "ping" {
		t.Fatalf("read %q %v", buf[:n], err)
	}
	_ = ws.SetDeadline(time.Time{})
}

func TestClientIPCookieSecureAndHelpers(t *testing.T) {
	nets := parseTrustedProxies("127.0.0.1,not-a-cidr,10.0.0.0/8")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:9"
	req.Header.Set("X-Forwarded-For", "203.0.113.5, 127.0.0.1")
	if clientIP(req, nets) != "203.0.113.5" {
		t.Fatalf("xff %s", clientIP(req, nets))
	}
	req.Header.Del("X-Forwarded-For")
	req.Header.Set("X-Real-IP", "198.51.100.8")
	if clientIP(req, nets) != "198.51.100.8" {
		t.Fatal(clientIP(req, nets))
	}
	req.RemoteAddr = "203.0.113.9:9"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	if clientIP(req, nets) != "203.0.113.9" {
		t.Fatal("untrusted xff")
	}
	req.RemoteAddr = "127.0.0.1:9"
	req.TLS = nil
	req.Header.Set("X-Forwarded-Proto", "https")
	if !cookieSecure(req, nets) {
		t.Fatal("secure cookie")
	}
	if cookieSecure(req, nil) {
		t.Fatal("no proxies")
	}
	if !looksLikeRequestID("abcd1234") || looksLikeRequestID("bad id") || looksLikeRequestID("short") {
		t.Fatal("request id")
	}
	if len(newUUID()) < 32 {
		t.Fatal("uuid")
	}
	if _, err := randomHex(4); err != nil {
		t.Fatal(err)
	}
	if !sleepContext(context.Background(), 0) {
		t.Fatal("sleep 0")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepContext(ctx, time.Second) {
		t.Fatal("cancelled sleep")
	}
	if nextBackoff(0, time.Second, 2*time.Second) != time.Second {
		t.Fatal("backoff")
	}
}

func TestPublisherHelpersAndDisconnect(t *testing.T) {
	opts := &Options{}
	opts.Publish.Topic = "rflink"
	opts.Publish.DedupWindowMs = 1000
	opts.Publish.PendingTTLSec = 30
	opts.Serial.WatchdogSec = 1
	opts.Serial.PingIntervalSec = 60
	opts.Serial.PingTimeoutSec = 1
	opts.Serial.PingFailThreshold = 1
	p := &Publisher{
		opts:          opts,
		pending:       map[string]pendingSensor{},
		dedupLast:     map[string]time.Time{},
		sensorSeen:    map[string]time.Time{},
		serialRestart: make(chan struct{}, 1),
		rawCap:        2,
		sampleCap:     1,
		startedAt:     time.Now().Add(-time.Hour),
		runCtx:        context.Background(),
	}
	p.lastSensorAt.Store(time.Time{})
	p.lastSensorHWID.Store("")
	p.lastSerialAt.Store(time.Now().Add(-time.Hour))
	p.appendRawLine("", "20;02;PONG;")
	p.appendRawLine("COM2", "20;03;VER=1;")
	p.appendRawLine("", "20;04;X;")
	raw := p.RecentRawLines()
	if len(raw) != 2 || raw[0].Device != "COM2" || raw[0].Message != "20;03;VER=1;" {
		t.Fatalf("raw %+v", raw)
	}
	temp := float32(1)
	sd := &SensorData{Hwid: "A_1", Model: "A", Id: "1", Temperature: &temp}
	p.appendSensorSample(sd, map[string]string{"temp": "1.0"})
	p.appendSensorSample(sd, map[string]string{"temp": "2.0"})
	if len(p.RecentSensorSamples()) != 1 {
		t.Fatal("samples")
	}
	p.noteSensorSeen("A_1", time.Now())
	if p.SensorsLastSeen()["A_1"] == "" {
		t.Fatal("seen")
	}
	p.checkSerialWatchdog()
	snap := p.Health()
	if !snap.SerialStale || snap.Status == "" {
		t.Fatalf("%+v", snap)
	}
	p.SetRawLineHook(func(rawLineEntry) {})
	p.appendRawLine("", "20;05;Y;")
	p.SetSensorHook(func(string, []byte) {})
	p.Disconnect()
	if p.PingOutstanding() {
		t.Fatal("ping")
	}
	p.DrainSerialRestart()
	p.ResetPingState()
}
