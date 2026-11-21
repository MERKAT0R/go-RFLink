/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

var log = slog.Default()

func initLogger(level string) {
	if debugEnabled() {
		level = "debug"
		log.Warn("!!!! APP DEBUG MODE !!!! LogLevel overrided by GORFLINK_DEBUG set to 'true'")
	}
	log = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLogLevel(level),
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
				a.Value = slog.StringValue(formatLogClock(a.Value.Time()))
			}
			return a
		},
	}))
}

// formatLogClock is the log timestamp: 17:20:06(UTC+3). The zone is the
// process zone (TZ). Seconds are whole; the old RFC3339 fraction looked like
// an offset and a UTC stamp looked late next to a local clock.
func formatLogClock(t time.Time) string {
	t = t.In(time.Local)
	_, offset := t.Zone()
	sign := "+"
	if offset < 0 {
		sign = "-"
		offset = -offset
	}
	hours := offset / 3600
	mins := (offset % 3600) / 60
	if mins == 0 {
		return fmt.Sprintf("%s(UTC%s%d)", t.Format("15:04:05"), sign, hours)
	}
	return fmt.Sprintf("%s(UTC%s%d:%02d)", t.Format("15:04:05"), sign, hours, mins)
}

func parseLogLevel(raw string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func debugEnabled() bool {
	return os.Getenv("GORFLINK_DEBUG") == "true"
}

// Full config logged in JSON then GORFLINK_DEBUGSHOWPARSEDCONFIGINLOGINSECURE mode - VERY INSECURE! Don't use without clearmind :)
func debugShowParsedConfigInLogINSECURE() bool {
	if debugEnabled() {
		return os.Getenv("GORFLINK_DEBUGSHOWPARSEDCONFIGINLOGINSECURE") == "true"
	}
	return false
}
