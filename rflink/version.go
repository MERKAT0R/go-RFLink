/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

// Set at build time:
//
//	go build -ldflags "-X rflink.Version=1.2.3 -X rflink.GitSHA=$(git rev-parse --short HEAD)"
var (
	Version = "dev"
	GitSHA  = "unknown"
)
