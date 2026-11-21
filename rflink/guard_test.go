/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-json"
)

func TestExpandWebhookPlaceholderDoesNotSplitPath(t *testing.T) {
	tmpl := "https://hooks.example/in/{{payload}}/end"
	got, err := expandWebhookPlaceholders(tmpl, []byte("a/../b&c=1"))
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "{") || strings.Contains(u.EscapedPath(), "/../") {
		t.Fatalf("expanded %s path %s", got, u.EscapedPath())
	}
	if strings.Count(u.EscapedPath(), "/") != strings.Count("/in/x/end", "/") {
		t.Fatalf("path segments changed: %s", u.EscapedPath())
	}
	if u.Hostname() != "hooks.example" {
		t.Fatalf("host %s", u.Hostname())
	}
}

func TestValidateRejectsSecondCommand(t *testing.T) {
	if err := ValidateRFLinkCommand("10;PING;10;REBOOT;"); err == nil {
		t.Fatal("stacked command accepted")
	}
	if err := ValidateRFLinkCommand("10;NewKaku;0cac142;10;ON;"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRFLinkCommand("11;20;01;LacrosseV4;ID=0002;TEMP=00cd;"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRFLinkCommand("10;PING ;10;REBOOT;"); err == nil {
		t.Fatal("spaced stacked command accepted")
	}
	if err := ValidateRFLinkCommand("10;NewKaku;00c142;1;15;10;REBOOT;"); err == nil {
		t.Fatal("dimmer stacked command accepted")
	}
	if err := ValidateRFLinkCommand("10;NewKaku;00c142;1;15;"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRFLinkCommand("10;PING;\x7f;"); err == nil {
		t.Fatal("DEL accepted")
	}
}

func TestCommandWhitelistFieldBoundary(t *testing.T) {
	o := &Options{commandWhitelist: parseCommandWhitelist("1")}
	if o.CommandAllowed("10;REBOOT;") || o.CommandAllowed("11;REBOOT;") {
		t.Fatal("digit 1 matched node 10 or 11")
	}
	o.commandWhitelist = parseCommandWhitelist("10")
	if !o.CommandAllowed("10;REBOOT;") {
		t.Fatal("10 should allow node 10")
	}
	if o.CommandAllowed("11;REBOOT;") {
		t.Fatal("10 allowed node 11")
	}
	o.commandWhitelist = parseCommandWhitelist("10;PING")
	if !o.CommandAllowed("10;PING;") {
		t.Fatal("ping prefix rejected PING")
	}
	if o.CommandAllowed("10;PINGPONG;") {
		t.Fatal("ping prefix allowed PINGPONG")
	}
	o.commandWhitelist = parseCommandWhitelist("10;ping;")
	if !o.CommandAllowed("10;PING;") {
		t.Fatal("case fold rejected PING")
	}
}

func TestSensorSeenDropsOldest(t *testing.T) {
	p := &Publisher{sensorSeen: map[string]time.Time{}}
	start := time.Now()
	for i := range maxSensorSeen + 10 {
		p.noteSensorSeen(fmt.Sprintf("ID%d", i), start.Add(time.Duration(i)*time.Millisecond))
	}
	if len(p.sensorSeen) != maxSensorSeen {
		t.Fatalf("len %d", len(p.sensorSeen))
	}
	if _, ok := p.sensorSeen["ID0"]; ok {
		t.Fatal("oldest kept")
	}
}

func TestStorePendingExpiresAndCaps(t *testing.T) {
	p := &Publisher{
		pending: make(map[string]pendingSensor),
		opts:    &Options{},
	}
	p.opts.Publish.PendingTTLSec = 60
	temp := float32(1)
	p.pending["OLD"] = pendingSensor{sd: &SensorData{Hwid: "OLD"}, storedAt: time.Now().Add(-time.Hour)}
	p.storePending(&SensorData{Hwid: "NEW", Temperature: &temp})
	if _, ok := p.pending["OLD"]; ok {
		t.Fatal("expired entry kept")
	}
	for i := range maxPendingSensors + 5 {
		p.storePending(&SensorData{Hwid: fmt.Sprintf("H%04d", i), Temperature: &temp})
	}
	if len(p.pending) != maxPendingSensors {
		t.Fatalf("pending %d", len(p.pending))
	}
}

func TestBadTempKeepsRestOfFrame(t *testing.T) {
	sd, err := SensorDataFromMessage("20;01;Oregon;ID=0001;TEMP=zz;HUM=55;")
	if err != nil {
		t.Fatal(err)
	}
	if sd.Id != "0001" || sd.Humidity == nil || *sd.Humidity != 55 || sd.Temperature != nil {
		t.Fatalf("id %q hum %#v temp %#v", sd.Id, sd.Humidity, sd.Temperature)
	}
}

func TestRetainedStateDropsMomentaryCommand(t *testing.T) {
	temp := float32(20)
	sd := &SensorData{
		Hwid: "OREGON_0001", Temperature: &temp, Cmd: "ON", Switch: "01",
		Extra: map[string]string{"smoke": "ON", "baro": "03E1"},
	}
	got := sd.RetainedState()
	if got.Cmd != "" || got.Switch != "" || got.Extra["smoke"] != "" || got.Extra["baro"] != "03E1" {
		t.Fatalf("%#v", got)
	}
	if sd.Cmd != "ON" {
		t.Fatal("source mutated")
	}
}

func TestPingWriteInFlightIsNotAMiss(t *testing.T) {
	p := &Publisher{opts: &Options{}}
	p.opts.Serial.PingIntervalSec = 60
	p.opts.Serial.PingTimeoutSec = 1
	p.opts.Serial.PingFailThreshold = 1
	p.pingWrite = true
	p.pingSentAt = time.Now().Add(-time.Minute)
	p.checkPingTimeout()
	if p.pingFails.Load() != 0 || p.rflinkUnresp.Load() {
		t.Fatalf("fails %d unresp %v", p.pingFails.Load(), p.rflinkUnresp.Load())
	}
	p.pingWrite = false
	p.checkPingTimeout()
	if p.pingFails.Load() != 1 || !p.rflinkUnresp.Load() {
		t.Fatalf("fails %d unresp %v", p.pingFails.Load(), p.rflinkUnresp.Load())
	}
	p.NotePong()
	if p.pingFails.Load() != 0 || p.rflinkUnresp.Load() {
		t.Fatal("pong did not clear the miss")
	}
}

func TestPingTimeoutSignalsSerialRestartOnce(t *testing.T) {
	p := &Publisher{
		opts:          &Options{},
		serialRestart: make(chan struct{}, 1),
	}
	p.opts.Serial.PingIntervalSec = 60
	p.opts.Serial.PingTimeoutSec = 1
	p.opts.Serial.PingFailThreshold = 2
	p.pingSentAt = time.Now().Add(-time.Minute)
	p.checkPingTimeout()
	select {
	case <-p.serialRestart:
		t.Fatal("restart signaled before the threshold")
	default:
	}
	p.pingSentAt = time.Now().Add(-time.Minute)
	p.checkPingTimeout()
	select {
	case <-p.serialRestart:
	default:
		t.Fatal("restart was not signaled")
	}
	p.pingSentAt = time.Now().Add(-time.Minute)
	p.checkPingTimeout()
	select {
	case <-p.serialRestart:
		t.Fatal("restart signaled again while already unresponsive")
	default:
	}
	p.ResetPingState()
	if p.pingFails.Load() != 0 || p.rflinkUnresp.Load() || p.PingOutstanding() {
		t.Fatal("reset left ping state behind")
	}
}

func TestExecuteCommandSkipsWriteWhileStopping(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := (&App{ctx: ctx}).ExecuteCommand("10;PING;")
	if res.Status != "stopping" {
		t.Fatalf("%+v", res)
	}
}

func TestPongSkipsRestartAndCaseFoldedPorts(t *testing.T) {
	p := &Publisher{
		opts:          &Options{},
		serialRestart: make(chan struct{}, 1),
	}
	p.opts.Serial.PingIntervalSec = 1
	p.opts.Serial.PingTimeoutSec = 1
	p.opts.Serial.PingFailThreshold = 1
	p.MarkPingSent()
	p.MarkPingWritten()
	p.pingMu.Lock()
	p.pingSentAt = time.Now().Add(-time.Minute)
	p.pingMu.Unlock()
	beforePingRestart = func() { p.NotePong() }
	defer func() { beforePingRestart = nil }()
	p.checkPingTimeout()
	select {
	case <-p.serialRestart:
		t.Fatal("restart after PONG")
	default:
	}

	opts := &Options{}
	opts.Serial.Device = "COM1"
	opts.Serial.Devices = "com1,COM2"
	if got := opts.serialDeviceList(); len(got) != 2 || got[0] != "COM1" || got[1] != "COM2" {
		t.Fatalf("%#v", got)
	}
}

type panicWrite struct{ *fakeSerial }

func (p *panicWrite) Write([]byte) (int, error) { panic("ping write") }

func TestPingPanicClearsOutstanding(t *testing.T) {
	sr := &SerialReader{port: &panicWrite{fakeSerial: newFakeSerial()}}
	p := &Publisher{}
	app := &App{publisher: p, serial: sr}
	func() {
		defer func() { _ = recover() }()
		app.sendPingTo(sr, true)
	}()
	if p.PingOutstanding() || p.pingWrite {
		t.Fatal("ping stayed outstanding after panic")
	}
}

func TestTLSConfigCarriesServerName(t *testing.T) {
	o := &Options{}
	o.Publish.Scheme = "mqtts"
	o.Publish.Host = "broker.example:8883"
	cfg, err := buildTLSConfig(o)
	if err != nil || cfg == nil || cfg.ServerName != "broker.example" {
		t.Fatalf("%v %+v", err, cfg)
	}
	if mqttServerName("[2001:db8::1]:8883") != "2001:db8::1" {
		t.Fatal(mqttServerName("[2001:db8::1]:8883"))
	}
}

func TestThrottleUsesSanitizedHWID(t *testing.T) {
	got := parseThrottleMap("UPM/Esic_0001:5000")
	if got["UPM_ESIC_0001"] != 5*time.Second {
		t.Fatalf("%v", got)
	}
	o := &Options{}
	o.throttleBy = got
	if o.throttleFor("UPM/Esic_0001") != 5*time.Second {
		t.Fatal(o.throttleFor("UPM/Esic_0001"))
	}
}

func TestSessionLivedLongEnoughIgnoresCloseWait(t *testing.T) {
	if sessionLivedLongEnough(time.Millisecond, 3*time.Second) {
		t.Fatal("a short read reset the delay")
	}
	if !sessionLivedLongEnough(3*time.Second, 3*time.Second) {
		t.Fatal("a live session kept growing")
	}
}

func TestWebhookIDsStayStableAndUnique(t *testing.T) {
	raw := `[{"name":"hook","kind":"push","url":"https://example.com/hook","method":"POST"},{"name":"other","kind":"push","url":"https://example.com/hook","method":"POST","id":"dup"},{"name":"third","kind":"push","url":"https://example.com/hook","method":"POST","id":"dup"}]`
	first := stabilizeWebhookIDs(parseWebhooksJSON(raw), nil)
	if len(first) != 3 {
		t.Fatalf("len %d", len(first))
	}
	if first[0].ID == "" || first[1].ID == first[2].ID {
		t.Fatalf("ids %#v %#v %#v", first[0].ID, first[1].ID, first[2].ID)
	}
	second := stabilizeWebhookIDs(parseWebhooksJSON(raw), first)
	if second[0].ID != first[0].ID || second[1].ID != first[1].ID || second[2].ID != first[2].ID {
		t.Fatalf("ids changed %#v -> %#v", []string{first[0].ID, first[1].ID, first[2].ID}, []string{second[0].ID, second[1].ID, second[2].ID})
	}

	same := `[{"name":"gate","kind":"push","url":"https://a.example/h","method":"POST"},{"name":"Gate","kind":"push","url":"https://b.example/h","method":"POST"}]`
	again := stabilizeWebhookIDs(parseWebhooksJSON(same), nil)
	kept := stabilizeWebhookIDs(parseWebhooksJSON(same), again)
	if again[0].ID == "" || again[0].ID == again[1].ID || kept[0].ID != again[0].ID || kept[1].ID != again[1].ID {
		t.Fatalf("same name ids %#v -> %#v", []string{again[0].ID, again[1].ID}, []string{kept[0].ID, kept[1].ID})
	}
}

func TestDiscoveryRetireFailureKeepsOldTopic(t *testing.T) {
	p := &Publisher{opts: &Options{}}
	const uid = "gorflink_LACROSSE_0001_sw_1"
	p.discovered.Store(uid, haDiscovered{component: "binary_sensor", topic: "ha/binary_sensor/x/config"})
	p.publishHomeAssistantEntity(context.Background(), &SensorData{
		Hwid: "LACROSSE_0001", Model: "Lacrosse", Id: "0001",
	}, "sw_1", "UP", "homeassistant")
	got, ok := p.discovered.Load(uid)
	if !ok {
		t.Fatal("cache entry dropped")
	}
	old, _ := got.(haDiscovered)
	if old.topic != "ha/binary_sensor/x/config" {
		t.Fatalf("topic %s", old.topic)
	}
}

func TestDedupMapIsCapped(t *testing.T) {
	p := &Publisher{dedupLast: map[string]time.Time{}, opts: &Options{}}
	p.opts.Publish.DedupWindowMs = 60000
	for i := range maxDedupEntries + 20 {
		p.markDedup(fmt.Sprintf("20;%d;Oregon;ID=%04d;TEMP=00c8;", i, i))
	}
	if len(p.dedupLast) > maxDedupEntries {
		t.Fatalf("len %d", len(p.dedupLast))
	}
}

func TestLoginLimiterCapsIPs(t *testing.T) {
	l := newLoginLimiter()
	for i := range 300 {
		l.allow(fmt.Sprintf("203.0.113.%d", i))
	}
	if len(l.attempts) > 256 {
		t.Fatalf("attempts %d", len(l.attempts))
	}
}

func TestWebhookRedirectDowngradeStripsSecrets(t *testing.T) {
	from, err := http.NewRequest(http.MethodPost, "https://ha.home/hook", nil)
	if err != nil {
		t.Fatal(err)
	}
	to, err := http.NewRequest(http.MethodPost, "http://HA.HOME/hook", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !webhookRedirectStripsSecrets(from, to) {
		t.Fatal("https to http kept the host")
	}
	if webhookRedirectStripsSecrets(to, from) {
		t.Fatal("http to https was treated as a downgrade")
	}
	same, err := http.NewRequest(http.MethodPost, "https://ha.home:443/hook", nil)
	if err != nil {
		t.Fatal(err)
	}
	if webhookRedirectStripsSecrets(from, same) {
		t.Fatal("explicit 443 was treated as another host")
	}
	if err := rejectResolvedIP("http", net.ParseIP("203.0.113.50")); err == nil {
		t.Fatal("public http address allowed")
	}
	if err := rejectResolvedIP("http", net.ParseIP("192.168.1.20")); err != nil {
		t.Fatal(err)
	}
	if err := rejectResolvedIP("https", net.ParseIP("203.0.113.50")); err != nil {
		t.Fatal(err)
	}
}

func TestMergeWebhookKeepsOmittedEnabled(t *testing.T) {
	existing := RuntimeWebhook{
		ID: "abc", Name: "hook", Enabled: true, Kind: "push",
		URL: "https://example.com/h", Method: "POST", Payload: "raw",
		Headers: map[string]string{"Authorization": "secret"},
	}
	raw := map[string]json.RawMessage{
		"url":     []byte(`"https://example.com/h2"`),
		"headers": []byte(`{}`),
	}
	got, err := mergeWebhookUpdate(existing, raw, RuntimeWebhook{URL: "https://example.com/h2"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.Headers["Authorization"] != "secret" || got.URL != "https://example.com/h2" {
		t.Fatalf("%#v", got)
	}
}

func TestCleanMQTTTopicDropsEmptySegments(t *testing.T) {
	if got := cleanMQTTTopic(" rflink/ "); got != "rflink" {
		t.Fatal(got)
	}
	if got := cleanMQTTTopic("homeassistant/"); got != "homeassistant" {
		t.Fatal(got)
	}
	if got := cleanMQTTTopic("rflink//send"); got != "rflink/send" {
		t.Fatal(got)
	}
}

func TestEmptyFieldNameIsSkipped(t *testing.T) {
	sd, err := SensorDataFromMessage("20;01;Oregon;ID=0001;TEMP=00c8;=99;")
	if err != nil {
		t.Fatal(err)
	}
	if sd.Temperature == nil || sd.Extra[""] != "" {
		t.Fatalf("temp %#v extra %#v", sd.Temperature, sd.Extra)
	}
}

func TestMissingRuntimeFileKeepsEnvTokens(t *testing.T) {
	dir := t.TempDir()
	opts := &Options{
		RuntimeConfigFile: filepath.Join(dir, "missing", "runtime.json"),
		APIAuthTokens:     "ops:supersecretvalue:never:admin",
		WebhooksJSON:      `[{"name":"gate","kind":"push","url":"https://hooks.example/h","method":"POST","enabled":true}]`,
	}
	rm, err := newRuntimeManager(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rm.Stop)
	if _, ok := opts.validAPIToken("supersecretvalue"); !ok {
		t.Fatal("env token not loaded")
	}
	if len(opts.webhooks) != 1 || opts.webhooks[0].ID == "" || opts.webhooks[0].Name != "gate" {
		t.Fatalf("%#v", opts.webhooks)
	}
}

func TestReconnectDelayIsCapped(t *testing.T) {
	o := &Options{}
	o.Serial.ReconnectMinDelaySec = 9223372037
	o.Serial.ReconnectMaxDelaySec = 9223372037
	if d := o.serialReconnectMinDelay(); d <= 0 || d > 86400*time.Second {
		t.Fatal(d)
	}
	if d := o.serialReconnectMaxDelay(); d <= 0 || d > 86400*time.Second || d < o.serialReconnectMinDelay() {
		t.Fatal(d)
	}
	o.Serial.ReconnectMinDelaySec = 100000
	o.Serial.ReconnectMaxDelaySec = 60
	if o.serialReconnectMinDelay() != 86400*time.Second || o.serialReconnectMaxDelay() < o.serialReconnectMinDelay() {
		t.Fatalf("min %s max %s", o.serialReconnectMinDelay(), o.serialReconnectMaxDelay())
	}
}

func TestLoadAndApplyRejectsNull(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	if err := os.WriteFile(path, []byte("null\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newRuntimeManager(&Options{RuntimeConfigFile: path}); err == nil {
		t.Fatal("expected error")
	}
}

func TestRestoreBackupRejectsNull(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	body := []byte("{\"version\":1}\n")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	rm, err := newRuntimeManager(&Options{RuntimeConfigFile: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rm.Stop)
	if err := os.WriteFile(path+".bak.1", []byte("null\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rm.RestoreBackup(1); err == nil {
		t.Fatal("expected error")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("live file changed: %s", got)
	}
}

func TestEmptyHAPrefixStaysEmptyWhenDiscoveryOff(t *testing.T) {
	opts := &Options{RuntimeConfigFile: filepath.Join(t.TempDir(), "runtime.json")}
	opts.Publish.HADiscoveryPrefix = "custom"
	rm, err := newRuntimeManager(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rm.Stop)
	empty := ""
	off := false
	if _, err := rm.SaveOverlay(RuntimeFile{HADiscovery: &off, HADiscoveryPrefix: &empty}); err != nil {
		t.Fatal(err)
	}
	if opts.Publish.HADiscoveryPrefix != "" {
		t.Fatalf("prefix %q", opts.Publish.HADiscoveryPrefix)
	}
	on := true
	if _, err := rm.SaveOverlay(RuntimeFile{HADiscovery: &on, HADiscoveryPrefix: &empty}); err != nil {
		t.Fatal(err)
	}
	if opts.Publish.HADiscoveryPrefix != "homeassistant" {
		t.Fatalf("prefix %q", opts.Publish.HADiscoveryPrefix)
	}
}

func TestEmptyPrimaryKeepsEveryExtra(t *testing.T) {
	opts := &Options{}
	opts.Serial.Devices = "COM2, COM3"
	got := opts.extraSerialDevices()
	if len(got) != 2 || got[0] != "COM2" || got[1] != "COM3" {
		t.Fatalf("%#v", got)
	}
	opts.Serial.Device = " COM1 "
	opts.Serial.Devices = "com1,COM2"
	got = opts.extraSerialDevices()
	if len(got) != 1 || got[0] != "COM2" {
		t.Fatalf("%#v", got)
	}
	sr := &SerialReader{device: " COM1 "}
	if sr.deviceName() != "COM1" {
		t.Fatal(sr.deviceName())
	}
}

func TestLinkLocalZoneReachesDialAddress(t *testing.T) {
	addr, err := networkDeviceAddress("tcp://[fe80::1%eth0]:2323")
	if err != nil || addr != "[fe80::1%eth0]:2323" {
		t.Fatalf("%v %q", err, addr)
	}
	addr, err = networkDeviceAddress("tcp://[fe80::1%12]:2323")
	if err != nil || addr != "[fe80::1%12]:2323" {
		t.Fatalf("%v %q", err, addr)
	}
	addr, err = networkDeviceAddress("tcp://[fe80::1%25eth0]:2323")
	if err != nil || addr != "[fe80::1%eth0]:2323" {
		t.Fatalf("%v %q", err, addr)
	}
	addr, err = networkDeviceAddress("tcp://[fe80::1%25]:2323")
	if err != nil || addr != "[fe80::1%25]:2323" {
		t.Fatalf("zone 25: %v %q", err, addr)
	}
	addr, err = networkDeviceAddress("tcp://[fe80::1%250]:2323")
	if err != nil || addr != "[fe80::1%250]:2323" {
		t.Fatalf("zone 250: %v %q", err, addr)
	}
}

func TestLoginWithoutPasswordDoesNotLock(t *testing.T) {
	h := &httpAPIServer{app: &App{opts: &Options{}}, loginLimit: newLoginLimiter()}
	body := strings.NewReader(`{"token":"whatever-token"}`)
	for i := range 6 {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/session", body)
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "203.0.113.9:1234"
		rr := httptest.NewRecorder()
		h.sessionLogin(rr, req)
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "gui_auth_not_configured") {
			t.Fatalf("try %d: %d %s", i, rr.Code, rr.Body.String())
		}
		body.Reset(`{"token":"whatever-token"}`)
	}
}

func TestOwnRuntimeWriteIsNotReloaded(t *testing.T) {
	rm := &runtimeManager{path: filepath.Join(t.TempDir(), "runtime.json"), opts: &Options{}}
	rm.mu.Lock()
	err := rm.persistLocked(RuntimeFile{Version: 1})
	rm.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(rm.path)
	if err != nil {
		t.Fatal(err)
	}
	if !rm.adoptOwnWrite(st) {
		t.Fatal("save was treated as an external edit")
	}
	if rm.adoptOwnWrite(st) {
		t.Fatal("own write matched twice")
	}
}

func TestThrottledTakeoverStillPublishesDiscovery(t *testing.T) {
	opts := &Options{}
	opts.Publish.Topic = "rflink"
	opts.Publish.HomeAssistantDiscovery = true
	opts.Publish.HADiscoveryPrefix = "homeassistant"
	opts.Publish.MinIntervalMs = 60000
	opts.Serial.Device = "COM1"
	opts.Serial.Devices = "COM2"
	p := &Publisher{opts: opts, sensorSeen: map[string]time.Time{}}
	p.SetDeviceConnected(context.Background(), "COM1", true)
	p.SetDeviceConnected(context.Background(), "COM2", true)
	var topics []string
	p.testPublish = func(topic string, payload []byte) error {
		if strings.Contains(topic, "/config") {
			topics = append(topics, topic+" "+string(payload))
		}
		return nil
	}
	msg := "20;01;Oregon;ID=0001;TEMP=00cd;"
	p.handleSerialMessage(context.Background(), "COM1", msg)
	p.SetDeviceConnected(context.Background(), "COM1", false)
	topics = nil
	p.handleSerialMessage(context.Background(), "COM2", msg)
	if len(topics) == 0 {
		t.Fatal("throttled takeover published no discovery")
	}
	joined := strings.Join(topics, "\n")
	if !strings.Contains(joined, "COM2") {
		t.Fatalf("%s", joined)
	}
	if p.sensorPort["OREGON_0001"] != "COM2" && p.sensorPort["OREGON_0001"] != "com2" {
		t.Fatalf("%#v", p.sensorPort)
	}
}

func TestStaleAvailabilityRechecksBeforeOffline(t *testing.T) {
	p := &Publisher{opts: &Options{}, sensorSeen: map[string]time.Time{}}
	p.opts.Publish.Topic = "rflink"
	p.opts.Publish.HASensorAvailability = true
	p.opts.Publish.HASensorStaleSec = 10
	old := time.Now().Add(-time.Minute)
	p.sensorSeen["AAA"] = old
	p.sensorSeen["BBB"] = old
	var off []string
	p.testPublish = func(topic string, payload []byte) error {
		if string(payload) != statusOffline {
			return nil
		}
		off = append(off, topic)
		if strings.Contains(topic, "AAA") {
			p.noteSensorSeen("BBB", time.Now())
		}
		return nil
	}
	p.publishStaleSensorAvailability(context.Background())
	if len(off) != 1 || !strings.Contains(off[0], "AAA") {
		t.Fatalf("%v", off)
	}
}

func TestPartialPurgeKeepsActuatorCounts(t *testing.T) {
	p := &Publisher{opts: &Options{}}
	p.opts.Publish.HADiscoveryPrefix = "homeassistant"
	p.haPrefix = "homeassistant"
	p.haBtnN = 2
	p.discovered.Store("gorflink_btn_0", true)
	p.discovered.Store("gorflink_btn_1", true)
	p.testPublish = func(topic string, _ []byte) error {
		if strings.Contains(topic, "btn_1") {
			return fmt.Errorf("broker down")
		}
		return nil
	}
	if n := p.PurgeHADiscovery(); n != 1 {
		t.Fatalf("cleared %d", n)
	}
	if p.haPrefix != "homeassistant" || p.haBtnN != 2 {
		t.Fatalf("prefix %q buttons %d", p.haPrefix, p.haBtnN)
	}
}

func TestEmptyPrimaryIsNotACommandTarget(t *testing.T) {
	extra := &SerialReader{device: "COM2", port: newFakeSerial()}
	app := &App{opts: &Options{}, serial: &SerialReader{}, extras: []*SerialReader{extra}}
	ports, err := app.writeCommandTo("10;PING;", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ports) != 1 || ports[0].Device != "COM2" || ports[0].Status != "ok" {
		t.Fatalf("%+v", ports)
	}
}

func TestReconnectKeepsDiscoveryTopicForRetire(t *testing.T) {
	p := &Publisher{opts: &Options{}}
	p.opts.Publish.Topic = "rflink"
	const uid = "gorflink_OREGON_0001_sw_04"
	oldTopic := "homeassistant/binary_sensor/" + uid + "/config"
	p.discovered.Store(uid, haDiscovered{component: "binary_sensor", topic: oldTopic})
	p.discovered.Store("gorflink_btn_0", true)
	p.markDiscoveryStale()
	if _, ok := p.discovered.Load(uid); !ok {
		t.Fatal("sensor topic forgotten")
	}
	if _, ok := p.discovered.Load("gorflink_btn_0"); ok {
		t.Fatal("button flag kept")
	}
	var cleared []string
	p.testPublish = func(topic string, payload []byte) error {
		if len(payload) == 0 {
			cleared = append(cleared, topic)
		}
		return nil
	}
	p.publishHomeAssistantEntity(context.Background(), &SensorData{
		Hwid: "OREGON_0001", Model: "Oregon", Id: "0001",
	}, "sw_04", "UP", "homeassistant")
	if len(cleared) != 1 || cleared[0] != oldTopic {
		t.Fatalf("%v", cleared)
	}
}

func TestPendingDropsAliasAfterHWIDMap(t *testing.T) {
	temp := float32(10)
	opts := &Options{}
	opts.hwidMap = map[string]string{"0001": "DOOR"}
	p := &Publisher{opts: opts, pending: map[string]pendingSensor{}}
	p.pending["OREGON_0001"] = pendingSensor{
		sd:       &SensorData{Hwid: "OREGON_0001", Id: "0001", Model: "Oregon", Temperature: &temp},
		storedAt: time.Now(),
	}
	p.clearPending("DOOR")
	if _, ok := p.pending["OREGON_0001"]; ok {
		t.Fatal("old hwid stayed buffered")
	}
}

func TestEvictedSensorAvailabilityGoesOffline(t *testing.T) {
	p := &Publisher{opts: &Options{}, sensorSeen: map[string]time.Time{}}
	p.opts.Publish.Topic = "rflink"
	p.opts.Publish.HASensorAvailability = true
	old := time.Now().Add(-time.Hour)
	for i := range maxSensorSeen {
		p.sensorSeen[fmt.Sprintf("H%03d", i)] = old.Add(time.Duration(i) * time.Millisecond)
	}
	var off []string
	p.testPublish = func(topic string, payload []byte) error {
		if string(payload) == statusOffline {
			off = append(off, topic)
		}
		return nil
	}
	p.noteSensorSeen("NEW", time.Now())
	if len(off) != 1 || !strings.Contains(off[0], "H000") {
		t.Fatalf("%v", off)
	}
}

func TestRuntimeFileReceivesWebhookIDs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	raw := []byte(`{"version":1,"webhooks":[{"name":"a","kind":"push","url":"https://example.com/a","method":"POST"},{"name":"b","kind":"push","url":"https://example.com/b","method":"POST","id":"same"},{"name":"c","kind":"push","url":"https://example.com/c","method":"POST","id":"same"}]}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	opts := &Options{RuntimeConfigFile: path}
	rm, err := newRuntimeManager(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rm.Stop)
	if len(opts.webhooks) != 3 || opts.webhooks[0].ID == "" || opts.webhooks[1].ID == opts.webhooks[2].ID {
		t.Fatalf("%#v", opts.webhooks)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f RuntimeFile
	if err := json.Unmarshal(body, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Webhooks) != 3 || f.Webhooks[0].ID != opts.webhooks[0].ID || f.Webhooks[1].ID != opts.webhooks[1].ID || f.Webhooks[2].ID != opts.webhooks[2].ID {
		t.Fatalf("file %#v", f.Webhooks)
	}
}

func TestTriggerPrefixChangeAndDisableRetire(t *testing.T) {
	p := &Publisher{opts: &Options{}}
	p.opts.Publish.Topic = "rflink"
	p.opts.Publish.HADeviceTriggers = true
	const uid = "gorflink_OREGON_0001_sw_04"
	trigID := uid + "_press"
	oldTopic := "oldprefix/device_automation/" + trigID + "/config"
	p.discovered.Store(trigID, haDiscovered{topic: oldTopic, deviceName: "Door"})
	p.discoveryEpoch.Store(1)
	var cleared []string
	p.testPublish = func(topic string, payload []byte) error {
		if len(payload) == 0 {
			cleared = append(cleared, topic)
		}
		return nil
	}
	sd := &SensorData{Hwid: "OREGON_0001", Id: "0001", Model: "Oregon", FriendlyName: "Door"}
	spec := haEntitySpec{component: "binary_sensor", payloadOn: "ON"}
	p.publishMomentaryTrigger(context.Background(), sd, "sw_04", "homeassistant", uid, spec)
	if len(cleared) != 1 || cleared[0] != oldTopic {
		t.Fatalf("prefix retire %v", cleared)
	}
	if _, ok := p.discovered.Load(trigID); !ok {
		t.Fatal("new trigger was not stored")
	}
	cleared = nil
	p.opts.Publish.HADeviceTriggers = false
	p.publishMomentaryTrigger(context.Background(), sd, "sw_04", "homeassistant", uid, spec)
	if len(cleared) != 1 || !strings.Contains(cleared[0], "homeassistant/device_automation/") {
		t.Fatalf("disable retire %v", cleared)
	}
	if _, ok := p.discovered.Load(trigID); ok {
		t.Fatal("disabled trigger stayed cached")
	}
}

func TestFlushKeepsNewestAliasAndOriginalAge(t *testing.T) {
	oldTemp := float32(10)
	newTemp := float32(22)
	opts := &Options{}
	opts.Publish.Topic = "rflink"
	opts.Publish.PendingTTLSec = 60
	opts.hwidMap = map[string]string{"0001": "DOOR"}
	p := &Publisher{opts: opts, pending: map[string]pendingSensor{}}
	older := time.Now().Add(-2 * time.Second)
	newer := time.Now().Add(-time.Second)
	p.pending["OREGON_0001"] = pendingSensor{
		sd:       &SensorData{Hwid: "OREGON_0001", Id: "0001", Model: "Oregon", Temperature: &oldTemp},
		storedAt: older,
	}
	p.pending["DOOR"] = pendingSensor{
		sd:       &SensorData{Hwid: "DOOR", Id: "0001", Model: "Oregon", Temperature: &newTemp},
		storedAt: newer,
	}
	var temps []string
	p.testPublish = func(topic string, payload []byte) error {
		if strings.HasSuffix(topic, "/temp") {
			temps = append(temps, string(payload))
		}
		return nil
	}
	p.flushPending(context.Background())
	if len(temps) != 1 || temps[0] != "22.0" {
		t.Fatalf("published %v", temps)
	}
	if len(p.pending) != 0 {
		t.Fatalf("pending %#v", p.pending)
	}

	failedAt := time.Now().Add(-50 * time.Second)
	p.pending["DOOR"] = pendingSensor{
		sd:       &SensorData{Hwid: "DOOR", Id: "0001", Model: "Oregon", Temperature: &newTemp},
		storedAt: failedAt,
	}
	p.testPublish = func(string, []byte) error { return fmt.Errorf("broker down") }
	p.flushPending(context.Background())
	got, ok := p.pending["DOOR"]
	if !ok || !got.storedAt.Equal(failedAt) {
		t.Fatalf("age %#v", p.pending)
	}
}

func TestBrokerURLKeepsIPv6Zone(t *testing.T) {
	opts := &Options{}
	opts.Publish.Scheme = "mqtt"
	opts.Publish.Host = "[fe80::1%eth0]:1883"
	u, err := buildBrokerURL(opts)
	if err != nil {
		t.Fatal(err)
	}
	if u.Hostname() != "fe80::1%eth0" || u.Port() != "1883" || u.Host != "[fe80::1%eth0]:1883" {
		t.Fatalf("%s host %q", u, u.Host)
	}
	opts.Publish.Host = "[fe80::1%12]:1883"
	u, err = buildBrokerURL(opts)
	if err != nil || u.Hostname() != "fe80::1%12" {
		t.Fatalf("%v %v", err, u)
	}
	opts.Publish.Scheme = "ws"
	opts.Publish.Host = "[fe80::1%eth0]:1883/mqtt%2Fv2?k=%41"
	u, err = buildBrokerURL(opts)
	if err != nil {
		t.Fatal(err)
	}
	plain := &Options{}
	plain.Publish.Scheme = "ws"
	plain.Publish.Host = "127.0.0.1:1883/mqtt%2Fv2?k=%41"
	v, err := buildBrokerURL(plain)
	if err != nil {
		t.Fatal(err)
	}
	if u.Hostname() != "fe80::1%eth0" || u.Path != v.Path || u.RawQuery != v.RawQuery {
		t.Fatalf("zone url %s path %q query %q; plain path %q query %q", u, u.Path, u.RawQuery, v.Path, v.RawQuery)
	}
	opts.Publish.Scheme = "mqtt"
	opts.Publish.Host = "[fe80::1%25]:1883"
	u, err = buildBrokerURL(opts)
	if err != nil || u.Hostname() != "fe80::1%25" {
		t.Fatalf("zone 25 broker %v %v", err, u)
	}
	opts.Publish.Host = "[fe80::1%250]:1883"
	u, err = buildBrokerURL(opts)
	if err != nil || u.Hostname() != "fe80::1%250" {
		t.Fatalf("zone 250 broker %v %v", err, u)
	}
}

func TestPrettyEmptyHeadersKeepSecrets(t *testing.T) {
	existing := RuntimeWebhook{
		ID: "abc", Name: "hook", Enabled: true, Kind: "push",
		URL: "https://example.com/h", Method: "POST", Payload: "raw",
		Headers: map[string]string{"Authorization": "secret"},
	}
	for _, rawHdr := range []string{"{ }", "{\n}", "{\r\n  \r\n}"} {
		got, err := mergeWebhookUpdate(existing, map[string]json.RawMessage{
			"headers": []byte(rawHdr),
		}, RuntimeWebhook{Headers: map[string]string{}})
		if err != nil {
			t.Fatal(err)
		}
		if got.Headers["Authorization"] != "secret" {
			t.Fatalf("%q cleared headers: %#v", rawHdr, got.Headers)
		}
	}
	got, err := mergeWebhookUpdate(existing, map[string]json.RawMessage{
		"headers": []byte("{\n  \"X-Token\": \"next\"\n}"),
	}, RuntimeWebhook{Headers: map[string]string{"X-Token": "next"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Headers["Authorization"] != "" || got.Headers["X-Token"] != "next" {
		t.Fatalf("%#v", got.Headers)
	}
}

func TestWriteErrorBeatsClosedPort(t *testing.T) {
	bad := newFakeSerial()
	bad.writeErr = errors.New("write failed")
	app := &App{
		opts:   &Options{},
		serial: &SerialReader{device: "COM1"},
		extras: []*SerialReader{{device: "COM2", port: bad}},
	}
	ports, err := app.writeCommandTo("10;PING;", nil)
	if err == nil || errors.Is(err, errSerialNotConnected) {
		t.Fatalf("err %v ports %+v", err, ports)
	}
	if !strings.Contains(err.Error(), "write failed") {
		t.Fatal(err)
	}
	app.cmdTokens = 2
	app.cmdLastFill = time.Now()
	app.opts.Publish.CommandRateLimit = 2
	app.refillCmdTokensLocked(time.Now().Add(-time.Hour), 2)
	if app.cmdTokens < 1 {
		t.Fatalf("rollback drained tokens to %v", app.cmdTokens)
	}
}

func TestConnectedRefreshFlushesAfterThrottle(t *testing.T) {
	opts := &Options{}
	opts.Publish.Topic = "rflink"
	opts.Publish.PendingTTLSec = 120
	opts.Publish.MinIntervalMs = 60000
	p := &Publisher{
		opts:       opts,
		pending:    map[string]pendingSensor{},
		sensorSeen: map[string]time.Time{},
	}
	p.mqttUp.Store(true)
	temp := float32(21)
	p.pending["OREGON_0001"] = pendingSensor{
		sd:       &SensorData{Hwid: "OREGON_0001", Temperature: &temp},
		storedAt: time.Now().Add(-time.Second),
	}
	p.sensorSeen["OREGON_0001"] = time.Now()
	var temps []string
	p.testPublish = func(topic string, payload []byte) error {
		if strings.HasSuffix(topic, "/temp") {
			temps = append(temps, string(payload))
		}
		return nil
	}
	p.maintainConnected(context.Background())
	if len(temps) != 0 || len(p.pending) != 1 {
		t.Fatalf("inside window temps %v pending %d", temps, len(p.pending))
	}
	p.sensorSeen["OREGON_0001"] = time.Now().Add(-2 * time.Minute)
	p.maintainConnected(context.Background())
	if len(temps) != 1 || temps[0] != "21.0" {
		t.Fatalf("after window %v", temps)
	}
}

func TestEvictOfflineRetriesAndSkipsWhenDisabled(t *testing.T) {
	p := &Publisher{opts: &Options{}, sensorSeen: map[string]time.Time{}}
	p.opts.Publish.Topic = "rflink"
	old := time.Now().Add(-time.Hour)
	for i := range maxSensorSeen {
		p.sensorSeen[fmt.Sprintf("H%03d", i)] = old.Add(time.Duration(i) * time.Millisecond)
	}
	var off []string
	p.testPublish = func(topic string, payload []byte) error {
		if string(payload) == statusOffline {
			off = append(off, topic)
		}
		return nil
	}
	p.noteSensorSeen("NEW", time.Now())
	if len(off) != 0 {
		t.Fatalf("availability off still published %v", off)
	}

	p.opts.Publish.HASensorAvailability = true
	p.testPublish = func(topic string, payload []byte) error {
		if string(payload) == statusOffline {
			return errors.New("broker down")
		}
		return nil
	}
	for i := range maxSensorSeen {
		p.sensorSeen[fmt.Sprintf("H%03d", i)] = old.Add(time.Duration(i) * time.Millisecond)
	}
	p.noteSensorSeen("NEW", time.Now())
	if _, ok := p.availMiss["H000"]; !ok {
		t.Fatalf("missed offline was forgotten: %#v", p.availMiss)
	}
	p.testPublish = func(topic string, payload []byte) error {
		if string(payload) == statusOffline {
			off = append(off, topic)
		}
		return nil
	}
	p.retryMissedAvailability(context.Background())
	if len(off) != 1 || !strings.Contains(off[0], "H000") {
		t.Fatalf("%v", off)
	}
	if _, ok := p.availMiss["H000"]; ok {
		t.Fatal("retry left the id queued")
	}
}

func TestCanonicalHWIDRetiresOldDiscovery(t *testing.T) {
	p := &Publisher{opts: &Options{}}
	p.opts.Publish.Topic = "rflink"
	oldID := "gorflink_OREGON_0001_temp"
	trigID := oldID + "_press"
	p.discovered.Store(oldID, haDiscovered{
		component: "sensor",
		topic:     "homeassistant/sensor/" + oldID + "/config",
	})
	p.discovered.Store(trigID, haDiscovered{
		component: "device_automation",
		topic:     "homeassistant/device_automation/" + trigID + "/config",
	})
	var cleared []string
	p.testPublish = func(topic string, payload []byte) error {
		if len(payload) == 0 {
			cleared = append(cleared, topic)
		}
		return nil
	}
	temp := float32(21)
	p.publishHomeAssistantEntity(context.Background(), &SensorData{
		Hwid: "DOOR", RawHWID: "OREGON_0001", Id: "0001", Model: "Oregon",
		FriendlyName: "Door", Temperature: &temp,
	}, "temp", "21.0", "homeassistant")
	if len(cleared) != 2 {
		t.Fatalf("cleared %v", cleared)
	}
	joined := strings.Join(cleared, "\n")
	if !strings.Contains(joined, oldID+"/config") || !strings.Contains(joined, trigID+"/config") {
		t.Fatalf("%s", joined)
	}
	if _, ok := p.discovered.Load(oldID); ok {
		t.Fatal("old sensor config stayed cached")
	}
	if _, ok := p.discovered.Load(trigID); ok {
		t.Fatal("old trigger stayed cached")
	}
}

func TestLongerCanonicalHWIDKeepsNewDiscovery(t *testing.T) {
	p := &Publisher{opts: &Options{}}
	p.opts.Publish.Topic = "rflink"
	p.opts.Publish.HADeviceTriggers = true
	oldID := "gorflink_OREGON_0001_temp"
	p.discovered.Store(oldID, haDiscovered{
		component: "sensor",
		topic:     "homeassistant/sensor/" + oldID + "/config",
		hwid:      "OREGON_0001",
	})
	var cleared []string
	p.testPublish = func(topic string, payload []byte) error {
		if len(payload) == 0 {
			cleared = append(cleared, topic)
		}
		return nil
	}
	sd := &SensorData{
		Hwid: "OREGON_0001_ROOM", RawHWID: "OREGON_0001", Id: "0001", Model: "Oregon", FriendlyName: "Door",
	}
	ctx := context.Background()
	p.publishHomeAssistantEntity(ctx, sd, "temp", "21.0", "homeassistant")
	p.publishHomeAssistantEntity(ctx, sd, "sw_01", "ON", "homeassistant")
	p.publishHomeAssistantEntity(ctx, sd, "hum", "40", "homeassistant")
	tempID := "gorflink_OREGON_0001_ROOM_temp"
	humID := "gorflink_OREGON_0001_ROOM_hum"
	trigID := "gorflink_OREGON_0001_ROOM_sw_01_press"
	for _, id := range []string{tempID, humID, trigID} {
		if _, ok := p.discovered.Load(id); !ok {
			t.Fatalf("missing %s after later fields, cleared %v", id, cleared)
		}
	}
	if len(cleared) != 1 || !strings.Contains(cleared[0], oldID) {
		t.Fatalf("cleared %v", cleared)
	}
}

func TestOnlineDropsMissedOffline(t *testing.T) {
	p := &Publisher{opts: &Options{}, availMiss: map[string]struct{}{"DOOR": {}}}
	p.opts.Publish.Topic = "rflink"
	p.opts.Publish.HASensorAvailability = true
	var payloads []string
	p.testPublish = func(topic string, payload []byte) error {
		if strings.Contains(topic, "DOOR") {
			payloads = append(payloads, string(payload))
		}
		return nil
	}
	if err := p.publishSensorAvailability(context.Background(), "DOOR", statusOnline); err != nil {
		t.Fatal(err)
	}
	p.retryMissedAvailability(context.Background())
	if _, ok := p.availMiss["DOOR"]; ok {
		t.Fatal("online left the offline retry queued")
	}
	for _, payload := range payloads {
		if payload == statusOffline {
			t.Fatal("retry published offline after online")
		}
	}
}

func TestAvailMissKeepsNewestWhenFull(t *testing.T) {
	p := &Publisher{opts: &Options{}, availMiss: map[string]struct{}{}}
	for i := range maxSensorSeen {
		p.availMiss[fmt.Sprintf("A%03d", i)] = struct{}{}
	}
	p.rememberAvailMiss([]string{"NEW"})
	if _, ok := p.availMiss["NEW"]; !ok {
		t.Fatal("newest eviction was dropped")
	}
	if len(p.availMiss) != maxSensorSeen {
		t.Fatalf("len %d", len(p.availMiss))
	}
	if _, ok := p.availMiss["A000"]; ok {
		t.Fatal("oldest miss kept the newest out")
	}
}

func TestPingClockStepDoesNotStick(t *testing.T) {
	p := &Publisher{opts: &Options{}}
	p.opts.Serial.PingIntervalSec = 30
	p.opts.Serial.PingTimeoutSec = 5
	p.opts.Serial.PingFailThreshold = 3
	p.pingSentAt = time.Now().Add(time.Hour)
	p.checkPingTimeout()
	if !p.PingOutstanding() {
		t.Fatal("clamped ping was dropped")
	}
	p.pingMu.Lock()
	stuck := p.pingSentAt.After(time.Now().Add(time.Second))
	p.pingMu.Unlock()
	if stuck {
		t.Fatal("ping timestamp stayed in the future")
	}
	p.extraPing = map[string]*extraPingState{
		"com2": {sentAt: time.Now().Add(time.Hour)},
	}
	p.checkPingTimeout()
	p.pingMu.Lock()
	stuck = p.extraPing["com2"].sentAt.After(time.Now().Add(time.Second))
	p.pingMu.Unlock()
	if stuck {
		t.Fatal("extra ping timestamp stayed in the future")
	}
}

func TestCanonicalHWIDStaysOnExactMatch(t *testing.T) {
	sd, err := SensorDataFromMessage("20;01;Oregon;ID=0001;TEMP=00c8;")
	if err != nil {
		t.Fatal(err)
	}
	o := &Options{hwidMap: parseColonMap("OREGON_0001:Kitchen,0001:Garage")}
	sd.ApplyCanonicalHWID(o)
	if sd.Hwid != "Kitchen" {
		t.Fatalf("first %q", sd.Hwid)
	}
	sd.ApplyCanonicalHWID(o)
	if sd.Hwid != "Kitchen" || sd.RawHWID != "OREGON_0001" {
		t.Fatalf("second hwid %q raw %q", sd.Hwid, sd.RawHWID)
	}
}

func TestPongKeepsGatewayVersion(t *testing.T) {
	p := &Publisher{opts: &Options{}}
	p.opts.Publish.Topic = "rflink"
	p.noteGatewayVersion("COM1", "1.2")
	info, err := ParseGatewayMessage("20;02;PONG;")
	if err != nil || info == nil || !info.Pong {
		t.Fatalf("%v %#v", err, info)
	}
	var body string
	p.testPublish = func(topic string, payload []byte) error {
		if strings.Contains(topic, "gateway") {
			body = string(payload)
		}
		return nil
	}
	if err := p.PublishGatewayInfo(context.Background(), "COM1", info); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"version":"1.2"`) || !strings.Contains(body, `"pong":true`) {
		t.Fatalf("%s", body)
	}
	if info.Version != "" {
		t.Fatal("source PONG gained a version")
	}
}

func TestCommandPortDoesNotUseCommandStatus(t *testing.T) {
	o := &Options{}
	o.Publish.Topic = "rflink"
	port := o.serialDeviceStatusTopic("command")
	agg := o.serialCommandStatusTopic()
	if port == agg || port != "rflink/serial/command_port/status" {
		t.Fatalf("port %s agg %s", port, agg)
	}
	if o.serialDeviceStatusTopic("COM1") != "rflink/serial/COM1/status" {
		t.Fatal(o.serialDeviceStatusTopic("COM1"))
	}
}

func TestWatchdogClockStepStillAges(t *testing.T) {
	p := &Publisher{opts: &Options{}, startedAt: time.Now()}
	p.opts.Serial.Device = "COM1"
	p.opts.Serial.WatchdogSec = 1
	p.serialRestart = make(chan struct{}, 1)
	p.SetDeviceConnected(context.Background(), "COM1", true)
	p.serialMu.Lock()
	p.serialHeard["COM1"] = time.Now().Add(time.Hour)
	p.serialMu.Unlock()
	p.checkSerialWatchdog()
	select {
	case <-p.serialRestart:
		t.Fatal("future stamp reopened the port")
	default:
	}
	p.serialMu.Lock()
	heard := p.serialHeard["COM1"]
	p.serialMu.Unlock()
	if heard.After(time.Now().Add(time.Second)) {
		t.Fatal("future stamp was left in the future")
	}
	p.serialMu.Lock()
	p.serialHeard["COM1"] = time.Now().Add(-2 * time.Second)
	p.serialHold = nil
	p.serialMu.Unlock()
	p.checkSerialWatchdog()
	select {
	case <-p.serialRestart:
	default:
		t.Fatal("quiet port was not reopened after the clock step")
	}
}

func TestInvalidAPITokenIsNotStored(t *testing.T) {
	if got := parseAPIAuthTokens("the-secret", ""); len(got) != 0 {
		t.Fatalf("%#v", got)
	}
	if got := parseAPIAuthTokens(":the-secret", ""); len(got) != 0 {
		t.Fatalf("%#v", got)
	}
	got := parseAPIAuthTokens("ops:the-secret-value", "")
	if len(got) != 1 || got[0].Name != "ops" {
		t.Fatalf("%#v", got)
	}
}

func TestCanonicalRenameRetiresPreviousName(t *testing.T) {
	p := &Publisher{opts: &Options{}}
	p.opts.Publish.Topic = "rflink"
	var cleared []string
	p.testPublish = func(topic string, payload []byte) error {
		if len(payload) == 0 {
			cleared = append(cleared, topic)
		}
		return nil
	}
	sd := &SensorData{Hwid: "KITCHEN", RawHWID: "OREGON_0001", Id: "0001", Model: "Oregon", FriendlyName: "Kitchen"}
	ctx := context.Background()
	p.publishHomeAssistantEntity(ctx, sd, "temp", "21.0", "homeassistant")
	sd.Hwid = "LIVING"
	sd.FriendlyName = "Living"
	p.publishHomeAssistantEntity(ctx, sd, "temp", "22.0", "homeassistant")
	if _, ok := p.discovered.Load("gorflink_KITCHEN_temp"); ok {
		t.Fatal("previous canonical name stayed cached")
	}
	if _, ok := p.discovered.Load("gorflink_LIVING_temp"); !ok {
		t.Fatal("new canonical name was not stored")
	}
	if len(cleared) != 1 || !strings.Contains(cleared[0], "gorflink_KITCHEN_temp") {
		t.Fatalf("cleared %v", cleared)
	}
	cleared = nil
	sd.Hwid = "OREGON_0001"
	p.publishHomeAssistantEntity(ctx, sd, "temp", "22.0", "homeassistant")
	if _, ok := p.discovered.Load("gorflink_LIVING_temp"); ok {
		t.Fatal("mapped name stayed after the map was cleared")
	}
}

func TestSwitchChannelRetiresPlainCmd(t *testing.T) {
	p := &Publisher{opts: &Options{}}
	p.opts.Publish.Topic = "rflink"
	var cleared []string
	p.testPublish = func(topic string, payload []byte) error {
		if len(payload) == 0 {
			cleared = append(cleared, topic)
		}
		return nil
	}
	sd := &SensorData{Hwid: "REMOTE_1", Id: "1", Model: "NewKaku", FriendlyName: "Remote", Cmd: "ON"}
	ctx := context.Background()
	p.publishHomeAssistantDiscovery(ctx, sd, sd.FieldsForPublish(""), "homeassistant")
	if _, ok := p.discovered.Load("gorflink_REMOTE_1_cmd"); !ok {
		t.Fatal("cmd entity was not discovered")
	}
	sd.Switch = "01"
	p.publishHomeAssistantDiscovery(ctx, sd, sd.FieldsForPublish(""), "homeassistant")
	if _, ok := p.discovered.Load("gorflink_REMOTE_1_cmd"); ok {
		t.Fatal("plain cmd entity stayed next to sw_")
	}
	if _, ok := p.discovered.Load("gorflink_REMOTE_1_sw_01"); !ok {
		t.Fatal("channel button was not discovered")
	}
	if len(cleared) != 1 || !strings.Contains(cleared[0], "gorflink_REMOTE_1_cmd") {
		t.Fatalf("cleared %v", cleared)
	}
}

func TestFailedOnlineAvailabilityIsRetried(t *testing.T) {
	p := &Publisher{opts: &Options{}}
	p.opts.Publish.Topic = "rflink"
	p.opts.Publish.HASensorAvailability = true
	temp := float32(21)
	sd := &SensorData{Hwid: "DOOR", Temperature: &temp}
	p.testPublish = func(topic string, payload []byte) error {
		if strings.HasSuffix(topic, "/availability") && string(payload) == statusOnline {
			return errors.New("down")
		}
		return nil
	}
	if err := p.PublishSensor(context.Background(), sd); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.availOnline["DOOR"]; !ok {
		t.Fatal("missed online was not queued")
	}
	var got string
	p.testPublish = func(topic string, payload []byte) error {
		if strings.HasSuffix(topic, "/availability") {
			got = string(payload)
		}
		return nil
	}
	p.retryAvailOnline(context.Background())
	if got != statusOnline {
		t.Fatalf("retry %q", got)
	}
	if _, ok := p.availOnline["DOOR"]; ok {
		t.Fatal("online retry stayed queued")
	}
}

func TestFutureDedupAndThrottleDoNotStick(t *testing.T) {
	p := &Publisher{opts: &Options{}, dedupLast: map[string]time.Time{}, sensorSeen: map[string]time.Time{}}
	p.opts.Publish.DedupWindowMs = 3000
	p.opts.Publish.MinIntervalMs = 60000
	p.dedupLast["k"] = time.Now().Add(time.Hour)
	if p.duplicate("k", "20;01;Oregon;ID=1;", true) {
		t.Fatal("future dedup stamp dropped the frame")
	}
	p.sensorSeen["DOOR"] = time.Now().Add(time.Hour)
	if p.throttled("DOOR", time.Now()) {
		t.Fatal("future throttle stamp blocked the sensor")
	}
	if !p.sensorSeen["DOOR"].After(time.Now().Add(time.Minute)) {
		t.Fatal("throttle recorded a publish that did not happen")
	}
	p.sensorSeen["DOOR"] = time.Now()
	if !p.throttled("DOOR", time.Now()) {
		t.Fatal("a fresh publish was not throttled")
	}
}

func TestOfflineAvailabilityDropsQueuedOnline(t *testing.T) {
	p := &Publisher{opts: &Options{}}
	p.opts.Publish.Topic = "rflink"
	p.opts.Publish.HASensorAvailability = true
	p.availOnline = map[string]struct{}{"DOOR": {}}
	var got []string
	p.testPublish = func(topic string, payload []byte) error {
		if strings.HasSuffix(topic, "/availability") {
			got = append(got, string(payload))
		}
		return nil
	}
	if err := p.publishSensorAvailability(context.Background(), "DOOR", statusOffline); err != nil {
		t.Fatal(err)
	}
	p.retryAvailOnline(context.Background())
	if len(got) != 1 || got[0] != statusOffline {
		t.Fatalf("payloads %v", got)
	}
	if _, ok := p.availOnline["DOOR"]; ok {
		t.Fatal("offline left an online retry queued")
	}
}

func TestWatchdogHoldDoesNotOutliveClockStep(t *testing.T) {
	p := &Publisher{opts: &Options{}, startedAt: time.Now().Add(-time.Hour)}
	p.opts.Serial.Device = "COM9"
	p.opts.Serial.WatchdogSec = 1
	p.serialRestart = make(chan struct{}, 1)
	p.SetDeviceConnected(context.Background(), "COM9", true)
	p.serialMu.Lock()
	p.serialHeard["COM9"] = time.Now().Add(-time.Hour)
	p.serialHold = map[string]time.Time{"COM9": time.Now().Add(time.Hour)}
	p.serialMu.Unlock()
	p.checkSerialWatchdog()
	select {
	case <-p.serialRestart:
	case <-time.After(time.Second):
		t.Fatal("a hold from before the clock step kept the quiet port closed")
	}
}

func TestDeletingRuntimeFileRestoresEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	body := []byte(`{"version":1,"friendly_names":"OREGON_0001:FromFile","api_tokens":[],"webhooks":[]}`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	opts := &Options{RuntimeConfigFile: path}
	opts.Publish.FriendlyNames = "OREGON_0001:FromEnv"
	rm, err := newRuntimeManager(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rm.Stop)
	if opts.FriendlyNameFor("OREGON_0001") != "FromFile" {
		t.Fatalf("file name %q", opts.FriendlyNameFor("OREGON_0001"))
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if opts.FriendlyNameFor("OREGON_0001") == "FromEnv" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("env name was not restored, still %q", opts.FriendlyNameFor("OREGON_0001"))
}

func TestWebhookReloadPublishesListBeforeDrain(t *testing.T) {
	opts := &Options{}
	opts.webhooks = []RuntimeWebhook{{
		Name: "new", Kind: "push", URL: "https://new.example/h", Method: "POST", Enabled: true,
	}}
	m := &webhookManager{
		app:   &App{opts: opts},
		hooks: []RuntimeWebhook{{Name: "old", Kind: "push", URL: "https://old.example/h", Method: "POST", Enabled: true}},
	}
	release := make(chan struct{})
	m.wg.Go(func() {
		<-release
	})
	done := make(chan struct{})
	go func() {
		m.Reload()
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	var name string
	for time.Now().Before(deadline) {
		m.mu.RLock()
		if len(m.hooks) > 0 {
			name = m.hooks[0].Name
		}
		m.mu.RUnlock()
		if name == "new" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reload did not finish after the in-flight worker returned")
	}
	if name != "new" {
		t.Fatalf("push list stayed %q while a worker was still draining", name)
	}
}

func TestFailedOfflineDoesNotReplayOnline(t *testing.T) {
	p := &Publisher{opts: &Options{}}
	p.opts.Publish.Topic = "rflink"
	p.opts.Publish.HASensorAvailability = true
	p.availOnline = map[string]struct{}{"DOOR": {}}
	var got []string
	p.testPublish = func(topic string, payload []byte) error {
		if strings.HasSuffix(topic, "/availability") {
			got = append(got, string(payload))
		}
		return nil
	}
	p.rememberAvailMiss([]string{"DOOR"})
	p.retryAvailOnline(context.Background())
	if len(got) != 0 {
		t.Fatalf("payloads %v", got)
	}
	if _, ok := p.availOnline["DOOR"]; ok {
		t.Fatal("queued online survived a failed offline")
	}
}

func TestThrottledFrameKeepsSensorAvailable(t *testing.T) {
	p := &Publisher{opts: &Options{}, sensorSeen: map[string]time.Time{}}
	p.opts.Publish.Topic = "rflink"
	p.opts.Publish.HASensorAvailability = true
	p.opts.Publish.HASensorStaleSec = 30
	p.opts.Publish.MinIntervalMs = 60000
	p.sensorSeen["DOOR"] = time.Now().Add(-45 * time.Second)
	if !p.throttled("DOOR", time.Now()) {
		t.Fatal("frame inside the throttle window was not counted")
	}
	p.testPublish = func(topic string, payload []byte) error {
		if string(payload) == statusOffline {
			t.Fatal("a throttled sensor was marked offline")
		}
		return nil
	}
	p.publishStaleSensorAvailability(context.Background())
}

func TestMomentaryFieldWaitsForRetainedPublish(t *testing.T) {
	p := &Publisher{opts: &Options{}}
	p.opts.Publish.Topic = "rflink"
	p.opts.Publish.Retain = true
	temp := float32(21)
	sd := &SensorData{Hwid: "DOOR", Model: "Oregon", Id: "1", Temperature: &temp, Switch: "01", Cmd: "ON"}
	var topics []string
	p.testPublish = func(topic string, payload []byte) error {
		topics = append(topics, topic)
		if strings.HasSuffix(topic, "/temp") {
			return errors.New("down")
		}
		return nil
	}
	if err := p.PublishSensor(context.Background(), sd); err == nil {
		t.Fatal("temp failure was ignored")
	}
	for _, topic := range topics {
		if strings.Contains(topic, "/sw_") {
			t.Fatalf("button published before the failed measurement: %v", topics)
		}
	}
}

func TestNewDiscoveryIsPublishedBeforeRetire(t *testing.T) {
	p := &Publisher{opts: &Options{}}
	p.opts.Publish.Topic = "rflink"
	temp := float32(21)
	sd := &SensorData{Hwid: "DOOR", Model: "Oregon", Id: "1", Temperature: &temp}
	p.discovered.Store("gorflink_DOOR_temp", haDiscovered{
		component: "sensor", topic: "homeassistant/sensor/OLD/config", hwid: "DOOR", origin: "DOOR",
	})
	var topics []string
	p.testPublish = func(topic string, payload []byte) error {
		topics = append(topics, topic)
		if strings.Contains(topic, "gorflink_DOOR_temp") {
			return errors.New("down")
		}
		return nil
	}
	p.publishHomeAssistantEntity(context.Background(), sd, "temp", "21.0", "homeassistant")
	for _, topic := range topics {
		if topic == "homeassistant/sensor/OLD/config" {
			t.Fatalf("old config cleared before the new one existed: %v", topics)
		}
	}
}

func TestNetWriteDoesNotBlockForever(t *testing.T) {
	prev := netWriteTimeout
	netWriteTimeout = 50 * time.Millisecond
	t.Cleanup(func() { netWriteTimeout = prev })
	left, right := net.Pipe()
	t.Cleanup(func() {
		_ = left.Close()
		_ = right.Close()
	})
	start := time.Now()
	_, err := (&netPort{conn: left}).Write([]byte{1})
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("err %v after %s", err, time.Since(start))
	}
	_ = right.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := right.Read(make([]byte, 1)); err == nil {
		t.Fatal("timed out write left the socket open")
	}
}

func TestForwardedProtoUsesClientScheme(t *testing.T) {
	_, network, err := net.ParseCIDR("127.0.0.1/32")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Forwarded-Proto", "https, http")
	if !cookieSecure(req, []*net.IPNet{network}) {
		t.Fatal("https, http was treated as plain HTTP")
	}
	req.Header.Set("X-Forwarded-Proto", "http, https")
	if cookieSecure(req, []*net.IPNet{network}) {
		t.Fatal("http, https was treated as HTTPS")
	}
}

func TestSessionListDoesNotReturnCookie(t *testing.T) {
	s := newSessionStore()
	current, err := s.Create("10.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.Create("10.2.2.2")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range s.List(current.ID) {
		if _, ok := item["id"]; ok {
			t.Fatal("session list included the cookie")
		}
		if item["id_prefix"] == other.ID {
			t.Fatal("prefix is the full cookie")
		}
	}
	if !s.Revoke(other.ID[:8]) {
		t.Fatal("prefix did not revoke")
	}
	if s.Get(other.ID) != nil {
		t.Fatal("revoked session still live")
	}
}

func TestLogoutPrefixDoesNotRevokeAnotherSession(t *testing.T) {
	app := &App{opts: &Options{}, sessions: newSessionStore()}
	h := &httpAPIServer{app: app}
	other, err := app.sessions.Create("10.9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/session", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: other.ID[:8]})
	h.sessionLogout(httptest.NewRecorder(), req)
	if app.sessions.Get(other.ID) == nil {
		t.Fatal("prefix on logout removed another session")
	}
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/session", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: other.ID})
	req.AddCookie(&http.Cookie{Name: cookieCSRF, Value: "csrf-token-csrf-token-csrf-token"})
	req.Header.Set(headerCSRF, "csrf-token-csrf-token-csrf-token")
	h.sessionLogout(httptest.NewRecorder(), req)
	if app.sessions.Get(other.ID) != nil {
		t.Fatal("full cookie did not log out")
	}
}

func TestThrottledReadingReplacesPending(t *testing.T) {
	p := &Publisher{opts: &Options{}, pending: map[string]pendingSensor{}, sensorSeen: map[string]time.Time{}}
	p.opts.Publish.Topic = "rflink"
	p.opts.Publish.PendingTTLSec = 60
	p.opts.Publish.MinIntervalMs = 60000
	p.testPublish = func(string, []byte) error { return errors.New("should not publish") }
	oldTemp := float32(21)
	stored := time.Now().Add(-30 * time.Second)
	p.pending["DOOR"] = pendingSensor{sd: &SensorData{Hwid: "DOOR", Temperature: &oldTemp}, storedAt: stored}
	p.sensorSeen["DOOR"] = time.Now()
	next := float32(22)
	err := p.PublishSensor(context.Background(), &SensorData{Hwid: "DOOR", Temperature: &next})
	if !errors.Is(err, errSensorThrottled) {
		t.Fatal(err)
	}
	got := p.pending["DOOR"]
	if got.sd == nil || got.sd.Temperature == nil || *got.sd.Temperature != 22 || !got.storedAt.Equal(stored) {
		t.Fatalf("pending %#v", got.sd)
	}
}

func TestFuturePendingDoesNotBeatNewerAlias(t *testing.T) {
	opts := &Options{}
	opts.Publish.Topic = "rflink"
	opts.Publish.PendingTTLSec = 60
	opts.hwidMap = map[string]string{"OLD": "SAME", "NEW": "SAME"}
	p := &Publisher{opts: opts, pending: map[string]pendingSensor{}}
	oldTemp := float32(1)
	newTemp := float32(2)
	p.pending["OLD"] = pendingSensor{sd: &SensorData{Hwid: "OLD", Temperature: &oldTemp}, storedAt: time.Now().Add(time.Hour)}
	p.pending["NEW"] = pendingSensor{sd: &SensorData{Hwid: "NEW", Temperature: &newTemp}, storedAt: time.Now()}
	var temps []string
	p.testPublish = func(topic string, payload []byte) error {
		if strings.HasSuffix(topic, "/temp") {
			temps = append(temps, string(payload))
		}
		return nil
	}
	p.flushPending(context.Background())
	if len(temps) != 1 || temps[0] != "2.0" {
		t.Fatalf("temps %v", temps)
	}
}

func TestFutureDedupStampsDoNotEvictFresh(t *testing.T) {
	p := &Publisher{opts: &Options{}, dedupLast: map[string]time.Time{}}
	p.opts.Publish.DedupWindowMs = 3000
	future := time.Now().Add(time.Hour)
	for i := range maxDedupEntries {
		p.dedupLast[fmt.Sprintf("old-%d", i)] = future
	}
	p.rememberDedup("fresh")
	if _, ok := p.dedupLast["fresh"]; !ok {
		t.Fatal("the line just published was evicted")
	}
}

func TestWebhookRedirectDropsHeaderNames(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://other.example/hook", strings.NewReader("secret"))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("User-Agent", "Bearer tok")
	req.Header.Set("Accept", "Bearer tok")
	stripWebhookRedirect(req)
	if req.Header.Get("Authorization") != "" || req.Header.Get("Accept") != "" || req.Header.Get("User-Agent") != webhookUserAgent {
		t.Fatalf("%v", req.Header)
	}
	if req.ContentLength != 0 {
		t.Fatal(req.ContentLength)
	}
}

func TestStopClearsSensorAvailability(t *testing.T) {
	p := &Publisher{opts: &Options{}, sensorSeen: map[string]time.Time{"DOOR": time.Now()}}
	p.opts.Publish.Topic = "rflink"
	p.opts.Publish.HASensorAvailability = true
	var got string
	p.testPublish = func(topic string, payload []byte) error {
		if strings.HasSuffix(topic, "/availability") {
			got = string(payload)
		}
		return nil
	}
	p.offlineSensorAvailability()
	if got != statusOffline {
		t.Fatalf("availability %q", got)
	}
}

func TestFailedButtonDiscoveryIsRetried(t *testing.T) {
	p := &Publisher{opts: &Options{}}
	p.opts.Publish.Topic = "rflink"
	p.opts.Publish.HomeAssistantDiscovery = true
	p.opts.Publish.HADiscoveryPrefix = "homeassistant"
	p.opts.haButtons = []haActuatorButton{{Name: "Bell", Command: "10;bell;"}}
	p.mqttUp.Store(true)
	n := 0
	p.testPublish = func(topic string, payload []byte) error {
		if strings.Contains(topic, "/button/gorflink_btn_0/config") && len(payload) > 0 {
			n++
			if n == 1 {
				return errors.New("down")
			}
		}
		return nil
	}
	p.maintainConnected(context.Background())
	p.maintainConnected(context.Background())
	if n != 2 {
		t.Fatalf("button publishes %d", n)
	}
}

func TestSharedGatewayInfoIsClearedForSeveralPorts(t *testing.T) {
	p := &Publisher{opts: &Options{}}
	p.opts.Publish.Topic = "rflink"
	p.opts.Serial.Device = "COM1"
	p.opts.Serial.Devices = "COM2"
	p.mqttUp.Store(true)
	var cleared bool
	p.testPublish = func(topic string, payload []byte) error {
		if topic == "rflink/gateway/info" && len(payload) == 0 {
			cleared = true
		}
		return nil
	}
	p.maintainConnected(context.Background())
	if !cleared {
		t.Fatal("shared gateway info stayed")
	}
}

func TestLaterFutureAliasWins(t *testing.T) {
	opts := &Options{}
	opts.Publish.Topic = "rflink"
	opts.Publish.PendingTTLSec = 60
	opts.hwidMap = map[string]string{"OLD": "SAME", "NEW": "SAME"}
	p := &Publisher{opts: opts, pending: map[string]pendingSensor{}}
	oldTemp := float32(1)
	newTemp := float32(2)
	p.pending["OLD"] = pendingSensor{sd: &SensorData{Hwid: "OLD", Temperature: &oldTemp}, storedAt: time.Now().Add(time.Hour)}
	p.pending["NEW"] = pendingSensor{sd: &SensorData{Hwid: "NEW", Temperature: &newTemp}, storedAt: time.Now().Add(2 * time.Hour)}
	var temps []string
	p.testPublish = func(topic string, payload []byte) error {
		if strings.HasSuffix(topic, "/temp") {
			temps = append(temps, string(payload))
		}
		return nil
	}
	p.flushPending(context.Background())
	if len(temps) != 1 || temps[0] != "2.0" {
		t.Fatalf("temps %v", temps)
	}
}

func TestFirmwareBannerIsVersion(t *testing.T) {
	info, err := ParseGatewayMessage("20;3F;RFLink Gateway V1.1 - R48;")
	if err != nil || info == nil || info.Version != "RFLink Gateway V1.1 - R48" {
		t.Fatalf("%v %#v", err, info)
	}
	info, err = ParseGatewayMessage("20;01;REV=48;")
	if err != nil || info == nil || info.Version != "48" {
		t.Fatalf("rev %v %#v", err, info)
	}
	info, err = ParseGatewayMessage("20;02;Oregon;ID=1;TEMP=00c8;")
	if err != nil || info != nil {
		t.Fatalf("sensor looked like firmware: %v %#v", err, info)
	}
}

func TestDroppedVersionLineIsRemembered(t *testing.T) {
	p := &Publisher{opts: &Options{}}
	line := "20;3F;RFLink Gateway V1.1 - R48;"
	info, err := ParseGatewayMessage(line)
	if err != nil || info == nil {
		t.Fatal(err)
	}
	p.noteGatewayVersion("COM9", info.Version)
	if p.gatewayVersion("COM9") != info.Version {
		t.Fatal(p.gatewayVersion("COM9"))
	}
}

func TestRemovedPortClearsGatewayInfo(t *testing.T) {
	p := &Publisher{opts: &Options{}}
	p.opts.Publish.Topic = "rflink"
	p.opts.Serial.Device = "COM1"
	p.opts.Serial.Devices = "COM2,COM3"
	p.mqttUp.Store(true)
	p.gatewayInfoMode.Store(2)
	p.testPublish = func(string, []byte) error { return nil }
	for _, dev := range []string{"COM1", "COM2", "COM3"} {
		if err := p.PublishGatewayInfo(context.Background(), dev, &GatewayInfo{Version: "1.2"}); err != nil {
			t.Fatal(err)
		}
	}
	var cleared []string
	p.testPublish = func(topic string, payload []byte) error {
		if len(payload) == 0 {
			cleared = append(cleared, topic)
		}
		return nil
	}
	p.opts.Serial.Devices = "COM3"
	p.maintainConnected(context.Background())
	if len(cleared) != 1 || cleared[0] != "rflink/gateway/COM2/info" {
		t.Fatalf("cleared %v", cleared)
	}
	cleared = nil
	p.maintainConnected(context.Background())
	if len(cleared) != 0 {
		t.Fatalf("cleared again %v", cleared)
	}
}

func TestRemovedPortClearsWhenOneGatewayRemains(t *testing.T) {
	p := &Publisher{opts: &Options{}}
	p.opts.Publish.Topic = "rflink"
	p.opts.Serial.Device = "COM1"
	p.opts.Serial.Devices = "COM2"
	p.mqttUp.Store(true)
	p.gatewayInfoMode.Store(2)
	p.testPublish = func(string, []byte) error { return nil }
	for _, dev := range []string{"COM1", "COM2"} {
		if err := p.PublishGatewayInfo(context.Background(), dev, &GatewayInfo{Version: "1.2"}); err != nil {
			t.Fatal(err)
		}
	}
	cleared := map[string]int{}
	p.testPublish = func(topic string, payload []byte) error {
		if len(payload) == 0 {
			cleared[topic]++
		}
		return nil
	}
	p.opts.Serial.Devices = ""
	p.maintainConnected(context.Background())
	if cleared["rflink/gateway/COM2/info"] == 0 || cleared["rflink/gateway/COM1/info"] == 0 {
		t.Fatalf("cleared %#v", cleared)
	}
	if cleared["rflink/gateway/info"] != 0 {
		t.Fatalf("shared topic was cleared %#v", cleared)
	}
}

func TestRetiredGatewayInfoRetriesWhenClearFails(t *testing.T) {
	p := &Publisher{opts: &Options{}}
	p.opts.Publish.Topic = "rflink"
	p.opts.Serial.Device = "COM1"
	p.opts.Serial.Devices = "COM2,COM3"
	p.mqttUp.Store(true)
	p.gatewayInfoMode.Store(2)
	p.testPublish = func(string, []byte) error { return nil }
	if err := p.PublishGatewayInfo(context.Background(), "COM2", &GatewayInfo{Version: "9"}); err != nil {
		t.Fatal(err)
	}
	fail := true
	n := 0
	p.testPublish = func(topic string, payload []byte) error {
		if topic == "rflink/gateway/COM2/info" && len(payload) == 0 {
			n++
			if fail {
				return errors.New("down")
			}
		}
		return nil
	}
	p.opts.Serial.Devices = "COM3"
	p.maintainConnected(context.Background())
	if n != 1 {
		t.Fatalf("attempts %d", n)
	}
	fail = false
	p.maintainConnected(context.Background())
	if n != 2 {
		t.Fatalf("retry attempts %d", n)
	}
	p.maintainConnected(context.Background())
	if n != 2 {
		t.Fatalf("extra clear %d", n)
	}
}

func TestGatewayInfoPublishRacesClear(t *testing.T) {
	p := &Publisher{opts: &Options{}}
	p.opts.Publish.Topic = "rflink"
	p.opts.Serial.Device = "COM1"
	p.opts.Serial.Devices = "COM3"
	p.mqttUp.Store(true)
	p.gatewayInfoMode.Store(2)
	p.testPublish = func(string, []byte) error { return nil }
	if err := p.PublishGatewayInfo(context.Background(), "COM2", &GatewayInfo{Version: "1"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 40 {
			_ = p.PublishGatewayInfo(context.Background(), "COM1", &GatewayInfo{Version: "1"})
		}
	}()
	go func() {
		defer wg.Done()
		for range 40 {
			p.clearRetiredGatewayTopics(context.Background())
		}
	}()
	wg.Wait()
}
