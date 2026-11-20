/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"net"
	"sync"
	"testing"
)

func TestLoginLimiterCountsBeforeCheck(t *testing.T) {
	l := newLoginLimiter()
	var wg sync.WaitGroup
	okN := 0
	var mu sync.Mutex
	for range 20 {
		wg.Go(func() {
			ok, _ := l.allow("10.0.0.1")
			if ok {
				mu.Lock()
				okN++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if okN != loginMaxAttempts {
		t.Fatalf("allowed %d parallel attempts, want %d", okN, loginMaxAttempts)
	}
	if ok, _ := l.allow("10.0.0.1"); ok {
		t.Fatal("attempt after the burst was allowed")
	}
}

func TestRejectMetadataIPv6(t *testing.T) {
	if err := rejectDangerousIP(net.ParseIP("fd00:ec2::254")); err == nil {
		t.Fatal("AWS IPv6 IMDS was allowed")
	}
	if err := rejectDangerousIP(net.ParseIP("192.168.1.10")); err != nil {
		t.Fatalf("LAN address rejected: %v", err)
	}
}
