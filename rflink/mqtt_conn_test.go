/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
)

// gateConn blocks inside Write so a test can see whether a second write entered.
type gateConn struct {
	mu      sync.Mutex
	entered int
	release chan struct{}
	buf     bytes.Buffer
}

func (c *gateConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.entered++
	n := c.entered
	c.mu.Unlock()
	if n == 1 {
		<-c.release
	}
	c.mu.Lock()
	c.buf.Write(p)
	c.mu.Unlock()
	return len(p), nil
}

func (c *gateConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *gateConn) Close() error                     { return nil }
func (c *gateConn) LocalAddr() net.Addr              { return nil }
func (c *gateConn) RemoteAddr() net.Addr             { return nil }
func (c *gateConn) SetDeadline(time.Time) error      { return nil }
func (c *gateConn) SetReadDeadline(time.Time) error  { return nil }
func (c *gateConn) SetWriteDeadline(time.Time) error { return nil }

// writeLikePaho matches packets.ControlPacket.WriteTo: lock if the conn is a
// sync.Locker, then write the whole packet.
func writeLikePaho(w io.Writer, p []byte) {
	if l, ok := w.(sync.Locker); ok {
		l.Lock()
		defer l.Unlock()
	}
	_, _ = w.Write(p)
}

func TestMQTTStreamSerializesPacketWrites(t *testing.T) {
	gate := &gateConn{release: make(chan struct{})}
	stream := newMQTTStream(gate)
	stream.writeTimeout = 0

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		writeLikePaho(stream, []byte("AAAA"))
	}()
	go func() {
		defer wg.Done()
		writeLikePaho(stream, []byte("BBBB"))
	}()

	deadline := time.Now().Add(time.Second)
	for {
		gate.mu.Lock()
		n := gate.entered
		gate.mu.Unlock()
		if n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first write did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(40 * time.Millisecond)
	gate.mu.Lock()
	entered := gate.entered
	gate.mu.Unlock()
	if entered != 1 {
		close(gate.release)
		t.Fatalf("concurrent packet writes entered the socket: %d", entered)
	}
	close(gate.release)
	wg.Wait()

	got := gate.buf.String()
	if got != "AAAABBBB" && got != "BBBBAAAA" {
		t.Fatalf("packet bytes interleaved: %q", got)
	}
}

func TestMQTTStreamWriteDeadlineUnblocks(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()

	stream := newMQTTStream(client)
	stream.writeTimeout = 40 * time.Millisecond
	done := make(chan error, 1)
	started := time.Now()
	go func() {
		_, err := stream.Write([]byte("x"))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("write succeeded on a pipe with no reader")
		}
		if time.Since(started) > time.Second {
			t.Fatalf("write deadline took %s", time.Since(started))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("write did not return")
	}
}

func TestMQTTDialRejectsUnknownScheme(t *testing.T) {
	_, err := mqttAttemptConnection(context.Background(), autopaho.ClientConfig{}, nil)
	if err == nil {
		t.Fatal("nil url was accepted")
	}
	_, err = mqttAttemptConnection(context.Background(), autopaho.ClientConfig{ConnectTimeout: time.Second}, &url.URL{
		Scheme: "http",
		Host:   "127.0.0.1:9",
	})
	if err == nil {
		t.Fatal("http scheme was accepted")
	}
}
