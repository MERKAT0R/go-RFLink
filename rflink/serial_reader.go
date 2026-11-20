/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.bug.st/serial"
)

const (
	maxSerialLine = 8192
	// serialCloseWait is how long a session waits for an in-flight Write
	// before it gives up on Close. Closing while the Windows driver is still
	// in Write can crash the process.
	serialCloseWait = 2 * time.Second
)

var (
	errSerialNotConnected = errors.New("serial port not connected")
	errSerialStreamClosed = errors.New("serial stream closed")
	errSerialWriteBusy    = errors.New("serial write still in progress")
)

// SerialReader manages the serial connection with RFLink and supports reconnect.
type SerialReader struct {
	opts *Options
	mu   sync.Mutex
	// writeMu serializes port.Write only. disconnect does not take it:
	// a stalled driver write must not block Stop, and sr.mu must not be
	// held across the syscall.
	writeMu sync.Mutex
	port    serial.Port
	// blockWrites rejects new commands. An in-flight Write is left to finish
	// so Close is not called while the driver is still inside Write.
	blockWrites atomic.Bool
}

// NewSerialReader creates a SerialReader and tries to open the port once.
// A failed initial open is logged; the reconnect loop will keep trying.
func NewSerialReader(o *Options) *SerialReader {
	sr := &SerialReader{opts: o}
	if debugEnabled() {
		listSerialPorts()
	}
	if err := sr.connect(); err != nil {
		log.Warn("initial serial open failed, will retry in background",
			"device", o.Serial.Device,
			"err", err,
		)
	}
	return sr
}

func listSerialPorts() {
	ports, err := serial.GetPortsList()
	if err != nil {
		log.Warn("failed to list serial ports", "err", err)
		return
	}
	if len(ports) == 0 {
		log.Warn("no serial ports found")
		return
	}
	for _, port := range ports {
		log.Debug("found serial port", "port", port)
	}
}

func (sr *SerialReader) IsConnected() bool {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	return sr.port != nil
}

func (sr *SerialReader) connect() error {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	return sr.connectLocked()
}

func (sr *SerialReader) connectLocked() error {
	if sr.port != nil {
		_ = sr.port.Close()
		sr.port = nil
	}

	port, err := serial.Open(sr.opts.Serial.Device, &serial.Mode{
		BaudRate: sr.opts.Serial.Baud,
		Parity:   serial.NoParity,
		DataBits: 8,
		StopBits: serial.OneStopBit,
	})
	if err != nil {
		return err
	}
	if err = port.SetReadTimeout(sr.opts.serialReadTimeout()); err != nil {
		_ = port.Close()
		return err
	}

	sr.port = port
	return nil
}

// Reconnect closes the current port and opens it again.
func (sr *SerialReader) Reconnect() error {
	sr.disconnect()
	return sr.connect()
}

func (sr *SerialReader) disconnect() {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	if sr.port != nil {
		if err := sr.port.Close(); err != nil {
			log.Debug("serial port close error", "err", err)
		}
		sr.port = nil
	}
}

// PauseWrites rejects commands that have not entered Write yet.
func (sr *SerialReader) PauseWrites() {
	if sr != nil {
		sr.blockWrites.Store(true)
	}
}

// ResumeWrites allows commands again after the port has been reopened.
func (sr *SerialReader) ResumeWrites() {
	if sr != nil {
		sr.blockWrites.Store(false)
	}
}

// WritesAllowed reports whether a new command may start.
func (sr *SerialReader) WritesAllowed() bool {
	return sr != nil && !sr.blockWrites.Load()
}

// CloseIfIdle closes the port after an in-flight Write returns.
// If the write is still inside the driver when wait elapses, the port stays
// open: Close during that Write can crash the Windows serial driver.
func (sr *SerialReader) CloseIfIdle(wait time.Duration) error {
	if !sr.waitWriteIdle(wait) {
		return errSerialWriteBusy
	}
	sr.disconnect()
	return nil
}

func (sr *SerialReader) waitWriteIdle(wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		if sr.writeMu.TryLock() {
			sr.writeMu.Unlock()
			return true
		}
		if wait <= 0 || !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Write sends a command to RFLink. Ensures the payload ends with CR+LF.
// The port pointer is copied under the lock; the syscall runs without it.
// go.bug.st/serial leaves write timeouts at zero, so holding sr.mu across
// Write would block disconnect, reconnect and Stop for as long as the driver stalls.
func (sr *SerialReader) Write(command string) error {
	if !sr.WritesAllowed() {
		return errSerialNotConnected
	}
	payload := strings.TrimRight(command, "\r\n")
	if payload == "" {
		return fmt.Errorf("empty rflink command")
	}
	payload += "\r\n"

	sr.mu.Lock()
	port := sr.port
	sr.mu.Unlock()
	if port == nil {
		return errSerialNotConnected
	}

	sr.writeMu.Lock()
	defer sr.writeMu.Unlock()
	buf := []byte(payload)
	for len(buf) > 0 {
		n, err := port.Write(buf)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		if n > len(buf) {
			return nil
		}
		buf = buf[n:]
	}
	return nil
}

// ReadLines reads newline-delimited messages until the port fails or ctx is cancelled.
// A read timeout — go.bug.st/serial returns (0, nil) — keeps the session open.
// ctx is checked between reads. The port is not closed from this loop: on Windows,
// CloseHandle while an overlapped Read is still running can crash the process.
// Shutdown therefore waits at most one SERIAL_READ_TIMEOUT_SEC, then Stop closes the port.
func (sr *SerialReader) ReadLines(ctx context.Context, handle func(string) error) error {
	sr.mu.Lock()
	port := sr.port
	sr.mu.Unlock()
	if port == nil {
		return errSerialNotConnected
	}

	buf := make([]byte, 0, 256)
	tmp := make([]byte, 256)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := port.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			var herr error
			buf, herr = deliverSerialLines(buf, handle)
			if herr != nil {
				return herr
			}
			if len(buf) > maxSerialLine {
				log.Warn("serial line exceeded max length, discarded", "buffered", len(buf))
				buf = buf[:0]
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return classifySerialReadError(err)
		}
		// n == 0, err == nil: read timeout. The device is quiet, not disconnected.
	}
}

// deliverSerialLines splits complete lines out of buf and invokes handle.
// A panicking handler is logged and skipped so one bad callback cannot drop the port.
func deliverSerialLines(buf []byte, handle func(string) error) ([]byte, error) {
	for {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			return buf, nil
		}
		line := string(bytes.TrimRight(buf[:i], "\r"))
		next := make([]byte, len(buf)-i-1)
		copy(next, buf[i+1:])
		buf = next
		if line == "" {
			continue
		}
		if err := invokeSerialHandler(handle, line); err != nil {
			return buf, err
		}
	}
}

func invokeSerialHandler(handle func(string) error, line string) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Error("serial line handler panic recovered", "panic", rec)
			err = nil
		}
	}()
	return handle(line)
}

func classifySerialReadError(err error) error {
	if err == nil {
		return nil
	}
	var pe *serial.PortError
	if errors.As(err, &pe) && pe.EncodedErrorString() == "Port has been closed" {
		return errSerialStreamClosed
	}
	if errors.Is(err, io.EOF) {
		return errSerialStreamClosed
	}
	return err
}

// Close closes the serial port.
func (sr *SerialReader) Close() error {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	if sr.port == nil {
		return nil
	}
	err := sr.port.Close()
	sr.port = nil
	return err
}
