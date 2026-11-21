/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"sort"
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
	statusOnline      = "online"
	statusOffline     = "offline"
	maxPendingSensors = 256
	maxSensorSeen     = 512
	maxDedupEntries   = 500
	// serialStatusTTL is how long a retained serial "online" stays if this
	// process stops refreshing it. A hard kill has no will on those topics.
	serialStatusTTL = 90 * time.Second
	// serialStatusRefresh republishes serial status before the expiry.
	serialStatusRefresh = 30 * time.Second
)

// errSensorThrottled means the frame was not published. A later copy from
// the radio must still be allowed once the interval has passed.
var errSensorThrottled = errors.New("sensor throttled")

type pendingSensor struct {
	sd       *SensorData
	storedAt time.Time
}

// CmdResult is published to {topic}/cmd/result after an actuator command attempt.
type CmdResult struct {
	Command            string      `json:"command"`
	Status             string      `json:"status"`
	Error              string      `json:"error,omitempty"`
	ReceivedAt         string      `json:"received_at"`
	RateLimitRemaining float64     `json:"rate_limit_remaining,omitempty"`
	RateLimitPerSec    int         `json:"rate_limit_per_sec,omitempty"`
	Ports              []PortWrite `json:"ports,omitempty"`
}

// PortWrite is the result of one gateway write inside a command.
type PortWrite struct {
	Device string `json:"device"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// SerialPortStatus is one configured gateway.
type SerialPortStatus struct {
	Device    string `json:"device"`
	Connected bool   `json:"connected"`
	Dropped   uint64 `json:"dropped,omitempty"`
	Version   string `json:"version,omitempty"`
	Queued    int    `json:"queued"`
	QueueCap  int    `json:"queue_cap"`
}

type HealthSnapshot struct {
	Version            string             `json:"version"`
	GitSHA             string             `json:"git_sha"`
	StartedAt          string             `json:"started_at"`
	Status             string             `json:"status"`
	Published          uint64             `json:"published"`
	PublishFailed      uint64             `json:"publish_failed"`
	PendingFlushed     uint64             `json:"pending_flushed"`
	PendingDropped     uint64             `json:"pending_dropped"`
	PendingBuffered    int                `json:"pending_buffered"`
	SerialDropped      uint64             `json:"serial_dropped"`
	DedupDropped       uint64             `json:"dedup_dropped"`
	ThrottleDropped    uint64             `json:"throttle_dropped"`
	Panics             uint64             `json:"panics"`
	CommandsRejected   uint64             `json:"commands_rejected"`
	CommandsLimited    uint64             `json:"commands_rate_limited"`
	LastSensorAt       string             `json:"last_sensor_at,omitempty"`
	LastSensorHWID     string             `json:"last_sensor_hwid,omitempty"`
	LastSerialAt       string             `json:"last_serial_at,omitempty"`
	SerialStale        bool               `json:"serial_stale"`
	SerialConnected    bool               `json:"serial_connected"`
	SerialPorts        []SerialPortStatus `json:"serial_ports,omitempty"`
	RFLinkUnresponsive bool               `json:"rflink_unresponsive"`
	LastPongAt         string             `json:"last_pong_at,omitempty"`
	PingFails          uint64             `json:"ping_fails"`
	UptimeSec          int64              `json:"uptime_sec"`
	MQTTConnected      bool               `json:"mqtt_connected"`
	SensorsLastSeen    map[string]string  `json:"sensors_last_seen,omitempty"`
}

// Publisher publishes SensorData to MQTT subtopics and optional Home Assistant discovery topics.
type Publisher struct {
	cm   *autopaho.ConnectionManager
	opts *Options
	// testPublish replaces the broker write in tests. Production leaves it nil.
	testPublish func(topic string, payload []byte) error
	discovered  sync.Map
	cmdHandler  func(string)
	cmdMu       sync.Mutex

	cancel     context.CancelFunc
	mqttCancel context.CancelFunc
	runCtx     context.Context

	publishMu sync.Mutex
	pendingMu sync.Mutex
	pending   map[string]pendingSensor

	dedupMu   sync.Mutex
	dedupLast map[string]time.Time

	sensorSeenMu sync.Mutex
	sensorSeen   map[string]time.Time // HWID -> last successful publish time
	sensorHeard  map[string]time.Time // HWID -> last radio frame, including throttled ones
	sensorPort   map[string]string    // HWID -> gateway that currently owns it
	sensorPortAt map[string]time.Time // HWID -> last frame from that owner
	availMu      sync.Mutex
	availDown    map[string]struct{} // HWID already marked offline on its availability topic
	availMiss    map[string]struct{} // evicted HWIDs whose offline publish still has to land
	availOnline  map[string]struct{} // sensors whose "online" availability publish failed

	haMu           sync.Mutex
	haPrefix       string
	discoveryEpoch atomic.Uint64
	haBtnN         int
	haSwN          int

	published        atomic.Uint64
	publishFailed    atomic.Uint64
	pendingFlushed   atomic.Uint64
	pendingDropped   atomic.Uint64
	serialDropped    atomic.Uint64
	dedupDropped     atomic.Uint64
	throttleDropped  atomic.Uint64
	panics           atomic.Uint64
	commandsRejected atomic.Uint64
	commandsLimited  atomic.Uint64
	lastSensorAt     atomic.Value
	lastSensorHWID   atomic.Value
	lastSerialAt     atomic.Value
	startedAt        time.Time
	mqttUp           atomic.Bool
	gatewayInfoMode  atomic.Int32 // 1: per-port info cleared, 2: shared info cleared
	gatewayInfoMu    sync.Mutex
	// gatewayTopics are retained info topics this process published. The value
	// bumps on every successful publish so a clear that raced a newer payload
	// leaves the topic to be retried instead of forgetting it.
	gatewayTopics map[string]uint64
	serialUp      atomic.Bool
	serialMu      sync.Mutex
	serialState   map[string]bool
	serialPorts   []SerialPortStatus
	portDrops     map[string]uint64
	gatewayVer    map[string]string
	queues        *serialIngress
	serialHeard   map[string]time.Time // last line from that gateway
	serialHold    map[string]time.Time // watchdog will not restart this gateway again until then

	pingMu       sync.Mutex
	pingSentAt   time.Time // zero = no outstanding ping
	pingWrite    bool      // Write of 10;PING; has not returned yet
	pingGen      uint64    // bumps on PONG and reset so a late timeout does not reopen the port
	lastPongAt   time.Time
	pingFails    atomic.Uint64 // consecutive missed PONGs on the primary gateway
	rflinkUnresp atomic.Bool
	extraPing    map[string]*extraPingState
	extraRestart map[string]chan struct{}
	// serialRestart asks the serial loop to finish the current Read and reopen.
	// Buffered so a signal raised between sessions is not lost.
	serialRestart chan struct{}

	// HTTP debug buffers (ring)
	rawMu     sync.Mutex
	rawLines  []rawLineEntry
	rawCap    int
	sampleMu  sync.Mutex
	samples   []sensorSample
	sampleCap int

	rawHookMu    sync.Mutex
	rawHook      func(rawLineEntry) // optional live stream (WebSocket hub)
	sensorHookMu sync.Mutex
	sensorHook   func(hwid string, sumJSON []byte)
}

type extraPingState struct {
	sentAt       time.Time
	writing      bool
	fails        uint64
	gen          uint64
	unresponsive bool
}

type rawLineEntry struct {
	At      string `json:"at"`
	Device  string `json:"device,omitempty"`
	Message string `json:"message"`
}

type sensorSample struct {
	At     string            `json:"at"`
	HWID   string            `json:"hwid"`
	Model  string            `json:"model,omitempty"`
	ID     string            `json:"id,omitempty"`
	Name   string            `json:"name,omitempty"`
	Fields map[string]string `json:"fields,omitempty"`
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
	// MQTT lifetime is not the app context. App.Stop cancels the parent before
	// Disconnect; tying the client to it sends a clean DISCONNECT and the broker
	// then keeps the retained status as "online" (LWT is not published).
	mqttCtx, mqttCancel := context.WithCancel(context.Background())
	rawCap := o.HTTP.RawBufferSize
	if rawCap <= 0 {
		rawCap = 100
	}
	sampleCap := o.HTTP.SensorBufferSize
	if sampleCap <= 0 {
		sampleCap = 50
	}
	p := &Publisher{
		opts:          o,
		cancel:        cancel,
		mqttCancel:    mqttCancel,
		runCtx:        ctx,
		pending:       make(map[string]pendingSensor),
		dedupLast:     make(map[string]time.Time),
		sensorSeen:    make(map[string]time.Time),
		startedAt:     time.Now(),
		rawLines:      make([]rawLineEntry, 0, rawCap),
		rawCap:        rawCap,
		samples:       make([]sensorSample, 0, sampleCap),
		sampleCap:     sampleCap,
		serialRestart: make(chan struct{}, 1),
	}
	p.lastSensorAt.Store(time.Time{})
	p.lastSensorHWID.Store("")
	p.lastSerialAt.Store(time.Time{})

	log.Info("mqtt client using MQTT v5 only (paho.golang/autopaho)")

	if o.Publish.MqttUsername == "username" && o.Publish.MqttPassword == "password" {
		log.Warn("MQTT is using the default username and password — set PUBLISH_MQTT_USERNAME and PUBLISH_MQTT_PASSWORD")
	}
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
		AttemptConnection:             mqttAttemptConnection,
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
			p.publishSerialStatus(ctx)
			p.publishHealth(ctx)
			p.clearAlternateGatewayInfo(ctx)
			if err := p.resubscribeCommands(cm); err != nil {
				log.Error("mqtt command resubscribe failed", "err", err)
			}
			p.markDiscoveryStale()
			p.publishHAActuators(ctx)
			p.flushPending(ctx)
			p.retryMissedAvailability(ctx)
			p.retryAvailOnline(ctx)
		},
		OnConnectError: func(err error) {
			p.mqttUp.Store(false)
			log.Warn("mqtt connect error", "err", err)
		},
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
			func(pr paho.PublishReceived) (handled bool, err error) {
				defer func() {
					if rec := recover(); rec != nil {
						p.panics.Add(1)
						log.Error("mqtt message handler panic recovered", "panic", rec)
						handled, err = true, nil
					}
				}()
				if pr.Packet == nil {
					return true, nil
				}
				if pr.Packet.Retain {
					log.Debug("ignoring retained mqtt message", "topic", pr.Packet.Topic)
					return true, nil
				}
				topic := pr.Packet.Topic
				payload := string(pr.Packet.Payload)
				if topic == p.opts.haRediscoverTopic() {
					haOn, _, _, _ := p.opts.snapshotHAActuators()
					if haOn {
						p.ClearHADiscovery()
					}
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
	}

	if o.Publish.InfinityReconnect {
		log.Info("mqtt connect retry enabled", "interval", o.mqttConnectRetryInterval())
	}

	cm, err := autopaho.NewConnection(mqttCtx, cliCfg)
	if err != nil {
		cancel()
		mqttCancel()
		return nil, fmt.Errorf("mqtt new connection: %w", err)
	}
	p.cm = cm

	if !o.Publish.InfinityReconnect {
		waitCtx, waitCancel := context.WithTimeout(ctx, 30*time.Second)
		defer waitCancel()
		if err := cm.AwaitConnection(waitCtx); err != nil {
			cancel()
			mqttCancel()
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
	u, err := url.Parse(encodeZonePercent(raw))
	if err != nil {
		return nil, fmt.Errorf("parse mqtt broker url %q: %w", raw, err)
	}
	if host, port := u.Hostname(), u.Port(); host != "" && port != "" {
		// Hostname unescapes a zone id. JoinHostPort puts back the brackets
		// and the literal percent that net.Dial accepts.
		u.Host = net.JoinHostPort(host, port)
	}
	return u, nil
}

func buildTLSConfig(o *Options) (*tls.Config, error) {
	if !o.isTLSScheme() {
		if o.Publish.TLSCAFile != "" || o.Publish.TLSInsecureSkipVerify || o.Publish.TLSCertFile != "" || o.Publish.TLSKeyFile != "" {
			log.Warn("TLS options ignored because MQTT scheme is not TLS", "scheme", o.Publish.Scheme)
		}
		return nil, nil
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: mqttServerName(o.Publish.Host)}
	if o.Publish.TLSInsecureSkipVerify {
		tlsConfig.InsecureSkipVerify = true
	}
	if (o.Publish.TLSCertFile == "") != (o.Publish.TLSKeyFile == "") {
		return nil, fmt.Errorf("PUBLISH_TLS_CERT_FILE and PUBLISH_TLS_KEY_FILE must both be set")
	}
	if o.Publish.TLSCertFile != "" {
		cert, err := tls.LoadX509KeyPair(o.Publish.TLSCertFile, o.Publish.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load mqtt client certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
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

func mqttServerName(hostport string) string {
	hostport = strings.TrimSpace(hostport)
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		hostport = host
	}
	return strings.Trim(hostport, "[]")
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
	if sd.RawHWID != "" && !strings.EqualFold(sd.RawHWID, sd.TopicID()) {
		if name := p.opts.FriendlyNameFor(sd.RawHWID); name != "" {
			sd.FriendlyName = name
			return
		}
	}
	// Default device label for HA / sumJson when no map entry is configured.
	sd.FriendlyName = fmt.Sprintf("goRFLink_%s %s", sd.Model, sd.Id)
}

// dedupKey strips the RFLink sequence counter (2nd field) so radio retransmits
// like 20;AC;EV1527;… and 20;AD;EV1527;… map to the same key.
// The counter is one or two hex digits. A payload word (20;PONG;) or an ID
// field is left in place, so those lines do not collapse onto each other.
func dedupKey(msg string) string {
	msg = strings.TrimSpace(msg)
	pieces := strings.Split(msg, ";")
	if len(pieces) >= 4 && pieces[0] == "20" && isRFLinkSeq(pieces[1]) && pieces[2] != "" {
		// 20;SEQ;PROTO;fields… → 20;PROTO;fields…
		return strings.Join(append([]string{pieces[0]}, pieces[2:]...), ";")
	}
	return msg
}

func isRFLinkSeq(s string) bool {
	if len(s) == 0 || len(s) > 2 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

func (p *Publisher) isDuplicate(msg string) bool {
	return p.duplicate(dedupKey(msg), msg, true)
}

// isDuplicateFrom keeps gateway replies separate per board. Two ports that
// answer VER or PONG with the same text both stay visible.
func (p *Publisher) isDuplicateFrom(device, msg string) bool {
	key := dedupKey(msg)
	device = strings.TrimSpace(device)
	if device != "" {
		key = device + "\x00" + key
	}
	return p.duplicate(key, msg, true)
}

func (p *Publisher) duplicate(key, msg string, count bool) bool {
	window := p.opts.dedupWindow()
	if window <= 0 {
		return false
	}
	now := time.Now()
	p.dedupMu.Lock()
	defer p.dedupMu.Unlock()
	if p.dedupLast == nil {
		p.dedupLast = map[string]time.Time{}
	}
	if last, ok := p.dedupLast[key]; ok {
		// A backward clock step leaves the stamp in the future. Until the
		// clock catches it, every frame would look like a duplicate.
		if last.After(now) {
			delete(p.dedupLast, key)
		} else if now.Sub(last) < window {
			if count {
				p.dedupDropped.Add(1)
				log.Warn("debounce: dropped duplicate rflink message",
					"key", key,
					"message", msg,
					"age_ms", now.Sub(last).Milliseconds(),
					"window_ms", window.Milliseconds(),
				)
			}
			return true
		}
	}
	p.evictDedupLocked(now, window)
	return false
}

func (p *Publisher) evictDedupLocked(now time.Time, window time.Duration) {
	if window <= 0 {
		return
	}
	cutoff := now.Add(-2 * window)
	for key, seen := range p.dedupLast {
		if seen.After(now) {
			// A future stamp sorts as the newest entry, so the cap would
			// evict the line that was just published.
			delete(p.dedupLast, key)
			continue
		}
		if seen.Before(cutoff) {
			delete(p.dedupLast, key)
		}
	}
	for len(p.dedupLast) > maxDedupEntries {
		oldestKey := ""
		var oldest time.Time
		for key, seen := range p.dedupLast {
			if oldestKey == "" || seen.Before(oldest) {
				oldestKey = key
				oldest = seen
			}
		}
		if oldestKey == "" {
			break
		}
		delete(p.dedupLast, oldestKey)
	}
}

// markDedup records a message only after it was published, so a failed attempt
// can still be retried by the radio's own retransmission.
func (p *Publisher) markDedup(msg string) {
	p.rememberDedup(dedupKey(msg))
}

func (p *Publisher) markDedupFrom(device, msg string) {
	key := dedupKey(msg)
	device = strings.TrimSpace(device)
	if device != "" {
		key = device + "\x00" + key
	}
	p.rememberDedup(key)
}

func (p *Publisher) rememberDedup(key string) {
	if p == nil || p.opts == nil || p.opts.dedupWindow() <= 0 {
		return
	}
	p.dedupMu.Lock()
	now := time.Now()
	if p.dedupLast == nil {
		p.dedupLast = map[string]time.Time{}
	}
	p.dedupLast[key] = now
	p.evictDedupLocked(now, p.opts.dedupWindow())
	p.dedupMu.Unlock()
}

func (p *Publisher) forgetDedup(msg string) {
	if p == nil || p.dedupLast == nil {
		return
	}
	p.dedupMu.Lock()
	delete(p.dedupLast, dedupKey(msg))
	p.dedupMu.Unlock()
}

// publishTakeoverDiscovery refreshes Home Assistant availability after the
// owner moved but the sensor values themselves were throttled.
func (p *Publisher) publishTakeoverDiscovery(ctx context.Context, sd *SensorData) {
	if p == nil || sd == nil || p.opts == nil {
		return
	}
	p.publishMu.Lock()
	defer p.publishMu.Unlock()
	haOn, haPrefix, _, _ := p.opts.snapshotHAActuators()
	if !haOn {
		return
	}
	p.publishHomeAssistantDiscovery(ctx, sd, sd.FieldsForPublish(p.opts.temperatureUnit()), haPrefix)
}

func (p *Publisher) PublishSensor(ctx context.Context, sd *SensorData) error {
	p.publishMu.Lock()
	defer p.publishMu.Unlock()
	return p.publishSensorLocked(ctx, sd)
}

func (p *Publisher) publishSensorLocked(ctx context.Context, sd *SensorData) error {
	if p.cm == nil && p.testPublish == nil {
		return fmt.Errorf("mqtt client not connected")
	}
	sd.ApplyCanonicalHWID(p.opts)
	if p.opts.SensorIgnored(sd.Model, sd.Hwid) || p.opts.SensorIgnored(sd.Model, sd.RawHWID) || p.opts.SensorIgnored(sd.Model, sd.Id) {
		log.Info("ignored sensor by ignore-list", "model", sd.Model, "hwid", sd.Hwid, "id", sd.Id)
		return nil
	}
	p.applyFriendlyName(sd)
	now := time.Now()
	topicID := sd.TopicID()
	if p.throttled(topicID, now) {
		p.throttleDropped.Add(1)
		// A newer reading must replace a snapshot that has not reached the
		// broker yet. The original age stays, so the buffer still expires.
		p.keepNewerPending(sd)
		log.Debug("sensor publish throttled", "hwid", topicID)
		return errSensorThrottled
	}
	p.lastSensorAt.Store(now)
	p.lastSensorHWID.Store(topicID)

	fields := sd.FieldsForPublish(p.opts.temperatureUnit())
	if len(fields) == 0 {
		log.Info("sensor message has no publishable fields", "id", sd.Id, "hwid", sd.Hwid, "model", sd.Model)
	}
	haOn, haPrefix, _, _ := p.opts.snapshotHAActuators()
	if haOn {
		p.publishHomeAssistantDiscovery(ctx, sd, fields, haPrefix)
	}

	qos := p.opts.mqttQoS()
	defaultRetain := p.opts.Publish.Retain
	// Retained measurements go out before one-shot fields. A failure while
	// publishing temperature must not emit sw_* first: the retry would press
	// the button again, and the pending snapshot drops CMD and SWITCH.
	var retained, momentary []string
	for field, value := range fields {
		if field == "cmd" || field == "switch" || strings.HasPrefix(field, "sw_") || isMomentaryState(value) {
			momentary = append(momentary, field)
			continue
		}
		retained = append(retained, field)
	}
	sort.Strings(retained)
	sort.Strings(momentary)
	publishField := func(field string, retain bool) error {
		topic := p.sensorFieldTopic(topicID, field)
		if err := p.publishRaw(ctx, topic, fields[field], qos, retain); err != nil {
			p.storePending(sd)
			p.publishFailed.Add(1)
			return fmt.Errorf("publish field %q: %w", field, err)
		}
		return nil
	}
	for _, field := range retained {
		if err := publishField(field, defaultRetain); err != nil {
			return err
		}
	}

	sumTopic := p.sensorFieldTopic(topicID, sumJSONSubtopic)
	sumPayload := any(sd)
	if defaultRetain {
		// A retained aggregate must not pin CMD=ON after a one-shot remote frame.
		sumPayload = sd.RetainedState()
	}
	if err := p.publishRaw(ctx, sumTopic, sumPayload, qos, defaultRetain); err != nil {
		p.storePending(sd)
		p.publishFailed.Add(1)
		return fmt.Errorf("publish sumJson: %w", err)
	}
	for _, field := range momentary {
		if err := publishField(field, false); err != nil {
			return err
		}
	}

	p.published.Add(1)
	p.clearPending(topicID)
	p.noteSensorSeen(topicID, now)
	if p.opts.Publish.HASensorAvailability {
		if err := p.publishSensorAvailability(ctx, topicID, statusOnline); err != nil {
			// State is already on the broker. Remember the missed online so a
			// later tick can mark the entity available without waiting for a
			// new radio frame that dedup or throttle might drop.
			p.rememberAvailOnline(topicID)
		}
	}
	p.noteSwitchEcho(ctx, sd, fields)
	p.appendSensorSample(sd, fields)
	log.Info("published sensor data", "id", sd.Id, "hwid", sd.Hwid, "fields", len(fields))

	p.sensorHookMu.Lock()
	sh := p.sensorHook
	p.sensorHookMu.Unlock()
	if sh != nil {
		if b, err := json.Marshal(sd); err == nil {
			sh(topicID, b)
		}
	}
	return nil
}

func (p *Publisher) PublishGatewayInfo(ctx context.Context, device string, info *GatewayInfo) error {
	if info == nil || p == nil || p.opts == nil {
		return nil
	}
	device = strings.TrimSpace(device)
	if device != "" {
		info.Device = canonicalDevice(p.opts.serialDeviceList(), device)
		device = info.Device
	}
	// A later PONG is retained on the same topic. Keep the version learned from
	// 10;version; so a new subscriber still sees the firmware.
	if strings.TrimSpace(info.Version) == "" {
		if v := p.gatewayVersion(device); v != "" {
			copied := *info
			copied.Version = v
			info = &copied
		}
	}
	topic := p.opts.gatewayInfoTopicFor(info.Device)
	// Held across the broker write so a retire cannot publish an empty payload
	// between this write and the map update, or the reverse.
	p.gatewayInfoMu.Lock()
	defer p.gatewayInfoMu.Unlock()
	if err := p.publishRaw(ctx, topic, info, p.opts.mqttQoS(), true); err != nil {
		return err
	}
	if topic == "" {
		return nil
	}
	if p.gatewayTopics == nil {
		p.gatewayTopics = map[string]uint64{}
	}
	p.gatewayTopics[topic]++
	return nil
}

func (p *Publisher) liveGatewayInfoTopics(devices []string) map[string]struct{} {
	live := make(map[string]struct{}, len(devices))
	switch {
	case len(devices) > 1:
		for _, device := range devices {
			live[p.opts.gatewayDeviceInfoTopic(device)] = struct{}{}
		}
	case len(devices) == 1:
		live[p.opts.gatewayInfoTopic()] = struct{}{}
	}
	return live
}

// clearRetiredGatewayTopics drops retained info for gateways that are no longer
// configured. A failed publish stays in the map and is retried on the next tick.
func (p *Publisher) clearRetiredGatewayTopics(ctx context.Context) {
	if p == nil || p.opts == nil {
		return
	}
	p.gatewayInfoMu.Lock()
	defer p.gatewayInfoMu.Unlock()
	live := p.liveGatewayInfoTopics(p.opts.serialDeviceList())
	stale := make([]string, 0, len(p.gatewayTopics))
	for topic := range p.gatewayTopics {
		if _, ok := live[topic]; ok {
			continue
		}
		stale = append(stale, topic)
	}
	sort.Strings(stale)
	for _, topic := range stale {
		live = p.liveGatewayInfoTopics(p.opts.serialDeviceList())
		if _, ok := live[topic]; ok {
			continue
		}
		gen := p.gatewayTopics[topic]
		if err := p.publishRaw(ctx, topic, "", p.opts.mqttQoS(), true); err != nil {
			log.Debug("failed to clear retired gateway info", "topic", topic, "err", err)
			continue
		}
		if p.gatewayTopics[topic] == gen {
			delete(p.gatewayTopics, topic)
		}
	}
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
	return p.publishRawTTL(ctx, topic, payload, qos, retain, 0)
}

func (p *Publisher) publishRawTTL(ctx context.Context, topic string, payload any, qos byte, retain bool, ttl time.Duration) error {
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
	pub := &paho.Publish{Topic: topic, QoS: qos, Retain: retain, Payload: data}
	if p.testPublish != nil {
		return p.testPublish(topic, data)
	}
	if p.cm == nil {
		return fmt.Errorf("mqtt client not connected")
	}
	if exp := messageExpirySeconds(ttl); exp != nil {
		pub.Properties = &paho.PublishProperties{MessageExpiry: exp}
	}
	pubCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := p.cm.Publish(pubCtx, pub)
	if err != nil {
		return fmt.Errorf("publish to %q: %w", topic, err)
	}
	return nil
}

func messageExpirySeconds(ttl time.Duration) *uint32 {
	if ttl <= 0 {
		return nil
	}
	sec := uint32((ttl + time.Second - 1) / time.Second)
	if sec == 0 {
		sec = 1
	}
	return &sec
}

func (p *Publisher) storePending(sd *SensorData) {
	ttl := p.opts.pendingTTL()
	if ttl <= 0 {
		return
	}
	safe := sd.RetainedState()
	if safe.Temperature == nil && safe.Humidity == nil && safe.Bat == "" && len(safe.Extra) == 0 {
		return
	}
	key := safe.TopicID()
	now := time.Now()
	p.pendingMu.Lock()
	p.pending[key] = pendingSensor{sd: safe, storedAt: now}
	p.evictPendingLocked(now, ttl, key)
	p.pendingMu.Unlock()
	log.Debug("buffered latest sensor value while mqtt down", "hwid", key, "ttl", ttl)
}

// keepNewerPending updates a snapshot that is already waiting. A throttled
// frame must not create a new buffer entry, and it must not refresh the age.
func (p *Publisher) keepNewerPending(sd *SensorData) {
	if p == nil || p.opts == nil || p.opts.pendingTTL() <= 0 || sd == nil {
		return
	}
	safe := sd.RetainedState()
	if safe.Temperature == nil && safe.Humidity == nil && safe.Bat == "" && len(safe.Extra) == 0 {
		return
	}
	key := safe.TopicID()
	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()
	if p.pending == nil {
		return
	}
	cur, ok := p.pending[key]
	if ok {
		cur.sd = safe
		p.pending[key] = cur
	}
}

func (p *Publisher) evictPendingLocked(now time.Time, ttl time.Duration, keep string) {
	for key, item := range p.pending {
		if item.storedAt.After(now) {
			item.storedAt = now
			p.pending[key] = item
		}
		if now.Sub(item.storedAt) > ttl {
			delete(p.pending, key)
			p.pendingDropped.Add(1)
		}
	}
	for len(p.pending) > maxPendingSensors {
		oldestKey := ""
		var oldest time.Time
		for key, item := range p.pending {
			if key == keep {
				continue
			}
			if oldestKey == "" || item.storedAt.Before(oldest) {
				oldestKey = key
				oldest = item.storedAt
			}
		}
		if oldestKey == "" {
			break
		}
		delete(p.pending, oldestKey)
		p.pendingDropped.Add(1)
	}
}

func (p *Publisher) restorePending(item pendingSensor) {
	if p == nil || item.sd == nil {
		return
	}
	key := item.sd.TopicID()
	if key == "" {
		return
	}
	p.pendingMu.Lock()
	if p.pending == nil {
		p.pending = map[string]pendingSensor{}
	}
	if _, exists := p.pending[key]; !exists {
		p.pending[key] = item
	}
	p.pendingMu.Unlock()
}

func (p *Publisher) clearPending(key string) {
	if p == nil || key == "" {
		return
	}
	p.pendingMu.Lock()
	snap := make(map[string]*SensorData, len(p.pending))
	for id, item := range p.pending {
		snap[id] = item.sd
	}
	p.pendingMu.Unlock()
	drop := map[string]struct{}{}
	for id, sd := range snap {
		if id == key {
			drop[id] = struct{}{}
			continue
		}
		if sd == nil {
			continue
		}
		cp := *sd
		cp.ApplyCanonicalHWID(p.opts)
		if cp.TopicID() == key {
			drop[id] = struct{}{}
		}
	}
	if len(drop) == 0 {
		return
	}
	p.pendingMu.Lock()
	for id := range drop {
		delete(p.pending, id)
	}
	p.pendingMu.Unlock()
}

func (p *Publisher) flushPending(ctx context.Context) {
	ttl := p.opts.pendingTTL()
	if ttl <= 0 {
		return
	}
	// Hold publishMu so a live frame cannot overwrite the snapshot mid-flush.
	p.publishMu.Lock()
	defer p.publishMu.Unlock()

	type held struct {
		key    string
		item   pendingSensor
		future bool
		orig   time.Time
	}
	p.pendingMu.Lock()
	helds := make([]held, 0, len(p.pending))
	now := time.Now()
	for key, item := range p.pending {
		if now.Sub(item.storedAt) > ttl {
			delete(p.pending, key)
			p.pendingDropped.Add(1)
			continue
		}
		helds = append(helds, held{key: key, item: item})
		delete(p.pending, key)
	}
	p.pendingMu.Unlock()

	// Two keys can name one sensor after a HWID map change. Publish the newest
	// and drop the older alias so map order cannot leave the stale value.
	best := make(map[string]held, len(helds))
	for _, h := range helds {
		canon := h.key
		if h.item.sd != nil {
			cp := *h.item.sd
			cp.ApplyCanonicalHWID(p.opts)
			if id := cp.TopicID(); id != "" {
				canon = id
			}
		}
		// A stamp left in the future by a backward clock step must not beat
		// a reading that was stored after the clock moved. Two future stamps
		// keep their original order: the later write is the newer value.
		orig := h.item.storedAt
		hFuture := orig.After(now)
		if prev, ok := best[canon]; ok {
			if hFuture && !prev.future {
				continue
			}
			if !hFuture && prev.future {
				h.future = false
				h.orig = orig
				best[canon] = h
				continue
			}
			if !orig.After(prev.orig) {
				continue
			}
		}
		if hFuture {
			h.item.storedAt = now
		}
		h.future = hFuture
		h.orig = orig
		best[canon] = h
	}
	for canon, h := range best {
		if h.item.sd == nil {
			continue
		}
		if err := p.publishSensorLocked(ctx, h.item.sd); err != nil {
			if errors.Is(err, errSensorThrottled) {
				// The value was removed before the attempt. A throttle is not a
				// failed publish: keep the original snapshot until the window ends.
				p.restorePending(h.item)
				continue
			}
			// publishSensorLocked buffers the value again with a fresh timestamp.
			// A broker outage must not extend the original TTL.
			p.keepPendingAge(canon, h.item.storedAt)
			log.Warn("failed to flush pending sensor", "hwid", h.item.sd.TopicID(), "err", err)
			continue
		}
		p.pendingFlushed.Add(1)
		log.Info("flushed pending sensor after reconnect", "hwid", h.item.sd.TopicID())
	}
}

func (p *Publisher) keepPendingAge(key string, storedAt time.Time) {
	if p == nil || key == "" || storedAt.IsZero() {
		return
	}
	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()
	cur, ok := p.pending[key]
	if !ok {
		return
	}
	cur.storedAt = storedAt
	p.pending[key] = cur
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

// Resubscribe refreshes command and HA rediscover subscriptions from current options.
func (p *Publisher) Resubscribe() error {
	if p == nil || p.cm == nil {
		return nil
	}
	return p.resubscribeCommands(p.cm)
}

func (p *Publisher) resubscribeCommands(cm *autopaho.ConnectionManager) error {
	haOn, _, _, _ := p.opts.snapshotHAActuators()
	cmdTopic := p.opts.Publish.CmdTopic
	rediscover := p.opts.haRediscoverTopic()
	qos := p.opts.mqttQoS()

	// Hold cmdMu across the broker calls. AwaitConnection returns before
	// OnConnectionUp, so a subscribe that lands in that gap must not be
	// removed by an unsubscribe that still sees the old empty handler.
	p.cmdMu.Lock()
	defer p.cmdMu.Unlock()
	handler := p.cmdHandler

	parent := p.runCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()

	var drop []string
	if handler == nil {
		drop = append(drop, cmdTopic)
	}
	if !haOn {
		drop = append(drop, rediscover)
	}
	if len(drop) > 0 {
		if _, err := cm.Unsubscribe(ctx, &paho.Unsubscribe{Topics: drop}); err != nil {
			log.Debug("mqtt unsubscribe", "topics", drop, "err", err)
		}
	}
	if handler == nil && !haOn {
		return nil
	}

	var subs []paho.SubscribeOptions
	if handler != nil {
		subs = append(subs, paho.SubscribeOptions{
			Topic:          cmdTopic,
			QoS:            qos,
			RetainHandling: 2, // do not replay retained commands on subscribe
		})
	}
	if haOn {
		subs = append(subs, paho.SubscribeOptions{
			Topic:          rediscover,
			QoS:            qos,
			RetainHandling: 2,
		})
	}
	if len(subs) == 0 {
		return nil
	}
	_, err := cm.Subscribe(ctx, &paho.Subscribe{Subscriptions: subs})
	if err != nil {
		return fmt.Errorf("subscribe commands/rediscover: %w", err)
	}
	if handler != nil {
		log.Info("subscribed to rflink command topic", "topic", cmdTopic)
	}
	if haOn {
		log.Info("subscribed to HA rediscover topic", "topic", rediscover)
	}
	return nil
}

func (p *Publisher) RunPublishLoop(ctx context.Context, serialCh <-chan serialFrame) {
	for ctx.Err() == nil {
		p.runPublishLoopInner(ctx, serialCh)
	}
}

func (p *Publisher) runPublishLoopInner(ctx context.Context, serialCh <-chan serialFrame) {
	defer func() {
		if recovered := recover(); recovered != nil {
			p.panics.Add(1)
			log.Error("publish loop panic recovered", "panic", recovered)
			sleepContext(ctx, time.Second)
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

	var staleTicker *time.Ticker
	var staleCh <-chan time.Time
	if p.opts.Publish.HASensorAvailability && p.opts.Publish.HASensorStaleSec > 0 {
		every := max(time.Duration(p.opts.Publish.HASensorStaleSec)*time.Second/4, 5*time.Second)
		if every > time.Minute {
			every = time.Minute
		}
		staleTicker = time.NewTicker(every)
		staleCh = staleTicker.C
		defer staleTicker.Stop()
	}

	statusTicker := time.NewTicker(serialStatusRefresh)
	defer statusTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-healthCh:
			p.reportHealth(ctx)
		case <-watchdogCh:
			p.checkSerialWatchdog()
			p.checkPingTimeout()
			p.publishStaleSensorAvailability(ctx)
		case <-pingCheckCh:
			p.checkPingTimeout()
			p.publishStaleSensorAvailability(ctx)
		case <-staleCh:
			p.publishStaleSensorAvailability(ctx)
		case <-statusTicker.C:
			p.maintainConnected(ctx)
			// These no-op while their options are off. The dedicated tickers
			// are chosen once when the loop starts, so a runtime enable would
			// otherwise never run until the process restarts.
			p.checkSerialWatchdog()
			p.checkPingTimeout()
			p.publishStaleSensorAvailability(ctx)
		case frame, ok := <-serialCh:
			if !ok {
				return
			}
			p.NoteSerialActivity()
			p.handleSerialMessage(ctx, frame.Device, frame.Line)
		}
	}
}

func (p *Publisher) handleSerialMessage(ctx context.Context, device, msg string) {
	p.appendRawLine(device, msg)
	// PONG is counted in the serial read callback, before this queue. Counting
	// it again here can clear a newer PING that was marked while this line waited.
	if info, err := ParseGatewayMessage(msg); err == nil && info != nil {
		if info.Version != "" {
			p.noteGatewayVersion(device, info.Version)
		}
		if p.isDuplicateFrom(device, msg) {
			return
		}
		if err := p.PublishGatewayInfo(ctx, device, info); err != nil {
			log.Warn("failed to publish gateway info", "err", err)
		} else {
			p.markDedupFrom(device, msg)
			log.Info("gateway info published", "device", device, "version", info.Version, "pong", info.Pong)
		}
		return
	}
	sd, err := SensorDataFromMessage(msg)
	if err != nil {
		log.Debug("skipped non-sensor rflink message", "message", msg, "err", err)
		return
	}
	sd.ApplyCanonicalHWID(p.opts)
	// A duplicate heard on the gateway that already owns this sensor is dropped.
	// Another gateway takes over after a short hold, or at once when the owner is down,
	// and that frame is published so discovery follows the radio that is hearing it.
	takeover := device != "" && p.bindSensorPort(sd.TopicID(), device)
	if p.duplicate(dedupKey(msg), msg, !takeover) && !takeover {
		return
	}
	if err := p.PublishSensor(ctx, sd); err != nil {
		if takeover {
			// The owner already moved, so the next copy is not a takeover.
			// Dropping the old mark lets that copy publish. A failed attempt
			// must not stay blocked until the dedup window ends.
			p.forgetDedup(msg)
		}
		if errors.Is(err, errSensorThrottled) {
			if takeover {
				// The owner already moved, but the state publish was skipped.
				// Discovery still has to follow that gateway or Home Assistant
				// keeps the previous port, which may already be down.
				p.publishTakeoverDiscovery(ctx, sd)
			}
			return
		}
		log.Warn("failed to publish sensor data", "id", sd.Id, "hwid", sd.Hwid, "err", err)
		return
	}
	p.markDedup(msg)
}

// sensorOwnerStick is how long a gateway keeps a sensor after its own last frame
// while another gateway also hears it. Overlapping radios then do not rewrite
// discovery on every burst. A dead owner is replaced on the next frame.
const sensorOwnerStick = 5 * time.Second

// bindSensorPort remembers which gateway owns a sensor.
// The owner is the gateway that last delivered a new frame. A second gateway
// takes over once the owner has been quiet for sensorOwnerStick, or immediately
// when that port is down. The caller republishes the frame after a takeover.
func (p *Publisher) bindSensorPort(topicID, device string) bool {
	if p == nil || topicID == "" {
		return false
	}
	device = strings.TrimSpace(device)
	if device == "" {
		return false
	}
	if p.opts != nil {
		device = canonicalDevice(p.opts.serialDeviceList(), device)
	}
	now := time.Now()
	for {
		p.sensorSeenMu.Lock()
		prev := ""
		var since time.Time
		if p.sensorPort != nil {
			prev = p.sensorPort[topicID]
			since = p.sensorPortAt[topicID]
		}
		p.sensorSeenMu.Unlock()
		if prev == device {
			p.touchSensorOwner(topicID, now)
			return false
		}
		if !since.IsZero() && since.After(now) {
			// The owner stamp is in the future after a backward clock step.
			// Pull it to now so the stick interval can elapse.
			since = now
			p.sensorSeenMu.Lock()
			if p.sensorPortAt != nil {
				p.sensorPortAt[topicID] = now
			}
			p.sensorSeenMu.Unlock()
		}
		ownerDown := prev != "" && !p.deviceConnected(prev)
		if prev != "" && !ownerDown && now.Sub(since) < sensorOwnerStick {
			return false
		}
		p.sensorSeenMu.Lock()
		if p.sensorPort == nil {
			p.sensorPort = map[string]string{}
			p.sensorPortAt = map[string]time.Time{}
		}
		cur := p.sensorPort[topicID]
		curSince := p.sensorPortAt[topicID]
		if cur != prev || !curSince.Equal(since) {
			p.sensorSeenMu.Unlock()
			continue
		}
		p.sensorPort[topicID] = device
		p.sensorPortAt[topicID] = now
		p.sensorSeenMu.Unlock()
		return prev != ""
	}
}

func (p *Publisher) touchSensorOwner(topicID string, now time.Time) {
	p.sensorSeenMu.Lock()
	if p.sensorPortAt == nil {
		p.sensorPortAt = map[string]time.Time{}
	}
	p.sensorPortAt[topicID] = now
	p.sensorSeenMu.Unlock()
}

func (p *Publisher) deviceConnected(device string) bool {
	if p == nil {
		return false
	}
	p.serialMu.Lock()
	defer p.serialMu.Unlock()
	for name, up := range p.serialState {
		if strings.EqualFold(name, device) {
			return up
		}
	}
	return false
}

func (p *Publisher) sensorSerialTopic(topicID string) string {
	if p == nil || p.opts == nil {
		return ""
	}
	if len(p.opts.serialDeviceList()) <= 1 {
		return p.opts.serialStatusTopic()
	}
	p.sensorSeenMu.Lock()
	dev := ""
	if p.sensorPort != nil {
		dev = p.sensorPort[topicID]
	}
	p.sensorSeenMu.Unlock()
	if dev == "" {
		return p.opts.serialStatusTopic()
	}
	return p.opts.serialDeviceStatusTopic(dev)
}

// actuatorSerialTopic is the serial availability topic for HA buttons and switches.
// One gateway keeps rflink/serial/status. Several gateways use a topic that is
// online when any command target is connected.
func (p *Publisher) actuatorSerialTopic() string {
	if p == nil || p.opts == nil {
		return ""
	}
	if len(p.opts.serialDeviceList()) <= 1 {
		return p.opts.serialStatusTopic()
	}
	return p.opts.serialCommandStatusTopic()
}

func (p *Publisher) actuatorAvailability() []map[string]string {
	if p == nil || p.opts == nil {
		return nil
	}
	return []map[string]string{
		{"topic": p.opts.statusTopic(), "payload_available": statusOnline, "payload_not_available": statusOffline},
		{"topic": p.actuatorSerialTopic(), "payload_available": statusOnline, "payload_not_available": statusOffline},
	}
}

func (p *Publisher) SetSerialConnected(ctx context.Context, connected bool) {
	device := ""
	if p != nil && p.opts != nil {
		device = p.opts.Serial.Device
	}
	p.SetDeviceConnected(ctx, device, connected)
}

// SetDeviceConnected records one gateway and republishes serial status.
// With a single port, rflink/serial/status follows that port.
// With more than one, that topic is online only when every configured port is up,
// and each port also gets rflink/serial/<name>/status.
func (p *Publisher) SetDeviceConnected(ctx context.Context, device string, connected bool) {
	if p == nil {
		return
	}
	device = strings.TrimSpace(device)
	if device == "" {
		device = "primary"
	}
	var known []string
	if p.opts != nil {
		known = p.opts.serialDeviceList()
	}
	device = canonicalDevice(known, device)
	usage := p.queueUsage()
	p.serialMu.Lock()
	if p.serialState == nil {
		p.serialState = map[string]bool{}
	}
	p.serialState[device] = connected
	if len(known) == 0 {
		known = []string{device}
	}
	ports := make([]SerialPortStatus, 0, len(known))
	allUp := true
	for _, name := range known {
		up := p.serialState[name]
		if !up {
			allUp = false
		}
		ports = append(ports, SerialPortStatus{Device: name, Connected: up})
	}
	p.applyPortMetaLocked(ports, usage)
	p.serialPorts = ports
	// Store while the map lock is still held. A later connect must not
	// overwrite this with a snapshot it took earlier.
	p.serialUp.Store(allUp)
	p.serialMu.Unlock()
	if connected {
		p.NoteSerialActivityFrom(device)
	}
	p.publishSerialStatus(ctx)
}

func canonicalDevice(known []string, name string) string {
	for _, item := range known {
		if strings.EqualFold(item, name) {
			return item
		}
	}
	return name
}

// maintainConnected refreshes retained status and retries values that could
// not leave on the previous attempt. flushPending runs on connect and again
// here, so a snapshot held back by the throttle window is published once that
// window ends instead of waiting for another broker reconnect.
func (p *Publisher) maintainConnected(ctx context.Context) {
	if p == nil {
		return
	}
	p.publishSerialStatus(ctx)
	p.publishHealth(ctx)
	if !p.mqttUp.Load() {
		return
	}
	p.flushPending(ctx)
	p.retryMissedAvailability(ctx)
	p.retryAvailOnline(ctx)
	p.publishHAActuators(ctx)
	p.clearAlternateGatewayInfo(ctx)
}

// clearAlternateGatewayInfo removes the gateway info topic that this process
// no longer publishes. Several ports use per-port topics, so the shared topic
// is cleared. One port uses the shared topic, so a leftover per-port payload
// is cleared. A failed publish is retried on the next status tick.
func (p *Publisher) clearAlternateGatewayInfo(ctx context.Context) {
	if p == nil || p.opts == nil || (p.cm == nil && p.testPublish == nil) {
		return
	}
	devices := p.opts.serialDeviceList()
	p.clearRetiredGatewayTopics(ctx)
	want := int32(1)
	topic := ""
	if len(devices) > 1 {
		want = 2
		topic = p.opts.gatewayInfoTopic()
	} else if len(devices) == 1 {
		topic = p.opts.gatewayDeviceInfoTopic(devices[0])
	} else {
		p.gatewayInfoMode.Store(want)
		return
	}
	if p.gatewayInfoMode.Load() == want {
		return
	}
	if err := p.publishRaw(ctx, topic, "", p.opts.mqttQoS(), true); err != nil {
		log.Debug("failed to clear alternate gateway info", "topic", topic, "err", err)
		return
	}
	p.gatewayInfoMode.Store(want)
}

func (p *Publisher) publishSerialStatus(ctx context.Context) {
	if p == nil || p.opts == nil || p.cm == nil {
		return
	}
	status := statusOffline
	if p.serialUp.Load() {
		status = statusOnline
	}
	if err := p.publishStatusValue(ctx, p.opts.serialStatusTopic(), status); err != nil {
		log.Debug("failed to publish serial status", "err", err)
	}
	ports := p.serialPortSnapshot()
	if len(ports) <= 1 {
		return
	}
	for _, port := range ports {
		st := statusOffline
		if port.Connected {
			st = statusOnline
		}
		if err := p.publishStatusValue(ctx, p.opts.serialDeviceStatusTopic(port.Device), st); err != nil {
			log.Debug("failed to publish serial port status", "device", port.Device, "err", err)
		}
	}
	cmd := statusOffline
	if p.commandTargetsUp(ports) {
		cmd = statusOnline
	}
	if err := p.publishStatusValue(ctx, p.opts.serialCommandStatusTopic(), cmd); err != nil {
		log.Debug("failed to publish command path status", "err", err)
	}
}

// publishStatusValue keeps "offline" until the next update. "online" expires
// unless this process keeps refreshing it, so a hard kill cannot leave a port online.
func (p *Publisher) publishStatusValue(ctx context.Context, topic, status string) error {
	if status == statusOnline {
		return p.publishRawTTL(ctx, topic, status, p.opts.mqttQoS(), true, serialStatusTTL)
	}
	return p.publishRaw(ctx, topic, status, p.opts.mqttQoS(), true)
}

func (p *Publisher) commandTargetsUp(ports []SerialPortStatus) bool {
	if p == nil || p.opts == nil {
		return false
	}
	for _, port := range ports {
		if port.Connected && p.opts.commandDeviceAllow(port.Device) {
			return true
		}
	}
	return false
}

func (p *Publisher) serialPortSnapshot() []SerialPortStatus {
	if p == nil {
		return nil
	}
	usage := p.queueUsage()
	p.serialMu.Lock()
	defer p.serialMu.Unlock()
	if len(p.serialPorts) > 0 {
		out := append([]SerialPortStatus(nil), p.serialPorts...)
		p.applyPortMetaLocked(out, usage)
		return out
	}
	if p.opts == nil {
		return nil
	}
	known := p.opts.serialDeviceList()
	if len(known) == 0 {
		return nil
	}
	out := make([]SerialPortStatus, 0, len(known))
	for _, name := range known {
		out = append(out, SerialPortStatus{Device: name, Connected: false})
	}
	p.applyPortMetaLocked(out, usage)
	return out
}

func (p *Publisher) queueUsage() map[string]queueFill {
	if p == nil || p.queues == nil {
		return nil
	}
	return p.queues.usage()
}

func (p *Publisher) applyPortMetaLocked(ports []SerialPortStatus, usage map[string]queueFill) {
	for i := range ports {
		if p.portDrops != nil {
			ports[i].Dropped = p.portDrops[ports[i].Device]
		}
		if p.gatewayVer != nil {
			ports[i].Version = p.gatewayVer[ports[i].Device]
		}
		ports[i].QueueCap = serialChannelBuffer
		fill, ok := usage[ports[i].Device]
		if !ok {
			for name, item := range usage {
				if strings.EqualFold(name, ports[i].Device) {
					fill = item
					ok = true
					break
				}
			}
		}
		if ok {
			ports[i].Queued = fill.Queued
			if fill.Cap > 0 {
				ports[i].QueueCap = fill.Cap
			}
		}
	}
}

// IncPortDropped counts a line discarded because that gateway's queue was full.
func (p *Publisher) IncPortDropped(device string) {
	if p == nil {
		return
	}
	p.IncSerialDropped()
	device = strings.TrimSpace(device)
	if p.opts != nil {
		device = canonicalDevice(p.opts.serialDeviceList(), device)
	}
	if device == "" {
		device = "primary"
	}
	p.serialMu.Lock()
	if p.portDrops == nil {
		p.portDrops = map[string]uint64{}
	}
	p.portDrops[device]++
	p.serialMu.Unlock()
}

func (p *Publisher) noteGatewayVersion(device, version string) {
	version = strings.TrimSpace(version)
	if p == nil || version == "" {
		return
	}
	device = strings.TrimSpace(device)
	if p.opts != nil {
		device = canonicalDevice(p.opts.serialDeviceList(), device)
	}
	if device == "" {
		device = "primary"
	}
	p.serialMu.Lock()
	if p.gatewayVer == nil {
		p.gatewayVer = map[string]string{}
	}
	p.gatewayVer[device] = version
	p.serialMu.Unlock()
}

func (p *Publisher) gatewayVersion(device string) string {
	if p == nil {
		return ""
	}
	device = strings.TrimSpace(device)
	if p.opts != nil {
		device = canonicalDevice(p.opts.serialDeviceList(), device)
	}
	if device == "" {
		device = "primary"
	}
	p.serialMu.Lock()
	defer p.serialMu.Unlock()
	if p.gatewayVer == nil {
		return ""
	}
	return p.gatewayVer[device]
}

func (p *Publisher) NoteSerialActivity() {
	p.NoteSerialActivityFrom("")
}

// NoteSerialActivityFrom records a line from one gateway. An empty device
// updates only the aggregate timestamp.
func (p *Publisher) NoteSerialActivityFrom(device string) {
	if p == nil {
		return
	}
	now := time.Now()
	p.lastSerialAt.Store(now)
	device = strings.TrimSpace(device)
	if device == "" {
		return
	}
	if p.opts != nil {
		device = canonicalDevice(p.opts.serialDeviceList(), device)
	}
	p.serialMu.Lock()
	if p.serialHeard == nil {
		p.serialHeard = map[string]time.Time{}
	}
	p.serialHeard[device] = now
	p.serialMu.Unlock()
}

func (p *Publisher) IncSerialDropped()   { p.serialDropped.Add(1) }
func (p *Publisher) IncCommandRejected() { p.commandsRejected.Add(1) }
func (p *Publisher) IncCommandRateLimited() {
	p.commandsLimited.Add(1)
}

// clampSerialTimes pulls stamps that sit in the future back to now. A backward
// clock step would otherwise keep the age negative, so a quiet port would not
// look stale until the clock caught the old stamp. The returned start time is
// the clamped process start.
func (p *Publisher) clampSerialTimes(now time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	if t, ok := p.lastSerialAt.Load().(time.Time); ok && t.After(now) {
		p.lastSerialAt.Store(now)
	}
	p.serialMu.Lock()
	defer p.serialMu.Unlock()
	if p.startedAt.After(now) {
		p.startedAt = now
	}
	for device, heard := range p.serialHeard {
		if heard.After(now) {
			p.serialHeard[device] = now
		}
	}
	return p.startedAt
}

func (p *Publisher) checkSerialWatchdog() {
	if p == nil || p.opts == nil {
		return
	}
	wd := p.opts.serialWatchdog()
	if wd <= 0 {
		return
	}
	now := time.Now()
	started := p.clampSerialTimes(now)
	ports := p.serialPortSnapshot()
	if len(ports) == 0 {
		t, ok := p.lastSerialAt.Load().(time.Time)
		if !ok || t.IsZero() {
			if !started.IsZero() && now.Sub(started) > wd {
				log.Warn("serial watchdog: no data received since start", "watchdog", wd, "uptime", now.Sub(started).Round(time.Second))
			}
			return
		}
		if age := now.Sub(t); age > wd {
			log.Warn("serial watchdog: no data from serial port", "last_serial_at", t.Format(time.RFC3339), "age", age.Round(time.Second), "watchdog", wd)
		}
		return
	}
	primary := ""
	if p.opts != nil {
		primary = strings.TrimSpace(p.opts.Serial.Device)
	}
	p.serialMu.Lock()
	var quiet []string
	for _, port := range ports {
		if !port.Connected {
			continue
		}
		if hold, ok := p.serialHold[port.Device]; ok {
			// A normal hold is at most one watchdog interval ahead. Farther
			// than that means the clock stepped backward after the hold was
			// armed, and the quiet port would stay closed until wall time
			// caught the old stamp.
			if hold.After(now.Add(wd)) {
				delete(p.serialHold, port.Device)
			} else if now.Before(hold) {
				continue
			}
		}
		heard := p.serialHeard[port.Device]
		if heard.IsZero() {
			heard = started
		}
		if heard.IsZero() || now.Sub(heard) <= wd {
			continue
		}
		quiet = append(quiet, port.Device)
		if p.serialHold == nil {
			p.serialHold = map[string]time.Time{}
		}
		p.serialHold[port.Device] = now.Add(wd)
	}
	p.serialMu.Unlock()
	for _, device := range quiet {
		log.Warn("serial watchdog: no data from gateway, reopening", "device", device, "watchdog", wd)
		if primary != "" && strings.EqualFold(device, primary) {
			p.SignalSerialRestart()
			continue
		}
		p.signalExtraRestart(device)
	}
}

func (p *Publisher) anyPortSilent(wd time.Duration) bool {
	if p == nil || wd <= 0 {
		return false
	}
	ports := p.serialPortSnapshot()
	now := time.Now()
	started := p.clampSerialTimes(now)
	p.serialMu.Lock()
	defer p.serialMu.Unlock()
	for _, port := range ports {
		if !port.Connected {
			continue
		}
		heard := time.Time{}
		if p.serialHeard != nil {
			heard = p.serialHeard[port.Device]
		}
		if heard.IsZero() {
			heard = started
		}
		if heard.IsZero() || now.Sub(heard) > wd {
			return true
		}
	}
	return false
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
	if err := p.publishRawTTL(ctx, p.opts.healthTopic(), snap, p.opts.mqttQoS(), true, serialStatusTTL); err != nil {
		log.Debug("failed to publish health", "err", err)
	}
}

// publishHealth refreshes the retained health snapshot. The message expires
// unless this loop keeps publishing it, so a hard kill cannot leave a fresh
// health JSON on the broker.
func (p *Publisher) publishHealth(ctx context.Context) {
	if p == nil || p.opts == nil || p.cm == nil {
		return
	}
	if err := p.publishRawTTL(ctx, p.opts.healthTopic(), p.snapshot(), p.opts.mqttQoS(), true, serialStatusTTL); err != nil {
		log.Debug("failed to publish health", "err", err)
	}
}

func (p *Publisher) snapshot() HealthSnapshot {
	p.pendingMu.Lock()
	pendingCount := len(p.pending)
	p.pendingMu.Unlock()

	status := statusOffline
	if p.mqttUp.Load() {
		status = statusOnline
	}
	started := p.clampSerialTimes(time.Now())

	var lastAtStr, lastHWID, lastSerialStr string
	if t, ok := p.lastSensorAt.Load().(time.Time); ok && !t.IsZero() {
		lastAtStr = t.Format(time.RFC3339)
	}
	if h, ok := p.lastSensorHWID.Load().(string); ok {
		lastHWID = h
	}
	serialStale := false
	wd := time.Duration(0)
	if p.opts != nil {
		wd = p.opts.serialWatchdog()
	}
	if t, ok := p.lastSerialAt.Load().(time.Time); ok && !t.IsZero() {
		lastSerialStr = t.Format(time.RFC3339)
		if wd > 0 && time.Since(t) > wd {
			serialStale = true
		}
	} else if wd > 0 && !started.IsZero() && time.Since(started) > wd {
		serialStale = true
	}
	if p.anyPortSilent(wd) {
		serialStale = true
	}

	p.sensorSeenMu.Lock()
	sensors := make(map[string]string, len(p.sensorSeen))
	for hwid, t := range p.sensorSeen {
		sensors[hwid] = t.Format(time.RFC3339)
	}
	p.sensorSeenMu.Unlock()

	return HealthSnapshot{
		Version: Version, GitSHA: GitSHA, StartedAt: started.Format(time.RFC3339),
		Status: status, Published: p.published.Load(), PublishFailed: p.publishFailed.Load(),
		PendingFlushed: p.pendingFlushed.Load(), PendingDropped: p.pendingDropped.Load(),
		PendingBuffered: pendingCount, SerialDropped: p.serialDropped.Load(),
		DedupDropped: p.dedupDropped.Load(), ThrottleDropped: p.throttleDropped.Load(), Panics: p.panics.Load(),
		CommandsRejected: p.commandsRejected.Load(), CommandsLimited: p.commandsLimited.Load(),
		LastSensorAt: lastAtStr, LastSensorHWID: lastHWID, LastSerialAt: lastSerialStr,
		SerialStale: serialStale, SerialConnected: p.serialUp.Load(),
		SerialPorts:        p.serialPortSnapshot(),
		RFLinkUnresponsive: p.anyUnresponsive(),
		LastPongAt: func() string {
			p.pingMu.Lock()
			defer p.pingMu.Unlock()
			if p.lastPongAt.IsZero() {
				return ""
			}
			return p.lastPongAt.Format(time.RFC3339)
		}(),
		PingFails: p.worstPingFails(),
		UptimeSec: int64(time.Since(started).Seconds()), MQTTConnected: p.mqttUp.Load(),
		SensorsLastSeen: sensors,
	}
}

// MarkPingSent records that 10;PING; is about to be written to serial.
func (p *Publisher) MarkPingSent() {
	p.pingMu.Lock()
	p.pingSentAt = time.Now()
	p.pingWrite = true
	p.pingMu.Unlock()
}

// MarkPingWritten records that the PING write returned successfully.
// The wait for PONG starts only then, so a slow write is not a missed PONG.
func (p *Publisher) MarkPingWritten() {
	p.pingMu.Lock()
	p.pingWrite = false
	if !p.pingSentAt.IsZero() {
		p.pingSentAt = time.Now()
	}
	p.pingMu.Unlock()
}

// ClearPingSent drops an outstanding ping after a failed write.
func (p *Publisher) ClearPingSent() {
	if p == nil {
		return
	}
	p.pingMu.Lock()
	p.pingSentAt = time.Time{}
	p.pingWrite = false
	p.pingMu.Unlock()
}

// abandonPingIfWriting clears a PING whose write panicked before it finished.
func (p *Publisher) abandonPingIfWriting() {
	if p == nil {
		return
	}
	p.pingMu.Lock()
	if p.pingWrite {
		p.pingSentAt = time.Time{}
		p.pingWrite = false
	}
	p.pingMu.Unlock()
}

// NotePong records a successful PONG from RFLink.
func (p *Publisher) NotePong() {
	p.pingMu.Lock()
	p.lastPongAt = time.Now()
	p.pingSentAt = time.Time{}
	p.pingWrite = false
	p.pingFails.Store(0)
	wasUnresp := p.rflinkUnresp.Swap(false)
	p.pingGen++
	p.pingMu.Unlock()
	if wasUnresp {
		log.Info("RFLink responded to PING again")
	}
}

// beforePingRestart runs after a timeout is counted and before the port is
// reopened. Tests use it to deliver a PONG in that gap. It is nil in production.
var beforePingRestart func()

func (p *Publisher) checkPingTimeout() {
	if p.opts.serialPingInterval() <= 0 {
		return
	}
	timeout := p.opts.serialPingTimeout()
	threshold := p.opts.serialPingFailThreshold()
	p.pingMu.Lock()
	now := time.Now()
	// A step backwards leaves the stamp in the future. time.Since stays
	// negative, so the timeout never fires and the next PING is skipped.
	if !p.pingSentAt.IsZero() && p.pingSentAt.After(now) {
		p.pingSentAt = now
	}
	// The port write has not finished. A PONG cannot be late yet.
	primaryDue := !p.pingWrite && !p.pingSentAt.IsZero() && now.Sub(p.pingSentAt) >= timeout
	var primaryFails uint64
	var primaryGen uint64
	var primaryMark bool
	if primaryDue {
		// Timed out. The counter and the unresponsive flag move under the same
		// lock as NotePong, so a PONG that arrived in between cannot be overwritten.
		p.pingSentAt = time.Time{}
		primaryFails = p.pingFails.Add(1)
		primaryMark = int(primaryFails) >= threshold && !p.rflinkUnresp.Swap(true)
		primaryGen = p.pingGen
	}
	type extraTimeout struct {
		device string
		fails  uint64
		mark   bool
		gen    uint64
	}
	var extras []extraTimeout
	for device, st := range p.extraPing {
		if st != nil && !st.sentAt.IsZero() && st.sentAt.After(now) {
			st.sentAt = now
		}
		if st == nil || st.writing || st.sentAt.IsZero() || now.Sub(st.sentAt) < timeout {
			continue
		}
		st.sentAt = time.Time{}
		st.fails++
		mark := int(st.fails) >= threshold && !st.unresponsive
		if mark {
			st.unresponsive = true
		}
		extras = append(extras, extraTimeout{device: device, fails: st.fails, mark: mark, gen: st.gen})
	}
	p.pingMu.Unlock()
	if primaryDue {
		log.Warn("RFLink PING timeout, no PONG received",
			"timeout", timeout,
			"consecutive_fails", primaryFails,
		)
		if primaryMark {
			log.Error("RFLink marked unresponsive after consecutive PING failures",
				"fails", primaryFails,
				"threshold", threshold,
			)
			if beforePingRestart != nil {
				beforePingRestart()
			}
			p.signalPrimaryIfDue(primaryGen)
		}
	}
	for _, item := range extras {
		log.Warn("RFLink PING timeout, no PONG received",
			"device", item.device,
			"timeout", timeout,
			"consecutive_fails", item.fails,
		)
		if item.mark {
			log.Error("RFLink marked unresponsive after consecutive PING failures",
				"device", item.device,
				"fails", item.fails,
				"threshold", threshold,
			)
			p.signalExtraIfDue(item.device, item.gen)
		}
	}
}

// signalPrimaryIfDue restarts only if this timeout is still the latest one.
// The check and the send share pingMu, so a PONG cannot land between them.
func (p *Publisher) signalPrimaryIfDue(gen uint64) {
	if p == nil {
		return
	}
	p.pingMu.Lock()
	defer p.pingMu.Unlock()
	if !p.rflinkUnresp.Load() || p.pingGen != gen || p.serialRestart == nil {
		return
	}
	select {
	case p.serialRestart <- struct{}{}:
	default:
	}
}

// signalExtraIfDue is the per-port form of signalPrimaryIfDue.
func (p *Publisher) signalExtraIfDue(device string, gen uint64) {
	if p == nil {
		return
	}
	p.pingMu.Lock()
	defer p.pingMu.Unlock()
	st := p.extraPing[p.extraKey(device)]
	if st == nil || !st.unresponsive || st.gen != gen {
		return
	}
	key := p.extraKey(device)
	if p.extraRestart == nil {
		p.extraRestart = map[string]chan struct{}{}
	}
	ch := p.extraRestart[key]
	if ch == nil {
		ch = make(chan struct{}, 1)
		p.extraRestart[key] = ch
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (p *Publisher) extraKey(device string) string {
	device = strings.TrimSpace(device)
	if p != nil && p.opts != nil {
		device = canonicalDevice(p.opts.serialDeviceList(), device)
	}
	if device == "" {
		device = "port"
	}
	return device
}

func (p *Publisher) extraState(device string) *extraPingState {
	key := p.extraKey(device)
	if p.extraPing == nil {
		p.extraPing = map[string]*extraPingState{}
	}
	st := p.extraPing[key]
	if st == nil {
		st = &extraPingState{}
		p.extraPing[key] = st
	}
	return st
}

func (p *Publisher) markExtraPing(device string) {
	if p == nil {
		return
	}
	p.pingMu.Lock()
	st := p.extraState(device)
	st.sentAt = time.Now()
	st.writing = true
	p.pingMu.Unlock()
}

func (p *Publisher) markExtraPingWritten(device string) {
	if p == nil {
		return
	}
	p.pingMu.Lock()
	if st := p.extraPing[p.extraKey(device)]; st != nil {
		st.writing = false
		if !st.sentAt.IsZero() {
			st.sentAt = time.Now()
		}
	}
	p.pingMu.Unlock()
}

func (p *Publisher) clearExtraPing(device string) {
	if p == nil {
		return
	}
	p.pingMu.Lock()
	if st := p.extraPing[p.extraKey(device)]; st != nil {
		st.sentAt = time.Time{}
		st.writing = false
	}
	p.pingMu.Unlock()
}

func (p *Publisher) abandonExtraPingIfWriting(device string) {
	if p == nil {
		return
	}
	p.pingMu.Lock()
	if st := p.extraPing[p.extraKey(device)]; st != nil && st.writing {
		st.sentAt = time.Time{}
		st.writing = false
	}
	p.pingMu.Unlock()
}

func (p *Publisher) noteExtraPong(device string) {
	if p == nil {
		return
	}
	p.pingMu.Lock()
	st := p.extraState(device)
	st.sentAt = time.Time{}
	st.writing = false
	st.fails = 0
	was := st.unresponsive
	st.unresponsive = false
	st.gen++
	p.lastPongAt = time.Now()
	p.pingMu.Unlock()
	if was {
		log.Info("RFLink responded to PING again", "device", p.extraKey(device))
	}
}

func (p *Publisher) resetExtraPing(device string) {
	if p == nil {
		return
	}
	p.pingMu.Lock()
	if p.extraPing == nil {
		p.pingMu.Unlock()
		return
	}
	if st := p.extraPing[p.extraKey(device)]; st != nil {
		st.sentAt = time.Time{}
		st.writing = false
		st.fails = 0
		st.unresponsive = false
	}
	p.pingMu.Unlock()
}

func (p *Publisher) extraPingOutstanding(device string) bool {
	if p == nil {
		return false
	}
	p.pingMu.Lock()
	defer p.pingMu.Unlock()
	st := p.extraPing[p.extraKey(device)]
	return st != nil && !st.sentAt.IsZero()
}

func (p *Publisher) extraRestartChan(device string) chan struct{} {
	if p == nil {
		return nil
	}
	key := p.extraKey(device)
	p.pingMu.Lock()
	defer p.pingMu.Unlock()
	if p.extraRestart == nil {
		p.extraRestart = map[string]chan struct{}{}
	}
	ch := p.extraRestart[key]
	if ch == nil {
		ch = make(chan struct{}, 1)
		p.extraRestart[key] = ch
	}
	return ch
}

func (p *Publisher) signalExtraRestart(device string) {
	ch := p.extraRestartChan(device)
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

// drainExtraRestart drops a restart left by the previous extra session.
// The channel is buffered, so a signal raised while the port was already down
// would cancel the next session immediately.
func (p *Publisher) drainExtraRestart(device string) {
	ch := p.extraRestartChan(device)
	if ch == nil {
		return
	}
	select {
	case <-ch:
	default:
	}
}

func (p *Publisher) anyUnresponsive() bool {
	if p == nil {
		return false
	}
	if p.rflinkUnresp.Load() {
		return true
	}
	p.pingMu.Lock()
	defer p.pingMu.Unlock()
	for _, st := range p.extraPing {
		if st != nil && st.unresponsive {
			return true
		}
	}
	return false
}

func (p *Publisher) worstPingFails() uint64 {
	if p == nil {
		return 0
	}
	n := p.pingFails.Load()
	p.pingMu.Lock()
	defer p.pingMu.Unlock()
	for _, st := range p.extraPing {
		if st != nil && st.fails > n {
			n = st.fails
		}
	}
	return n
}

// SignalSerialRestart asks the open serial session to reopen after the current read.
func (p *Publisher) SignalSerialRestart() {
	if p == nil || p.serialRestart == nil {
		return
	}
	select {
	case p.serialRestart <- struct{}{}:
	default:
	}
}

// DrainSerialRestart drops a restart left over from the previous session.
func (p *Publisher) DrainSerialRestart() {
	if p == nil || p.serialRestart == nil {
		return
	}
	select {
	case <-p.serialRestart:
	default:
	}
}

// ResetPingState forgets the current PING so a reopened port starts a new count.
func (p *Publisher) ResetPingState() {
	if p == nil {
		return
	}
	p.pingMu.Lock()
	p.pingSentAt = time.Time{}
	p.pingWrite = false
	p.pingFails.Store(0)
	p.rflinkUnresp.Store(false)
	p.pingGen++
	p.pingMu.Unlock()
}

// PingOutstanding reports whether a PING is waiting for PONG (avoid stacking pings).
func (p *Publisher) PingOutstanding() bool {
	p.pingMu.Lock()
	defer p.pingMu.Unlock()
	return !p.pingSentAt.IsZero()
}

func (p *Publisher) publishHAActuators(ctx context.Context) {
	p.haMu.Lock()
	defer p.haMu.Unlock()
	enabled, prefix, buttons, switches := p.opts.snapshotHAActuators()
	nBtn, nSw := 0, 0
	if enabled {
		nBtn, nSw = len(buttons), len(switches)
	}
	p.retireHAActuatorsLocked(ctx, prefix, nBtn, nSw)
	if !enabled {
		return
	}
	p.publishHAButtons(ctx, prefix, buttons)
	p.publishHASwitches(ctx, prefix, switches)
}

// retireHAActuatorsLocked publishes an empty retained config for actuators that
// left the list or moved to another discovery prefix. Caller holds haMu.
// Counts advance only after the empty payload is published, so a failed retire
// is retried and a concurrent publish cannot erase a button that was just created.
func (p *Publisher) retireHAActuatorsLocked(ctx context.Context, prefix string, nBtn, nSw int) {
	oldPrefix, oldBtn, oldSw := p.haPrefix, p.haBtnN, p.haSwN
	if oldPrefix == "" {
		p.haPrefix, p.haBtnN, p.haSwN = prefix, nBtn, nSw
		return
	}
	qos := p.opts.mqttQoS()
	btnOK, swOK := true, true
	for i := range oldBtn {
		if oldPrefix == prefix && i < nBtn {
			continue
		}
		id := fmt.Sprintf("gorflink_btn_%d", i)
		topic := fmt.Sprintf("%s/button/%s/config", oldPrefix, id)
		if err := p.publishRaw(ctx, topic, "", qos, true); err != nil {
			btnOK = false
			log.Warn("failed to retire HA button", "topic", topic, "err", err)
			continue
		}
		p.discovered.Delete(id)
	}
	for i := range oldSw {
		if oldPrefix == prefix && i < nSw {
			continue
		}
		id := fmt.Sprintf("gorflink_sw_%d", i)
		topic := fmt.Sprintf("%s/switch/%s/config", oldPrefix, id)
		if err := p.publishRaw(ctx, topic, "", qos, true); err != nil {
			swOK = false
			log.Warn("failed to retire HA switch", "topic", topic, "err", err)
			continue
		}
		p.discovered.Delete(id)
	}
	if btnOK {
		p.haBtnN = nBtn
	}
	if swOK {
		p.haSwN = nSw
	}
	if btnOK && swOK {
		p.haPrefix = prefix
	}
}

func (p *Publisher) publishHAButtons(ctx context.Context, prefix string, buttons []haActuatorButton) {
	cmdTopic := p.opts.Publish.CmdTopic
	for i, btn := range buttons {
		objectID := fmt.Sprintf("gorflink_btn_%d", i)
		uniqueID := objectID
		if _, loaded := p.discovered.LoadOrStore(uniqueID, true); loaded {
			continue
		}
		payload := map[string]any{
			"name":              btn.Name,
			"unique_id":         uniqueID,
			"command_topic":     cmdTopic,
			"payload_press":     btn.Command,
			"origin":            haOriginInfo(),
			"availability":      p.actuatorAvailability(),
			"availability_mode": "all",
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

func (p *Publisher) publishHASwitches(ctx context.Context, prefix string, switches []haActuatorSwitch) {
	cmdTopic := p.opts.Publish.CmdTopic
	for i, sw := range switches {
		objectID := fmt.Sprintf("gorflink_sw_%d", i)
		uniqueID := objectID
		if _, loaded := p.discovered.LoadOrStore(uniqueID, true); loaded {
			continue
		}
		// Optimistic so the control still moves if the radio never echoes.
		// state_topic receives ON/OFF when a later RX frame matches this switch.
		payload := map[string]any{
			"name":              sw.Name,
			"unique_id":         uniqueID,
			"command_topic":     cmdTopic,
			"payload_on":        sw.CommandOn,
			"payload_off":       sw.CommandOff,
			"state_topic":       p.switchStateTopic(i),
			"state_on":          "ON",
			"state_off":         "OFF",
			"optimistic":        true,
			"origin":            haOriginInfo(),
			"availability":      p.actuatorAvailability(),
			"availability_mode": "all",
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
	if p == nil || hwid == "" {
		return
	}
	var dropped []string
	p.sensorSeenMu.Lock()
	if p.sensorSeen == nil {
		p.sensorSeen = map[string]time.Time{}
	}
	p.sensorSeen[hwid] = t
	if p.sensorHeard == nil {
		p.sensorHeard = map[string]time.Time{}
	}
	p.sensorHeard[hwid] = t
	for len(p.sensorSeen) > maxSensorSeen {
		oldestKey := ""
		var oldest time.Time
		for key, seen := range p.sensorSeen {
			if key == hwid {
				continue
			}
			if oldestKey == "" || seen.Before(oldest) {
				oldestKey = key
				oldest = seen
			}
		}
		if oldestKey == "" {
			break
		}
		delete(p.sensorSeen, oldestKey)
		delete(p.sensorHeard, oldestKey)
		dropped = append(dropped, oldestKey)
	}
	p.sensorSeenMu.Unlock()
	ctx := p.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	// Per-sensor availability is a separate topic. With the option off, eviction
	// only forgets the id. With it on, a failed offline publish is remembered
	// so the retained "online" does not stay after the id left sensorSeen.
	if p.opts == nil || !p.opts.Publish.HASensorAvailability {
		return
	}
	var missed []string
	for _, id := range dropped {
		if err := p.publishSensorAvailability(ctx, id, statusOffline); err != nil {
			missed = append(missed, id)
		}
	}
	p.rememberAvailMiss(missed)
}

// markDiscoveryStale forces the next publish to send discovery again, but keeps
// the previous topic so a later frame can still clear a retained config.
// Button and switch entries are plain flags and are dropped; actuators are
// published again by the caller.
func (p *Publisher) markDiscoveryStale() {
	if p == nil {
		return
	}
	p.discoveryEpoch.Add(1)
	p.discovered.Range(func(key, val any) bool {
		if _, ok := val.(haDiscovered); !ok {
			p.discovered.Delete(key)
		}
		return true
	})
}

func (p *Publisher) throttled(hwid string, now time.Time) bool {
	window := p.opts.throttleFor(hwid)
	if window <= 0 || hwid == "" {
		return false
	}
	p.sensorSeenMu.Lock()
	last, ok := p.sensorSeen[hwid]
	if ok && last.After(now) {
		// A future stamp would throttle every later publish until the clock
		// catches it. Leave the stamp alone: this attempt has not been
		// published yet, and noteSensorSeen records the time only on success.
		p.sensorSeenMu.Unlock()
		return false
	}
	if ok && now.Sub(last) < window {
		// The frame arrived. Stale availability follows this stamp, while
		// sensorSeen stays on the last successful publish so the throttle
		// window does not slide forward on a dropped value.
		if p.sensorHeard == nil {
			p.sensorHeard = map[string]time.Time{}
		}
		p.sensorHeard[hwid] = now
		p.sensorSeenMu.Unlock()
		return true
	}
	p.sensorSeenMu.Unlock()
	return false
}

func (p *Publisher) publishSensorAvailability(ctx context.Context, topicID, payload string) error {
	if topicID == "" {
		return nil
	}
	if p.cm == nil && p.testPublish == nil {
		return fmt.Errorf("mqtt client not connected")
	}
	if err := p.publishRaw(ctx, p.sensorFieldTopic(topicID, "availability"), payload, p.opts.mqttQoS(), true); err != nil {
		log.Warn("sensor availability publish failed", "hwid", topicID, "payload", payload, "err", err)
		return err
	}
	p.availMu.Lock()
	if p.availDown == nil {
		p.availDown = map[string]struct{}{}
	}
	if payload == statusOnline {
		delete(p.availDown, topicID)
		delete(p.availMiss, topicID)
		delete(p.availOnline, topicID)
	} else {
		p.availDown[topicID] = struct{}{}
		// An offline that landed wins over a queued online retry. Otherwise
		// the next tick would mark an evicted or stale sensor available again.
		delete(p.availOnline, topicID)
	}
	p.availMu.Unlock()
	return nil
}

func (p *Publisher) rememberAvailMiss(ids []string) {
	if p == nil || len(ids) == 0 {
		return
	}
	p.availMu.Lock()
	defer p.availMu.Unlock()
	if p.availMiss == nil {
		p.availMiss = map[string]struct{}{}
	}
	for _, id := range ids {
		if id == "" {
			continue
		}
		// A failed offline must not leave a queued online that the next tick
		// would publish after the sensor has already left sensorSeen.
		delete(p.availOnline, id)
		if _, ok := p.availMiss[id]; ok {
			continue
		}
		if len(p.availMiss) >= maxSensorSeen {
			// Keep the id that just failed. Dropping it leaves its retained
			// online in place, because it is already gone from sensorSeen.
			drop := ""
			for key := range p.availMiss {
				if drop == "" || key < drop {
					drop = key
				}
			}
			delete(p.availMiss, drop)
		}
		p.availMiss[id] = struct{}{}
	}
}

func (p *Publisher) rememberAvailOnline(id string) {
	if p == nil || id == "" {
		return
	}
	p.availMu.Lock()
	defer p.availMu.Unlock()
	delete(p.availMiss, id)
	if p.availOnline == nil {
		p.availOnline = map[string]struct{}{}
	}
	if _, ok := p.availOnline[id]; ok {
		return
	}
	if len(p.availOnline) >= maxSensorSeen {
		drop := ""
		for key := range p.availOnline {
			if drop == "" || key < drop {
				drop = key
			}
		}
		delete(p.availOnline, drop)
	}
	p.availOnline[id] = struct{}{}
}

// retryAvailOnline repeats an availability "online" that failed after the
// sensor values themselves were published.
func (p *Publisher) retryAvailOnline(ctx context.Context) {
	if p == nil || p.opts == nil || !p.opts.Publish.HASensorAvailability {
		return
	}
	p.availMu.Lock()
	if len(p.availOnline) == 0 {
		p.availMu.Unlock()
		return
	}
	ids := make([]string, 0, len(p.availOnline))
	for id := range p.availOnline {
		ids = append(ids, id)
	}
	p.availMu.Unlock()
	sort.Strings(ids)
	p.publishMu.Lock()
	defer p.publishMu.Unlock()
	for _, id := range ids {
		p.availMu.Lock()
		_, still := p.availOnline[id]
		_, waitingOffline := p.availMiss[id]
		p.availMu.Unlock()
		if !still || waitingOffline {
			continue
		}
		if err := p.publishSensorAvailability(ctx, id, statusOnline); err != nil {
			continue
		}
		p.availMu.Lock()
		delete(p.availOnline, id)
		p.availMu.Unlock()
	}
}

// retryMissedAvailability publishes offline for sensors that were evicted
// while the broker rejected the first attempt.
func (p *Publisher) retryMissedAvailability(ctx context.Context) {
	if p == nil || p.opts == nil || !p.opts.Publish.HASensorAvailability {
		return
	}
	p.availMu.Lock()
	if len(p.availMiss) == 0 {
		p.availMu.Unlock()
		return
	}
	ids := make([]string, 0, len(p.availMiss))
	for id := range p.availMiss {
		ids = append(ids, id)
	}
	p.availMu.Unlock()
	sort.Strings(ids)
	// Hold publishMu so a successful frame cannot publish online and then lose
	// the race to this offline. Online removes the id from availMiss.
	p.publishMu.Lock()
	defer p.publishMu.Unlock()
	for _, id := range ids {
		p.availMu.Lock()
		_, still := p.availMiss[id]
		p.availMu.Unlock()
		if !still {
			continue
		}
		if err := p.publishSensorAvailability(ctx, id, statusOffline); err != nil {
			continue
		}
		p.availMu.Lock()
		delete(p.availMiss, id)
		p.availMu.Unlock()
	}
}

func (p *Publisher) publishStaleSensorAvailability(ctx context.Context) {
	if p == nil || p.opts == nil || !p.opts.Publish.HASensorAvailability || p.opts.Publish.HASensorStaleSec <= 0 {
		return
	}
	cutoff := time.Now().Add(-time.Duration(p.opts.Publish.HASensorStaleSec) * time.Second)
	now := time.Now()
	p.sensorSeenMu.Lock()
	var stale []string
	for id, seen := range p.sensorSeen {
		if seen.After(now) {
			seen = now
			p.sensorSeen[id] = now
		}
		if heard, ok := p.sensorHeard[id]; ok {
			if heard.After(now) {
				heard = now
				p.sensorHeard[id] = now
			}
			if heard.After(seen) {
				seen = heard
			}
		}
		if seen.Before(cutoff) {
			stale = append(stale, id)
		}
	}
	p.sensorSeenMu.Unlock()
	p.availMu.Lock()
	if p.availDown == nil {
		p.availDown = map[string]struct{}{}
	}
	pending := make([]string, 0, len(stale))
	for _, id := range stale {
		if _, down := p.availDown[id]; down {
			continue
		}
		pending = append(pending, id)
	}
	p.availMu.Unlock()
	sort.Strings(pending)
	// Hold publishMu across the recheck and the offline publish. A reconnect
	// flush takes the same lock while it marks the sensor seen and publishes
	// online, so a stale snapshot cannot overwrite that online afterwards.
	p.publishMu.Lock()
	for _, id := range pending {
		p.sensorSeenMu.Lock()
		seen, ok := p.sensorSeen[id]
		if ok {
			if heard, heardOK := p.sensorHeard[id]; heardOK && heard.After(seen) {
				seen = heard
			}
		}
		fresh := ok && !seen.Before(cutoff)
		p.sensorSeenMu.Unlock()
		if !ok || fresh {
			continue
		}
		_ = p.publishSensorAvailability(ctx, id, statusOffline)
	}
	p.publishMu.Unlock()
}

func (p *Publisher) switchStateTopic(i int) string {
	return fmt.Sprintf("%s/gateway/switch/gorflink_sw_%d/state", p.opts.Publish.Topic, i)
}

// noteSwitchEcho publishes ON/OFF when an RX frame carries the same id and command
// as a configured HA switch. unique_id of the switch itself is unchanged.
func (p *Publisher) noteSwitchEcho(ctx context.Context, sd *SensorData, fields map[string]string) {
	if p == nil || p.opts == nil || sd == nil || p.cm == nil {
		return
	}
	_, _, _, switches := p.opts.snapshotHAActuators()
	for i, sw := range switches {
		state := switchEchoState(sw, sd, fields)
		if state == "" {
			continue
		}
		if err := p.publishRaw(ctx, p.switchStateTopic(i), state, p.opts.mqttQoS(), false); err != nil {
			log.Warn("HA switch state publish failed", "name", sw.Name, "state", state, "err", err)
		}
	}
}

func switchEchoState(sw haActuatorSwitch, sd *SensorData, fields map[string]string) string {
	// A frame with both SWITCH and CMD is published as sw_<channel>, so the
	// fields map has no "cmd". The command still lives on the sensor.
	cmd := ""
	if sd != nil {
		cmd = strings.ToUpper(strings.TrimSpace(sd.Cmd))
	}
	if cmd != "ON" && cmd != "OFF" && fields != nil {
		cmd = strings.ToUpper(strings.TrimSpace(fields["cmd"]))
	}
	if cmd != "ON" && cmd != "OFF" {
		return ""
	}
	id := strings.TrimSpace(sd.Id)
	swID := ""
	if sd != nil {
		swID = sd.Switch
	}
	if cmd == "ON" && commandMatchesSensor(sw.CommandOn, id, swID) {
		return "ON"
	}
	if cmd == "OFF" && commandMatchesSensor(sw.CommandOff, id, swID) {
		return "OFF"
	}
	return ""
}

// commandAddressFields is the device address inside an RFLink command, without
// the leading "10", the protocol name, or the trailing ON/OFF word.
func commandAddressFields(cmd string) []string {
	var parts []string
	for part := range strings.SplitSeq(cmd, ";") {
		part = strings.TrimSpace(part)
		if part != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) > 0 && strings.EqualFold(parts[0], "10") {
		parts = parts[1:]
		if len(parts) > 0 {
			parts = parts[1:]
		}
	}
	if len(parts) > 0 && isMomentaryState(parts[len(parts)-1]) {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// commandMatchesSensor is true when the first address field is the sensor id.
// A later field is the channel and must be the sensor switch. The "10" prefix
// and the protocol name do not count, so an ID of 10 does not echo every switch.
func commandMatchesSensor(cmd, id, switchID string) bool {
	addrs := commandAddressFields(cmd)
	if len(addrs) == 0 || strings.TrimSpace(id) == "" {
		return false
	}
	if !strings.EqualFold(addrs[0], id) {
		return false
	}
	if len(addrs) == 1 {
		return true
	}
	switchID = strings.TrimSpace(switchID)
	if switchID == "" {
		return false
	}
	for _, field := range addrs[1:] {
		if !strings.EqualFold(field, switchID) {
			return false
		}
	}
	return true
}

func commandHasField(cmd, field string) bool {
	field = strings.TrimSpace(field)
	if field == "" {
		return false
	}
	for part := range strings.SplitSeq(cmd, ";") {
		if strings.EqualFold(strings.TrimSpace(part), field) {
			return true
		}
	}
	return false
}

func (p *Publisher) discoveryTopic(key, val any) string {
	if d, ok := val.(haDiscovered); ok {
		return d.topic
	}
	id, ok := key.(string)
	if !ok || p == nil || p.opts == nil {
		return ""
	}
	p.haMu.Lock()
	prefix := p.haPrefix
	p.haMu.Unlock()
	if prefix == "" {
		prefix = p.opts.Publish.HADiscoveryPrefix
	}
	if prefix == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(id, "gorflink_btn_"):
		return prefix + "/button/" + id + "/config"
	case strings.HasPrefix(id, "gorflink_sw_"):
		return prefix + "/switch/" + id + "/config"
	default:
		return ""
	}
}

// PurgeHADiscovery publishes an empty retained config for every discovery topic
// this process still remembers, then drops those cache entries.
// A failed publish leaves the entry in place so a later purge can retry.
func (p *Publisher) PurgeHADiscovery() int {
	if p == nil {
		return 0
	}
	cleared := 0
	p.discovered.Range(func(key, val any) bool {
		topic := p.discoveryTopic(key, val)
		if topic == "" {
			return true
		}
		ctx := p.runCtx
		if ctx == nil {
			ctx = context.Background()
		}
		if err := p.publishRaw(ctx, topic, "", p.opts.mqttQoS(), true); err != nil {
			log.Warn("HA discovery purge skipped", "topic", topic, "err", err)
			return true
		}
		p.discovered.Delete(key)
		cleared++
		return true
	})
	if cleared > 0 {
		leftover := false
		p.discovered.Range(func(_, _ any) bool {
			leftover = true
			return false
		})
		if !leftover {
			p.haMu.Lock()
			p.haPrefix = ""
			p.haBtnN = 0
			p.haSwN = 0
			p.haMu.Unlock()
		}
	}
	log.Info("HA discovery purge finished", "cleared", cleared)
	return cleared
}

// ClearHADiscovery marks discovery stale so the next frame publishes it again.
// Remembered sensor topics stay, so a changed component can still clear the old config.
// Actuator button/switch discovery is republished immediately.
func (p *Publisher) ClearHADiscovery() {
	p.markDiscoveryStale()
	log.Info("HA discovery marked for republish; entities will be rediscovered on next sensor messages")
	p.publishHAActuators(p.runCtx)
}

// Health returns a snapshot identical to the MQTT health topic payload.
func (p *Publisher) Health() HealthSnapshot {
	return p.snapshot()
}

// SensorsLastSeen returns a copy of HWID → last-seen RFC3339 timestamps.
func (p *Publisher) SensorsLastSeen() map[string]string {
	p.sensorSeenMu.Lock()
	defer p.sensorSeenMu.Unlock()
	out := make(map[string]string, len(p.sensorSeen))
	for k, t := range p.sensorSeen {
		out[k] = t.Format(time.RFC3339)
	}
	return out
}

func (p *Publisher) appendRawLine(device, msg string) {
	entry := rawLineEntry{At: time.Now().Format(time.RFC3339Nano), Device: strings.TrimSpace(device), Message: msg}
	if p.rawCap > 0 {
		p.rawMu.Lock()
		if len(p.rawLines) >= p.rawCap {
			copy(p.rawLines, p.rawLines[1:])
			p.rawLines[len(p.rawLines)-1] = entry
		} else {
			p.rawLines = append(p.rawLines, entry)
		}
		p.rawMu.Unlock()
	}
	p.rawHookMu.Lock()
	hook := p.rawHook
	p.rawHookMu.Unlock()
	if hook != nil {
		hook(entry)
	}
}

// SetRawLineHook registers a callback invoked for every serial line (HTTP WebSocket live feed).
func (p *Publisher) SetRawLineHook(fn func(rawLineEntry)) {
	p.rawHookMu.Lock()
	p.rawHook = fn
	p.rawHookMu.Unlock()
}

// SetSensorHook registers a callback after successful sumJson payload build.
func (p *Publisher) SetSensorHook(fn func(hwid string, sumJSON []byte)) {
	p.sensorHookMu.Lock()
	p.sensorHook = fn
	p.sensorHookMu.Unlock()
}

// RecentRawLines returns a copy of the recent serial line buffer (oldest first).
func (p *Publisher) RecentRawLines() []rawLineEntry {
	p.rawMu.Lock()
	defer p.rawMu.Unlock()
	out := make([]rawLineEntry, len(p.rawLines))
	copy(out, p.rawLines)
	return out
}

func (p *Publisher) appendSensorSample(sd *SensorData, fields map[string]string) {
	if p.sampleCap <= 0 || sd == nil {
		return
	}
	fcopy := make(map[string]string, len(fields))
	maps.Copy(fcopy, fields)
	entry := sensorSample{
		At:     time.Now().Format(time.RFC3339Nano),
		HWID:   sd.TopicID(),
		Model:  sd.Model,
		ID:     sd.Id,
		Name:   sd.FriendlyName,
		Fields: fcopy,
	}
	p.sampleMu.Lock()
	defer p.sampleMu.Unlock()
	if len(p.samples) >= p.sampleCap {
		copy(p.samples, p.samples[1:])
		p.samples[len(p.samples)-1] = entry
		return
	}
	p.samples = append(p.samples, entry)
}

// RecentSensorSamples returns recent published sensor samples (oldest first).
func (p *Publisher) RecentSensorSamples() []sensorSample {
	p.sampleMu.Lock()
	defer p.sampleMu.Unlock()
	out := make([]sensorSample, len(p.samples))
	copy(out, p.samples)
	return out
}

func (p *Publisher) Disconnect() {
	if p.cm != nil {
		// Each retained offline gets its own deadline. One slow publish must
		// not expire the context before serial/status is cleared.
		topics := []string{p.opts.statusTopic(), p.opts.serialStatusTopic()}
		p.serialMu.Lock()
		if len(p.serialPorts) > 1 || len(p.opts.serialDeviceList()) > 1 {
			for i := range p.serialPorts {
				topics = append(topics, p.opts.serialDeviceStatusTopic(p.serialPorts[i].Device))
				p.serialPorts[i].Connected = false
			}
			topics = append(topics, p.opts.serialCommandStatusTopic())
		}
		p.serialMu.Unlock()
		p.serialUp.Store(false)
		for _, topic := range topics {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = p.publishRaw(ctx, topic, statusOffline, p.opts.mqttQoS(), true)
			cancel()
		}
		p.offlineSensorAvailability()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = p.publishRaw(ctx, p.opts.healthTopic(), "", p.opts.mqttQoS(), true)
		cancel()
	}
	// Cancel the client only after the offline retained publishes.
	if p.mqttCancel != nil {
		p.mqttCancel()
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

// offlineSensorAvailability clears retained per-sensor availability. Status
// topics go offline on a clean stop; these topics have no expiry, so a later
// start would mark the entity available before any new frame arrived.
func (p *Publisher) offlineSensorAvailability() {
	if p == nil || p.opts == nil || !p.opts.Publish.HASensorAvailability {
		return
	}
	ids := map[string]struct{}{}
	p.sensorSeenMu.Lock()
	for id := range p.sensorSeen {
		if id != "" {
			ids[id] = struct{}{}
		}
	}
	p.sensorSeenMu.Unlock()
	p.availMu.Lock()
	for id := range p.availMiss {
		if id != "" {
			ids[id] = struct{}{}
		}
	}
	p.availMu.Unlock()
	if len(ids) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for id := range ids {
		if ctx.Err() != nil {
			return
		}
		_ = p.publishSensorAvailability(ctx, id, statusOffline)
	}
}
