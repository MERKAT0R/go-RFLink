/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"log/slog"
	"os"
	"strings"
)

var log = slog.Default()

func initLogger(level string) {
	if debugEnabled() {
		level = "debug"
		log.Warn("!!!! APP DEBUG MODE !!!! LogLevel overrided by GORFLINK_DEBUG set to 'true'")
	}
	log = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLogLevel(level),
	}))
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
