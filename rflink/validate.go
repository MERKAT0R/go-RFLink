/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// RFLink protocol (https://rflink.nl/protref.php):
//
//	10;…  master → gateway (TX / control): 10;PING;  10;NewKaku;0cac142;3;ON;
//	11;…  master echo / resend (device creation): 11;20;01;LacrosseV4;ID=0002;TEMP=00cd;
//	20;…  gateway → master (RX; allowed for resend)
//
// Fields are ';' separated. Protocol names may contain spaces and '/' (e.g. "Ikea Koppla", "UPM/Esic").
var (
	reRFLinkNode = regexp.MustCompile(`^[0-9]{1,2}$`)
	reHWID       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	reLabel      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _./-]{0,63}$`)
	reHAPrefix   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_/-]{0,63}$`)
	reIgnoreTok  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
)

// ValidateRFLinkCommand checks a normalized RFLink command (must end with ';').
func ValidateRFLinkCommand(cmd string) error {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return fmt.Errorf("empty command")
	}
	if !strings.HasSuffix(cmd, ";") {
		return fmt.Errorf("command must end with ';'")
	}
	if strings.ContainsAny(cmd, "\x00\r\n") {
		return fmt.Errorf("command contains control characters")
	}
	for _, r := range cmd {
		if r > unicode.MaxASCII || r < 0x20 {
			return fmt.Errorf("command contains non-printable character %q", r)
		}
	}

	body := strings.TrimSuffix(cmd, ";")
	parts := strings.Split(body, ";")
	if len(parts) < 2 {
		return fmt.Errorf("invalid RFLink command: need at least node and one field (got %q)", cmd)
	}
	if !reRFLinkNode.MatchString(parts[0]) {
		return fmt.Errorf("invalid RFLink node %q (want 1–2 digits, e.g. 10/11/20)", parts[0])
	}
	for i, p := range parts {
		if p == "" {
			return fmt.Errorf("invalid RFLink command: empty field at position %d", i)
		}
		if len(p) > 96 {
			return fmt.Errorf("invalid RFLink command: field %d too long", i)
		}
	}
	return nil
}

// ValidateFriendlyNamesCSV validates HWID:Name,HWID2:Name2 lists.
func ValidateFriendlyNamesCSV(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := splitCSV(raw)
	if len(parts) == 0 {
		return fmt.Errorf("friendly_names: empty after trim")
	}
	for _, part := range parts {
		kv := strings.SplitN(part, ":", 2)
		if len(kv) != 2 {
			return fmt.Errorf("friendly_names: invalid entry %q (want HWID:Name)", part)
		}
		key := strings.TrimSpace(kv[0])
		val := strings.TrimSpace(kv[1])
		if key == "" || val == "" {
			return fmt.Errorf("friendly_names: empty key or name in %q", part)
		}
		if !reHWID.MatchString(key) {
			return fmt.Errorf("friendly_names: invalid HWID %q (use A-Z, 0-9, _, ., -)", key)
		}
		if !isPrintableName(val) {
			return fmt.Errorf("friendly_names: invalid name %q", val)
		}
		if strings.Contains(val, ",") {
			return fmt.Errorf("friendly_names: name must not contain comma: %q", val)
		}
	}
	return nil
}

// ValidateHWIDMapCSV validates ID:HWID or MODEL_ID:HWID maps.
func ValidateHWIDMapCSV(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	for _, part := range splitCSV(raw) {
		kv := strings.SplitN(part, ":", 2)
		if len(kv) != 2 {
			return fmt.Errorf("hwid_map: invalid entry %q (want Key:HWID)", part)
		}
		key := strings.TrimSpace(kv[0])
		val := strings.TrimSpace(kv[1])
		if key == "" || val == "" {
			return fmt.Errorf("hwid_map: empty key or value in %q", part)
		}
		if !reHWID.MatchString(key) {
			return fmt.Errorf("hwid_map: invalid key %q", key)
		}
		if !reHWID.MatchString(val) {
			return fmt.Errorf("hwid_map: invalid HWID %q", val)
		}
	}
	return nil
}

// ValidateIgnoreListCSV validates comma-separated model/HWID tokens.
func ValidateIgnoreListCSV(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	for _, part := range splitCSV(raw) {
		if !reIgnoreTok.MatchString(part) {
			return fmt.Errorf("ignore_list: invalid token %q", part)
		}
	}
	return nil
}

// ValidateHAButtonsCSV validates Label:RFLinkCmd,… entries.
func ValidateHAButtonsCSV(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	for _, part := range splitCSV(raw) {
		kv := strings.SplitN(part, ":", 2)
		if len(kv) != 2 {
			return fmt.Errorf("ha_buttons: invalid entry %q (want Label:10;CMD;)", part)
		}
		name := strings.TrimSpace(kv[0])
		cmd := strings.TrimSpace(kv[1])
		if name == "" || cmd == "" {
			return fmt.Errorf("ha_buttons: empty label or command in %q", part)
		}
		if !reLabel.MatchString(name) {
			return fmt.Errorf("ha_buttons: invalid label %q", name)
		}
		if !strings.HasSuffix(cmd, ";") {
			cmd += ";"
		}
		if err := ValidateRFLinkCommand(cmd); err != nil {
			return fmt.Errorf("ha_buttons: command for %q: %w", name, err)
		}
	}
	return nil
}

// ValidateHASwitchesCSV validates Label:ON_CMD:OFF_CMD,… entries.
func ValidateHASwitchesCSV(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	for _, part := range splitCSV(raw) {
		kv := strings.SplitN(part, ":", 3)
		if len(kv) != 3 {
			return fmt.Errorf("ha_switches: invalid entry %q (want Label:ON_CMD:OFF_CMD)", part)
		}
		name := strings.TrimSpace(kv[0])
		on := strings.TrimSpace(kv[1])
		off := strings.TrimSpace(kv[2])
		if name == "" || on == "" || off == "" {
			return fmt.Errorf("ha_switches: empty field in %q", part)
		}
		if !reLabel.MatchString(name) {
			return fmt.Errorf("ha_switches: invalid label %q", name)
		}
		if !strings.HasSuffix(on, ";") {
			on += ";"
		}
		if !strings.HasSuffix(off, ";") {
			off += ";"
		}
		if err := ValidateRFLinkCommand(on); err != nil {
			return fmt.Errorf("ha_switches: ON for %q: %w", name, err)
		}
		if err := ValidateRFLinkCommand(off); err != nil {
			return fmt.Errorf("ha_switches: OFF for %q: %w", name, err)
		}
	}
	return nil
}

// ValidateHADiscoveryPrefix checks MQTT discovery prefix.
func ValidateHADiscoveryPrefix(pfx string) error {
	pfx = strings.TrimSpace(pfx)
	if pfx == "" {
		return nil
	}
	if !reHAPrefix.MatchString(pfx) {
		return fmt.Errorf("ha_discovery_prefix: invalid %q", pfx)
	}
	return nil
}

func splitCSV(raw string) []string {
	var out []string
	for p := range strings.SplitSeq(raw, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func isPrintableName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}
