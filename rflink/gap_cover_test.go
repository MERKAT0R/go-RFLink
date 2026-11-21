/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"context"
	"net"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
)

func TestUntestedHelpersAndQueues(t *testing.T) {
	ch := newSerialChannel()
	if cap(ch) != serialChannelBuffer {
		t.Fatalf("cap %d", cap(ch))
	}

	opts := &Options{}
	opts.Publish.Topic = "rflink"
	opts.Publish.LWTQoS = 1
	opts.Publish.ConnectRetryInterval = 3
	opts.Publish.HealthLogIntervalSec = 2
	opts.Publish.HomeAssistantDiscovery = true
	opts.Publish.HADiscoveryPrefix = "homeassistant"
	opts.Serial.Device = "COM1"
	opts.Serial.Devices = "COM2"
	opts.ignoreSet = map[string]struct{}{"OREGON": {}}
	if opts.lwtQoS() != 1 || opts.mqttConnectRetryInterval() != 3*time.Second {
		t.Fatal("qos or retry")
	}
	if opts.healthLogInterval() != 2*time.Second || (&Options{}).healthLogInterval() != 0 {
		t.Fatal("health interval")
	}
	if opts.healthTopic() != "rflink/health" || opts.haRediscoverTopic() != "rflink/admin/rediscover" {
		t.Fatal("topics")
	}
	if !opts.SensorIgnored("Oregon", "other") || opts.SensorIgnored("X", "Y") {
		t.Fatal("ignore")
	}
	opts.LogMQTTTopics()

	var none *Publisher
	none.NoteSerialActivity()
	none.ClearPingSent()
	none.markExtraPingWritten("COM2")
	none.clearExtraPing("COM2")
	none.resetExtraPing("COM2")
	if none.extraPingOutstanding("COM2") || none.actuatorAvailability() != nil {
		t.Fatal("nil publisher")
	}

	p := &Publisher{opts: opts, dedupLast: map[string]time.Time{}, pending: map[string]pendingSensor{}}
	p.pending["K"] = pendingSensor{storedAt: time.Now()}
	p.clearPending("K")
	if _, ok := p.pending["K"]; ok {
		t.Fatal("pending")
	}
	p.touchSensorOwner("OREGON_1", time.Now())
	if p.sensorPortAt["OREGON_1"].IsZero() {
		t.Fatal("owner stamp")
	}
	if len(p.actuatorAvailability()) != 2 || p.actuatorAvailability()[1]["topic"] != "rflink/serial/command/status" {
		t.Fatal(p.actuatorAvailability())
	}
	p.NoteSerialActivity()
	p.MarkPingSent()
	p.ClearPingSent()
	if p.PingOutstanding() {
		t.Fatal("ping still outstanding")
	}
	p.markExtraPing("COM2")
	if !p.extraPingOutstanding("COM2") {
		t.Fatal("extra ping")
	}
	p.markExtraPingWritten("COM2")
	p.clearExtraPing("COM2")
	if p.extraPingOutstanding("COM2") {
		t.Fatal("extra ping cleared")
	}
	p.markExtraPing("COM2")
	p.resetExtraPing("COM2")
	if p.extraPingOutstanding("COM2") {
		t.Fatal("extra ping reset")
	}
	if err := p.publishStatusValue(context.Background(), "rflink/serial/status", statusOnline); err == nil {
		t.Fatal("publish without mqtt")
	}
	p.publishHealth(context.Background())
	p.reportHealth(context.Background())

	app := &App{publisher: p, serialCh: make(chan serialFrame, 1)}
	if !app.enqueueSerial(serialFrame{Device: "COM1", Line: "a"}) {
		t.Fatal("enqueue")
	}
	if app.enqueueSerial(serialFrame{Device: "COM1", Line: "b"}) {
		t.Fatal("shared queue accepted past its cap")
	}
	app.noteSerialQueueFull("COM1")
	app.noteSerialQueueFull("COM1")
	if p.serialDropped.Load() != 2 {
		t.Fatal(p.serialDropped.Load())
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	np := &netPort{conn: conn}
	if err := np.ResetInputBuffer(); err != nil || np.ResetOutputBuffer() != nil || np.SetRTS(true) != nil {
		t.Fatal("net port stubs")
	}
	_ = np.Close()

	initLogger("error")
	listSerialPorts()

	rec := httptest.NewRecorder()
	hw := &hijackTracker{ResponseWriter: rec}
	hw.Flush()
	if hw.Unwrap() != rec {
		t.Fatal("unwrap")
	}
	sr := &statusRecorder{ResponseWriter: rec}
	sr.Flush()
	if sr.Unwrap() != rec {
		t.Fatal("status unwrap")
	}
	sr.hijacked = true
	sr.Flush()
}

func TestPublishLoopAndExtraSerialStop(t *testing.T) {
	opts := &Options{}
	opts.Publish.Topic = "rflink"
	opts.Publish.DedupWindowMs = 1000
	opts.Serial.Device = "COM1"
	opts.Serial.Devices = "COM2"
	opts.Serial.ReadTimeoutSec = 1
	opts.Serial.ReconnectMinDelaySec = 1
	opts.Serial.ReconnectMaxDelaySec = 2
	p := &Publisher{opts: opts, dedupLast: map[string]time.Time{}, pending: map[string]pendingSensor{}, startedAt: time.Now()}
	ctx, cancel := context.WithCancel(context.Background())
	serialCh := make(chan serialFrame, 2)
	serialCh <- serialFrame{Device: "COM1", Line: "20;01;Oregon;ID=0001;TEMP=00cd;"}
	done := make(chan struct{})
	go func() {
		p.RunPublishLoop(ctx, serialCh)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publish loop did not stop")
	}

	fp := newFakeSerial([]byte("20;01;VER=1.2;\n"), []byte("20;02;PONG;\n"))
	sr := &SerialReader{opts: opts, device: "COM2", port: fp}
	app := &App{
		opts:      opts,
		publisher: p,
		serialCh:  make(chan serialFrame, 4),
	}
	ctx, cancel = context.WithCancel(context.Background())
	app.wg.Add(1)
	go app.runExtraSerial(ctx, sr)
	got := 0
	deadline := time.After(2 * time.Second)
	for got < 2 {
		select {
		case frame := <-app.serialCh:
			got++
			if frame.Device != "COM2" {
				t.Fatal(frame.Device)
			}
		case <-deadline:
			t.Fatalf("extra frames %d", got)
		}
	}
	cancel()
	wait := make(chan struct{})
	go func() {
		app.wg.Wait()
		close(wait)
	}()
	select {
	case <-wait:
	case <-time.After(3 * time.Second):
		t.Fatal("extra serial loop did not stop")
	}
}

func TestStatusRecorderHijackGuard(t *testing.T) {
	rec := httptest.NewRecorder()
	bare := &statusRecorder{ResponseWriter: rec, hijacked: true}
	if _, err := bare.Write([]byte("x")); err == nil {
		t.Fatal("write after hijack")
	}
	bare.Flush()
}

func TestStaleExtraRestartDoesNotKillSession(t *testing.T) {
	opts := &Options{}
	opts.Publish.Topic = "rflink"
	opts.Serial.Device = "COM1"
	opts.Serial.Devices = "COM2"
	opts.Serial.ReconnectMinDelaySec = 1
	opts.Serial.ReconnectMaxDelaySec = 2
	p := &Publisher{opts: opts}
	p.signalExtraRestart("COM2")
	sr := &SerialReader{opts: opts, device: "COM2", port: newFakeSerial()}
	app := &App{opts: opts, publisher: p, serialCh: make(chan serialFrame, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	app.wg.Add(1)
	go app.runExtraSerial(ctx, sr)
	time.Sleep(150 * time.Millisecond)
	alive := sr.IsConnected()
	cancel()
	done := make(chan struct{})
	go func() {
		app.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("extra loop did not stop")
	}
	if !alive {
		t.Fatal("a restart left from the previous session closed the new one")
	}
}

func TestAggregateStatusAndThrottleAndCanonicalOwner(t *testing.T) {
	opts := &Options{}
	opts.Publish.Topic = "rflink"
	opts.Publish.DedupWindowMs = 5000
	opts.Publish.MinIntervalMs = 5000
	opts.Serial.Device = "COM1"
	opts.Serial.Devices = "COM2"
	opts.hwidMap = map[string]string{"0001": "DOOR"}
	p := &Publisher{
		opts:       opts,
		dedupLast:  map[string]time.Time{},
		pending:    map[string]pendingSensor{},
		sensorSeen: map[string]time.Time{},
	}
	var wg sync.WaitGroup
	for range 30 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			p.SetDeviceConnected(context.Background(), "COM1", true)
		}()
		go func() {
			defer wg.Done()
			p.SetDeviceConnected(context.Background(), "COM2", true)
		}()
		wg.Wait()
		if !p.serialUp.Load() {
			t.Fatal("aggregate status stuck offline while both ports are up")
		}
	}

	msg := "20;01;Oregon;ID=0001;TEMP=00cd;"
	p.handleSerialMessage(context.Background(), "COM2", msg)
	if p.sensorSerialTopic("DOOR") != "rflink/serial/COM2/status" {
		t.Fatal(p.sensorSerialTopic("DOOR"))
	}

	p.cm = &autopaho.ConnectionManager{}
	p.sensorSeenMu.Lock()
	p.sensorSeen["DOOR"] = time.Now()
	p.sensorSeenMu.Unlock()
	p.handleSerialMessage(context.Background(), "COM2", "20;02;Oregon;ID=0001;TEMP=00ce;")
	if p.isDuplicate("20;02;Oregon;ID=0001;TEMP=00ce;") {
		t.Fatal("throttled frame was remembered as published")
	}
}

func TestTakeoverDoesNotCountDedupDrop(t *testing.T) {
	opts := &Options{}
	opts.Publish.Topic = "rflink"
	opts.Publish.DedupWindowMs = 5000
	opts.Serial.Device = "COM1"
	opts.Serial.Devices = "COM2"
	p := &Publisher{
		opts:       opts,
		dedupLast:  map[string]time.Time{},
		pending:    map[string]pendingSensor{},
		sensorSeen: map[string]time.Time{},
	}
	msg := "20;01;Oregon;ID=0001;TEMP=00c8;"
	p.SetDeviceConnected(context.Background(), "COM1", true)
	p.handleSerialMessage(context.Background(), "COM1", msg)
	p.markDedup(msg)
	p.SetDeviceConnected(context.Background(), "COM1", false)
	p.SetDeviceConnected(context.Background(), "COM2", true)
	before := p.dedupDropped.Load()
	p.handleSerialMessage(context.Background(), "COM2", msg)
	if p.dedupDropped.Load() != before {
		t.Fatalf("takeover counted as a drop: %d", p.dedupDropped.Load())
	}
	p.handleSerialMessage(context.Background(), "COM2", msg)
	if p.dedupDropped.Load() != before {
		t.Fatalf("failed takeover still blocked the retry: %d", p.dedupDropped.Load())
	}
	p.markDedup(msg)
	p.handleSerialMessage(context.Background(), "COM2", msg)
	if p.dedupDropped.Load() != before+1 {
		t.Fatalf("repeat drop %d", p.dedupDropped.Load())
	}
	if promLabel("a\"b\n\t") != "\"a\\\"b\\n\t\"" {
		t.Fatalf("label %s", promLabel("a\"b\n\t"))
	}
}

func TestFlushKeepsPendingWhenThrottled(t *testing.T) {
	opts := &Options{}
	opts.Publish.PendingTTLSec = 60
	opts.Publish.MinIntervalMs = 60000
	p := &Publisher{
		opts:       opts,
		pending:    map[string]pendingSensor{},
		sensorSeen: map[string]time.Time{},
		cm:         &autopaho.ConnectionManager{},
	}
	temp := float32(21)
	stored := time.Now().Add(-30 * time.Second)
	p.pending["OREGON_0001"] = pendingSensor{
		sd:       &SensorData{Hwid: "OREGON_0001", Temperature: &temp},
		storedAt: stored,
	}
	p.sensorSeen["OREGON_0001"] = time.Now()
	p.flushPending(context.Background())
	got, ok := p.pending["OREGON_0001"]
	if !ok || got.sd == nil || got.sd.Temperature == nil || *got.sd.Temperature != 21 || !got.storedAt.Equal(stored) {
		t.Fatalf("throttled flush dropped %#v", got)
	}
}

func TestPendingSkipsOneShotAndEchoIgnoresProtocolPrefix(t *testing.T) {
	opts := &Options{}
	opts.Publish.PendingTTLSec = 60
	p := &Publisher{opts: opts, pending: map[string]pendingSensor{}}
	temp := float32(20)
	p.storePending(&SensorData{
		Hwid: "OREGON_0001", Temperature: &temp,
		Extra: map[string]string{"smoke": "ON", "baro": "03E1"},
	})
	got := p.pending["OREGON_0001"].sd
	if got == nil || got.Temperature == nil || got.Extra["smoke"] != "" || got.Extra["baro"] != "03E1" {
		t.Fatalf("%#v", got)
	}
	p.storePending(&SensorData{Hwid: "ALARM", Extra: map[string]string{"smoke": "ON"}})
	if _, ok := p.pending["ALARM"]; ok {
		t.Fatal("one-shot alarm was buffered")
	}

	plain := haActuatorSwitch{CommandOn: "10;NewKaku;AABB;ON;", CommandOff: "10;NewKaku;AABB;OFF;"}
	if switchEchoState(plain, &SensorData{Id: "AABB"}, map[string]string{"cmd": "ON"}) != "ON" {
		t.Fatal("address echo")
	}
	if switchEchoState(plain, &SensorData{Id: "10"}, map[string]string{"cmd": "ON"}) != "" {
		t.Fatal("the 10 prefix matched a sensor")
	}
	ch := haActuatorSwitch{CommandOn: "10;NewKaku;AABB;1;ON;", CommandOff: "10;NewKaku;AABB;1;OFF;"}
	if switchEchoState(ch, &SensorData{Id: "AABB"}, map[string]string{"cmd": "ON"}) != "" {
		t.Fatal("channel switch matched a frame with no channel")
	}
	if switchEchoState(ch, &SensorData{Id: "AABB", Switch: "2"}, map[string]string{"cmd": "ON"}) != "" {
		t.Fatal("channel 2 matched channel 1")
	}
	if switchEchoState(ch, &SensorData{Id: "AABB", Switch: "1"}, map[string]string{"cmd": "ON"}) != "ON" {
		t.Fatal("channel 1 did not match")
	}
	frame := &SensorData{Id: "AABB", Switch: "1", Cmd: "ON"}
	fields := frame.FieldsForPublish("")
	if _, ok := fields["cmd"]; ok {
		t.Fatal("cmd published beside sw_")
	}
	if switchEchoState(ch, frame, fields) != "ON" {
		t.Fatal("switch+cmd frame did not echo")
	}
	cached := haDiscovered{component: "sensor", topic: "ha/sensor/x/config", serialTopic: "rflink/serial/status", deviceName: "Old"}
	if cached.sameSensor("sensor", "ha/sensor/x/config", "rflink/serial/status", "Front") {
		t.Fatal("rename counted as the same discovery")
	}
	if !cached.sameSensor("sensor", "ha/sensor/x/config", "rflink/serial/status", "Old") {
		t.Fatal("same discovery")
	}
	trig := haDiscovered{component: "device_automation", topic: "ha/device_automation/x/config", deviceName: "Old"}
	if trig.sameTrigger(trig.topic, "Front") {
		t.Fatal("trigger rename ignored")
	}
}

func TestIngressPutsFrameBackOnShutdown(t *testing.T) {
	q := newSerialIngress()
	if !q.offer(serialFrame{Device: "COM1", Line: "keep"}) {
		t.Fatal("offer")
	}
	out := make(chan serialFrame)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		q.run(ctx, out)
		close(done)
	}()
	time.Sleep(40 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ingress did not stop")
	}
	got := q.usage()["COM1"]
	if got.Queued != 1 {
		t.Fatalf("queued %d", got.Queued)
	}
}

func TestHandleCommandAuditsMQTTActor(t *testing.T) {
	opts := &Options{}
	opts.Publish.ClientID = "bridge"
	pub := &Publisher{opts: opts}
	pub.testPublish = func(string, []byte) error { return nil }
	app := &App{
		opts:      opts,
		publisher: pub,
		serial:    &SerialReader{opts: opts, device: "COM1"},
	}
	app.handleCommand("10;PING;")
}

func TestAbandonExtraPingOnlyWhileWriteIsInFlight(t *testing.T) {
	p := &Publisher{extraPing: map[string]*extraPingState{}}
	sent := time.Now().Add(-time.Second)
	p.extraPing["COM9"] = &extraPingState{sentAt: sent, writing: true}
	p.abandonExtraPingIfWriting("COM9")
	if !p.extraPing["COM9"].sentAt.IsZero() || p.extraPing["COM9"].writing {
		t.Fatal("in-flight ping was kept")
	}
	p.extraPing["COM8"] = &extraPingState{sentAt: sent, writing: false}
	p.abandonExtraPingIfWriting("COM8")
	if p.extraPing["COM8"].sentAt.IsZero() {
		t.Fatal("idle ping was cleared")
	}
	(&Publisher{}).abandonExtraPingIfWriting("COM1")
}

func TestNetworkGatewayConnectAndReconnect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	opts := &Options{}
	opts.Serial.ReadTimeoutSec = 1
	sr := &SerialReader{opts: opts, device: "tcp://" + ln.Addr().String()}
	if err := sr.connect(); err != nil {
		t.Fatal(err)
	}
	if !sr.IsConnected() {
		t.Fatal("tcp gateway did not connect")
	}
	if err := sr.Reconnect(); err != nil {
		t.Fatal(err)
	}
	if !sr.IsConnected() {
		t.Fatal("reconnect left the gateway down")
	}
	sr.disconnect()
	if sr.IsConnected() {
		t.Fatal("disconnect left the gateway up")
	}
	closed := &SerialReader{opts: opts, device: "tcp://127.0.0.1:1"}
	if err := closed.connect(); err == nil {
		t.Fatal("refused port connected")
	}
}

func TestWebhookStartSnapshotAndPollStop(t *testing.T) {
	opts := &Options{}
	opts.webhooks = []RuntimeWebhook{{
		Name: "off", Kind: "poll", URL: "https://example.com/hook", Method: "GET", IntervalSec: 30,
	}}
	app := &App{opts: opts}
	m := newWebhookManager(app)
	m.Start(context.Background())
	got := m.Snapshot()
	if len(got) != 1 || got[0].Name != "off" {
		t.Fatalf("%#v", got)
	}
	m.doPollOnce(context.Background(), RuntimeWebhook{Name: "bad", URL: "http://127.0.0.1/hook"}, "GET", 1, 64)
	m.wg.Add(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.runPoll(ctx, RuntimeWebhook{Name: "stopped", IntervalSec: 5})
	m.mu.Lock()
	m.hooks = []RuntimeWebhook{{
		Name: "sum", Kind: "push", Payload: "sumjson", URL: "http://127.0.0.1/hook", Enabled: true,
	}}
	m.mu.Unlock()
	m.NotifySumJSON("OREGON_1", []byte(`{"temp":1}`))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(m.pushSem) > 0 {
		time.Sleep(10 * time.Millisecond)
	}
	m.Stop()
}
