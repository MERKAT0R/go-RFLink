/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"go.bug.st/serial"
)

// fakeSerial is an in-memory serial.Port. A nil chunk is a read timeout (0, nil).
type fakeSerial struct {
	mu           sync.Mutex
	chunks       [][]byte
	idx          int
	closed       bool
	wake         chan struct{}
	writeStarted chan struct{}
	writeRelease chan struct{}
	written      [][]byte
}

func newFakeSerial(chunks ...[]byte) *fakeSerial {
	return &fakeSerial{chunks: chunks, wake: make(chan struct{})}
}

func (f *fakeSerial) Read(p []byte) (int, error) {
	for {
		f.mu.Lock()
		closed := f.closed
		var chunk []byte
		has := f.idx < len(f.chunks)
		if has {
			chunk = f.chunks[f.idx]
			f.idx++
		}
		wake := f.wake
		f.mu.Unlock()

		if closed {
			return 0, io.EOF
		}
		if has {
			if chunk == nil {
				return 0, nil
			}
			return copy(p, chunk), nil
		}
		// No queued chunk: a short wait models the driver read timeout and
		// returns to ReadLines, which is where ctx is observed.
		select {
		case <-wake:
		case <-time.After(20 * time.Millisecond):
			return 0, nil
		}
	}
}

func (f *fakeSerial) Write(p []byte) (int, error) {
	if f.writeStarted != nil {
		close(f.writeStarted)
		<-f.writeRelease
	}
	f.mu.Lock()
	f.written = append(f.written, append([]byte(nil), p...))
	f.mu.Unlock()
	return len(p), nil
}
func (f *fakeSerial) SetMode(*serial.Mode) error { return nil }
func (f *fakeSerial) Drain() error               { return nil }
func (f *fakeSerial) ResetInputBuffer() error    { return nil }
func (f *fakeSerial) ResetOutputBuffer() error   { return nil }
func (f *fakeSerial) SetDTR(bool) error          { return nil }
func (f *fakeSerial) SetRTS(bool) error          { return nil }
func (f *fakeSerial) GetModemStatusBits() (*serial.ModemStatusBits, error) {
	return &serial.ModemStatusBits{}, nil
}
func (f *fakeSerial) SetReadTimeout(time.Duration) error { return nil }
func (f *fakeSerial) Break(time.Duration) error          { return nil }

func (f *fakeSerial) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.wake)
	}
	return nil
}

func TestReadLinesTimeoutDoesNotDropSession(t *testing.T) {
	fp := newFakeSerial(nil, nil, []byte("20;01;EV1527;ID=0b25f2;\n"))
	sr := &SerialReader{port: fp}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var got []string
	done := make(chan error, 1)
	go func() {
		done <- sr.ReadLines(ctx, func(line string) error {
			got = append(got, line)
			cancel()
			return nil
		})
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReadLines did not return after cancel")
	}
	if len(got) != 1 || got[0] != "20;01;EV1527;ID=0b25f2;" {
		t.Fatalf("lines = %#v", got)
	}
}

func TestReadLinesSplitsChunksAndSurvivesHandlerPanic(t *testing.T) {
	fp := newFakeSerial(
		[]byte("20;01;A;ID=1;"),
		[]byte("CMD=ON;\n20;02;B;ID=2;\n"),
	)
	sr := &SerialReader{port: fp}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var got []string
	done := make(chan error, 1)
	go func() {
		done <- sr.ReadLines(ctx, func(line string) error {
			if len(got) == 0 {
				got = append(got, line)
				panic("handler boom")
			}
			got = append(got, line)
			cancel()
			return nil
		})
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReadLines did not return after cancel")
	}
	if len(got) != 2 {
		t.Fatalf("lines = %#v", got)
	}
	if got[0] != "20;01;A;ID=1;CMD=ON;" || got[1] != "20;02;B;ID=2;" {
		t.Fatalf("lines = %#v", got)
	}
}

func TestWriteUnlocksBeforeSyscall(t *testing.T) {
	fp := newFakeSerial()
	fp.writeStarted = make(chan struct{})
	fp.writeRelease = make(chan struct{})
	sr := &SerialReader{port: fp}

	done := make(chan error, 1)
	go func() {
		done <- sr.Write("10;PING;")
	}()
	select {
	case <-fp.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("Write did not enter the port syscall")
	}

	disconnected := make(chan struct{})
	go func() {
		sr.disconnect()
		close(disconnected)
	}()
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		t.Fatal("disconnect blocked while Write was in progress")
	}
	close(fp.writeRelease)
	if err := <-done; err != nil {
		t.Fatalf("Write: %v", err)
	}
	if sr.IsConnected() {
		t.Fatal("port still connected after disconnect")
	}
}

func TestWritePausedIsRejected(t *testing.T) {
	sr := &SerialReader{port: newFakeSerial()}
	sr.PauseWrites()
	if err := sr.Write("10;PING;"); !errors.Is(err, errSerialNotConnected) {
		t.Fatalf("err = %v", err)
	}
	sr.ResumeWrites()
	if err := sr.Write("10;PING;"); err != nil {
		t.Fatalf("Write after resume: %v", err)
	}
}

func TestCloseIfIdleWaitsForWrite(t *testing.T) {
	fp := newFakeSerial()
	fp.writeStarted = make(chan struct{})
	fp.writeRelease = make(chan struct{})
	sr := &SerialReader{port: fp}

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- sr.Write("10;PING;")
	}()
	select {
	case <-fp.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("Write did not enter the port")
	}

	closed := make(chan error, 1)
	go func() {
		closed <- sr.CloseIfIdle(time.Second)
	}()
	select {
	case err := <-closed:
		t.Fatalf("closed while write in flight: %v", err)
	case <-time.After(80 * time.Millisecond):
	}
	close(fp.writeRelease)
	if err := <-closed; err != nil {
		t.Fatalf("CloseIfIdle: %v", err)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("Write: %v", err)
	}
	if sr.IsConnected() {
		t.Fatal("port still connected")
	}
}

func TestCloseIfIdleLeavesPortWhenWriteStaysBusy(t *testing.T) {
	sr := &SerialReader{port: newFakeSerial()}
	sr.writeMu.Lock()
	defer sr.writeMu.Unlock()
	err := sr.CloseIfIdle(40 * time.Millisecond)
	if !errors.Is(err, errSerialWriteBusy) {
		t.Fatalf("err = %v", err)
	}
	if !sr.IsConnected() {
		t.Fatal("port closed while the write lock was held")
	}
}

func TestUnresponsiveSessionReopensAfterReadReturns(t *testing.T) {
	fp := newFakeSerial()
	sr := &SerialReader{port: fp}
	p := &Publisher{
		opts:          &Options{},
		serialRestart: make(chan struct{}, 1),
	}
	p.opts.Publish.Topic = "rflink"
	ctx := t.Context()
	app := &App{
		publisher: p,
		serial:    sr,
		serialCh:  make(chan string, 4),
		ctx:       ctx,
	}
	delay := time.Duration(0)
	done := make(chan bool, 1)
	go func() {
		done <- app.runSerialSession(ctx, &delay, 0, 0)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		p.SignalSerialRestart()
		select {
		case cont := <-done:
			if !cont {
				t.Fatal("session stopped instead of reopening")
			}
			if sr.IsConnected() {
				t.Fatal("port still open after unresponsive restart")
			}
			if !sr.WritesAllowed() {
				t.Fatal("writes left paused")
			}
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatal("session did not restart")
}

func TestReadLinesNotConnected(t *testing.T) {
	sr := &SerialReader{}
	err := sr.ReadLines(context.Background(), func(string) error { return nil })
	if !errors.Is(err, errSerialNotConnected) {
		t.Fatalf("err = %v", err)
	}
}
