/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"os"
	"strings"
)

// loadDotEnv reads .env (or GORFLINK_ENV_FILE) into the environment.
// Variables that are already set win, so a shell export is kept.
func loadDotEnv() {
	path := strings.TrimSpace(os.Getenv("GORFLINK_ENV_FILE"))
	if path == "" {
		path = ".env"
	}
	loadDotEnvFile(path)
}

func loadDotEnvFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	loaded := 0
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if after, ok := strings.CutPrefix(line, "export "); ok {
			line = strings.TrimSpace(after)
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if key == "" {
			continue
		}
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, val); err != nil {
			continue
		}
		loaded++
	}
	if loaded > 0 {
		log.Info("loaded env file", "path", path, "keys", loaded)
	}
}
