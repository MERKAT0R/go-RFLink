/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"
	"github.com/goccy/go-json"
)

const (
	statusOnline  = "online"
	statusOffline = "offline"
)

type pendingSensor struct {
	sd       *SensorData
	storedAt time.Time
}

// CmdResult is published to {topic}/cmd/result after an actuator command attempt.
type CmdResult struct {
	Command    string `json:"command"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	ReceivedAt string `json:"received_at"`
}

type healthSnapshot struct {
	Version            string            `json:"version"`
	GitSHA             string            `json:"git_sha"`
	StartedAt          string            `json:"started_at"`
	Status             string            `json:"status"`
	Published          uint64            `json:"published"`
	PublishFailed      uint64            `json:"publish_failed"`
	PendingFlushed     uint64            `json:"pending_flushed"`
	PendingDropped     uint64            `json:"pending_dropped"`
	PendingBuffered    int               `json:"pending_buffered"`
	SerialDropped      uint64            `json:"serial_dropped"`
	DedupDropped       uint64            `json:"dedup_dropped"`
	Panics             uint64            `json:"panics"`
	CommandsRejected   uint64            `json:"commands_rejected"`
	CommandsLimited    uint64            `json:"commands_rate_limited"`
	LastSensorAt       string            `json:"last_sensor_at,omitempty"`
	LastSensorHWID     string            `json:"last_sensor_hwid,omitempty"`
	LastSerialAt       string            `json:"last_serial_at,omitempty"`
	SerialStale        bool              `json:"serial_stale"`
	SerialConnected    bool              `json:"serial_connected"`
	RFLinkUnresponsive bool              `json:"rflink_unresponsive"`
	LastPongAt         string            `json:"last_pong_at,omitempty"`
	PingFails          uint64            `json:"ping_fails"`
	UptimeSec          int64             `json:"uptime_sec"`
	MQTTConnected      bool              `json:"mqtt_connected"`
	SensorsLastSeen    map[string]string `json:"sensors_last_seen,omitempty"`
}

// Publisher publishes SensorData to MQTT subtopics and optional Home Assistant discovery topics.
type Publisher struct {
	cm         *autopaho.ConnectionManager
	opts       *Options
	discovered sync.Map
	cmdHandler func(string)
	cmdMu      sync.Mutex

	cancel context.CancelFunc
	runCtx context.Context

	pendingMu sync.Mutex
	pending   map[string]pendingSensor

	dedupMu   sync.Mutex
	dedupLast map[string]time.Time

	sensorSeenMu sync.Mutex
	sensorSeen   map[string]time.Time // HWID -> last successful publish time

	published        atomic.Uint64
	publishFailed    atomic.Uint64
	pendingFlushed   atomic.Uint64
	pendingDropped   atomic.Uint64
	serialDropped    atomic.Uint64
	dedupDropped     atomic.Uint64
	panics           atomic.Uint64
	commandsRejected atomic.Uint64
	commandsLimited  atomic.Uint64
	lastSensorAt     atomic.Value
	lastSensorHWID   atomic.Value
	lastSerialAt     atomic.Value
	startedAt        time.Time
	mqttUp           atomic.Bool
	serialUp         atomic.Bool

	pingMu       sync.Mutex
	pingSentAt   time.Time // zero = no outstanding ping
	lastPongAt   time.Time
	pingFails    atomic.Uint64 // consecutive missed PONGs
	rflinkUnresp atomic.Bool
}

// NewPublisher returns a Publisher according to the options specified.
func NewPublisher(parent context.Context, o *Options) (*Publisher, error) {
	tlsConfig, err := buildTLSConfig(o)
	if err != nil {
		return nil, err
	}

	brokerURL, err := buildBrokerURL(o)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(parent)
	p := &Publisher{
		opts:       o,
		cancel:     cancel,
		runCtx:     ctx,
		pending:    make(map[string]pendingSensor),
		dedupLast:  make(map[string]time.Time),
		sensorSeen: make(map[string]time.Time),
		startedAt:  time.Now(),
	}
	p.lastSensorAt.Store(time.Time{})
	p.lastSensorHWID.Store("")
	p.lastSerialAt.Store(time.Time{})

	log.Info("mqtt client using MQTT v5 only (paho.golang/autopaho)")

	if o.Publish.TLSInsecureSkipVerify {
		log.Warn("TLS InsecureSkipVerify is enabled — connection is vulnerable to MITM attacks")
	}
	if o.isTLSScheme() && o.Publish.TLSCAFile == "" && !o.Publish.TLSInsecureSkipVerify {
		log.Warn("TLS scheme configured without PUBLISH_TLS_CA_FILE — system root CAs will be used")
	}

	lwtTopic := o.Publish.LWTTopic
	lwtMessage := o.Publish.LWTMessage
	lwtRetain := o.Publish.LWTRetain
	lwtQoS := o.lwtQoS()
	if lwtTopic == "" {
		lwtTopic = o.statusTopic()
		lwtMessage = statusOffline
		lwtRetain = true
		lwtQoS = o.mqttQoS()
	}

	cliCfg := autopaho.ClientConfig{
		ServerUrls:                    []*url.URL{brokerURL},
		KeepAlive:                     o.mqttKeepAlive(),
		ReconnectBackoff:              autopaho.NewConstantBackoff(o.mqttConnectRetryInterval()),
		ConnectTimeout:                30 * time.Second,
		CleanStartOnInitialConnection: o.Publish.CleanSession,
		SessionExpiryInterval:         o.mqttSessionExpiry(),
		TlsCfg:                        tlsConfig,
		ConnectUsername:               o.Publish.MqttUsername,
		ConnectPassword:               []byte(o.Publish.MqttPassword),
		WillMessage: &paho.WillMessage{
			Topic:   lwtTopic,
			Payload: []byte(lwtMessage),
			QoS:     lwtQoS,
			Retain:  lwtRetain,
		},
		OnConnectionUp: func(cm *autopaho.ConnectionManager, _ *paho.Connack) {
			p.mqttUp.Store(true)
			log.Info("mqtt connected", "broker", brokerURL.String())
			if err := p.publishRaw(ctx, o.statusTopic(), statusOnline, o.mqttQoS(), true); err != nil {
				log.Warn("failed to publish online status", "err", err)
			}
			serialStatus := statusOffline
			if p.serialUp.Load() {
				serialStatus = statusOnline
			}
			if err := p.publishRaw(ctx, o.serialStatusTopic(), serialStatus, o.mqttQoS(), true); err != nil {
				log.Warn("failed to publish serial status", "err", err)
			}
			if err := p.resubscribeCommands(cm); err != nil {
				log.Error("mqtt command resubscribe failed", "err", err)
			}
			p.publishHAActuators(ctx)
			p.flushPending(ctx)
		},
		OnConnectError: func(err error) {
			p.mqttUp.Store(false)
			log.Warn("mqtt connect error", "err", err)
		},
		ClientConfig: paho.ClientConfig{
			ClientID: o.Publish.ClientID,
			OnClientError: func(err error) {
				p.mqttUp.Store(false)
				log.Warn("mqtt client error", "err", err)
			},
			OnServerDisconnect: func(d *paho.Disconnect) {
				p.mqttUp.Store(false)
				if d.Properties != nil && d.Properties.ReasonString != "" {
					log.Warn("mqtt server disconnect", "reason", d.Properties.ReasonString, "code", d.ReasonCode)
				} else {
					log.Warn("mqtt server disconnect", "code", d.ReasonCode)
				}
			},
			OnPublishReceived: []func(paho.PublishReceived) (bool, error){
				func(pr paho.PublishReceived) (bool, error) {
					if pr.Packet == nil {
						return true, nil
					}
					topic := pr.Packet.Topic
					payload := string(pr.Packet.Payload)
					if topic == p.opts.haRediscoverTopic() {
						p.ClearHADiscovery()
						return true, nil
					}
					if topic == p.opts.Publish.CmdTopic {
						p.cmdMu.Lock()
						handler := p.cmdHandler
						p.cmdMu.Unlock()
						if handler != nil {
							handler(payload)
						}
					}
					return true, nil
				},
			},
		},
	}

	if o.Publish.InfinityReconnect {
		log.Info("mqtt connect retry enabled", "interval", o.mqttConnectRetryInterval())
	}

	cm, err := autopaho.NewConnection(ctx, cliCfg)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("mqtt new connection: %w", err)
	}
	p.cm = cm

	if !o.Publish.InfinityReconnect {
		waitCtx, waitCancel := context.WithTimeout(ctx, 30*time.Second)
		defer waitCancel()
		if err := cm.AwaitConnection(waitCtx); err != nil {
			cancel()
			return nil, fmt.Errorf("mqtt connect: %w", err)
		}
	} else {
		waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
		err := cm.AwaitConnection(waitCtx)
		waitCancel()
		if err != nil {
			log.Warn("mqtt initial connect timed out or failed, client will keep retrying in background", "err", err)
		}
	}

	return p, nil
}

func buildBrokerURL(o *Options) (*url.URL, error) {
	scheme := strings.ToLower(strings.TrimSpace(o.Publish.Scheme))
	switch scheme {
	case "tcp", "mqtt":
		scheme = "mqtt"
	case "ssl", "tls", "mqtts":
		scheme = "tls"
	case "ws":
		scheme = "ws"
	case "wss":
		scheme = "wss"
	}
	raw := fmt.Sprintf("%s://%s", scheme, o.Publish.Host)
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse mqtt broker url %q: %w", raw, err)
	}
	return u, nil
}

func buildTLSConfig(o *Options) (*tls.Config, error) {
	if !o.isTLSScheme() {
		if o.Publish.TLSCAFile != "" || o.Publish.TLSInsecureSkipVerify {
			log.Warn("TLS options ignored because MQTT scheme is not TLS", "scheme", o.Publish.Scheme)
		}
		return nil, nil
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if o.Publish.TLSInsecureSkipVerify {
		tlsConfig.InsecureSkipVerify = true
	}
	if o.Publish.TLSCAFile != "" {
		caCert, err := os.ReadFile(o.Publish.TLSCAFile)
		if err != nil {
			return nil, fmt.Errorf("read tls ca file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse tls ca file %q", o.Publish.TLSCAFile)
		}
		tlsConfig.RootCAs = pool
	}
	return tlsConfig, nil
}

func (p *Publisher) sensorTopic(sensorID string) string {
	return fmt.Sprintf("%s/%s", p.opts.Publish.Topic, sanitizeTopicSegment(sensorID))
}

func (p *Publisher) sensorFieldTopic(sensorID, field string) string {
	return fmt.Sprintf("%s/%s", p.sensorTopic(sensorID), sanitizeTopicSegment(field))
}

func sanitizeTopicSegment(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	for _, r := range value {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func (p *Publisher) applyFriendlyName(sd *SensorData) {
	if sd.FriendlyName != "" {
		return
	}
	if name := p.opts.FriendlyNameFor(sd.TopicID()); name != "" {
		sd.FriendlyName = name
		return
	}
	// Default device label for HA / sumJson when no map entry is configured.
	sd.FriendlyName = fmt.Sprintf("goRFLink_%s %s", sd.Model, sd.Id)
}

// dedupKey strips the RFLink sequence counter (2nd field) so radio retransmits
// like 20;AC;EV1527;… and 20;AD;EV1527;… map to the same key.
func dedupKey(msg string) string {
	msg = strings.TrimSpace(msg)
	pieces := strings.Split(msg, ";")
	if len(pieces) >= 3 && pieces[0] == "20" {
		// 20;SEQ;PROTO;fields… → 20;PROTO;fields…
		return strings.Join(append([]string{pieces[0]}, pieces[2:]...), ";")
	}
	return msg
}

func (p *Publisher) isDuplicate(msg string) bool {
	window := p.opts.dedupWindow()
	if window <= 0 {
		return false
	}
	key := dedupKey(msg)
	now := time.Now()
	p.dedupMu.Lock()
	defer p.dedupMu.Unlock()
	if last, ok := p.dedupLast[key]; ok && now.Sub(last) < window {
		p.dedupDropped.Add(1)
		log.Warn("debounce: dropped duplicate rflink message",
			"key", key,
			"message", msg,
			"age_ms", now.Sub(last).Milliseconds(),
			"window_ms", window.Milliseconds(),
		)
		return true
	}
	p.dedupLast[key] = now
	if len(p.dedupLast) > 500 {
		for k, t := range p.dedupLast {
			if now.Sub(t) > window*2 {
				delete(p.dedupLast, k)
			}
		}
	}
	return false
}

func (p *Publisher) PublishSensor(ctx context.Context, sd *SensorData) error {
	if p.cm == nil {
		return fmt.Errorf("mqtt client not connected")
	}
	sd.ApplyCanonicalHWID(p.opts)
	if p.opts.SensorIgnored(sd.Model, sd.Hwid) || p.opts.SensorIgnored(sd.Model, sd.Id) {
		log.Debug("ignored sensor by ignore-list", "model", sd.Model, "hwid", sd.Hwid, "id", sd.Id)
		return nil
	}
	p.applyFriendlyName(sd)
	now := time.Now()
	p.lastSensorAt.Store(now)
	p.lastSensorHWID.Store(sd.TopicID())
	p.noteSensorSeen(sd.TopicID(), now)

	fields := sd.FieldsForPublish(p.opts.temperatureUnit())
	if len(fields) == 0 {
		log.Debug("sensor message has no publishable fields", "id", sd.Id, "hwid", sd.Hwid, "model", sd.Model)
	}
	if p.opts.Publish.HomeAssistantDiscovery {
		p.publishHomeAssistantDiscovery(ctx, sd, fields)
	}

	qos := p.opts.mqttQoS()
	defaultRetain := p.opts.Publish.Retain
	topicID := sd.TopicID()

	for field, value := range fields {
		retain := defaultRetain
		if field == "cmd" || field == "switch" || strings.HasPrefix(field, "sw_") {
			retain = false
		}
		topic := p.sensorFieldTopic(topicID, field)
		if err := p.publishRaw(ctx, topic, value, qos, retain); err != nil {
			p.storePending(sd)
			p.publishFailed.Add(1)
			return fmt.Errorf("publish field %q: %w", field, err)
		}
	}

	sumTopic := p.sensorFieldTopic(topicID, sumJSONSubtopic)
	if err := p.publishRaw(ctx, sumTopic, sd, qos, defaultRetain); err != nil {
		p.storePending(sd)
		p.publishFailed.Add(1)
		return fmt.Errorf("publish sumJson: %w", err)
	}

	p.published.Add(1)
	p.clearPending(topicID)
	log.Debug("published sensor data", "id", sd.Id, "hwid", sd.Hwid, "fields", len(fields))
	return nil
}

func (p *Publisher) PublishGatewayInfo(ctx context.Context, info *GatewayInfo) error {
	if info == nil {
		return nil
	}
	return p.publishRaw(ctx, p.opts.gatewayInfoTopic(), info, p.opts.mqttQoS(), true)
}

func (p *Publisher) PublishCmdResult(ctx context.Context, result CmdResult) {
	if result.ReceivedAt == "" {
		result.ReceivedAt = time.Now().Format(time.RFC3339Nano)
	}
	if err := p.publishRaw(ctx, p.opts.cmdResultTopic(), result, p.opts.mqttQoS(), false); err != nil {
		log.Debug("failed to publish cmd result", "err", err)
	}
}

func (p *Publisher) publishRaw(ctx context.Context, topic string, payload any, qos byte, retain bool) error {
	if p.cm == nil {
		return fmt.Errorf("mqtt client not connected")
	}
	var data []byte
	switch value := payload.(type) {
	case string:
		data = []byte(value)
	case []byte:
		data = value
	default:
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		data = encoded
	}
	pubCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := p.cm.Publish(pubCtx, &paho.Publish{
		Topic: topic, QoS: qos, Retain: retain, Payload: data,
	})
	if err != nil {
		return fmt.Errorf("publish to %q: %w", topic, err)
	}
	return nil
}

func (p *Publisher) storePending(sd *SensorData) {
	ttl := p.opts.pendingTTL()
	if ttl <= 0 {
		return
	}
	safe := sd.StripEventFields()
	if safe.Temperature == nil && safe.Humidity == nil && safe.Bat == "" && len(safe.Extra) == 0 {
		return
	}
	key := safe.TopicID()
	p.pendingMu.Lock()
	p.pending[key] = pendingSensor{sd: safe, storedAt: time.Now()}
	p.pendingMu.Unlock()
	log.Debug("buffered latest sensor value while mqtt down", "hwid", key, "ttl", ttl)
}

func (p *Publisher) clearPending(key string) {
	p.pendingMu.Lock()
	delete(p.pending, key)
	p.pendingMu.Unlock()
}

func (p *Publisher) flushPending(ctx context.Context) {
	ttl := p.opts.pendingTTL()
	if ttl <= 0 {
		return
	}
	p.pendingMu.Lock()
	items := make([]pendingSensor, 0, len(p.pending))
	now := time.Now()
	for key, item := range p.pending {
		if now.Sub(item.storedAt) > ttl {
			delete(p.pending, key)
			p.pendingDropped.Add(1)
			continue
		}
		items = append(items, item)
		delete(p.pending, key)
	}
	p.pendingMu.Unlock()
	for _, item := range items {
		if err := p.PublishSensor(ctx, item.sd); err != nil {
			log.Warn("failed to flush pending sensor", "hwid", item.sd.TopicID(), "err", err)
			continue
		}
		p.pendingFlushed.Add(1)
		log.Info("flushed pending sensor after reconnect", "hwid", item.sd.TopicID())
	}
}

func (p *Publisher) SubscribeCommands(handler func(payload string)) error {
	p.cmdMu.Lock()
	p.cmdHandler = handler
	p.cmdMu.Unlock()
	if p.cm != nil {
		ctx, cancel := context.WithTimeout(p.runCtx, 10*time.Second)
		defer cancel()
		if err := p.cm.AwaitConnection(ctx); err == nil {
			return p.resubscribeCommands(p.cm)
		}
		log.Info("mqtt command subscription deferred until connected", "topic", p.opts.Publish.CmdTopic)
	}
	return nil
}

func (p *Publisher) resubscribeCommands(cm *autopaho.ConnectionManager) error {
	p.cmdMu.Lock()
	handler := p.cmdHandler
	p.cmdMu.Unlock()
	if handler == nil && !p.opts.Publish.HomeAssistantDiscovery {
		return nil
	}

	ctx, cancel := context.WithTimeout(p.runCtx, 10*time.Second)
	defer cancel()

	var subs []paho.SubscribeOptions
	if handler != nil {
		subs = append(subs, paho.SubscribeOptions{Topic: p.opts.Publish.CmdTopic, QoS: p.opts.mqttQoS()})
	}
	if p.opts.Publish.HomeAssistantDiscovery {
		subs = append(subs, paho.SubscribeOptions{Topic: p.opts.haRediscoverTopic(), QoS: p.opts.mqttQoS()})
	}
	if len(subs) == 0 {
		return nil
	}
	_, err := cm.Subscribe(ctx, &paho.Subscribe{Subscriptions: subs})
	if err != nil {
		return fmt.Errorf("subscribe commands/rediscover: %w", err)
	}
	if handler != nil {
		log.Info("subscribed to rflink command topic", "topic", p.opts.Publish.CmdTopic)
	}
	if p.opts.Publish.HomeAssistantDiscovery {
		log.Info("subscribed to HA rediscover topic", "topic", p.opts.haRediscoverTopic())
	}
	return nil
}

func (p *Publisher) RunPublishLoop(ctx context.Context, serialCh <-chan string) {
	for ctx.Err() == nil {
		p.runPublishLoopInner(ctx, serialCh)
	}
}

func (p *Publisher) runPublishLoopInner(ctx context.Context, serialCh <-chan string) {
	defer func() {
		if recovered := recover(); recovered != nil {
			p.panics.Add(1)
			log.Error("publish loop panic recovered", "panic", recovered)
		}
	}()

	var healthTicker *time.Ticker
	var healthCh <-chan time.Time
	if interval := p.opts.healthLogInterval(); interval > 0 {
		healthTicker = time.NewTicker(interval)
		healthCh = healthTicker.C
		defer healthTicker.Stop()
	}

	var watchdogTicker *time.Ticker
	var watchdogCh <-chan time.Time
	if wd := p.opts.serialWatchdog(); wd > 0 {
		tick := max(wd/4, 10*time.Second)
		watchdogTicker = time.NewTicker(tick)
		watchdogCh = watchdogTicker.C
		defer watchdogTicker.Stop()
	}

	// Ping timeout checks even when data watchdog is disabled.
	var pingCheckTicker *time.Ticker
	var pingCheckCh <-chan time.Time
	if p.opts.serialPingInterval() > 0 {
		pt := max(p.opts.serialPingTimeout(), time.Second)
		pingCheckTicker = time.NewTicker(pt)
		pingCheckCh = pingCheckTicker.C
		defer pingCheckTicker.Stop()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-healthCh:
			p.reportHealth(ctx)
		case <-watchdogCh:
			p.checkSerialWatchdog()
			p.checkPingTimeout()
		case <-pingCheckCh:
			p.checkPingTimeout()
		case msg, ok := <-serialCh:
			if !ok {
				return
			}
			p.NoteSerialActivity()
			p.handleSerialMessage(ctx, msg)
		}
	}
}

func (p *Publisher) handleSerialMessage(ctx context.Context, msg string) {
	if p.isDuplicate(msg) {
		return
	}
	if info, err := ParseGatewayMessage(msg); err == nil && info != nil {
		if info.Pong {
			p.NotePong()
		}
		if err := p.PublishGatewayInfo(ctx, info); err != nil {
			log.Warn("failed to publish gateway info", "err", err)
		} else {
			log.Info("gateway info published", "version", info.Version, "pong", info.Pong)
		}
		return
	}
	sd, err := SensorDataFromMessage(msg)
	if err != nil {
		log.Debug("skipped non-sensor rflink message", "message", msg, "err", err)
		return
	}
	if err := p.PublishSensor(ctx, sd); err != nil {
		log.Warn("failed to publish sensor data", "id", sd.Id, "hwid", sd.Hwid, "err", err)
	}
}

func (p *Publisher) SetSerialConnected(ctx context.Context, connected bool) {
	p.serialUp.Store(connected)
	status := statusOffline
	if connected {
		status = statusOnline
		p.NoteSerialActivity()
	}
	if err := p.publishRaw(ctx, p.opts.serialStatusTopic(), status, p.opts.mqttQoS(), true); err != nil {
		log.Debug("failed to publish serial status", "err", err)
	}
}

func (p *Publisher) NoteSerialActivity() {
	p.lastSerialAt.Store(time.Now())
}

func (p *Publisher) IncSerialDropped()   { p.serialDropped.Add(1) }
func (p *Publisher) IncCommandRejected() { p.commandsRejected.Add(1) }
func (p *Publisher) IncCommandRateLimited() {
	p.commandsLimited.Add(1)
}

func (p *Publisher) checkSerialWatchdog() {
	wd := p.opts.serialWatchdog()
	if wd <= 0 {
		return
	}
	t, ok := p.lastSerialAt.Load().(time.Time)
	if !ok || t.IsZero() {
		if time.Since(p.startedAt) > wd {
			log.Warn("serial watchdog: no data received since start", "watchdog", wd, "uptime", time.Since(p.startedAt).Round(time.Second))
		}
		return
	}
	if age := time.Since(t); age > wd {
		log.Warn("serial watchdog: no data from serial port", "last_serial_at", t.Format(time.RFC3339), "age", age.Round(time.Second), "watchdog", wd)
	}
}

func (p *Publisher) reportHealth(ctx context.Context) {
	snap := p.snapshot()
	log.Info("health",
		"version", snap.Version,
		"git_sha", snap.GitSHA,
		"status", snap.Status,
		"published", snap.Published,
		"publish_failed", snap.PublishFailed,
		"serial_dropped", snap.SerialDropped,
		"dedup_dropped", snap.DedupDropped,
		"panics", snap.Panics,
		"serial_stale", snap.SerialStale,
		"serial_connected", snap.SerialConnected,
		"rflink_unresponsive", snap.RFLinkUnresponsive,
		"last_pong_at", snap.LastPongAt,
		"ping_fails", snap.PingFails,
		"mqtt_connected", snap.MQTTConnected,
	)
	if err := p.publishRaw(ctx, p.opts.healthTopic(), snap, p.opts.mqttQoS(), true); err != nil {
		log.Debug("failed to publish health", "err", err)
	}
}

func (p *Publisher) snapshot() healthSnapshot {
	p.pendingMu.Lock()
	pendingCount := len(p.pending)
	p.pendingMu.Unlock()

	status := statusOffline
	if p.mqttUp.Load() {
		status = statusOnline
	}

	var lastAtStr, lastHWID, lastSerialStr string
	if t, ok := p.lastSensorAt.Load().(time.Time); ok && !t.IsZero() {
		lastAtStr = t.Format(time.RFC3339)
	}
	if h, ok := p.lastSensorHWID.Load().(string); ok {
		lastHWID = h
	}
	serialStale := false
	if t, ok := p.lastSerialAt.Load().(time.Time); ok && !t.IsZero() {
		lastSerialStr = t.Format(time.RFC3339)
		if wd := p.opts.serialWatchdog(); wd > 0 && time.Since(t) > wd {
			serialStale = true
		}
	} else if wd := p.opts.serialWatchdog(); wd > 0 && time.Since(p.startedAt) > wd {
		serialStale = true
	}

	p.sensorSeenMu.Lock()
	sensors := make(map[string]string, len(p.sensorSeen))
	for hwid, t := range p.sensorSeen {
		sensors[hwid] = t.Format(time.RFC3339)
	}
	p.sensorSeenMu.Unlock()

	return healthSnapshot{
		Version: Version, GitSHA: GitSHA, StartedAt: p.startedAt.Format(time.RFC3339),
		Status: status, Published: p.published.Load(), PublishFailed: p.publishFailed.Load(),
		PendingFlushed: p.pendingFlushed.Load(), PendingDropped: p.pendingDropped.Load(),
		PendingBuffered: pendingCount, SerialDropped: p.serialDropped.Load(),
		DedupDropped: p.dedupDropped.Load(), Panics: p.panics.Load(),
		CommandsRejected: p.commandsRejected.Load(), CommandsLimited: p.commandsLimited.Load(),
		LastSensorAt: lastAtStr, LastSensorHWID: lastHWID, LastSerialAt: lastSerialStr,
		SerialStale: serialStale, SerialConnected: p.serialUp.Load(),
		RFLinkUnresponsive: p.rflinkUnresp.Load(),
		LastPongAt: func() string {
			p.pingMu.Lock()
			defer p.pingMu.Unlock()
			if p.lastPongAt.IsZero() {
				return ""
			}
			return p.lastPongAt.Format(time.RFC3339)
		}(),
		PingFails: p.pingFails.Load(),
		UptimeSec: int64(time.Since(p.startedAt).Seconds()), MQTTConnected: p.mqttUp.Load(),
		SensorsLastSeen: sensors,
	}
}

// MarkPingSent records that 10;PING; was written to serial.
func (p *Publisher) MarkPingSent() {
	p.pingMu.Lock()
	p.pingSentAt = time.Now()
	p.pingMu.Unlock()
}

// NotePong records a successful PONG from RFLink.
func (p *Publisher) NotePong() {
	p.pingMu.Lock()
	p.lastPongAt = time.Now()
	p.pingSentAt = time.Time{}
	p.pingMu.Unlock()
	p.pingFails.Store(0)
	if p.rflinkUnresp.Swap(false) {
		log.Info("RFLink responded to PING again")
	}
}

func (p *Publisher) checkPingTimeout() {
	if p.opts.serialPingInterval() <= 0 {
		return
	}
	timeout := p.opts.serialPingTimeout()
	p.pingMu.Lock()
	sent := p.pingSentAt
	p.pingMu.Unlock()
	if sent.IsZero() {
		return
	}
	if time.Since(sent) < timeout {
		return
	}
	// timed out — clear pending so next interval can send again
	p.pingMu.Lock()
	p.pingSentAt = time.Time{}
	p.pingMu.Unlock()

	fails := p.pingFails.Add(1)
	log.Warn("RFLink PING timeout, no PONG received",
		"timeout", timeout,
		"consecutive_fails", fails,
	)
	if int(fails) >= p.opts.serialPingFailThreshold() {
		if !p.rflinkUnresp.Swap(true) {
			log.Error("RFLink marked unresponsive after consecutive PING failures",
				"fails", fails,
				"threshold", p.opts.serialPingFailThreshold(),
			)
		}
	}
}

// PingOutstanding reports whether a PING is waiting for PONG (avoid stacking pings).
func (p *Publisher) PingOutstanding() bool {
	p.pingMu.Lock()
	defer p.pingMu.Unlock()
	return !p.pingSentAt.IsZero()
}

func (p *Publisher) publishHAActuators(ctx context.Context) {
	if !p.opts.Publish.HomeAssistantDiscovery {
		return
	}
	p.publishHAButtons(ctx)
	p.publishHASwitches(ctx)
}

func (p *Publisher) publishHAButtons(ctx context.Context) {
	prefix := p.opts.Publish.HADiscoveryPrefix
	cmdTopic := p.opts.Publish.CmdTopic
	for i, btn := range p.opts.haButtons {
		objectID := fmt.Sprintf("gorflink_btn_%d", i)
		uniqueID := objectID
		if _, loaded := p.discovered.LoadOrStore(uniqueID, true); loaded {
			continue
		}
		payload := map[string]any{
			"name":                  btn.Name,
			"unique_id":             uniqueID,
			"command_topic":         cmdTopic,
			"payload_press":         btn.Command,
			"availability_topic":    p.opts.statusTopic(),
			"payload_available":     statusOnline,
			"payload_not_available": statusOffline,
			"device": map[string]any{
				"identifiers":  []string{"gorflink_gateway"},
				"name":         "RFLink Gateway",
				"manufacturer": "RFLink",
				"model":        "go-rflink",
			},
		}
		topic := fmt.Sprintf("%s/button/%s/config", prefix, objectID)
		if err := p.publishRaw(ctx, topic, payload, p.opts.mqttQoS(), true); err != nil {
			p.discovered.Delete(uniqueID)
			log.Error("HA button discovery failed", "name", btn.Name, "err", err)
			continue
		}
		log.Info("HA button discovered", "name", btn.Name, "command", btn.Command, "topic", topic)
	}
}

func (p *Publisher) publishHASwitches(ctx context.Context) {
	prefix := p.opts.Publish.HADiscoveryPrefix
	cmdTopic := p.opts.Publish.CmdTopic
	for i, sw := range p.opts.haSwitches {
		objectID := fmt.Sprintf("gorflink_sw_%d", i)
		uniqueID := objectID
		if _, loaded := p.discovered.LoadOrStore(uniqueID, true); loaded {
			continue
		}
		// Optimistic switch: no state_topic from RFLink; HA assumes success.
		payload := map[string]any{
			"name":                  sw.Name,
			"unique_id":             uniqueID,
			"command_topic":         cmdTopic,
			"payload_on":            sw.CommandOn,
			"payload_off":           sw.CommandOff,
			"optimistic":            true,
			"availability_topic":    p.opts.statusTopic(),
			"payload_available":     statusOnline,
			"payload_not_available": statusOffline,
			"device": map[string]any{
				"identifiers":  []string{"gorflink_gateway"},
				"name":         "RFLink Gateway",
				"manufacturer": "RFLink",
				"model":        "go-rflink",
			},
		}
		topic := fmt.Sprintf("%s/switch/%s/config", prefix, objectID)
		if err := p.publishRaw(ctx, topic, payload, p.opts.mqttQoS(), true); err != nil {
			p.discovered.Delete(uniqueID)
			log.Error("HA switch discovery failed", "name", sw.Name, "err", err)
			continue
		}
		log.Info("HA switch discovered", "name", sw.Name, "on", sw.CommandOn, "off", sw.CommandOff, "topic", topic)
	}
}

func (p *Publisher) noteSensorSeen(hwid string, t time.Time) {

	if hwid == "" {
		return
	}
	p.sensorSeenMu.Lock()
	p.sensorSeen[hwid] = t
	p.sensorSeenMu.Unlock()
}

// ClearHADiscovery drops the discovery dedup cache so entities are re-published on next sensor data.
// Actuator button/switch discovery is republished immediately.
func (p *Publisher) ClearHADiscovery() {
	count := 0
	p.discovered.Range(func(key, _ any) bool {
		p.discovered.Delete(key)
		count++
		return true
	})
	log.Info("HA discovery cache cleared; entities will be rediscovered on next sensor messages",
		"cleared", count,
	)
	p.publishHAActuators(p.runCtx)
}

func (p *Publisher) Disconnect() {

	if p.cm != nil && p.mqttUp.Load() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = p.publishRaw(ctx, p.opts.statusTopic(), statusOffline, p.opts.mqttQoS(), true)
		_ = p.publishRaw(ctx, p.opts.serialStatusTopic(), statusOffline, p.opts.mqttQoS(), true)
		cancel()
	}
	if p.cancel != nil {
		p.cancel()
	}
	if p.cm == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.cm.Disconnect(ctx); err != nil {
		log.Debug("mqtt disconnect error", "err", err)
	}
	select {
	case <-p.cm.Done():
	case <-time.After(3 * time.Second):
		log.Debug("mqtt connection manager shutdown timed out")
	}
}
