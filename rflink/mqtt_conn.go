/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/gorilla/websocket"
	"golang.org/x/net/proxy"
)

// mqttWriteTimeout bounds one MQTT packet write. paho ignores the publish
// context for QoS 0 and writes straight to the socket; without a deadline a
// black-holed broker holds that write, and every other write behind it, until
// the process is killed.
const mqttWriteTimeout = 10 * time.Second

// mqttAttemptConnection is autopaho's dial hook.
// The library passes the long-lived client context here, not the connect
// timeout, so the timeout is applied in this function.
// Plain TCP from autopaho is not safe for concurrent writes. TLS and websocket
// in that library already serialize writes; this dial keeps that and adds the
// same serialization plus a write deadline for TCP and TLS.
func mqttAttemptConnection(ctx context.Context, cfg autopaho.ClientConfig, u *url.URL) (net.Conn, error) {
	if u == nil {
		return nil, fmt.Errorf("mqtt broker url is nil")
	}
	timeout := cfg.ConnectTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return dialMQTT(dialCtx, cfg, u)
}

func dialMQTT(ctx context.Context, cfg autopaho.ClientConfig, u *url.URL) (net.Conn, error) {
	switch strings.ToLower(u.Scheme) {
	case "mqtt", "tcp", "":
		conn, err := proxy.Dial(ctx, "tcp", u.Host)
		if err != nil {
			return nil, err
		}
		return newMQTTStream(conn), nil
	case "ssl", "tls", "mqtts", "mqtt+ssl", "tcps":
		raw, err := proxy.Dial(ctx, "tcp", u.Host)
		if err != nil {
			return nil, err
		}
		tlsConn := tls.Client(raw, cfg.TlsCfg)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = raw.Close()
			return nil, err
		}
		return newMQTTStream(tlsConn), nil
	case "ws":
		return dialMQTTWebsocket(ctx, nil, cfg, u)
	case "wss":
		return dialMQTTWebsocket(ctx, cfg.TlsCfg, cfg, u)
	default:
		return nil, fmt.Errorf("unsupported mqtt scheme %q", u.Scheme)
	}
}

// mqttStream serializes packet writes and gives each write a deadline.
// paho locks the connection when it implements sync.Locker, then calls Write.
// Write must not take mu again.
type mqttStream struct {
	net.Conn
	mu           sync.Mutex
	writeTimeout time.Duration
}

func newMQTTStream(conn net.Conn) *mqttStream {
	if conn == nil {
		return nil
	}
	return &mqttStream{Conn: conn, writeTimeout: mqttWriteTimeout}
}

func (c *mqttStream) Lock()   { c.mu.Lock() }
func (c *mqttStream) Unlock() { c.mu.Unlock() }

func (c *mqttStream) Write(p []byte) (int, error) {
	if c.writeTimeout > 0 {
		_ = c.SetWriteDeadline(time.Now().Add(c.writeTimeout))
		defer func() { _ = c.SetWriteDeadline(time.Time{}) }()
	}
	return c.Conn.Write(p)
}

// mqttWebsocket is one MQTT packet per websocket binary message.
// The write lock is the same sync.Locker paho uses, so PUBLISH and PINGREQ
// cannot interleave. Each write gets a deadline. Gorilla marks the connection
// dead only when that deadline fires mid-frame; a write that finishes clears
// the deadline so the next packet is not failed by a stale timer.
type mqttWebsocket struct {
	*websocket.Conn
	reader       io.Reader
	readMu       sync.Mutex
	mu           sync.Mutex
	writeTimeout time.Duration
}

func (c *mqttWebsocket) Lock()   { c.mu.Lock() }
func (c *mqttWebsocket) Unlock() { c.mu.Unlock() }

func (c *mqttWebsocket) Write(p []byte) (int, error) {
	timeout := c.writeTimeout
	if timeout <= 0 {
		timeout = mqttWriteTimeout
	}
	if err := c.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return 0, err
	}
	err := c.WriteMessage(websocket.BinaryMessage, p)
	_ = c.SetWriteDeadline(time.Time{})
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *mqttWebsocket) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for {
		if c.reader == nil {
			_, r, err := c.NextReader()
			if err != nil {
				return 0, err
			}
			c.reader = r
		}
		n, err := c.reader.Read(p)
		if err == io.EOF {
			c.reader = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (c *mqttWebsocket) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}

func dialMQTTWebsocket(ctx context.Context, tlsc *tls.Config, cfg autopaho.ClientConfig, u *url.URL) (net.Conn, error) {
	var dialer *websocket.Dialer
	var header http.Header
	if cfg.WebSocketCfg != nil {
		if cfg.WebSocketCfg.Dialer != nil {
			dialer = cfg.WebSocketCfg.Dialer(u, tlsc)
		}
		if cfg.WebSocketCfg.Header != nil {
			header = cfg.WebSocketCfg.Header(u, tlsc)
		}
	}
	if dialer == nil {
		copied := *websocket.DefaultDialer
		copied.TLSClientConfig = tlsc
		copied.Subprotocols = []string{"mqtt"}
		dialer = &copied
	}
	ws, _, err := dialer.DialContext(ctx, u.String(), header)
	if err != nil {
		return nil, fmt.Errorf("websocket connection failed: %w", err)
	}
	return &mqttWebsocket{Conn: ws, writeTimeout: mqttWriteTimeout}, nil
}
