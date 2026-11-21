/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// App runs the RFLink serial reader, MQTT publisher and optional command bridge.
type App struct {
	opts      *Options
	publisher *Publisher
	serial    *SerialReader
	extras    []*SerialReader
	ingress   *serialIngress
	serialCh  chan serialFrame
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup

	cmdMu       sync.Mutex
	cmdTokens   float64
	cmdLastFill time.Time
	dropMu      sync.Mutex
	dropWarned  map[string]bool

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

	app.ingress = newSerialIngress()
	publisher.queues = app.ingress
	app.wg.Add(3)
	go app.runSerialLoop(ctx)
	go func() {
		defer app.wg.Done()
		app.ingress.run(ctx, app.serialCh)
	}()
	go func() {
		defer app.wg.Done()
		publisher.RunPublishLoop(ctx, app.serialCh)
	}()
	if opts.serialPingInterval() > 0 {
		app.wg.Add(1)
		go app.runPingLoop(ctx)
	}
	for _, dev := range opts.extraSerialDevices() {
		sr := &SerialReader{opts: opts, device: dev}
		app.extras = append(app.extras, sr)
		app.wg.Add(1)
		go app.runExtraSerial(ctx, sr)
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
	actor := ""
	if a.opts != nil {
		actor = a.opts.Publish.ClientID
	}
	_ = a.ExecuteCommandFrom(payload, "mqtt", actor)
}

// ExecuteCommand runs the same normalize / whitelist / rate-limit path as MQTT commands
// and returns the outcome (also published to cmd/result when MQTT is up).
func (a *App) ExecuteCommand(payload string) CmdResult {
	return a.ExecuteCommandFrom(payload, "internal", "")
}

// ExecuteCommandFrom is ExecuteCommand plus the audit source and actor.
// actor is a session prefix or token name, never the secret itself.
func (a *App) ExecuteCommandFrom(payload, source, actor string) CmdResult {
	return a.executeCommand(payload, source, actor, nil)
}

// executeCommand sends a command. A nil device list uses SERIAL_COMMAND_DEVICES,
// which is every connected port when that list is empty. A non-nil list is the
// one-shot target set for this call.
func (a *App) executeCommand(payload, source, actor string, devices []string) CmdResult {
	ctx := context.Background()
	now := time.Now().Format(time.RFC3339Nano)
	if a.ctx != nil && a.ctx.Err() != nil {
		log.Info("rflink command audit", "source", source, "actor", actor, "command", payload, "status", "stopping", "at", now)
		return CmdResult{Command: payload, Status: "stopping", Error: "shutting down", ReceivedAt: now}
	}

	cmd, err := a.opts.NormalizeCommand(payload)
	if err != nil {
		a.publisher.IncCommandRejected()
		log.Warn("rflink command rejected by normalization", "command", payload, "err", err)
		res := CmdResult{Command: payload, Status: "rejected", Error: err.Error(), ReceivedAt: now}
		a.auditCommand(source, actor, res)
		a.publisher.PublishCmdResult(ctx, res)
		return res
	}

	if !a.opts.CommandAllowed(cmd) {
		a.publisher.IncCommandRejected()
		log.Warn("rflink command rejected by whitelist", "command", cmd)
		res := CmdResult{Command: cmd, Status: "rejected", ReceivedAt: now}
		a.auditCommand(source, actor, res)
		a.publisher.PublishCmdResult(ctx, res)
		return res
	}
	payload = cmd

	if !a.allowCommand() {
		a.publisher.IncCommandRateLimited()
		log.Warn("rflink command rate-limited", "command", payload)
		lim, rem, _ := a.CommandRateStatus()
		res := CmdResult{Command: payload, Status: "rate_limited", ReceivedAt: now, RateLimitRemaining: rem, RateLimitPerSec: lim}
		a.auditCommand(source, actor, res)
		a.publisher.PublishCmdResult(ctx, res)
		return res
	}

	ports, err := a.writeCommandTo(payload, devices)
	if err != nil {
		if errors.Is(err, errPartialCommand) {
			log.Warn("rflink command partially sent", "command", payload, "ports", portAudit(ports))
			res := CmdResult{Command: payload, Status: "partial", Error: err.Error(), ReceivedAt: now, Ports: ports}
			a.auditCommand(source, actor, res)
			a.publisher.PublishCmdResult(ctx, res)
			return res
		}
		if errors.Is(err, errNoCommandTarget) {
			log.Warn("rflink command skipped, no matching gateway", "command", payload, "err", err)
			res := CmdResult{Command: payload, Status: "rejected", Error: err.Error(), ReceivedAt: now, Ports: ports}
			a.auditCommand(source, actor, res)
			a.publisher.PublishCmdResult(ctx, res)
			return res
		}
		if errors.Is(err, errSerialNotConnected) {
			log.Warn("rflink command skipped, serial not connected", "command", payload)
			res := CmdResult{Command: payload, Status: "serial_down", Error: err.Error(), ReceivedAt: now, Ports: ports}
			a.auditCommand(source, actor, res)
			a.publisher.PublishCmdResult(ctx, res)
			return res
		}
		log.Error("failed to send rflink command", "command", payload, "err", err)
		res := CmdResult{Command: payload, Status: "error", Error: err.Error(), ReceivedAt: now, Ports: ports}
		a.auditCommand(source, actor, res)
		a.publisher.PublishCmdResult(ctx, res)
		return res
	}
	log.Info("rflink command sent", "command", payload, "ports", portAudit(ports))
	lim, rem, _ := a.CommandRateStatus()
	res := CmdResult{Command: payload, Status: "ok", ReceivedAt: now, RateLimitRemaining: rem, RateLimitPerSec: lim, Ports: ports}
	a.auditCommand(source, actor, res)
	a.publisher.PublishCmdResult(ctx, res)
	return res
}

func (a *App) auditCommand(source, actor string, res CmdResult) {
	log.Info("rflink command audit",
		"source", source,
		"actor", actor,
		"command", res.Command,
		"status", res.Status,
		"ports", portAudit(res.Ports),
		"at", res.ReceivedAt,
	)
}

func portAudit(ports []PortWrite) string {
	if len(ports) == 0 {
		return ""
	}
	parts := make([]string, len(ports))
	for i, port := range ports {
		if port.Error != "" {
			parts[i] = port.Device + "=" + port.Status + "(" + port.Error + ")"
			continue
		}
		parts[i] = port.Device + "=" + port.Status
	}
	return strings.Join(parts, ",")
}

// writeCommandTo sends a command to the selected gateways.
// A nil device list uses SERIAL_COMMAND_DEVICES, which is every connected port
// when that list is empty. The result is ok only when every matched gateway accepts the write.
func (a *App) writeCommandTo(payload string, devices []string) ([]PortWrite, error) {
	allow := func(name string) bool {
		if devices == nil {
			return a.opts.commandDeviceAllow(name)
		}
		for _, item := range devices {
			if strings.EqualFold(strings.TrimSpace(item), strings.TrimSpace(name)) {
				return true
			}
		}
		return false
	}
	readers := make([]*SerialReader, 0, 1+len(a.extras))
	if a.serial != nil {
		readers = append(readers, a.serial)
	}
	readers = append(readers, a.extras...)
	var first, failedWrite error
	wrote := 0
	failed := 0
	matched := false
	ports := make([]PortWrite, 0, len(readers))
	for _, sr := range readers {
		if sr == nil {
			continue
		}
		name := sr.deviceName()
		// An empty SERIAL_DEVICE still owns a reader that never opens.
		// It must not turn a successful write on a real gateway into a partial result.
		if name == "" && !sr.IsConnected() {
			continue
		}
		if !allow(name) {
			continue
		}
		matched = true
		if err := sr.Write(payload); err != nil {
			failed++
			if first == nil {
				first = err
			}
			// A later gateway that actually failed the write must not be reported
			// as "serial not connected" just because an earlier port was closed.
			if failedWrite == nil && !errors.Is(err, errSerialNotConnected) {
				failedWrite = err
			}
			ports = append(ports, PortWrite{Device: name, Status: "error", Error: err.Error()})
			if sr != a.serial {
				log.Warn("extra serial command failed", "device", name, "err", err)
			}
			continue
		}
		wrote++
		ports = append(ports, PortWrite{Device: name, Status: "ok"})
	}
	if wrote > 0 && failed > 0 {
		return ports, errPartialCommand
	}
	if wrote > 0 {
		return ports, nil
	}
	if !matched && (devices != nil || a.opts.commandDevicesConfigured()) {
		return ports, errNoCommandTarget
	}
	if failedWrite != nil {
		return ports, failedWrite
	}
	if first != nil {
		return ports, first
	}
	return ports, errSerialNotConnected
}

// enqueueSerial parks a line on that gateway's queue. Without an ingress
// (tests) it uses the shared channel. A full queue drops only that gateway.
func (a *App) enqueueSerial(frame serialFrame) bool {
	if a == nil {
		return false
	}
	ok := false
	if a.ingress != nil {
		ok = a.ingress.offer(frame)
	} else if a.serialCh != nil {
		select {
		case a.serialCh <- frame:
			ok = true
		default:
		}
	}
	if ok {
		a.clearSerialDropWarn(frame.Device)
	}
	return ok
}

// noteSerialQueueFull counts a discarded line and warns once per gateway
// until that queue accepts a line again.
func (a *App) noteSerialQueueFull(device string) {
	if a == nil {
		return
	}
	if a.publisher != nil {
		a.publisher.IncPortDropped(device)
	}
	if a.serialDropWarned(device) {
		return
	}
	log.Warn("serial queue full, discarding further lines until the queue drains", "device", device)
}

func (a *App) serialDropWarned(device string) bool {
	a.dropMu.Lock()
	defer a.dropMu.Unlock()
	if a.dropWarned == nil {
		a.dropWarned = map[string]bool{}
	}
	if a.dropWarned[device] {
		return true
	}
	a.dropWarned[device] = true
	return false
}

func (a *App) clearSerialDropWarn(device string) {
	if a == nil {
		return
	}
	a.dropMu.Lock()
	delete(a.dropWarned, device)
	a.dropMu.Unlock()
}

func (a *App) noteSerial(ctx context.Context, device string, up bool) {
	if a == nil || a.publisher == nil {
		return
	}
	a.publisher.SetDeviceConnected(ctx, device, up)
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
	// A step backwards (NTP, manual clock) must not drive the bucket negative.
	if elapsed < 0 {
		elapsed = 0
	}
	a.cmdLastFill = now
	if a.cmdTokens < 0 {
		a.cmdTokens = 0
	}
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
	if a.serial == nil || strings.TrimSpace(a.serial.deviceName()) == "" {
		return
	}

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
			a.noteSerial(ctx, a.serial.deviceName(), false)
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
			a.noteSerial(ctx, a.serial.deviceName(), false)
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
	sessionCtx, sessionCancel := context.WithCancel(ctx)
	defer sessionCancel()
	if a.publisher != nil && a.publisher.serialRestart != nil {
		// Drop a signal left by the previous session, then listen before
		// noteSerial. That publish can block for the MQTT write budget, and a
		// ping timeout in that window must still cancel this session. Draining
		// afterwards used to throw the only restart signal away.
		a.publisher.DrainSerialRestart()
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
	if a.serial.IsConnected() {
		if a.publisher != nil {
			// Drop a PING from the previous session before status is published.
			// noteSerial can block in MQTT, and the ping loop can write 10;PING;
			// in that window. Resetting afterwards forgets that write: MarkPingWritten
			// only stamps a timestamp that is still set.
			a.publisher.ResetPingState()
		}
		a.noteSerial(ctx, a.serial.deviceName(), true)
		if sessionCtx.Err() == nil {
			if err := a.serial.Write("10;version;"); err != nil {
				log.Warn("failed to request rflink version", "device", a.serial.deviceName(), "err", err)
			}
		}
	}

	sessionStart := time.Now()
	err := a.serial.ReadLines(sessionCtx, func(line string) error {
		log.Debug("serial raw message", "message", line)
		a.publisher.NoteSerialActivityFrom(a.serial.deviceName())
		// PONG must be counted even when the publish queue is full.
		if info, perr := ParseGatewayMessage(line); perr == nil && info != nil {
			if info.Pong {
				a.publisher.NotePong()
			}
			// The queue may drop the line. The firmware still has to be kept
			// for the next retained PONG.
			if info.Version != "" {
				a.publisher.noteGatewayVersion(a.serial.deviceName(), info.Version)
			}
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}
		frame := serialFrame{Device: a.serial.deviceName(), Line: line}
		if a.enqueueSerial(frame) {
			return nil
		}
		a.noteSerialQueueFull(frame.Device)
		return nil
	})
	sessionUp := time.Since(sessionStart)

	if ctx.Err() != nil {
		return false
	}

	unresponsive := sessionCtx.Err() != nil
	a.serial.PauseWrites()
	if a.publisher != nil {
		a.noteSerial(ctx, a.serial.deviceName(), false)
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
		for {
			if !sleepContext(ctx, *reconnectDelay) {
				return false
			}
			*reconnectDelay = nextBackoff(*reconnectDelay, minDelay, maxDelay)
			if cerr = a.serial.CloseIfIdle(serialCloseWait); cerr == nil {
				break
			}
			log.Warn("serial close skipped, write still in the driver", "err", cerr, "retry_in", *reconnectDelay)
		}
	}
	a.serial.ResumeWrites()

	// Reset only after a read that stayed up. Time spent waiting for a stuck
	// write to leave the driver is not a live session.
	if sessionLivedLongEnough(sessionUp, minDelay) {
		*reconnectDelay = minDelay
	}
	if !sleepContext(ctx, *reconnectDelay) {
		return false
	}
	*reconnectDelay = nextBackoff(*reconnectDelay, minDelay, maxDelay)
	return true
}

// runExtraSerial reads another gateway into the same publish queue.
// Each port updates the shared serial status. With more than one port the
// aggregate topic is online only when every configured port is up.
func (a *App) runExtraSerial(ctx context.Context, sr *SerialReader) {
	defer a.wg.Done()
	minDelay := a.opts.serialReconnectMinDelay()
	maxDelay := a.opts.serialReconnectMaxDelay()
	delay := minDelay
	for {
		if ctx.Err() != nil {
			return
		}
		if !sr.IsConnected() {
			if err := sr.Reconnect(); err != nil {
				log.Warn("extra serial reconnect failed", "device", sr.deviceName(), "err", err, "retry_in", delay)
				a.noteSerial(ctx, sr.deviceName(), false)
				if !sleepContext(ctx, delay) {
					return
				}
				delay = nextBackoff(delay, minDelay, maxDelay)
				continue
			}
			log.Info("extra serial connected", "device", sr.deviceName())
		}
		sessionCtx, sessionCancel := context.WithCancel(ctx)
		if a.publisher != nil {
			a.publisher.drainExtraRestart(sr.deviceName())
			restart := a.publisher.extraRestartChan(sr.deviceName())
			go func() {
				select {
				case <-ctx.Done():
				case <-sessionCtx.Done():
				case <-restart:
					sessionCancel()
				}
			}()
		}
		if sr.IsConnected() {
			if a.publisher != nil {
				// Same order as the primary: clear the previous session before
				// noteSerial, which can block while a new PING is written.
				a.publisher.resetExtraPing(sr.deviceName())
			}
			a.noteSerial(ctx, sr.deviceName(), true)
			if sessionCtx.Err() == nil {
				if err := sr.Write("10;version;"); err != nil {
					log.Warn("failed to request rflink version", "device", sr.deviceName(), "err", err)
				}
			}
		}
		sessionStart := time.Now()
		err := sr.ReadLines(sessionCtx, func(line string) error {
			if a.publisher != nil {
				a.publisher.NoteSerialActivityFrom(sr.deviceName())
			}
			if info, perr := ParseGatewayMessage(line); perr == nil && info != nil && a.publisher != nil {
				if info.Pong {
					a.publisher.noteExtraPong(sr.deviceName())
				}
				if info.Version != "" {
					a.publisher.noteGatewayVersion(sr.deviceName(), info.Version)
				}
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			frame := serialFrame{Device: sr.deviceName(), Line: line}
			if a.enqueueSerial(frame) {
				return nil
			}
			a.noteSerialQueueFull(frame.Device)
			return nil
		})
		sessionUp := time.Since(sessionStart)
		sessionCancel()
		if ctx.Err() != nil {
			return
		}
		a.noteSerial(ctx, sr.deviceName(), false)
		if a.publisher != nil {
			a.publisher.drainExtraRestart(sr.deviceName())
			a.publisher.resetExtraPing(sr.deviceName())
		}
		sr.PauseWrites()
		if cerr := sr.CloseIfIdle(serialCloseWait); cerr != nil {
			log.Warn("extra serial close skipped, write still in the driver", "device", sr.deviceName(), "err", cerr)
			for {
				if !sleepContext(ctx, delay) {
					return
				}
				delay = nextBackoff(delay, minDelay, maxDelay)
				if cerr = sr.CloseIfIdle(serialCloseWait); cerr == nil {
					break
				}
				log.Warn("extra serial close skipped, write still in the driver", "device", sr.deviceName(), "err", cerr)
			}
		}
		sr.ResumeWrites()
		if sessionLivedLongEnough(sessionUp, minDelay) {
			delay = minDelay
		}
		log.Warn("extra serial read ended, reconnecting", "device", sr.deviceName(), "err", err, "retry_in", delay)
		if !sleepContext(ctx, delay) {
			return
		}
		delay = nextBackoff(delay, minDelay, maxDelay)
	}
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
	readers := make([]*SerialReader, 0, 1+len(a.extras))
	if a.serial != nil {
		readers = append(readers, a.serial)
	}
	readers = append(readers, a.extras...)
	for _, sr := range readers {
		a.sendPingTo(sr, sr == a.serial)
	}
}

func (a *App) sendPingTo(sr *SerialReader, primary bool) {
	if sr == nil || !sr.IsConnected() || !sr.WritesAllowed() {
		return
	}
	name := sr.deviceName()
	if a.publisher != nil {
		if primary && a.publisher.PingOutstanding() {
			log.Debug("skipping PING, previous still outstanding", "device", name)
			return
		}
		if !primary && a.publisher.extraPingOutstanding(name) {
			log.Debug("skipping PING, previous still outstanding", "device", name)
			return
		}
	}
	const cmd = "10;PING;"
	// Mark before the write so a PONG already sitting in the driver buffer
	// is not applied to a still-zero ping timestamp.
	if a.publisher != nil {
		if primary {
			a.publisher.MarkPingSent()
		} else {
			a.publisher.markExtraPing(name)
		}
		defer func() {
			if rec := recover(); rec != nil {
				if primary {
					a.publisher.abandonPingIfWriting()
				} else {
					a.publisher.abandonExtraPingIfWriting(name)
				}
				panic(rec)
			}
		}()
	}
	if err := sr.Write(cmd); err != nil {
		if a.publisher != nil {
			if primary {
				a.publisher.ClearPingSent()
			} else {
				a.publisher.clearExtraPing(name)
			}
		}
		log.Warn("failed to send RFLink PING", "device", name, "err", err)
		return
	}
	if a.publisher != nil {
		if primary {
			a.publisher.MarkPingWritten()
		} else {
			a.publisher.markExtraPingWritten(name)
		}
	}
	log.Debug("RFLink PING sent", "device", name)
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
	if a.serial != nil {
		a.serial.PauseWrites()
	}
	for _, sr := range a.extras {
		if sr != nil {
			sr.PauseWrites()
		}
	}
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
		if err := a.serial.CloseIfIdle(serialCloseWait); err != nil {
			log.Warn("serial close skipped, write still in the driver", "err", err)
		}
	}
	for _, sr := range a.extras {
		if sr == nil {
			continue
		}
		if err := sr.CloseIfIdle(serialCloseWait); err != nil {
			log.Warn("serial close skipped, write still in the driver", "device", sr.deviceName(), "err", err)
		}
	}

	log.Info("go-rflink stopped")
}
