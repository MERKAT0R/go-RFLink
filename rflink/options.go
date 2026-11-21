/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/goccy/go-json"
)

// Temperature unit constants.
const (
	TempUnitC   = "C"
	TempUnitF   = "F"
	TempUnitRaw = "RAW"
)

// Options stores the options needed to communicate with RFLink and the
// message queue.
type Options struct {
	LogLevel string `env:"LOG_LEVEL" envDefault:"info"`

	Publish struct {
		Host                   string `env:"PUBLISH_HOST" envDefault:"localhost:1883"`
		Scheme                 string `env:"PUBLISH_SCHEME" envDefault:"tcp"`
		MqttUsername           string `env:"PUBLISH_MQTT_USERNAME" envDefault:"username"`
		MqttPassword           string `env:"PUBLISH_MQTT_PASSWORD" envDefault:"password"`
		InfinityReconnect      bool   `env:"PUBLISH_INFINITY_RECONNECT" envDefault:"true"`
		ConnectRetryInterval   int    `env:"PUBLISH_CONNECT_RETRY_INTERVAL" envDefault:"5"`
		ClientID               string `env:"PUBLISH_CLIENT_ID" envDefault:"gorflink"`
		Topic                  string `env:"PUBLISH_TOPIC" envDefault:"rflink"`
		QoS                    int    `env:"PUBLISH_QOS" envDefault:"0"`
		CleanSession           bool   `env:"PUBLISH_CLEAN_SESSION" envDefault:"true"`
		Retain                 bool   `env:"PUBLISH_RETAIN" envDefault:"false"`
		KeepAlive              int    `env:"PUBLISH_KEEPALIVE" envDefault:"30"`
		SessionExpiry          int    `env:"PUBLISH_SESSION_EXPIRY" envDefault:"0"`
		TLSCAFile              string `env:"PUBLISH_TLS_CA_FILE"`
		TLSCertFile            string `env:"PUBLISH_TLS_CERT_FILE"`
		TLSKeyFile             string `env:"PUBLISH_TLS_KEY_FILE"`
		TLSInsecureSkipVerify  bool   `env:"PUBLISH_TLS_INSECURE_SKIP_VERIFY" envDefault:"false"`
		LWTTopic               string `env:"PUBLISH_LWT_TOPIC"`
		LWTMessage             string `env:"PUBLISH_LWT_MESSAGE"`
		LWTQoS                 int    `env:"PUBLISH_LWT_QOS" envDefault:"0"`
		LWTRetain              bool   `env:"PUBLISH_LWT_RETAIN" envDefault:"false"`
		CmdTopic               string `env:"PUBLISH_CMD_TOPIC"`
		HomeAssistantDiscovery bool   `env:"PUBLISH_HOME_ASSISTANT_DISCOVERY" envDefault:"false"`
		HADiscoveryPrefix      string `env:"PUBLISH_HA_DISCOVERY_PREFIX" envDefault:"homeassistant"`
		// FriendlyNames maps HWID → human-readable name.
		// Format: "HWID1:Living Room,HWID2:Kitchen"
		FriendlyNames string `env:"PUBLISH_FRIENDLY_NAMES"`
		// HWIDMap maps RFLink ID (or MODEL_ID) → canonical HWID used in MQTT topics.
		// Format: "2A01:LIVING_TEMP,BBQ_1234:GARDEN_SENSOR"
		HWIDMap string `env:"PUBLISH_HWID_MAP"`
		// HealthLogIntervalSec — how often to log and publish health metrics (0 = disabled).
		HealthLogIntervalSec int `env:"PUBLISH_HEALTH_LOG_INTERVAL_SEC" envDefault:"300"`
		// PendingTTLSec — max age of a buffered latest sensor value while MQTT is down (0 = disable buffer).
		PendingTTLSec int `env:"PUBLISH_PENDING_TTL_SEC" envDefault:"60"`
		// TemperatureUnit: C (default), F, or RAW (hex tenths as received from RFLink).
		TemperatureUnit string `env:"PUBLISH_TEMPERATURE_UNIT" envDefault:"C"`
		// CommandWhitelist — comma-separated allowed prefixes, e.g. "10;,11;,20;".
		// Empty = allow all commands.
		CommandWhitelist string `env:"PUBLISH_COMMAND_WHITELIST"`
		// CommandRateLimit — max commands per second written to serial (0 = unlimited).
		CommandRateLimit int `env:"PUBLISH_COMMAND_RATE_LIMIT" envDefault:"5"`
		// DedupWindowMs — drop identical RFLink messages within this window (0 = disabled).
		DedupWindowMs int `env:"PUBLISH_DEDUP_WINDOW_MS" envDefault:"3000"`
		// MinIntervalMs — minimum gap between publishes of the same HWID (0 = off).
		MinIntervalMs int `env:"PUBLISH_MIN_INTERVAL_MS" envDefault:"0"`
		// Throttle — per-HWID override, "HWID:milliseconds,HWID2:500".
		Throttle string `env:"PUBLISH_THROTTLE"`
		// HASensorAvailability adds a per-sensor availability topic. Off keeps the
		// existing global status + serial/status pair unchanged.
		HASensorAvailability bool `env:"PUBLISH_HA_SENSOR_AVAILABILITY" envDefault:"false"`
		// HASensorStaleSec — publish offline on that topic after this silence (0 = never).
		HASensorStaleSec int `env:"PUBLISH_HA_SENSOR_STALE_SEC" envDefault:"0"`
		// HADeviceTriggers publishes device_automation triggers beside sw_* binary sensors.
		HADeviceTriggers bool `env:"PUBLISH_HA_DEVICE_TRIGGERS" envDefault:"true"`
		// IgnoreList — comma-separated HWID and/or model names to skip publishing.
		// Matches against sensor HWID and Model (case-insensitive).
		IgnoreList string `env:"PUBLISH_IGNORE_LIST" envDefault:"Keeloq"`
		// CommandMaxLen — max bytes for a normalized command (0 = default 256).
		CommandMaxLen int `env:"PUBLISH_COMMAND_MAX_LEN" envDefault:"256"`
		// HAButtons — MQTT HA button entities: "Label:10;cmd;,Label2:10;cmd2;"
		HAButtons string `env:"PUBLISH_HA_BUTTONS"`
		// HASwitches — MQTT HA switch entities: "Label:10;ON;:10;OFF;,Label2:on:off"
		HASwitches string `env:"PUBLISH_HA_SWITCHES"`
	}

	Serial struct {
		Device string `env:"SERIAL_DEVICE" envDefault:"/dev/ttyUSB0"`
		// Devices — extra gateways, comma-separated. SERIAL_DEVICE stays the primary.
		// Each entry is a serial path or tcp://host:port (ESP32 / networked RFLink).
		Devices string `env:"SERIAL_DEVICES"`
		// CommandDevices — which gateways receive commands. Empty sends to every connected port.
		CommandDevices       string `env:"SERIAL_COMMAND_DEVICES"`
		Baud                 int    `env:"SERIAL_BAUD" envDefault:"57600"`
		ReadTimeoutSec       int    `env:"SERIAL_READ_TIMEOUT_SEC" envDefault:"10"`
		ReconnectMinDelaySec int    `env:"SERIAL_RECONNECT_MIN_DELAY_SEC" envDefault:"3"`
		ReconnectMaxDelaySec int    `env:"SERIAL_RECONNECT_MAX_DELAY_SEC" envDefault:"60"`
		// WatchdogSec — warn + health flag when no serial data for this many seconds (0 = disabled).
		WatchdogSec int `env:"SERIAL_WATCHDOG_SEC" envDefault:"120"`
		// PingIntervalSec — send 10;PING; this often (0 = disabled). Confirms RFLink firmware is responsive.
		PingIntervalSec int `env:"SERIAL_PING_INTERVAL_SEC" envDefault:"60"`
		// PingTimeoutSec — time to wait for PONG after PING before counting a failure.
		PingTimeoutSec int `env:"SERIAL_PING_TIMEOUT_SEC" envDefault:"5"`
		// PingFailThreshold — consecutive missed PONGs before marking RFLink unresponsive.
		PingFailThreshold int `env:"SERIAL_PING_FAIL_THRESHOLD" envDefault:"3"`
	}

	// HTTP — local control API + optional embedded Web GUI (empty Listen = disabled).
	HTTP struct {
		// Listen address, e.g. ":8080" or "127.0.0.1:8080". Empty disables HTTP.
		Listen string `env:"HTTP_LISTEN"`
		// AuthToken — GUI login only (session). Not accepted as long-lived API Bearer.
		AuthToken string `env:"HTTP_AUTH_TOKEN"`
		// GUI — serve embedded status UI at / (default true when Listen is set).
		GUI bool `env:"HTTP_GUI" envDefault:"true"`
		// RawBufferSize — ring buffer of recent serial lines for GET /api/v1/rflink/raw (0 = default 100).
		RawBufferSize int `env:"HTTP_RAW_BUFFER_SIZE" envDefault:"100"`
		// SensorBufferSize — recent sensor samples for GET /api/v1/sensors (0 = default 50).
		SensorBufferSize int `env:"HTTP_SENSOR_BUFFER_SIZE" envDefault:"50"`
		// CORSOrigins — comma-separated allowed Origins for CORS (empty = CORS disabled).
		// Example: "https://ha.example.com,http://localhost:8123"
		CORSOrigins string `env:"HTTP_CORS_ORIGINS"`
		// TrustedProxies — CIDRs/IPs allowed to set X-Forwarded-For / X-Real-IP (empty = never trust).
		// Example: "10.0.0.0/8,192.168.0.0/16,127.0.0.1"
		TrustedProxies string `env:"HTTP_TRUSTED_PROXIES"`
		// ReadOnly — when true, reject command / config / rediscover mutations (default false).
		ReadOnly bool `env:"HTTP_READ_ONLY" envDefault:"false"`
	}

	// RuntimeConfigFile — overlay for GUI-editable settings (default data/runtime.json).
	RuntimeConfigFile string `env:"RUNTIME_CONFIG_FILE" envDefault:"data/runtime.json"`

	// APIAuthTokens — comma-separated name:TOKEN[:expire] for machine API clients only.
	// Expire: never | 30m | 24h | RFC3339 date. GUI login uses HTTP_AUTH_TOKEN, not these.
	APIAuthTokens string `env:"API_AUTH_TOKEN"`
	// APITokenPepper — optional HMAC pepper for API token hashes (empty = plain SHA-256).
	APITokenPepper string `env:"API_TOKEN_PEPPER"`

	// WebhooksJSON — optional JSON array of webhooks (same schema as runtime webhooks).
	WebhooksJSON string `env:"WEBHOOKS_JSON"`

	// parsed at GetOptions time
	friendlyNameMap  map[string]string
	hwidMap          map[string]string // key: upper ID or MODEL_ID → canonical HWID
	commandWhitelist []string          // empty = allow all
	ignoreSet        map[string]struct{}
	haButtons        []haActuatorButton
	haSwitches       []haActuatorSwitch
	apiTokens        []apiToken // from env and/or runtime file
	webhooks         []RuntimeWebhook
	throttleBy       map[string]time.Duration
	runtimeMu        sync.RWMutex
}

// haActuatorButton is a Home Assistant button discovery entity.
type haActuatorButton struct {
	Name    string
	Command string
}

// haActuatorSwitch is a Home Assistant switch discovery entity (optimistic).
type haActuatorSwitch struct {
	Name       string
	CommandOn  string
	CommandOff string
}

// GetOptions reads the options from the environment and returns an Options struct.
func GetOptions() (*Options, error) {
	loadDotEnv()
	var opts Options

	if err := env.Parse(&opts); err != nil {
		return nil, err
	}

	opts.Publish.Topic = cleanMQTTTopic(opts.Publish.Topic)
	if opts.Publish.Topic == "" {
		opts.Publish.Topic = "rflink"
	}
	opts.Publish.CmdTopic = cleanMQTTTopic(opts.Publish.CmdTopic)
	if opts.Publish.CmdTopic == "" {
		opts.Publish.CmdTopic = opts.Publish.Topic + "/send"
	}
	opts.Publish.LWTTopic = cleanMQTTTopic(opts.Publish.LWTTopic)
	opts.Publish.HADiscoveryPrefix = cleanMQTTTopic(opts.Publish.HADiscoveryPrefix)
	if opts.Publish.HADiscoveryPrefix == "" {
		opts.Publish.HADiscoveryPrefix = "homeassistant"
	}

	if err := validateCmdTopic(opts.Publish.Topic, opts.Publish.CmdTopic); err != nil {
		return nil, err
	}

	if opts.Publish.QoS < 0 || opts.Publish.QoS > 2 {
		return nil, fmt.Errorf("PUBLISH_QOS must be 0, 1 or 2, got %d", opts.Publish.QoS)
	}

	if opts.Publish.LWTQoS < 0 || opts.Publish.LWTQoS > 2 {
		return nil, fmt.Errorf("PUBLISH_LWT_QOS must be 0, 1 or 2, got %d", opts.Publish.LWTQoS)
	}

	if opts.Publish.KeepAlive <= 0 {
		opts.Publish.KeepAlive = 30
	}
	if opts.Publish.KeepAlive > 65535 {
		opts.Publish.KeepAlive = 65535
	}

	if opts.Publish.SessionExpiry < 0 {
		opts.Publish.SessionExpiry = 0
	}
	if opts.Publish.SessionExpiry > 4294967295 {
		opts.Publish.SessionExpiry = 4294967295
	}

	if opts.Serial.Baud <= 0 {
		opts.Serial.Baud = 57600
	}

	if opts.Serial.ReadTimeoutSec <= 0 {
		opts.Serial.ReadTimeoutSec = 10
	}
	if opts.Serial.ReadTimeoutSec > 120 {
		opts.Serial.ReadTimeoutSec = 120
	}

	if opts.Serial.ReconnectMinDelaySec <= 0 {
		opts.Serial.ReconnectMinDelaySec = 3
	}
	// time.Duration overflows past ~292 years; a day is already a long serial backoff.
	if opts.Serial.ReconnectMinDelaySec > 86400 {
		opts.Serial.ReconnectMinDelaySec = 86400
	}
	if opts.Serial.ReconnectMaxDelaySec < opts.Serial.ReconnectMinDelaySec {
		opts.Serial.ReconnectMaxDelaySec = opts.Serial.ReconnectMinDelaySec
	}
	if opts.Serial.ReconnectMaxDelaySec > 86400 {
		opts.Serial.ReconnectMaxDelaySec = 86400
	}

	if opts.Publish.ConnectRetryInterval <= 0 {
		opts.Publish.ConnectRetryInterval = 5
	}
	if opts.Publish.ConnectRetryInterval > 86400 {
		opts.Publish.ConnectRetryInterval = 86400
	}

	if opts.Publish.PendingTTLSec < 0 {
		opts.Publish.PendingTTLSec = 0
	}
	if opts.Publish.PendingTTLSec > 86400 {
		opts.Publish.PendingTTLSec = 86400
	}
	if opts.Publish.HealthLogIntervalSec > 86400 {
		opts.Publish.HealthLogIntervalSec = 86400
	}
	if opts.Publish.DedupWindowMs > 86400000 {
		opts.Publish.DedupWindowMs = 86400000
	}

	if opts.Publish.CommandRateLimit < 0 {
		opts.Publish.CommandRateLimit = 0
	}

	if opts.Publish.DedupWindowMs < 0 {
		opts.Publish.DedupWindowMs = 0
	}
	if opts.Publish.MinIntervalMs < 0 {
		opts.Publish.MinIntervalMs = 0
	}
	if opts.Publish.MinIntervalMs > 86400000 {
		opts.Publish.MinIntervalMs = 86400000
	}
	if opts.Publish.HASensorStaleSec < 0 {
		opts.Publish.HASensorStaleSec = 0
	}
	if opts.Publish.HASensorStaleSec > 86400 {
		opts.Publish.HASensorStaleSec = 86400
	}
	opts.throttleBy = parseThrottleMap(opts.Publish.Throttle)

	if opts.Serial.PingIntervalSec < 0 {
		opts.Serial.PingIntervalSec = 0
	}
	if opts.Serial.PingIntervalSec > 86400 {
		opts.Serial.PingIntervalSec = 86400
	}
	if opts.Serial.PingTimeoutSec < 0 {
		opts.Serial.PingTimeoutSec = 5
	}
	if opts.Serial.PingTimeoutSec > 86400 {
		opts.Serial.PingTimeoutSec = 86400
	}
	if opts.Serial.WatchdogSec > 86400 {
		opts.Serial.WatchdogSec = 86400
	}
	if opts.Serial.PingFailThreshold < 0 {
		opts.Serial.PingFailThreshold = 3
	}

	unit := strings.ToUpper(strings.TrimSpace(opts.Publish.TemperatureUnit))
	switch unit {
	case TempUnitC, TempUnitF, TempUnitRaw:
		opts.Publish.TemperatureUnit = unit
	case "":
		opts.Publish.TemperatureUnit = TempUnitC
	default:
		return nil, fmt.Errorf("PUBLISH_TEMPERATURE_UNIT must be C, F or RAW, got %q", opts.Publish.TemperatureUnit)
	}

	opts.friendlyNameMap = parseColonMap(opts.Publish.FriendlyNames)
	opts.hwidMap = parseColonMap(opts.Publish.HWIDMap)
	opts.commandWhitelist = parseCommandWhitelist(opts.Publish.CommandWhitelist)
	opts.ignoreSet = parseIgnoreList(opts.Publish.IgnoreList)
	opts.haButtons = parseHAButtons(opts.Publish.HAButtons)
	opts.haSwitches = parseHASwitches(opts.Publish.HASwitches)

	if opts.Publish.CommandMaxLen <= 0 {
		opts.Publish.CommandMaxLen = 256
	}

	if opts.HTTP.RawBufferSize <= 0 {
		opts.HTTP.RawBufferSize = 100
	}
	if opts.HTTP.SensorBufferSize <= 0 {
		opts.HTTP.SensorBufferSize = 50
	}

	// Full config in JSON then GORFLINK_DEBUGSHOWPARSEDCONFIGINLOGINSECURE mode - VERY INSECURE!
	if debugShowParsedConfigInLogINSECURE() {
		b, _ := json.MarshalIndent(&opts, "", "  ")
		fmt.Println(string(b))
	}

	return &opts, nil
}

// cleanMQTTTopic drops surrounding space and slashes so "rflink/" does not
// become a topic with an empty segment.
func cleanMQTTTopic(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "/")
	for strings.Contains(s, "//") {
		s = strings.ReplaceAll(s, "//", "/")
	}
	return s
}

// validateCmdTopic ensures the command topic cannot form a feedback loop with sensor topics.
func validateCmdTopic(topic, cmdTopic string) error {
	topic = strings.TrimSuffix(topic, "/")
	cmdTopic = strings.TrimSuffix(cmdTopic, "/")
	if cmdTopic == topic {
		return fmt.Errorf("PUBLISH_CMD_TOPIC must not equal PUBLISH_TOPIC (%q)", topic)
	}
	// cmd must not be a parent of sensor tree: "rflink" must not be cmd when topic is "rflink/sensors"
	// and cmd must not equal or be under the sensor prefix in a way that receives our own publishes.
	if strings.HasPrefix(topic+"/", cmdTopic+"/") {
		return fmt.Errorf("PUBLISH_CMD_TOPIC %q must not be a parent of PUBLISH_TOPIC %q", cmdTopic, topic)
	}
	if strings.HasPrefix(cmdTopic+"/", topic+"/") {
		// cmd is a child of topic (e.g. rflink/send) — that is the intended default.
		// The status, serial, gateway, health, and result topics are published by
		// this process. Subscribing to one of them would treat our own retained
		// status as a command and let a command overwrite availability.
		rest := strings.TrimPrefix(cmdTopic, topic+"/")
		switch {
		case rest == "status" || rest == "health" || strings.HasPrefix(rest, "serial/") || rest == "serial" ||
			strings.HasPrefix(rest, "gateway/") || rest == "gateway" ||
			strings.HasPrefix(rest, "cmd/") || rest == "cmd" ||
			strings.HasPrefix(rest, "admin/") || rest == "admin":
			return fmt.Errorf("PUBLISH_CMD_TOPIC %q overlaps a topic this process publishes", cmdTopic)
		}
		return nil
	}
	return nil
}

// parseColonMap parses "KEY1:Value One,KEY2:Value2" into a map with upper-case keys.
func parseColonMap(raw string) map[string]string {
	out := make(map[string]string)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out
	}
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, ":", 2)
		if len(kv) != 2 {
			continue
		}
		key := sanitizeTopicSegment(strings.TrimSpace(kv[0]))
		val := strings.TrimSpace(kv[1])
		if key != "" && val != "" {
			out[strings.ToUpper(key)] = val
		}
	}
	return out
}

func parseCommandWhitelist(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// Prefixes are whole fields. "10" is "10;", and "1" must not allow "10;" or "11;".
		if !strings.HasSuffix(part, ";") {
			part += ";"
		}
		out = append(out, part)
	}
	return out
}

// FriendlyNameFor returns a configured friendly name for the given HWID, or "".
func (o *Options) FriendlyNameFor(hwid string) string {
	o.runtimeMu.RLock()
	defer o.runtimeMu.RUnlock()
	if o.friendlyNameMap == nil {
		return ""
	}
	return o.friendlyNameMap[strings.ToUpper(hwid)]
}

// CanonicalHWID resolves an optional fixed HWID for a sensor ID / default HWID.
// Lookup order: exact defaultHWID, then raw ID.
func (o *Options) CanonicalHWID(id, defaultHWID string) string {
	o.runtimeMu.RLock()
	defer o.runtimeMu.RUnlock()
	if o.hwidMap == nil {
		return defaultHWID
	}
	if v, ok := o.hwidMap[strings.ToUpper(defaultHWID)]; ok {
		return v
	}
	if v, ok := o.hwidMap[strings.ToUpper(id)]; ok {
		return v
	}
	// Map keys are stored in the same sanitized form as MQTT topic segments.
	if san := strings.ToUpper(sanitizeTopicSegment(strings.TrimSpace(id))); san != "" {
		if v, ok := o.hwidMap[san]; ok {
			return v
		}
	}
	return defaultHWID
}

// CommandAllowed returns true if payload is allowed by the whitelist.
func (o *Options) CommandAllowed(payload string) bool {
	if len(o.commandWhitelist) == 0 {
		return true
	}
	trimmed := strings.TrimSpace(payload)
	for _, prefix := range o.commandWhitelist {
		if len(prefix) > 0 && len(trimmed) >= len(prefix) && strings.EqualFold(trimmed[:len(prefix)], prefix) {
			return true
		}
	}
	return false
}

// NormalizeCommand trims whitespace and trailing CR/LF, enforces trailing ';',
// max length, and strict RFLink command shape (NN;Name;…;).
func (o *Options) NormalizeCommand(payload string) (string, error) {
	cmd := strings.TrimSpace(payload)
	cmd = strings.TrimRight(cmd, "\r\n")
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return "", fmt.Errorf("empty command")
	}
	if !strings.HasSuffix(cmd, ";") {
		cmd += ";"
	}
	maxLen := o.Publish.CommandMaxLen
	if maxLen <= 0 {
		maxLen = 256
	}
	if len(cmd) > maxLen {
		return "", fmt.Errorf("command too long (%d > %d)", len(cmd), maxLen)
	}
	if err := ValidateRFLinkCommand(cmd); err != nil {
		return "", err
	}
	return cmd, nil
}

// SensorIgnored reports whether this sensor should be dropped.
func (o *Options) SensorIgnored(model, hwid string) bool {
	o.runtimeMu.RLock()
	defer o.runtimeMu.RUnlock()
	if len(o.ignoreSet) == 0 {
		return false
	}
	if _, ok := o.ignoreSet[strings.ToUpper(strings.TrimSpace(hwid))]; ok {
		return true
	}
	if _, ok := o.ignoreSet[strings.ToUpper(strings.TrimSpace(model))]; ok {
		return true
	}
	return false
}

func parseIgnoreList(raw string) map[string]struct{} {
	out := make(map[string]struct{})
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.ToUpper(sanitizeTopicSegment(strings.TrimSpace(part)))
		if part != "" && part != "_" {
			out[part] = struct{}{}
		}
	}
	return out
}

func parseHAButtons(raw string) []haActuatorButton {
	var out []haActuatorButton
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out
	}
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, ":", 2)
		if len(kv) != 2 {
			continue
		}
		name := strings.TrimSpace(kv[0])
		cmd := strings.TrimSpace(kv[1])
		if name == "" || cmd == "" {
			continue
		}
		if !strings.HasSuffix(cmd, ";") {
			cmd += ";"
		}
		out = append(out, haActuatorButton{Name: name, Command: cmd})
	}
	return out
}

func parseHASwitches(raw string) []haActuatorSwitch {
	var out []haActuatorSwitch
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out
	}
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// Label:on_cmd:off_cmd — split into 3 parts max by ':' is wrong because cmds contain ';'
		// Format uses ':' separators: Name:ONCMD:OFFCMD where cmds themselves use ';'
		kv := strings.SplitN(part, ":", 3)
		if len(kv) != 3 {
			continue
		}
		name := strings.TrimSpace(kv[0])
		on := strings.TrimSpace(kv[1])
		off := strings.TrimSpace(kv[2])
		if name == "" || on == "" || off == "" {
			continue
		}
		if !strings.HasSuffix(on, ";") {
			on += ";"
		}
		if !strings.HasSuffix(off, ";") {
			off += ";"
		}
		out = append(out, haActuatorSwitch{Name: name, CommandOn: on, CommandOff: off})
	}
	return out
}

func (o *Options) mqttQoS() byte {
	return byte(o.Publish.QoS)
}

func (o *Options) lwtQoS() byte {
	return byte(o.Publish.LWTQoS)
}

func (o *Options) mqttKeepAlive() uint16 {
	ka := o.Publish.KeepAlive
	if ka <= 0 {
		ka = 30
	}
	if ka > 65535 {
		ka = 65535
	}
	return uint16(ka)
}

func (o *Options) mqttSessionExpiry() uint32 {
	se := o.Publish.SessionExpiry
	if se <= 0 {
		return 0
	}
	if se > 4294967295 {
		se = 4294967295
	}
	return uint32(se)
}

// snapshotHAActuators copies HA discovery settings under runtimeMu.
func (o *Options) snapshotHAActuators() (enabled bool, prefix string, buttons []haActuatorButton, switches []haActuatorSwitch) {
	o.runtimeMu.RLock()
	defer o.runtimeMu.RUnlock()
	enabled = o.Publish.HomeAssistantDiscovery
	prefix = o.Publish.HADiscoveryPrefix
	buttons = append([]haActuatorButton(nil), o.haButtons...)
	switches = append([]haActuatorSwitch(nil), o.haSwitches...)
	return
}

func (o *Options) pendingTTL() time.Duration {
	return time.Duration(o.Publish.PendingTTLSec) * time.Second
}

func (o *Options) healthLogInterval() time.Duration {
	if o.Publish.HealthLogIntervalSec <= 0 {
		return 0
	}
	return time.Duration(o.Publish.HealthLogIntervalSec) * time.Second
}

func (o *Options) serialWatchdog() time.Duration {
	if o.Serial.WatchdogSec <= 0 {
		return 0
	}
	return time.Duration(o.Serial.WatchdogSec) * time.Second
}

func (o *Options) serialPingInterval() time.Duration {
	if o.Serial.PingIntervalSec <= 0 {
		return 0
	}
	return time.Duration(o.Serial.PingIntervalSec) * time.Second
}

func (o *Options) serialPingTimeout() time.Duration {
	sec := o.Serial.PingTimeoutSec
	if sec <= 0 {
		sec = 5
	}
	return time.Duration(sec) * time.Second
}

func (o *Options) serialPingFailThreshold() int {
	n := o.Serial.PingFailThreshold
	if n <= 0 {
		return 3
	}
	return n
}

func (o *Options) dedupWindow() time.Duration {
	if o.Publish.DedupWindowMs <= 0 {
		return 0
	}
	return time.Duration(o.Publish.DedupWindowMs) * time.Millisecond
}

func (o *Options) statusTopic() string {
	return o.Publish.Topic + "/status"
}

func (o *Options) serialStatusTopic() string {
	return o.Publish.Topic + "/serial/status"
}

// serialCommandStatusTopic is online when at least one command target is connected.
// Home Assistant buttons and switches watch it once more than one gateway is configured,
// so one dead board does not mark every control unavailable.
func (o *Options) serialCommandStatusTopic() string {
	return o.Publish.Topic + "/serial/command/status"
}

// serialDeviceStatusTopic is the per-port status used when more than one gateway is configured.
func (o *Options) serialDeviceStatusTopic(device string) string {
	name := sanitizeTopicSegment(device)
	if name == "" {
		name = "port"
	}
	// serial/command/status is the aggregate "any command target is up" topic.
	// A gateway whose name sanitizes to command would publish on that same topic.
	if name == "command" {
		name = "command_port"
	}
	return o.Publish.Topic + "/serial/" + name + "/status"
}

func (o *Options) healthTopic() string {
	return o.Publish.Topic + "/health"
}

func (o *Options) cmdResultTopic() string {
	return o.Publish.Topic + "/cmd/result"
}

func (o *Options) gatewayInfoTopic() string {
	return o.Publish.Topic + "/gateway/info"
}

// gatewayDeviceInfoTopic is one board's version and PONG when several gateways are configured.
func (o *Options) gatewayDeviceInfoTopic(device string) string {
	name := sanitizeTopicSegment(device)
	if name == "" {
		name = "port"
	}
	return o.Publish.Topic + "/gateway/" + name + "/info"
}

// gatewayInfoTopicFor keeps the single shared topic while one gateway is configured.
func (o *Options) gatewayInfoTopicFor(device string) string {
	if o == nil {
		return ""
	}
	device = strings.TrimSpace(device)
	if device == "" || len(o.serialDeviceList()) <= 1 {
		return o.gatewayInfoTopic()
	}
	return o.gatewayDeviceInfoTopic(device)
}

func (o *Options) haRediscoverTopic() string {
	return o.Publish.Topic + "/admin/rediscover"
}

func (o *Options) temperatureUnit() string {
	return o.Publish.TemperatureUnit
}

func (o *Options) serialReadTimeout() time.Duration {
	return time.Duration(o.Serial.ReadTimeoutSec) * time.Second
}

func (o *Options) serialReconnectMinDelay() time.Duration {
	sec := o.Serial.ReconnectMinDelaySec
	if sec <= 0 {
		sec = 3
	}
	if sec > 86400 {
		sec = 86400
	}
	return time.Duration(sec) * time.Second
}

func (o *Options) serialReconnectMaxDelay() time.Duration {
	minDelay := o.serialReconnectMinDelay()
	sec := o.Serial.ReconnectMaxDelaySec
	if sec <= 0 {
		return minDelay
	}
	if sec > 86400 {
		sec = 86400
	}
	maxDelay := time.Duration(sec) * time.Second
	if maxDelay < minDelay {
		return minDelay
	}
	return maxDelay
}

func (o *Options) mqttConnectRetryInterval() time.Duration {
	return time.Duration(o.Publish.ConnectRetryInterval) * time.Second
}

// serialDeviceList is the primary SERIAL_DEVICE plus SERIAL_DEVICES, without duplicates.
func (o *Options) serialDeviceList() []string {
	if o == nil {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		if _, ok := seen[strings.ToLower(s)]; ok {
			return
		}
		seen[strings.ToLower(s)] = struct{}{}
		out = append(out, s)
	}
	add(o.Serial.Device)
	for part := range strings.SplitSeq(o.Serial.Devices, ",") {
		add(part)
	}
	return out
}

// extraSerialDevices is every gateway except the primary reader.
// An empty SERIAL_DEVICE means there is no primary, so the whole list is extra.
func (o *Options) extraSerialDevices() []string {
	if o == nil {
		return nil
	}
	all := o.serialDeviceList()
	if strings.TrimSpace(o.Serial.Device) == "" {
		return all
	}
	if len(all) < 2 {
		return nil
	}
	return all[1:]
}

// commandDeviceAllow reports whether a gateway should receive commands.
// An empty SERIAL_COMMAND_DEVICES list means every port.
func (o *Options) commandDeviceAllow(name string) bool {
	if o == nil {
		return true
	}
	o.runtimeMu.RLock()
	raw := o.Serial.CommandDevices
	o.runtimeMu.RUnlock()
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true
	}
	name = strings.TrimSpace(name)
	for _, item := range splitCSV(raw) {
		if strings.EqualFold(item, name) {
			return true
		}
	}
	return false
}

// commandDevicesConfigured is true when a target list is set (env or runtime).
func (o *Options) commandDevicesConfigured() bool {
	if o == nil {
		return false
	}
	o.runtimeMu.RLock()
	raw := o.Serial.CommandDevices
	o.runtimeMu.RUnlock()
	return strings.TrimSpace(raw) != ""
}

// throttleFor is the minimum publish gap for one HWID. A map entry wins over the default.
func (o *Options) throttleFor(hwid string) time.Duration {
	if o == nil {
		return 0
	}
	key := strings.ToUpper(sanitizeTopicSegment(strings.TrimSpace(hwid)))
	o.runtimeMu.RLock()
	defer o.runtimeMu.RUnlock()
	if d, ok := o.throttleBy[key]; ok {
		return d
	}
	if o.Publish.MinIntervalMs <= 0 {
		return 0
	}
	return time.Duration(o.Publish.MinIntervalMs) * time.Millisecond
}

// parseThrottleMap reads "HWID:milliseconds,OTHER:500". Bad pairs are skipped.
func parseThrottleMap(raw string) map[string]time.Duration {
	out := map[string]time.Duration{}
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		hwid, msText, ok := strings.Cut(part, ":")
		if !ok {
			continue
		}
		ms, err := strconv.Atoi(strings.TrimSpace(msText))
		if err != nil || ms <= 0 {
			continue
		}
		if ms > 86400000 {
			ms = 86400000
		}
		key := strings.ToUpper(sanitizeTopicSegment(strings.TrimSpace(hwid)))
		if key == "" {
			continue
		}
		out[key] = time.Duration(ms) * time.Millisecond
	}
	return out
}

// isTLSScheme reports whether the configured MQTT scheme uses TLS.
func (o *Options) isTLSScheme() bool {
	s := strings.ToLower(strings.TrimSpace(o.Publish.Scheme))
	return s == "ssl" || s == "tls" || s == "mqtts" || s == "wss"
}

// LogMQTTTopics writes a table of all fixed MQTT topics used by the application.
func (o *Options) LogMQTTTopics() {
	base := o.Publish.Topic
	rows := []struct{ role, topic string }{
		{"sensor base", base + "/{HWID}/{field}"},
		{"sensor JSON", base + "/{HWID}/sumJson"},
		{"command in (subscribe)", o.Publish.CmdTopic},
		{"command result", o.cmdResultTopic()},
		{"app status", o.statusTopic()},
		{"serial status", o.serialStatusTopic()},
		{"health", o.healthTopic()},
		{"gateway info", o.gatewayInfoTopic()},
	}
	if len(o.serialDeviceList()) > 1 {
		rows = append(rows, struct{ role, topic string }{"gateway info per port", o.Publish.Topic + "/gateway/<name>/info"})
	}
	if o.Publish.HomeAssistantDiscovery {
		rows = append(rows,
			struct{ role, topic string }{"HA discovery", o.Publish.HADiscoveryPrefix + "/{component}/gorflink_{HWID}_{field}/config"},
			struct{ role, topic string }{"HA rediscover (subscribe)", o.haRediscoverTopic()},
		)
	}
	if o.Publish.LWTTopic != "" {
		rows = append(rows, struct{ role, topic string }{"LWT (custom)", o.Publish.LWTTopic})
	} else {
		rows = append(rows, struct{ role, topic string }{"LWT (default)", o.statusTopic()})
	}

	log.Info("mqtt topics in use")
	for _, r := range rows {
		log.Info("mqtt topic", "role", r.role, "topic", r.topic)
	}
}
