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
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDotEnvThrottleAndDevices(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.env")
	if err := os.WriteFile(path, []byte("# c\nexport GORFLINK_TEST_DOTENV=from-file\nGORFLINK_TEST_KEEP=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GORFLINK_TEST_KEEP", "from-env")
	t.Cleanup(func() { _ = os.Unsetenv("GORFLINK_TEST_DOTENV") })
	loadDotEnvFile(path)
	if os.Getenv("GORFLINK_TEST_DOTENV") != "from-file" {
		t.Fatal(os.Getenv("GORFLINK_TEST_DOTENV"))
	}
	if os.Getenv("GORFLINK_TEST_KEEP") != "from-env" {
		t.Fatal("env was replaced")
	}

	opts := &Options{}
	opts.Publish.Topic = "rflink"
	opts.Publish.MinIntervalMs = 5000
	opts.throttleBy = parseThrottleMap("OREGON_0001:100,bad,NOPE:0")
	if opts.throttleFor("other") != 5*time.Second {
		t.Fatal(opts.throttleFor("other"))
	}
	if opts.throttleFor("OREGON_0001") != 100*time.Millisecond {
		t.Fatal("override")
	}
	p := &Publisher{opts: opts, sensorSeen: map[string]time.Time{
		"OREGON_0001": time.Now(),
		"OTHER":       time.Now().Add(-6 * time.Second),
	}}
	if !p.throttled("OREGON_0001", time.Now()) {
		t.Fatal("mapped window")
	}
	if p.throttled("OTHER", time.Now()) {
		t.Fatal("default window already elapsed")
	}
	if p.throttled("NEW", time.Now()) {
		t.Fatal("unseen")
	}

	opts.Serial.Device = "/dev/ttyUSB0"
	opts.Serial.Devices = "/dev/ttyUSB0, tcp://10.1.2.3:9999"
	list := opts.serialDeviceList()
	if len(list) != 2 || list[0] != "/dev/ttyUSB0" || list[1] != "tcp://10.1.2.3:9999" {
		t.Fatalf("%#v", list)
	}
	if isNetworkDevice(list[0]) || !isNetworkDevice(list[1]) || isNetworkDevice("COM3") {
		t.Fatal("network detect")
	}
}

func TestMetricsAvailabilitySwitchAndPurge(t *testing.T) {
	opts := &Options{}
	opts.Publish.Topic = "rflink"
	opts.Publish.HADiscoveryPrefix = "homeassistant"
	p := &Publisher{opts: opts, runCtx: t.Context()}
	p.published.Store(4)
	p.mqttUp.Store(true)
	text := p.MetricsText()
	if !strings.Contains(text, "gorflink_published_total 4") || !strings.Contains(text, "gorflink_mqtt_connected 1") {
		t.Fatal(text)
	}
	if len(p.sensorAvailability("OREGON_1")) != 2 {
		t.Fatal("default availability")
	}
	opts.Publish.HASensorAvailability = true
	got := p.sensorAvailability("OREGON_1")
	if len(got) != 3 || !strings.HasSuffix(got[2].Topic, "/OREGON_1/availability") {
		t.Fatalf("%+v", got)
	}

	sw := haActuatorSwitch{CommandOn: "10;NewKaku;AABB;ON;", CommandOff: "10;NewKaku;AABB;OFF;"}
	sd := &SensorData{Id: "AABB"}
	if switchEchoState(sw, sd, map[string]string{"cmd": "ON"}) != "ON" {
		t.Fatal("on")
	}
	if switchEchoState(sw, sd, map[string]string{"cmd": "OFF"}) != "OFF" {
		t.Fatal("off")
	}
	if switchEchoState(sw, sd, map[string]string{"cmd": "UP"}) != "" {
		t.Fatal("up")
	}
	if switchEchoState(sw, &SensorData{Id: "ZZ"}, map[string]string{"cmd": "ON"}) != "" {
		t.Fatal("other id")
	}
	short := haActuatorSwitch{CommandOn: "10;NewKaku;1;1;ON;", CommandOff: "10;NewKaku;1;1;OFF;"}
	if switchEchoState(short, &SensorData{Id: "1", Switch: "1", Cmd: "ON"}, nil) != "ON" {
		t.Fatal("short id echo")
	}
	if switchEchoState(short, &SensorData{Id: "1", Switch: "2", Cmd: "ON"}, nil) != "" {
		t.Fatal("short id wrong channel")
	}

	p.discovered.Store("keep", haDiscovered{component: "sensor", topic: "ha/sensor/keep/config"})
	if n := p.PurgeHADiscovery(); n != 0 {
		t.Fatal(n)
	}
	if _, ok := p.discovered.Load("keep"); !ok {
		t.Fatal("purge dropped a topic it could not clear")
	}
}

func TestMQTTClientCertAndNetPort(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeTestKeyPair(t, dir)
	missingKey := &Options{}
	missingKey.Publish.Scheme = "mqtts"
	missingKey.Publish.TLSCertFile = certPath
	if _, err := buildTLSConfig(missingKey); err == nil {
		t.Fatal("cert without key")
	}
	okOpts := &Options{}
	okOpts.Publish.Scheme = "mqtts"
	okOpts.Publish.TLSCertFile = certPath
	okOpts.Publish.TLSKeyFile = keyPath
	cfg, err := buildTLSConfig(okOpts)
	if err != nil || cfg == nil || len(cfg.Certificates) != 1 {
		t.Fatalf("%v %v", cfg, err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		buf := make([]byte, 32)
		n, _ := c.Read(buf)
		_, _ = c.Write(buf[:n])
	}()
	port, err := dialNetworkDevice("tcp://"+ln.Addr().String(), 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = port.Close() }()
	if _, err := port.Write([]byte("10;PING;\r\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 32)
	n, err := port.Read(buf)
	if err != nil || string(buf[:n]) != "10;PING;\r\n" {
		t.Fatalf("%q %v", buf[:n], err)
	}
	<-done
	if _, err := dialNetworkDevice("tcp://", time.Second); err == nil {
		t.Fatal("empty host")
	}
}

func TestMetricsAndPurgeHTTP(t *testing.T) {
	_, h := newCoverageStack(t)
	rr := doReq(h, http.MethodGet, "/metrics", "", nil, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("metrics %d", rr.Code)
	}
	rr = doReq(h, http.MethodGet, "/metrics", "", map[string]string{"Authorization": "Bearer readtoken123456"}, nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "gorflink_published_total") {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	rr = doReq(h, http.MethodPost, "/api/v1/ha/purge", "", map[string]string{"Authorization": "Bearer readtoken123456"}, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("purge scope %d", rr.Code)
	}
	rr = doReq(h, http.MethodPost, "/api/v1/ha/purge", "", map[string]string{"Authorization": "Bearer cmdtoken12345678"}, nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"cleared": 0`) {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	rr = doReq(h, http.MethodPost, "/metrics", "", map[string]string{"Authorization": "Bearer readtoken123456"}, nil)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("metrics post %d", rr.Code)
	}
	rr = doReq(h, http.MethodGet, "/api/v1/ha/purge", "", map[string]string{"Authorization": "Bearer cmdtoken12345678"}, nil)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("purge get %d", rr.Code)
	}
}

func TestGatewayFanoutStaleAndDotEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "q.env")
	body := "# skip\nnot a pair\nGORFLINK_TEST_QUOTED=\"hello\"\nGORFLINK_TEST_SINGLE='x'\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GORFLINK_ENV_FILE", path)
	t.Cleanup(func() {
		_ = os.Unsetenv("GORFLINK_TEST_QUOTED")
		_ = os.Unsetenv("GORFLINK_TEST_SINGLE")
	})
	loadDotEnv()
	if os.Getenv("GORFLINK_TEST_QUOTED") != "hello" || os.Getenv("GORFLINK_TEST_SINGLE") != "x" {
		t.Fatalf("quoted %q %q", os.Getenv("GORFLINK_TEST_QUOTED"), os.Getenv("GORFLINK_TEST_SINGLE"))
	}

	t.Setenv("PUBLISH_QOS", "0")
	t.Setenv("PUBLISH_LWT_QOS", "0")
	t.Setenv("PUBLISH_TEMPERATURE_UNIT", "C")
	t.Setenv("PUBLISH_MIN_INTERVAL_MS", "999999999")
	t.Setenv("PUBLISH_HA_SENSOR_STALE_SEC", "-3")
	t.Setenv("PUBLISH_THROTTLE", "ABC:1500,HUGE:999999999")
	t.Setenv("PUBLISH_HA_DEVICE_TRIGGERS", "false")
	t.Setenv("SERIAL_DEVICE", "COM1")
	t.Setenv("SERIAL_DEVICES", "tcp://10.0.0.8:23,COM1")
	opts, err := GetOptions()
	if err != nil {
		t.Fatal(err)
	}
	if opts.Publish.MinIntervalMs != 86400000 || opts.Publish.HASensorStaleSec != 0 || opts.Publish.HADeviceTriggers {
		t.Fatalf("interval %d stale %d triggers %v", opts.Publish.MinIntervalMs, opts.Publish.HASensorStaleSec, opts.Publish.HADeviceTriggers)
	}
	if opts.throttleFor("ABC") != 1500*time.Millisecond || opts.throttleFor("HUGE") != 86400000*time.Millisecond {
		t.Fatal("throttle map")
	}
	if len(opts.serialDeviceList()) != 2 {
		t.Fatalf("%#v", opts.serialDeviceList())
	}
	if len((&Options{}).serialDeviceList()) != 0 {
		t.Fatal("empty device list")
	}

	app, _ := newCoverageStack(t)
	app.extras = []*SerialReader{{opts: app.opts}}
	res := app.ExecuteCommandFrom("10;PING;", "http", "token:night")
	if res.Status != "partial" || len(res.Ports) != 2 || res.Ports[0].Status != "ok" || res.Ports[1].Status != "error" {
		t.Fatalf("primary ok extra down: %+v", res)
	}
	app.serial = &SerialReader{opts: app.opts}
	app.extras = []*SerialReader{{opts: app.opts, port: newFakeSerial()}}
	res = app.ExecuteCommandFrom("10;VERSION;", "webhook", "poller")
	if res.Status != "partial" || len(res.Ports) != 2 || res.Ports[0].Status != "error" || res.Ports[1].Status != "ok" {
		t.Fatalf("extra ok primary down: %+v", res)
	}
	app.extras = nil
	res = app.ExecuteCommandFrom("10;PING;", "mqtt", "gorflink")
	if res.Status != "serial_down" {
		t.Fatalf("all down: %+v", res)
	}

	p := app.publisher
	var none *Publisher
	if none.MetricsText() != "" {
		t.Fatal("nil metrics")
	}
	p.dedupDropped.Store(2)
	p.throttleDropped.Store(3)
	p.serialDropped.Store(4)
	p.pendingDropped.Store(5)
	p.pingFails.Store(6)
	text := p.MetricsText()
	for _, part := range []string{
		`reason="dedup"`, `reason="throttle"`, `reason="serial"`, `reason="pending"`,
		"gorflink_ping_fails 6", "gorflink_serial_connected", "gorflink_rflink_unresponsive",
	} {
		if !strings.Contains(text, part) {
			t.Fatalf("missing %s in %s", part, text)
		}
	}
	if p.switchStateTopic(2) != "rflink/gateway/switch/gorflink_sw_2/state" {
		t.Fatal(p.switchStateTopic(2))
	}
	p.noteSwitchEcho(t.Context(), &SensorData{Id: "AABB"}, map[string]string{"cmd": "ON"})
	_ = p.publishSensorAvailability(t.Context(), "OREGON_1", statusOnline)

	p.opts.Publish.HASensorAvailability = true
	p.opts.Publish.HASensorStaleSec = 30
	p.sensorSeen["OLD"] = time.Now().Add(-time.Hour)
	p.availDown = map[string]struct{}{"OLD": {}}
	p.sensorSeen["ALSO_OLD"] = time.Now().Add(-time.Hour)
	p.publishStaleSensorAvailability(t.Context())
	if _, marked := p.availDown["ALSO_OLD"]; marked {
		t.Fatal("offline marked without mqtt")
	}

	p.opts.Publish.HADeviceTriggers = false
	spec := resolveHAEntitySpec("sw_04", "ON")
	sd := &SensorData{Model: "NewKaku", Id: "AABB", Hwid: "NEWKAKU_AABB", FriendlyName: "Remote"}
	p.publishMomentaryTrigger(t.Context(), sd, "sw_04", "homeassistant", "gorflink_NEWKAKU_AABB_sw_04", spec)
	if _, ok := p.discovered.Load("gorflink_NEWKAKU_AABB_sw_04_press"); ok {
		t.Fatal("trigger stored while disabled")
	}
	p.opts.Publish.HADeviceTriggers = true
	p.publishMomentaryTrigger(t.Context(), sd, "sw_04", "homeassistant", "gorflink_NEWKAKU_AABB_sw_04", spec)
	if _, ok := p.discovered.Load("gorflink_NEWKAKU_AABB_sw_04_press"); ok {
		t.Fatal("trigger stored without mqtt")
	}
	trigID := "gorflink_NEWKAKU_AABB_sw_04_press"
	p.discovered.Store(trigID, haDiscovered{component: "device_automation", topic: "homeassistant/device_automation/" + trigID + "/config", deviceName: sd.FriendlyName})
	p.publishMomentaryTrigger(t.Context(), sd, "sw_04", "homeassistant", "gorflink_NEWKAKU_AABB_sw_04", spec)
	up := resolveHAEntitySpec("sw_04", "UP")
	p.publishMomentaryTrigger(t.Context(), sd, "sw_04", "homeassistant", "gorflink_NEWKAKU_AABB_sw_04", up)
	if _, ok := p.discovered.Load(trigID); !ok {
		t.Fatal("trigger removed before the empty config was published")
	}
	p.discovered.Store(trigID, haDiscovered{component: "device_automation", topic: "", deviceName: sd.FriendlyName})
	p.publishMomentaryTrigger(t.Context(), sd, "sw_04", "homeassistant", "gorflink_NEWKAKU_AABB_sw_04", up)
	if _, ok := p.discovered.Load(trigID); ok {
		t.Fatal("button trigger stayed after the field became a sensor")
	}

	if got := p.discoveryTopic("gorflink_btn_0", true); got != "homeassistant/button/gorflink_btn_0/config" {
		t.Fatal(got)
	}
	if got := p.discoveryTopic("nope", true); got != "" {
		t.Fatal(got)
	}
	if !commandHasField("10;NewKaku;AABB;ON;", "aabb") || commandHasField("10;PING;", "") {
		t.Fatal("field match")
	}

	keyOnly := &Options{}
	keyOnly.Publish.Scheme = "mqtts"
	keyOnly.Publish.TLSKeyFile = "only.key"
	if _, err := buildTLSConfig(keyOnly); err == nil {
		t.Fatal("key without cert")
	}
}

func TestNetPortTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	accepted := make(chan struct{})
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		close(accepted)
		time.Sleep(300 * time.Millisecond)
		_ = c.Close()
	}()
	port, err := dialNetworkDevice("net://"+ln.Addr().String(), 40*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = port.Close() }()
	if err := port.SetDTR(true); err != nil || port.Drain() != nil || port.Break(time.Millisecond) != nil || port.SetMode(nil) != nil {
		t.Fatal("noop serial methods")
	}
	if _, err := port.GetModemStatusBits(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	n, err := port.Read(buf)
	if n != 0 || err != nil {
		t.Fatalf("timeout read n=%d err=%v", n, err)
	}
	<-accepted
	_ = port.Close()
	if _, err := port.Read(buf); err == nil {
		t.Fatal("read after close")
	}
	if _, err := dialNetworkDevice("tcp://127.0.0.1:1", time.Second); err == nil {
		t.Fatal("closed port accepted")
	}
	if _, err := dialNetworkDevice("tcp://::1:1", time.Second); err == nil || !strings.Contains(err.Error(), "[::1]:1") {
		t.Fatal(err)
	}
	if _, err := dialNetworkDevice("tcp://[2001:db8::1]:9", time.Second); err == nil || !strings.Contains(err.Error(), "[2001:db8::1]:9") {
		t.Fatal(err)
	}
}

func writeTestKeyPair(t *testing.T, dir string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "mqtt-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(dir, "client.pem")
	keyPath := filepath.Join(dir, "client.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func TestMultiPortStatusTargetsAndConfig(t *testing.T) {
	ctx := context.Background()
	opts := &Options{}
	opts.Publish.Topic = "rflink"
	opts.Serial.Device = "COM1"
	opts.Serial.Devices = "COM2"
	p := &Publisher{opts: opts}
	p.SetDeviceConnected(ctx, "COM1", true)
	if p.serialUp.Load() {
		t.Fatal("aggregate online while COM2 is down")
	}
	snap := p.Health()
	if len(snap.SerialPorts) != 2 || snap.SerialPorts[0].Device != "COM1" || !snap.SerialPorts[0].Connected || snap.SerialPorts[1].Device != "COM2" {
		t.Fatalf("ports %#v", snap.SerialPorts)
	}
	if snap.SerialPorts[1].Connected {
		t.Fatal("COM2 should be down")
	}
	p.SetDeviceConnected(ctx, "com2", true)
	if !p.serialUp.Load() {
		t.Fatal("aggregate should be online when every port is up")
	}
	if got := opts.serialDeviceStatusTopic("tcp://10.0.0.8:23"); got != "rflink/serial/tcp___10_0_0_8_23/status" {
		t.Fatal(got)
	}
	p.SetDeviceConnected(ctx, "COM2", false)
	if p.serialUp.Load() {
		t.Fatal("aggregate stayed online after COM2 dropped")
	}

	one := &Publisher{opts: &Options{}}
	one.opts.Publish.Topic = "rflink"
	one.opts.Serial.Device = "COM9"
	one.SetSerialConnected(ctx, true)
	if !one.serialUp.Load() || len(one.serialPortSnapshot()) != 1 {
		t.Fatal("single port status")
	}

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "runtime.json")
	if err := os.WriteFile(cfgPath, []byte(`{"version":1,"friendly_names":"A_1:Lamp"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	kept := &Options{}
	kept.Publish.MinIntervalMs = 2500
	kept.Publish.Throttle = "ABC:1000"
	kept.Publish.HADeviceTriggers = true
	kept.Serial.CommandDevices = "COM9"
	kept.Serial.Device = "COM9"
	kept.Publish.HADiscoveryPrefix = "homeassistant"
	kept.RuntimeConfigFile = cfgPath
	rm, err := newRuntimeManager(kept)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rm.Stop)
	if kept.Publish.MinIntervalMs != 2500 || kept.Serial.CommandDevices != "COM9" || kept.throttleFor("ABC") != time.Second {
		t.Fatalf("env keys stolen interval=%d devices=%q", kept.Publish.MinIntervalMs, kept.Serial.CommandDevices)
	}
	_, src := rm.snapshot()
	if src.MinIntervalMs != "env" || src.CommandDevices != "env" || src.FriendlyNames != "runtime" {
		t.Fatalf("sources %+v", src)
	}

	app, h := newCoverageStack(t)
	primary := newFakeSerial()
	extra := newFakeSerial()
	app.opts.Serial.Device = "COM1"
	app.opts.Serial.Devices = "COM2"
	app.serial = &SerialReader{port: primary, opts: app.opts, device: "COM1"}
	app.extras = []*SerialReader{{port: extra, opts: app.opts, device: "COM2"}}
	app.opts.Serial.CommandDevices = "COM2"
	res := app.ExecuteCommandFrom("10;PING;", "internal", "")
	if res.Status != "ok" || len(primary.written) != 0 || len(extra.written) != 1 {
		t.Fatalf("filter %+v primary=%d extra=%d", res, len(primary.written), len(extra.written))
	}
	res = app.executeCommand("10;PING;", "http", "gui", []string{"COM1"})
	if res.Status != "ok" || len(primary.written) != 1 {
		t.Fatalf("override %+v", res)
	}
	res = app.executeCommand("10;PING;", "http", "gui", []string{"NOPE"})
	if res.Status != "rejected" {
		t.Fatalf("miss %+v", res)
	}
	app.opts.Serial.CommandDevices = ""
	res = app.ExecuteCommandFrom("10;VERSION;", "mqtt", "gorflink")
	if res.Status != "ok" || len(primary.written) != 2 || len(extra.written) != 2 {
		t.Fatalf("all %+v primary=%d extra=%d", res, len(primary.written), len(extra.written))
	}

	// A config save re-applies env token strings. The coverage stack stores tokens
	// only in the parsed map, so put the same list back before the save.
	app.opts.APIAuthTokens = "reader:readtoken123456:never:read,cmd:cmdtoken12345678:never:command,admin:admintoken123456:never:admin"
	auth := map[string]string{"Authorization": "Bearer admintoken123456", "Content-Type": "application/json"}
	rr := doReq(h, http.MethodPut, "/api/v1/config", `{"friendly_names":"OREGON_0001:Kitchen"}`, auth, nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "friendly_names") {
		t.Fatalf("friendly %s", rr.Body.String())
	}
	raw, err := os.ReadFile(app.runtime.pathString())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "min_interval_ms") || strings.Contains(string(raw), "command_devices") {
		t.Fatalf("partial save took new keys: %s", raw)
	}
	if app.opts.Publish.MinIntervalMs != 0 {
		t.Fatal("min interval changed")
	}
	rr = doReq(h, http.MethodPut, "/api/v1/config", `{"throttle":"nope"}`, auth, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad throttle %d %s", rr.Code, rr.Body.String())
	}
	body := `{"min_interval_ms":1500,"throttle":"OREGON_0001:5000","ha_device_triggers":false,"ha_sensor_availability":true,"ha_sensor_stale_sec":30,"command_devices":"COM2"}`
	rr = doReq(h, http.MethodPut, "/api/v1/config", body, auth, nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"warning"`) || !strings.Contains(rr.Body.String(), "min_interval_ms") {
		t.Fatalf("first save %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "These keys are now owned") {
		t.Fatalf("missing warning %s", rr.Body.String())
	}
	if app.opts.Publish.MinIntervalMs != 1500 || app.opts.throttleFor("OREGON_0001") != 5*time.Second || app.opts.Serial.CommandDevices != "COM2" {
		t.Fatalf("interval %d devices %q", app.opts.Publish.MinIntervalMs, app.opts.Serial.CommandDevices)
	}
	if !app.opts.Publish.HASensorAvailability || app.opts.Publish.HASensorStaleSec != 30 || app.opts.Publish.HADeviceTriggers {
		t.Fatal("ha flags")
	}
	rr = doReq(h, http.MethodPut, "/api/v1/config", `{"min_interval_ms":1500}`, auth, nil)
	if rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), "These keys are now owned") {
		t.Fatalf("repeat save %s", rr.Body.String())
	}
	for _, part := range []string{`id="minInt"`, `id="portPick"`, `id="cmdDev"`, `id="haTrig"`, `id="haAvail"`, "writes it into the runtime file"} {
		if !strings.Contains(guiHTML, part) {
			t.Fatalf("gui missing %s", part)
		}
	}
	cmdAuth := map[string]string{"Authorization": "Bearer cmdtoken12345678", "Content-Type": "application/json"}
	rr = doReq(h, http.MethodPost, "/api/v1/command", `{"command":"10;PING;","devices":["COM1"]}`, cmdAuth, nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"status": "ok"`) {
		t.Fatalf("http target %d %s", rr.Code, rr.Body.String())
	}
}

func TestPortStatusEdgesAndConfigBounds(t *testing.T) {
	ctx := context.Background()
	opts := &Options{}
	opts.Publish.Topic = "rflink"
	opts.Serial.Device = "COM1"
	opts.Serial.Devices = "COM2"
	p := &Publisher{opts: opts, startedAt: time.Now()}
	before := p.serialPortSnapshot()
	if len(before) != 2 || before[0].Connected || before[1].Connected {
		t.Fatalf("initial ports %#v", before)
	}
	p.SetDeviceConnected(ctx, "", true)
	if p.serialUp.Load() {
		t.Fatal("unnamed device marked the aggregate online")
	}
	p.SetDeviceConnected(ctx, "COM1", true)
	p.SetDeviceConnected(ctx, "COM2", true)
	if !strings.Contains(p.MetricsText(), "gorflink_serial_connected 1") {
		t.Fatal(p.MetricsText())
	}
	p.SetDeviceConnected(ctx, "COM2", false)
	if !strings.Contains(p.MetricsText(), "gorflink_serial_connected 0") || !strings.Contains(p.MetricsText(), "every configured serial port") {
		t.Fatal(p.MetricsText())
	}
	var none *Publisher
	none.SetDeviceConnected(ctx, "COM1", true)
	p.Disconnect()
	(&App{}).noteSerial(ctx, "COM1", false)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "runtime.json")
	if err := os.WriteFile(cfgPath, []byte(`{"version":1,"min_interval_ms":999999999,"ha_sensor_stale_sec":-3,"command_devices":"  COM1  ","ha_device_triggers":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	kept := &Options{}
	kept.Publish.MinIntervalMs = 10
	kept.Publish.HASensorStaleSec = 4
	kept.Publish.HADeviceTriggers = false
	kept.Publish.HADiscoveryPrefix = "homeassistant"
	kept.Serial.Device = "COM1"
	kept.RuntimeConfigFile = cfgPath
	rm, err := newRuntimeManager(kept)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rm.Stop)
	if kept.Publish.MinIntervalMs != 86400000 || kept.Publish.HASensorStaleSec != 0 || kept.Serial.CommandDevices != "COM1" || !kept.Publish.HADeviceTriggers {
		t.Fatalf("clamped interval=%d stale=%d devices=%q triggers=%v", kept.Publish.MinIntervalMs, kept.Publish.HASensorStaleSec, kept.Serial.CommandDevices, kept.Publish.HADeviceTriggers)
	}
	off := false
	first, err := rm.SaveOverlay(RuntimeFile{HADeviceTriggers: &off})
	if err != nil || len(first) != 0 {
		t.Fatalf("overlay %v %v", first, err)
	}
	if !rm.consumeHAChanged() || kept.Publish.HADeviceTriggers {
		t.Fatal("trigger change did not ask for rediscovery")
	}
	empty := ""
	if _, err := rm.SaveOverlay(RuntimeFile{CommandDevices: &empty}); err != nil {
		t.Fatal(err)
	}
	if kept.commandDevicesConfigured() || !kept.commandDeviceAllow("COM2") {
		t.Fatal("empty runtime list should mean every port")
	}
	_, src := rm.snapshot()
	if src.CommandDevices != "runtime" || src.HADeviceTriggers != "runtime" {
		t.Fatalf("sources %+v", src)
	}

	app, h := newCoverageStack(t)
	app.opts.APIAuthTokens = "reader:readtoken123456:never:read,cmd:cmdtoken12345678:never:command,admin:admintoken123456:never:admin"
	app.opts.Serial.Device = "COM1"
	app.opts.Serial.Devices = "COM2"
	app.opts.Serial.CommandDevices = "COM1,COM2"
	down := &SerialReader{opts: app.opts, device: "COM1"}
	downExtra := &SerialReader{opts: app.opts, device: "COM2"}
	app.serial = down
	app.extras = []*SerialReader{downExtra}
	res := app.ExecuteCommandFrom("10;PING;", "internal", "")
	if res.Status != "serial_down" {
		t.Fatalf("matched but closed: %+v", res)
	}
	res = app.executeCommand("10;PING;", "http", "gui", []string{})
	if res.Status != "rejected" {
		t.Fatalf("empty target list: %+v", res)
	}

	auth := map[string]string{"Authorization": "Bearer admintoken123456", "Content-Type": "application/json"}
	readAuth := map[string]string{"Authorization": "Bearer readtoken123456"}
	cmdAuth := map[string]string{"Authorization": "Bearer cmdtoken12345678", "Content-Type": "application/json"}
	rr := doReq(h, http.MethodGet, "/api/v1/health", "", readAuth, nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"device": "COM1"`) || !strings.Contains(rr.Body.String(), `"device": "COM2"`) {
		t.Fatalf("health %s", rr.Body.String())
	}
	rr = doReq(h, http.MethodGet, "/api/v1/serial", "", readAuth, nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"ports"`) || !strings.Contains(rr.Body.String(), "COM1,COM2") {
		t.Fatalf("serial %s", rr.Body.String())
	}
	rr = doReq(h, http.MethodPost, "/api/v1/command", `{"command":"10;PING;","devices":[]}`, cmdAuth, nil)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "rejected") {
		t.Fatalf("empty devices %d %s", rr.Code, rr.Body.String())
	}
	for _, body := range []string{
		`{"min_interval_ms":86400001}`,
		`{"ha_sensor_stale_sec":-1}`,
		`{"ha_sensor_stale_sec":86401}`,
		`{"command_devices":"COM1\nCOM2"}`,
		`{"throttle":"BAD/ID:10"}`,
		`{"throttle":"OREGON_0001:0"}`,
	} {
		rr = doReq(h, http.MethodPut, "/api/v1/config", body, auth, nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s -> %d %s", body, rr.Code, rr.Body.String())
		}
	}
	raw, err := os.ReadFile(app.runtime.pathString())
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "min_interval_ms") || strings.Contains(string(raw), "throttle") {
		t.Fatalf("rejected save was persisted: %s", raw)
	}
	for _, part := range []string{"validateBoundedInt", "validateThrottleCSV", "validateCommandDevicesCSV", "renderPortPicker", "selectedCommandDevices", "syncCmdDevFromPicker", "rawLineText"} {
		if !strings.Contains(guiHTML, part) {
			t.Fatalf("gui missing %s", part)
		}
	}
}

func TestSensorFollowsLiveGatewayAndPartialCommand(t *testing.T) {
	opts := &Options{}
	opts.Publish.Topic = "rflink"
	opts.Serial.Device = "COM1"
	p := &Publisher{opts: opts}
	got := p.sensorAvailability("OREGON_1")
	if len(got) != 2 || got[1].Topic != "rflink/serial/status" || p.actuatorSerialTopic() != "rflink/serial/status" {
		t.Fatalf("single port %+v actuator %s", got, p.actuatorSerialTopic())
	}
	opts.Serial.Devices = "COM2"
	if p.bindSensorPort("OREGON_1", "COM1") {
		t.Fatal("first bind is not a takeover")
	}
	got = p.sensorAvailability("OREGON_1")
	if len(got) != 2 || got[1].Topic != "rflink/serial/COM1/status" {
		t.Fatalf("owned %+v", got)
	}
	ctx := t.Context()
	p.SetDeviceConnected(ctx, "COM1", true)
	p.SetDeviceConnected(ctx, "COM2", true)
	if p.bindSensorPort("OREGON_1", "COM2") || p.sensorAvailability("OREGON_1")[1].Topic != "rflink/serial/COM1/status" {
		t.Fatal("overlapping frame replaced the owner")
	}
	p.sensorSeenMu.Lock()
	p.sensorPortAt["OREGON_1"] = time.Now().Add(-sensorOwnerStick - time.Second)
	p.sensorSeenMu.Unlock()
	if !p.bindSensorPort("OREGON_1", "COM2") || p.sensorAvailability("OREGON_1")[1].Topic != "rflink/serial/COM2/status" {
		t.Fatal("quiet owner kept the sensor")
	}
	p.SetDeviceConnected(ctx, "COM2", false)
	if !p.bindSensorPort("OREGON_1", "COM1") || p.sensorAvailability("OREGON_1")[1].Topic != "rflink/serial/COM1/status" {
		t.Fatal("down owner did not hand the sensor over")
	}
	if p.actuatorSerialTopic() != "rflink/serial/command/status" {
		t.Fatal(p.actuatorSerialTopic())
	}
	p.SetDeviceConnected(ctx, "COM1", false)
	p.SetDeviceConnected(ctx, "COM2", true)
	if !p.commandTargetsUp(p.serialPortSnapshot()) {
		t.Fatal("COM2 is up and every port is a command target")
	}
	opts.Serial.CommandDevices = "COM1"
	if p.commandTargetsUp(p.serialPortSnapshot()) {
		t.Fatal("the only command target is down")
	}

	opts.Serial.CommandDevices = ""
	primary := newFakeSerial()
	extra := newFakeSerial()
	extra.writeErr = net.ErrClosed
	app := &App{
		opts:      opts,
		publisher: p,
		serial:    &SerialReader{port: primary, opts: opts, device: "COM1"},
		extras:    []*SerialReader{{port: extra, opts: opts, device: "COM2"}},
	}
	res := app.executeCommand("10;PING;", "http", "gui", nil)
	if res.Status != "partial" || len(res.Ports) != 2 || len(primary.written) != 1 || len(extra.written) != 0 {
		t.Fatalf("%+v primary=%d extra=%d", res, len(primary.written), len(extra.written))
	}
	if res.Ports[0].Device != "COM1" || res.Ports[0].Status != "ok" || res.Ports[1].Device != "COM2" || res.Ports[1].Status != "error" {
		t.Fatalf("ports %+v", res.Ports)
	}

	p.opts.Serial.PingIntervalSec = 1
	p.opts.Serial.PingTimeoutSec = 1
	p.opts.Serial.PingFailThreshold = 1
	p.markExtraPing("COM2")
	p.pingMu.Lock()
	st := p.extraPing[p.extraKey("COM2")]
	st.sentAt = time.Now().Add(-2 * time.Second)
	st.writing = false
	p.pingMu.Unlock()
	ch := p.extraRestartChan("COM2")
	p.checkPingTimeout()
	select {
	case <-ch:
	default:
		t.Fatal("extra restart was not signaled")
	}
	if !p.anyUnresponsive() {
		t.Fatal("extra stayed responsive")
	}
	p.noteExtraPong("COM2")
	if p.anyUnresponsive() {
		t.Fatal("pong did not clear the extra gateway")
	}
	if !strings.Contains(guiHTML, "await tickConfig(); await tickHealth()") || !strings.Contains(guiHTML, "config still loading") || !strings.Contains(guiHTML, "if (!cfgReady) return") || !strings.Contains(guiHTML, "cmdDevFromPicker") {
		t.Fatal("port picker can render before command_devices is loaded")
	}
	if !strings.Contains(guiHTML, "if (lastPorts.length > 1) renderPortPicker(lastPorts);") {
		t.Fatal("send reads port checkboxes before the command-device field is applied")
	}
	if strings.Contains(guiHTML, "c.ha_discovery_prefix||'homeassistant'") || !strings.Contains(guiHTML, "c.ha_discovery_prefix||''") {
		t.Fatal("empty HA prefix is filled before save")
	}
	if strings.Contains(guiHTML, "$('haPfx').value = 'homeassistant'") {
		t.Fatal("checking discovery writes a prefix the save will keep")
	}
	if !strings.Contains(guiHTML, `data-sid="'+esc(s.id_prefix)+'"`) {
		t.Fatal("session revoke sends the cookie value")
	}
	if !strings.Contains(guiHTML, "serial_dropped") || !strings.Contains(guiHTML, "throttle_dropped") || !strings.Contains(guiHTML, "p.dropped") || !strings.Contains(guiHTML, "queue_cap") {
		t.Fatal("health rows")
	}

	p.serialRestart = make(chan struct{}, 1)
	opts.Serial.WatchdogSec = 1
	p.SetDeviceConnected(ctx, "COM1", true)
	p.SetDeviceConnected(ctx, "COM2", true)
	p.serialMu.Lock()
	p.serialHeard["COM2"] = time.Now().Add(-2 * time.Second)
	p.serialMu.Unlock()
	extraCh := p.extraRestartChan("COM2")
	p.checkSerialWatchdog()
	select {
	case <-extraCh:
	default:
		t.Fatal("silent extra was not reopened")
	}
	select {
	case <-p.serialRestart:
		t.Fatal("primary was reopened with the extra")
	default:
	}
	if !p.Health().SerialStale {
		t.Fatal("silent gateway left health fresh")
	}
	p.checkSerialWatchdog()
	select {
	case <-extraCh:
		t.Fatal("watchdog restarted the extra again inside the hold")
	default:
	}
}

func TestNetworkGatewayConnectsInBackground(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	opts := &Options{}
	opts.Serial.Device = "tcp://" + ln.Addr().String()
	sr := NewSerialReader(opts)
	if sr.IsConnected() {
		t.Fatal("tcp gateway opened during startup")
	}
	_ = ln.(*net.TCPListener).SetDeadline(time.Now().Add(200 * time.Millisecond))
	conn, err := ln.Accept()
	if err == nil {
		_ = conn.Close()
		t.Fatal("startup dialed the network gateway")
	}
}
