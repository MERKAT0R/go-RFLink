/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// App runs the RFLink serial reader, MQTT publisher and optional command bridge.
type App struct {
	opts      *Options
	publisher *Publisher
	serial    *SerialReader
	serialCh  chan string
	cancel    context.CancelFunc
	wg        sync.WaitGroup

	cmdMu       sync.Mutex
	cmdTokens   float64
	cmdLastFill time.Time
}

// Init creates and starts the RFLink application.
func Init() (*App, error) {
	opts, err := GetOptions()
	if err != nil {
		return nil, fmt.Errorf("load options: %w", err)
	}

	initLogger(opts.LogLevel)
	log.Info("go-rflink starting", "log_level", opts.LogLevel)

	ctx, cancel := context.WithCancel(context.Background())

	publisher, err := NewPublisher(ctx, opts)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create mqtt publisher: %w", err)
	}

	serialReader := NewSerialReader(opts)

	app := &App{
		opts:        opts,
		publisher:   publisher,
		serial:      serialReader,
		serialCh:    newSerialChannel(),
		cancel:      cancel,
		cmdTokens:   float64(opts.Publish.CommandRateLimit),
		cmdLastFill: time.Now(),
	}

	if err := publisher.SubscribeCommands(app.handleCommand); err != nil {
		app.Stop()
		return nil, fmt.Errorf("subscribe command topic: %w", err)
	}

	app.wg.Add(2)
	go app.runSerialLoop(ctx)
	go func() {
		defer app.wg.Done()
		publisher.RunPublishLoop(ctx, app.serialCh)
	}()
	if opts.serialPingInterval() > 0 {
		app.wg.Add(1)
		go app.runPingLoop(ctx)
	}

	log.Info("go-rflink started",
		"serial_device", opts.Serial.Device,
		"mqtt_topic", opts.Publish.Topic,
		"cmd_topic", opts.Publish.CmdTopic,
		"cmd_result_topic", opts.cmdResultTopic(),
		"ha_discovery", opts.Publish.HomeAssistantDiscovery,
		"temp_unit", opts.Publish.TemperatureUnit,
		"cmd_rate_limit", opts.Publish.CommandRateLimit,
		"cmd_whitelist", opts.Publish.CommandWhitelist,
		"dedup_ms", opts.Publish.DedupWindowMs,
		"ping_interval_sec", opts.Serial.PingIntervalSec,
		"ping_timeout_sec", opts.Serial.PingTimeoutSec,
		"ignore_list", opts.Publish.IgnoreList,
		"ha_buttons", len(opts.haButtons),
		"ha_switches", len(opts.haSwitches),
		"version", Version,
		"git_sha", GitSHA,
	)
	opts.LogMQTTTopics()

	return app, nil
}

func (a *App) handleCommand(payload string) {
	ctx := context.Background()
	now := time.Now().Format(time.RFC3339Nano)

	cmd, err := a.opts.NormalizeCommand(payload)
	if err != nil {
		a.publisher.IncCommandRejected()
		log.Warn("rflink command rejected by normalization", "command", payload, "err", err)
		a.publisher.PublishCmdResult(ctx, CmdResult{
			Command: payload, Status: "rejected", Error: err.Error(), ReceivedAt: now,
		})
		return
	}

	if !a.opts.CommandAllowed(cmd) {
		a.publisher.IncCommandRejected()
		log.Warn("rflink command rejected by whitelist", "command", cmd)
		a.publisher.PublishCmdResult(ctx, CmdResult{
			Command: cmd, Status: "rejected", ReceivedAt: now,
		})
		return
	}
	payload = cmd

	if !a.allowCommand() {
		a.publisher.IncCommandRateLimited()
		log.Warn("rflink command rate-limited", "command", payload)
		a.publisher.PublishCmdResult(ctx, CmdResult{
			Command: payload, Status: "rate_limited", ReceivedAt: now,
		})
		return
	}

	if err := a.serial.Write(payload); err != nil {
		if errors.Is(err, errSerialNotConnected) {
			log.Warn("rflink command skipped, serial not connected", "command", payload)
			a.publisher.PublishCmdResult(ctx, CmdResult{
				Command: payload, Status: "serial_down", Error: err.Error(), ReceivedAt: now,
			})
			return
		}
		log.Error("failed to send rflink command", "command", payload, "err", err)
		a.publisher.PublishCmdResult(ctx, CmdResult{
			Command: payload, Status: "error", Error: err.Error(), ReceivedAt: now,
		})
		return
	}
	log.Info("rflink command sent", "command", payload)
	a.publisher.PublishCmdResult(ctx, CmdResult{
		Command: payload, Status: "ok", ReceivedAt: now,
	})
}

func (a *App) allowCommand() bool {
	limit := a.opts.Publish.CommandRateLimit
	if limit <= 0 {
		return true
	}
	a.cmdMu.Lock()
	defer a.cmdMu.Unlock()

	now := time.Now()
	elapsed := now.Sub(a.cmdLastFill).Seconds()
	a.cmdLastFill = now
	a.cmdTokens += elapsed * float64(limit)
	if a.cmdTokens > float64(limit) {
		a.cmdTokens = float64(limit)
	}
	if a.cmdTokens < 1 {
		return false
	}
	a.cmdTokens--
	return true
}

func (a *App) runSerialLoop(ctx context.Context) {
	defer a.wg.Done()

	minDelay := a.opts.serialReconnectMinDelay()
	maxDelay := a.opts.serialReconnectMaxDelay()
	reconnectDelay := minDelay

	for {
		if ctx.Err() != nil {
			return
		}
		if !a.runSerialSession(ctx, &reconnectDelay, minDelay, maxDelay) {
			return
		}
	}
}

func (a *App) runSerialSession(
	ctx context.Context,
	reconnectDelay *time.Duration,
	minDelay, maxDelay time.Duration,
) (continueLoop bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if a.publisher != nil {
				a.publisher.panics.Add(1)
			}
			log.Error("serial session panic recovered", "panic", recovered)
			a.serial.disconnect()
			a.publisher.SetSerialConnected(ctx, false)
			continueLoop = true
		}
	}()

	if !a.serial.IsConnected() {
		if err := a.serial.Reconnect(); err != nil {
			log.Warn("serial reconnect failed",
				"device", a.opts.Serial.Device,
				"err", err,
				"retry_in", *reconnectDelay,
			)
			a.publisher.SetSerialConnected(ctx, false)
			if !sleepContext(ctx, *reconnectDelay) {
				return false
			}
			*reconnectDelay = nextBackoff(*reconnectDelay, minDelay, maxDelay)
			return true
		}

		log.Info("serial port connected",
			"device", a.opts.Serial.Device,
			"baud", a.opts.Serial.Baud,
		)
		*reconnectDelay = minDelay
	}

	// Always publish serial online when the port is open (including initial open from NewSerialReader).
	if a.serial.IsConnected() {
		a.publisher.SetSerialConnected(ctx, true)
		if err := a.serial.Write("10;version;"); err != nil {
			log.Warn("failed to request rflink version", "err", err)
		}
	}

	err := a.serial.ReadLines(ctx, func(line string) error {
		log.Debug("serial raw message", "message", line)
		a.publisher.NoteSerialActivity()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case a.serialCh <- line:
			return nil
		default:
			a.publisher.IncSerialDropped()
			log.Warn("serial channel full, discarding message", "message", line)
			return nil
		}
	})

	if ctx.Err() != nil {
		return false
	}

	a.serial.disconnect()
	a.publisher.SetSerialConnected(ctx, false)

	switch {
	case errors.Is(err, errSerialNotConnected):
		log.Warn("serial disconnected, reconnecting", "retry_in", *reconnectDelay)
	case errors.Is(err, errSerialStreamClosed):
		log.Warn("serial stream closed, reconnecting", "retry_in", *reconnectDelay)
	default:
		log.Warn("serial read error, reconnecting", "err", err, "retry_in", *reconnectDelay)
	}

	if !sleepContext(ctx, *reconnectDelay) {
		return false
	}
	*reconnectDelay = nextBackoff(*reconnectDelay, minDelay, maxDelay)
	return true
}

// runPingLoop periodically sends 10;PING; to confirm RFLink firmware is alive.
// Bypasses the MQTT command rate-limit (service path).
func (a *App) runPingLoop(ctx context.Context) {
	defer a.wg.Done()

	interval := a.opts.serialPingInterval()
	if interval <= 0 {
		return
	}
	// First ping shortly after start so we do not wait a full interval.
	first := min(max(interval/4, 5*time.Second), interval)

	timer := time.NewTimer(first)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			a.sendPing()
			timer.Reset(interval)
		}
	}
}

func (a *App) sendPing() {
	if a.serial == nil || !a.serial.IsConnected() {
		return
	}
	if a.publisher != nil && a.publisher.PingOutstanding() {
		log.Debug("skipping PING, previous still outstanding")
		return
	}
	const cmd = "10;PING;"
	if err := a.serial.Write(cmd); err != nil {
		log.Warn("failed to send RFLink PING", "err", err)
		return
	}
	if a.publisher != nil {
		a.publisher.MarkPingSent()
	}
	log.Debug("RFLink PING sent")
}

// Stop gracefully shuts down background workers and closes connections.
func (a *App) Stop() {
	a.cancel()
	a.wg.Wait()

	if a.publisher != nil {
		a.publisher.Disconnect()
	}
	if a.serial != nil {
		if err := a.serial.Close(); err != nil {
			log.Warn("failed to close serial port", "err", err)
		}
	}

	log.Info("go-rflink stopped")
}
