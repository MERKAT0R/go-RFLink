/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"fmt"
	"strings"
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
		Device               string `env:"SERIAL_DEVICE" envDefault:"/dev/ttyUSB0"`
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

	// parsed at GetOptions time
	friendlyNameMap  map[string]string
	hwidMap          map[string]string // key: upper ID or MODEL_ID → canonical HWID
	commandWhitelist []string          // empty = allow all
	ignoreSet        map[string]struct{}
	haButtons        []haActuatorButton
	haSwitches       []haActuatorSwitch
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
	var opts Options

	if err := env.Parse(&opts); err != nil {
		return nil, err
	}

	if opts.Publish.CmdTopic == "" {
		opts.Publish.CmdTopic = opts.Publish.Topic + "/send"
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

	if opts.Publish.SessionExpiry < 0 {
		opts.Publish.SessionExpiry = 0
	}

	if opts.Serial.Baud <= 0 {
		opts.Serial.Baud = 57600
	}

	if opts.Serial.ReadTimeoutSec <= 0 {
		opts.Serial.ReadTimeoutSec = 10
	}

	if opts.Serial.ReconnectMinDelaySec <= 0 {
		opts.Serial.ReconnectMinDelaySec = 3
	}

	if opts.Serial.ReconnectMaxDelaySec < opts.Serial.ReconnectMinDelaySec {
		opts.Serial.ReconnectMaxDelaySec = opts.Serial.ReconnectMinDelaySec
	}

	if opts.Publish.ConnectRetryInterval <= 0 {
		opts.Publish.ConnectRetryInterval = 5
	}

	if opts.Publish.PendingTTLSec < 0 {
		opts.Publish.PendingTTLSec = 0
	}

	if opts.Publish.CommandRateLimit < 0 {
		opts.Publish.CommandRateLimit = 0
	}

	if opts.Publish.DedupWindowMs < 0 {
		opts.Publish.DedupWindowMs = 0
	}

	if opts.Serial.PingIntervalSec < 0 {
		opts.Serial.PingIntervalSec = 0
	}
	if opts.Serial.PingTimeoutSec < 0 {
		opts.Serial.PingTimeoutSec = 5
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

	// Full config in JSON then GORFLINK_DEBUGSHOWPARSEDCONFIGINLOGINSECURE mode - VERY INSECURE!
	if debugShowParsedConfigInLogINSECURE() {
		b, _ := json.MarshalIndent(opts, "", "  ")
		fmt.Println(string(b))
	}

	return &opts, nil
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
		// cmd is a child of topic (e.g. rflink/send) — that is the intended default, OK
		// unless it collides with field subtopics; /send is fine.
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
		key := strings.TrimSpace(kv[0])
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
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// FriendlyNameFor returns a configured friendly name for the given HWID, or "".
func (o *Options) FriendlyNameFor(hwid string) string {
	if o.friendlyNameMap == nil {
		return ""
	}
	return o.friendlyNameMap[strings.ToUpper(hwid)]
}

// CanonicalHWID resolves an optional fixed HWID for a sensor ID / default HWID.
// Lookup order: exact defaultHWID, then raw ID.
func (o *Options) CanonicalHWID(id, defaultHWID string) string {
	if o.hwidMap == nil {
		return defaultHWID
	}
	if v, ok := o.hwidMap[strings.ToUpper(defaultHWID)]; ok {
		return v
	}
	if v, ok := o.hwidMap[strings.ToUpper(id)]; ok {
		return v
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
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

// NormalizeCommand trims whitespace and trailing CR/LF, enforces trailing ';',
// rejects empty/too-long payloads.
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
	// basic sanity: RFLink commands start with digits
	if cmd[0] < '0' || cmd[0] > '9' {
		return "", fmt.Errorf("command must start with a digit (got %q)", cmd[:1])
	}
	return cmd, nil
}

// SensorIgnored reports whether this sensor should be dropped.
func (o *Options) SensorIgnored(model, hwid string) bool {
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
		part = strings.ToUpper(strings.TrimSpace(part))
		if part != "" {
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
	return uint16(o.Publish.KeepAlive)
}

func (o *Options) mqttSessionExpiry() uint32 {
	return uint32(o.Publish.SessionExpiry)
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

func (o *Options) healthTopic() string {
	return o.Publish.Topic + "/health"
}

func (o *Options) cmdResultTopic() string {
	return o.Publish.Topic + "/cmd/result"
}

func (o *Options) gatewayInfoTopic() string {
	return o.Publish.Topic + "/gateway/info"
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
	return time.Duration(o.Serial.ReconnectMinDelaySec) * time.Second
}

func (o *Options) serialReconnectMaxDelay() time.Duration {
	return time.Duration(o.Serial.ReconnectMaxDelaySec) * time.Second
}

func (o *Options) mqttConnectRetryInterval() time.Duration {
	return time.Duration(o.Publish.ConnectRetryInterval) * time.Second
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
