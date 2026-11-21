/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"
)

const sumJSONSubtopic = "sumJson"

// knownFieldKeys lists RFLink fields with dedicated parsing logic.
var knownFieldKeys = map[string]struct{}{
	"ID":     {},
	"TEMP":   {},
	"HUM":    {},
	"BAT":    {},
	"SWITCH": {},
	"CMD":    {},
}

// SensorData represents one message received from RFLink.
type SensorData struct {
	Hwid         string            `json:"hwid"`
	RawHWID      string            `json:"-"` // HWID before the canonical map rewrite
	Model        string            `json:"model"`
	Id           string            `json:"id"`
	FriendlyName string            `json:"name,omitempty"`
	Temperature  *float32          `json:"temp,omitempty"` // always Celsius when parsed
	TempRaw      string            `json:"temp_raw,omitempty"`
	Humidity     *uint16           `json:"hum,omitempty"`
	Bat          string            `json:"bat,omitempty"`
	Switch       string            `json:"switch,omitempty"`
	Cmd          string            `json:"cmd,omitempty"`
	Extra        map[string]string `json:"extra,omitempty"`
	Timestamp    string            `json:"timestamp"`
	ReceivedAt   string            `json:"received_at"`
}

// SensorDataFromMessage crafts a SensorData struct from a message read from RFLink.
func SensorDataFromMessage(msg string) (*SensorData, error) {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return nil, errors.New("empty message")
	}

	pieces := strings.Split(msg, ";")
	if len(pieces) < 3 {
		return nil, fmt.Errorf("message too short: %q", msg)
	}

	nowStr := time.Now().Format(time.RFC3339Nano)

	sd := SensorData{
		Model:      sanitizeTopicSegment(strings.TrimSpace(pieces[2])),
		Extra:      make(map[string]string),
		ReceivedAt: nowStr,
		Timestamp:  nowStr,
	}

	for i := 3; i < len(pieces); i++ {
		piece := strings.TrimSpace(pieces[i])
		if piece == "" {
			continue
		}

		arr := strings.SplitN(piece, "=", 2)
		if len(arr) != 2 {
			log.Warn("sensor field skipped", "field", piece)
			continue
		}

		key := strings.ToUpper(strings.TrimSpace(arr[0]))
		value := strings.TrimSpace(arr[1])
		if key == "" {
			log.Warn("sensor field skipped", "field", piece)
			continue
		}

		if _, known := knownFieldKeys[key]; !known {
			sd.Extra[fieldTopicName(key)] = value
			continue
		}

		switch key {
		case "ID":
			sd.Id = value
			sd.Hwid = strings.ToUpper(sd.Model + "_" + sanitizeTopicSegment(sd.Id))
			sd.RawHWID = sd.Hwid
		case "TEMP":
			t, err := strToUint16(value, 16)
			if err != nil {
				log.Warn("temperature field skipped", "value", value, "err", err)
				break
			}
			sd.TempRaw = value
			// RFLink: high bit is the sign, lower 15 bits are |°C|×10.
			temp := tempCelsiusFromRFLink(t)
			sd.Temperature = &temp
		case "HUM":
			// Decimal plugins use 0-9. Hex plugins (LaCrosse, UPM/Esic F2, %02x)
			// stay hex even when the digits are only 0-9, so HUM=32 is 50%.
			base := 10
			if humidityIsHex(sd.Model, value) {
				base = 16
			}
			h, err := strToUint16(value, base)
			if err != nil || h > 100 {
				log.Warn("humidity field skipped", "value", value, "err", err)
				break
			}
			sd.Humidity = &h
		case "BAT":
			sd.Bat = value
		case "SWITCH":
			sd.Switch = value
		case "CMD":
			sd.Cmd = value
		}
	}

	if sd.Id == "" {
		return nil, errors.New("message has no sensor ID")
	}

	return &sd, nil
}

// ApplyCanonicalHWID rewrites Hwid using the configured ID→HWID map when present.
// A second call keeps using the original HWID. Looking up the already rewritten
// name would miss and fall through to the bare ID, which can name a different device.
func (sd *SensorData) ApplyCanonicalHWID(o *Options) {
	if sd == nil || o == nil {
		return
	}
	base := sd.Hwid
	if sd.RawHWID != "" {
		base = sd.RawHWID
	}
	sd.Hwid = o.CanonicalHWID(sd.Id, base)
}

func isMomentaryState(v string) bool {
	switch strings.ToUpper(strings.TrimSpace(v)) {
	case "ON", "OFF", "ALLON", "ALLOFF", "UP", "DOWN", "STOP":
		return true
	default:
		return false
	}
}

// RetainedState is the sensor snapshot safe to store with MQTT retain.
// One-shot button and alarm words are omitted so a broker restart does not
// restore CMD=ON.
func (sd *SensorData) RetainedState() *SensorData {
	cp := sd.StripEventFields()
	for k, v := range cp.Extra {
		if isMomentaryState(v) {
			delete(cp.Extra, k)
		}
	}
	return cp
}

// StripEventFields clears cmd/switch so they are not replayed from the pending buffer.
func (sd *SensorData) StripEventFields() *SensorData {
	cp := *sd
	cp.Cmd = ""
	cp.Switch = ""
	if sd.Extra != nil {
		cp.Extra = make(map[string]string, len(sd.Extra))
		maps.Copy(cp.Extra, sd.Extra)
	}
	return &cp
}

// FieldsForPublish returns MQTT subtopic suffixes and payload values for a sensor.
func (sd *SensorData) FieldsForPublish(tempUnit string) map[string]string {
	fields := make(map[string]string)

	switch strings.ToUpper(tempUnit) {
	case TempUnitRaw:
		if sd.TempRaw != "" {
			fields["temp"] = sd.TempRaw
		}
	case TempUnitF:
		if sd.Temperature != nil {
			f := float64(*sd.Temperature)*9/5 + 32
			fields["temp"] = strconv.FormatFloat(f, 'f', 1, 32)
		}
	default:
		if sd.Temperature != nil {
			fields["temp"] = strconv.FormatFloat(float64(*sd.Temperature), 'f', 1, 32)
		}
	}

	if sd.Humidity != nil {
		fields["hum"] = strconv.FormatUint(uint64(*sd.Humidity), 10)
	}
	if sd.Bat != "" {
		fields["bat"] = sd.Bat
	}

	// Multi-button remotes: SWITCH identifies the channel, CMD is ON/OFF.
	// Publish one binary channel per SWITCH so HA sees separate buttons.
	if sd.Switch != "" && sd.Cmd != "" {
		ch := fieldTopicName(sd.Switch)
		fields["sw_"+ch] = strings.ToUpper(strings.TrimSpace(sd.Cmd))
		// Keep raw values in sumJson only (via SensorData); avoid a single shared switch entity.
	} else {
		if sd.Switch != "" {
			fields["switch"] = sd.Switch
		}
		if sd.Cmd != "" {
			fields["cmd"] = sd.Cmd
		}
	}

	for key, value := range sd.Extra {
		if value != "" {
			fields[key] = value
		}
	}

	return fields
}

// TopicID returns the MQTT topic segment for this sensor (HWID preferred, fallback to raw ID).
func (sd *SensorData) TopicID() string {
	if sd.Hwid != "" {
		return sd.Hwid
	}
	return sd.Id
}

func fieldTopicName(key string) string {
	return strings.ToLower(strings.TrimSpace(key))
}

// tempCelsiusFromRFLink decodes an RFLink TEMP word.
// The usual encoding is a sign flag plus a magnitude: 0x8066 is −10.2°C.
// UPM/Esic prints a signed int with %04x, so −1.0°C arrives as fff6. A
// magnitude above 100.0°C is not the sign-flag form, so that word is a
// two's-complement tenth of a degree.
func tempCelsiusFromRFLink(raw uint16) float32 {
	if raw&0x8000 != 0 {
		mag := raw & 0x7FFF
		if mag > 0x3E8 {
			return float32(int16(raw)) / 10
		}
		return -float32(mag) / 10
	}
	return float32(raw) / 10
}

func humidityIsHex(model, value string) bool {
	if strings.ContainsAny(strings.ToLower(value), "abcdef") {
		return true
	}
	m := strings.ToUpper(model)
	return strings.Contains(m, "LACROSSE") || strings.Contains(m, "ESIC_F2")
}

// String outputs a string representing the SensorData.
func (sd *SensorData) String() string {
	format := "%s [%s]"
	args := []any{sd.Model, sd.Id}

	if sd.Bat != "" {
		format += " bat=%s"
		args = append(args, sd.Bat)
	}
	if sd.Switch != "" {
		format += " switch=%s"
		args = append(args, sd.Switch)
	}
	if sd.Cmd != "" {
		format += " cmd=%s"
		args = append(args, sd.Cmd)
	}
	if sd.Temperature != nil {
		format += " temp=%.1f°C"
		args = append(args, *sd.Temperature)
	}
	if sd.Humidity != nil {
		format += " hum=%d%%"
		args = append(args, *sd.Humidity)
	}

	return fmt.Sprintf(format, args...)
}

// GatewayInfo is published to {topic}/gateway/info, or {topic}/gateway/<name>/info
// when more than one gateway is configured.
type GatewayInfo struct {
	Raw        string            `json:"raw"`
	Device     string            `json:"device,omitempty"`
	Version    string            `json:"version,omitempty"`
	Pong       bool              `json:"pong,omitempty"`
	ReceivedAt string            `json:"received_at"`
	Extra      map[string]string `json:"extra,omitempty"`
}

// ParseGatewayMessage tries to parse RFLink *gateway* responses (PONG / VER / REV).
// Important: almost ALL RFLink RX lines start with "20;" — including sensors.
// Only pure gateway replies (no device ID= field) are treated as gateway info.
// Returns nil, nil if the message is not a gateway-only response.
func ParseGatewayMessage(msg string) (*GatewayInfo, error) {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return nil, nil
	}
	if !strings.HasPrefix(msg, "20;") {
		return nil, nil
	}

	upperMsg := strings.ToUpper(msg)
	// Device / protocol frames always carry ID=… — those are sensors/actuators, not gateway.
	if strings.Contains(upperMsg, "ID=") {
		return nil, nil
	}

	hasPong := false
	hasVer := false
	info := &GatewayInfo{
		Raw:        msg,
		ReceivedAt: time.Now().Format(time.RFC3339Nano),
		Extra:      make(map[string]string),
	}

	pieces := strings.SplitSeq(msg, ";")
	for piece := range pieces {
		piece = strings.TrimSpace(piece)
		if piece == "" {
			continue
		}
		upper := strings.ToUpper(piece)
		if upper == "PONG" {
			info.Pong = true
			hasPong = true
			continue
		}
		if strings.Contains(piece, "=") {
			kv := strings.SplitN(piece, "=", 2)
			key := strings.ToUpper(strings.TrimSpace(kv[0]))
			val := strings.TrimSpace(kv[1])
			switch key {
			case "VER", "VERSION":
				info.Version = val
				hasVer = true
			case "REV", "BUILD", "NAME":
				info.Extra[strings.ToLower(key)] = val
				hasVer = true // version-ish metadata from 10;version;
			default:
				info.Extra[strings.ToLower(key)] = val
			}
			continue
		}
		// Stock firmware answers 10;version; with a banner and no VER= field:
		// "RFLink Gateway V1.1 - R48".
		if info.Version == "" && strings.Contains(upper, "RFLINK") && strings.ContainsAny(piece, "0123456789") {
			info.Version = piece
			hasVer = true
		}
	}
	if info.Version == "" {
		if rev := info.Extra["rev"]; rev != "" {
			info.Version = rev
		} else if build := info.Extra["build"]; build != "" {
			info.Version = build
		}
	}

	// Must look like a real gateway reply, otherwise leave it for the sensor parser
	// (or skip as unknown).
	if !hasPong && !hasVer && info.Version == "" {
		return nil, nil
	}

	if len(info.Extra) == 0 {
		info.Extra = nil
	}
	return info, nil
}
