/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"go.bug.st/serial"
)

var (
	errSerialNotConnected = errors.New("serial port not connected")
	errSerialStreamClosed = errors.New("serial stream closed")
)

// SerialReader manages the serial connection with RFLink and supports reconnect.
type SerialReader struct {
	opts *Options
	mu   sync.Mutex
	port serial.Port
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

// Write sends a command to RFLink. Ensures the payload ends with CR+LF.
func (sr *SerialReader) Write(command string) error {
	payload := strings.TrimRight(command, "\r\n")
	if payload == "" {
		return fmt.Errorf("empty rflink command")
	}
	payload += "\r\n"

	sr.mu.Lock()
	defer sr.mu.Unlock()
	if sr.port == nil {
		return errSerialNotConnected
	}
	_, err := sr.port.Write([]byte(payload))
	return err
}

// ReadLines reads newline-delimited messages until an error or context cancellation.
func (sr *SerialReader) ReadLines(ctx context.Context, handle func(string) error) error {
	sr.mu.Lock()
	port := sr.port
	sr.mu.Unlock()
	if port == nil {
		return errSerialNotConnected
	}

	scanner := bufio.NewScanner(port)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := handle(scanner.Text()); err != nil {
			return err
		}
	}

	if err := scanner.Err(); err != nil {
		if errors.Is(err, io.EOF) {
			return errSerialStreamClosed
		}
		return err
	}
	return errSerialStreamClosed
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
