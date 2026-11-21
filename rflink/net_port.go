/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.bug.st/serial"
)

func isNetworkDevice(device string) bool {
	d := strings.ToLower(strings.TrimSpace(device))
	return strings.HasPrefix(d, "tcp://") || strings.HasPrefix(d, "net://")
}

// dialNetworkDevice opens a raw TCP RFLink (ESP32 and other networked gateways).
// The returned port uses the same Read timeout shape as a UART: a timeout is (0, nil).
func dialNetworkDevice(device string, readTimeout time.Duration) (serial.Port, error) {
	addr, err := networkDeviceAddress(device)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	p := &netPort{conn: conn}
	if err := p.SetReadTimeout(readTimeout); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return p, nil
}

// networkDeviceAddress returns the host:port passed to net.Dial.
// A link-local zone is written as fe80::1%eth0. url.Parse treats % as an
// escape, so the percent is encoded first and JoinHostPort puts the brackets back.
func networkDeviceAddress(device string) (string, error) {
	raw := strings.TrimSpace(device)
	u, err := url.Parse(encodeZonePercent(raw))
	if err != nil {
		return "", fmt.Errorf("network rflink device %q: %w", device, err)
	}
	host := u.Hostname()
	port := u.Port()
	if host == "" || port == "" {
		return "", fmt.Errorf("network rflink device %q: missing host or port", device)
	}
	// JoinHostPort adds brackets around IPv6. Dial rejects tcp://::1:2323
	// ("too many colons") when the host is passed through unchanged.
	return net.JoinHostPort(host, port), nil
}

// encodeZonePercent keeps an IPv6 zone id such as %eth0 or %12. %25 is already encoded.
func encodeZonePercent(raw string) string {
	i := strings.Index(raw, "://")
	if i < 0 || !strings.Contains(raw[i+3:], "%") {
		return raw
	}
	rest := raw[i+3:]
	// A zone id is only meaningful inside brackets. A bare "%" stays invalid,
	// and a "%" in the path or query must stay as the client wrote it.
	if !strings.Contains(rest, "[") {
		return raw
	}
	var b strings.Builder
	b.WriteString(raw[:i+3])
	inBrackets := false
	for j := 0; j < len(rest); j++ {
		switch rest[j] {
		case '[':
			inBrackets = true
		case ']':
			inBrackets = false
		}
		// %25eth0 is already encoded. %25 and %250 are zone ids 25 and 250:
		// leaving them as %25 makes url.Parse turn zone 25 into an empty zone
		// and zone 250 into zone 0.
		if inBrackets && rest[j] == '%' && !zoneNamePercent(rest, j) {
			b.WriteString("%25")
			continue
		}
		b.WriteByte(rest[j])
	}
	return b.String()
}

// zoneNamePercent reports whether rest[j] is an already-encoded percent that
// introduces a zone name (%25eth0). A numeric zone (%25, %250) is not that form.
func zoneNamePercent(rest string, j int) bool {
	if rest[j] != '%' || j+2 >= len(rest) || rest[j+1:j+3] != "25" {
		return false
	}
	k := j + 3
	if k >= len(rest) || rest[k] == ']' {
		return false
	}
	for ; k < len(rest) && rest[k] != ']'; k++ {
		if rest[k] < '0' || rest[k] > '9' {
			return true
		}
	}
	return false
}

// netWriteTimeout bounds a TCP gateway write. A zero window at the peer must
// not pin the session goroutine until process shutdown.
var netWriteTimeout = 10 * time.Second

type netPort struct {
	conn    net.Conn
	mu      sync.Mutex
	timeout time.Duration
}

func (p *netPort) Read(b []byte) (int, error) {
	p.mu.Lock()
	timeout := p.timeout
	p.mu.Unlock()
	if timeout > 0 {
		_ = p.conn.SetReadDeadline(time.Now().Add(timeout))
	} else {
		_ = p.conn.SetReadDeadline(time.Time{})
	}
	n, err := p.conn.Read(b)
	if err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			return n, nil
		}
	}
	return n, err
}

func (p *netPort) Write(b []byte) (int, error) {
	// A peer that stops reading would otherwise block this Write forever.
	// Stop and the ping loop wait on it, so the process never publishes offline.
	// The deadline is only on this socket. The serial driver is left alone.
	if netWriteTimeout > 0 {
		_ = p.conn.SetWriteDeadline(time.Now().Add(netWriteTimeout))
	}
	n, err := p.conn.Write(b)
	if err != nil {
		// The bytes may already be split. Leave the socket closed so the read
		// loop reconnects instead of appending the next command to the fragment.
		_ = p.conn.Close()
		return n, err
	}
	_ = p.conn.SetWriteDeadline(time.Time{})
	return n, nil
}

func (p *netPort) Close() error { return p.conn.Close() }

func (p *netPort) SetMode(*serial.Mode) error { return nil }
func (p *netPort) Drain() error               { return nil }
func (p *netPort) ResetInputBuffer() error    { return nil }
func (p *netPort) ResetOutputBuffer() error   { return nil }
func (p *netPort) SetDTR(bool) error          { return nil }
func (p *netPort) SetRTS(bool) error          { return nil }
func (p *netPort) Break(time.Duration) error  { return nil }

func (p *netPort) GetModemStatusBits() (*serial.ModemStatusBits, error) {
	return &serial.ModemStatusBits{}, nil
}

func (p *netPort) SetReadTimeout(t time.Duration) error {
	if t < 0 {
		t = 0
	}
	p.mu.Lock()
	p.timeout = t
	p.mu.Unlock()
	return nil
}
