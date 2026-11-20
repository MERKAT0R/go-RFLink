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
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup

	cmdMu       sync.Mutex
	cmdTokens   float64
	cmdLastFill time.Time

	httpServer *httpAPIServer
	runtime    *runtimeManager
	sessions   *sessionStore
	webhooks   *webhookManager
}

// Init creates and starts the RFLink application.
func Init() (*App, error) {
	opts, err := GetOptions()
	if err != nil {
		return nil, fmt.Errorf("load options: %w", err)
	}

	initLogger(opts.LogLevel)
	log.Info("go-rflink starting", "log_level", opts.LogLevel)

	runtimeMgr, err := newRuntimeManager(opts)
	if err != nil {
		return nil, fmt.Errorf("runtime config: %w", err)
	}

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
		ctx:         ctx,
		cancel:      cancel,
		cmdTokens:   float64(opts.Publish.CommandRateLimit),
		cmdLastFill: time.Now(),
		runtime:     runtimeMgr,
		sessions:    newSessionStore(),
	}
	app.webhooks = newWebhookManager(app)
	runtimeMgr.onChange = func() {
		if app.webhooks != nil {
			app.webhooks.Reload()
		}
		if app.runtime.consumeHAChanged() && app.publisher != nil {
			app.publisher.ClearHADiscovery()
			if err := app.publisher.Resubscribe(); err != nil {
				log.Warn("mqtt resubscribe after ha change failed", "err", err)
			}
		}
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

	app.publisher.SetRawLineHook(func(e rawLineEntry) {
		if app.webhooks != nil {
			app.webhooks.NotifyRaw(e)
		}
	})
	app.publisher.SetSensorHook(func(hwid string, sumJSON []byte) {
		if app.webhooks != nil {
			app.webhooks.NotifySumJSON(hwid, sumJSON)
		}
	})
	app.webhooks.Start(ctx)

	if opts.HTTP.Listen != "" {
		srv, err := startHTTPServer(app)
		if err != nil {
			app.Stop()
			return nil, fmt.Errorf("start http server: %w", err)
		}
		app.httpServer = srv
	} else {
		log.Info("http server disabled",
			"api", false,
			"gui", false,
			"hint", "set HTTP_LISTEN=127.0.0.1:8080 to enable API/GUI",
		)
	}

	return app, nil
}

func (a *App) handleCommand(payload string) {
	_ = a.ExecuteCommand(payload)
}

// ExecuteCommand runs the same normalize / whitelist / rate-limit path as MQTT commands
// and returns the outcome (also published to cmd/result when MQTT is up).
func (a *App) ExecuteCommand(payload string) CmdResult {
	ctx := context.Background()
	now := time.Now().Format(time.RFC3339Nano)
	if a.ctx != nil && a.ctx.Err() != nil {
		log.Info("rflink command skipped, shutting down", "command", payload)
		return CmdResult{Command: payload, Status: "stopping", Error: "shutting down", ReceivedAt: now}
	}

	cmd, err := a.opts.NormalizeCommand(payload)
	if err != nil {
		a.publisher.IncCommandRejected()
		log.Warn("rflink command rejected by normalization", "command", payload, "err", err)
		res := CmdResult{Command: payload, Status: "rejected", Error: err.Error(), ReceivedAt: now}
		a.publisher.PublishCmdResult(ctx, res)
		return res
	}

	if !a.opts.CommandAllowed(cmd) {
		a.publisher.IncCommandRejected()
		log.Warn("rflink command rejected by whitelist", "command", cmd)
		res := CmdResult{Command: cmd, Status: "rejected", ReceivedAt: now}
		a.publisher.PublishCmdResult(ctx, res)
		return res
	}
	payload = cmd

	if !a.allowCommand() {
		a.publisher.IncCommandRateLimited()
		log.Warn("rflink command rate-limited", "command", payload)
		lim, rem, _ := a.CommandRateStatus()
		res := CmdResult{Command: payload, Status: "rate_limited", ReceivedAt: now, RateLimitRemaining: rem, RateLimitPerSec: lim}
		a.publisher.PublishCmdResult(ctx, res)
		return res
	}

	if err := a.serial.Write(payload); err != nil {
		if errors.Is(err, errSerialNotConnected) {
			log.Warn("rflink command skipped, serial not connected", "command", payload)
			res := CmdResult{Command: payload, Status: "serial_down", Error: err.Error(), ReceivedAt: now}
			a.publisher.PublishCmdResult(ctx, res)
			return res
		}
		log.Error("failed to send rflink command", "command", payload, "err", err)
		res := CmdResult{Command: payload, Status: "error", Error: err.Error(), ReceivedAt: now}
		a.publisher.PublishCmdResult(ctx, res)
		return res
	}
	log.Info("rflink command sent", "command", payload)
	lim, rem, _ := a.CommandRateStatus()
	res := CmdResult{Command: payload, Status: "ok", ReceivedAt: now, RateLimitRemaining: rem, RateLimitPerSec: lim}
	a.publisher.PublishCmdResult(ctx, res)
	return res
}

func (a *App) allowCommand() bool {
	limit := a.opts.Publish.CommandRateLimit
	if limit <= 0 {
		return true
	}
	a.cmdMu.Lock()
	defer a.cmdMu.Unlock()
	a.refillCmdTokensLocked(time.Now(), limit)
	if a.cmdTokens < 1 {
		return false
	}
	a.cmdTokens--
	return true
}

func (a *App) refillCmdTokensLocked(now time.Time, limit int) {
	elapsed := now.Sub(a.cmdLastFill).Seconds()
	a.cmdLastFill = now
	a.cmdTokens += elapsed * float64(limit)
	if a.cmdTokens > float64(limit) {
		a.cmdTokens = float64(limit)
	}
}

// CommandRateStatus returns the configured rate limit and approximate remaining tokens.
func (a *App) CommandRateStatus() (limit int, remaining float64, unlimited bool) {
	limit = a.opts.Publish.CommandRateLimit
	if limit <= 0 {
		return 0, 0, true
	}
	a.cmdMu.Lock()
	defer a.cmdMu.Unlock()
	a.refillCmdTokensLocked(time.Now(), limit)
	return limit, a.cmdTokens, false
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
		recovered := recover()
		if recovered == nil {
			return
		}
		if a.publisher != nil {
			a.publisher.panics.Add(1)
		}
		log.Error("serial session panic recovered", "panic", recovered)
		if a.serial != nil {
			a.serial.disconnect()
		}
		if a.publisher != nil {
			a.publisher.SetSerialConnected(ctx, false)
		}
		// Back off. Returning immediately would tight-loop a repeating panic.
		if !sleepContext(ctx, *reconnectDelay) {
			continueLoop = false
			return
		}
		*reconnectDelay = nextBackoff(*reconnectDelay, minDelay, maxDelay)
		continueLoop = true
	}()

	// A previous close was skipped because a Write was still inside the driver.
	// Do not start another Read on that port, and do not Close during the Write.
	if a.serial != nil && !a.serial.WritesAllowed() {
		if err := a.serial.CloseIfIdle(serialCloseWait); err != nil {
			log.Warn("serial close still waiting for the driver", "err", err, "retry_in", *reconnectDelay)
			if !sleepContext(ctx, *reconnectDelay) {
				return false
			}
			*reconnectDelay = nextBackoff(*reconnectDelay, minDelay, maxDelay)
			return true
		}
		a.serial.ResumeWrites()
	}

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
	}

	// Always publish serial online when the port is open (including initial open from NewSerialReader).
	if a.serial.IsConnected() {
		a.publisher.SetSerialConnected(ctx, true)
		if err := a.serial.Write("10;version;"); err != nil {
			log.Warn("failed to request rflink version", "err", err)
		}
	}

	sessionCtx, sessionCancel := context.WithCancel(ctx)
	defer sessionCancel()
	if a.publisher != nil && a.publisher.serialRestart != nil {
		a.publisher.DrainSerialRestart()
		a.publisher.ResetPingState()
		restart := a.publisher.serialRestart
		go func() {
			select {
			case <-ctx.Done():
			case <-sessionCtx.Done():
			case <-restart:
				sessionCancel()
			}
		}()
	}

	sessionStart := time.Now()
	err := a.serial.ReadLines(sessionCtx, func(line string) error {
		log.Debug("serial raw message", "message", line)
		a.publisher.NoteSerialActivity()
		// PONG must be counted even when the publish queue is full.
		if info, perr := ParseGatewayMessage(line); perr == nil && info != nil && info.Pong {
			a.publisher.NotePong()
		}

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

	unresponsive := sessionCtx.Err() != nil
	a.serial.PauseWrites()
	if a.publisher != nil {
		a.publisher.SetSerialConnected(ctx, false)
		// A reopened port must collect a fresh run of missed PONGs.
		a.publisher.ResetPingState()
		a.publisher.DrainSerialRestart()
	}
	if unresponsive {
		log.Warn("RFLink unresponsive, reopening serial port", "retry_in", *reconnectDelay)
	} else {
		switch {
		case errors.Is(err, errSerialNotConnected):
			log.Warn("serial disconnected, reconnecting", "retry_in", *reconnectDelay)
		case errors.Is(err, errSerialStreamClosed):
			log.Warn("serial stream closed, reconnecting", "retry_in", *reconnectDelay)
		default:
			log.Warn("serial read error, reconnecting", "err", err, "retry_in", *reconnectDelay)
		}
	}

	if cerr := a.serial.CloseIfIdle(serialCloseWait); cerr != nil {
		log.Warn("serial close skipped, write still in the driver", "err", cerr, "retry_in", *reconnectDelay)
		if !sleepContext(ctx, *reconnectDelay) {
			return false
		}
		*reconnectDelay = nextBackoff(*reconnectDelay, minDelay, maxDelay)
		return true
	}
	a.serial.ResumeWrites()

	// A port that opens and then fails the read immediately must keep growing
	// the delay. Reset only after a session that actually stayed up.
	if minDelay > 0 && time.Since(sessionStart) >= minDelay {
		*reconnectDelay = minDelay
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
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						if a.publisher != nil {
							a.publisher.panics.Add(1)
						}
						log.Error("rflink ping panic recovered", "panic", rec)
					}
				}()
				a.sendPing()
			}()
			timer.Reset(interval)
		}
	}
}

func (a *App) sendPing() {
	if a.serial == nil || !a.serial.IsConnected() || !a.serial.WritesAllowed() {
		return
	}
	if a.publisher != nil && a.publisher.PingOutstanding() {
		log.Debug("skipping PING, previous still outstanding")
		return
	}
	const cmd = "10;PING;"
	// Mark before the write so a PONG already sitting in the driver buffer
	// is not applied to a still-zero pingSentAt.
	if a.publisher != nil {
		a.publisher.MarkPingSent()
	}
	if err := a.serial.Write(cmd); err != nil {
		if a.publisher != nil {
			a.publisher.ClearPingSent()
		}
		log.Warn("failed to send RFLink PING", "err", err)
		return
	}
	if a.publisher != nil {
		a.publisher.MarkPingWritten()
	}
	log.Debug("RFLink PING sent")
}

// ShutdownGrace is how long main should wait for Stop. Serial reads block until
// the current read timeout returns, so the deadline has to cover that plus MQTT.
func (a *App) ShutdownGrace() time.Duration {
	grace := 25 * time.Second
	if a != nil && a.opts != nil {
		if extra := a.opts.serialReadTimeout() + 10*time.Second; extra > grace {
			grace = extra
		}
	}
	return grace
}

// Stop gracefully shuts down background workers and closes connections.
func (a *App) Stop() {
	a.cancel()
	a.wg.Wait()

	if a.httpServer != nil {
		a.httpServer.Shutdown()
		a.httpServer = nil
	}
	if a.webhooks != nil {
		a.webhooks.Stop()
	}
	if a.runtime != nil {
		a.runtime.Stop()
	}
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
