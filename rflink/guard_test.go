/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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
	if second[0].ID != first[0].ID || second[1].ID != first[1].ID {
		t.Fatalf("ids changed %s %s", second[0].ID, second[1].ID)
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
