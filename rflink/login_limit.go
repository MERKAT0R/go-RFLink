/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"sync"
	"time"
)

// loginLimiter is a simple per-IP sliding-window limiter for session login.
type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
	locked   map[string]time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{
		attempts: make(map[string][]time.Time),
		locked:   make(map[string]time.Time),
	}
}

const (
	loginWindow      = 5 * time.Minute
	loginMaxAttempts = 5
	loginLockout     = 15 * time.Minute
)

func (l *loginLimiter) allow(ip string) (ok bool, retryAfter time.Duration) {
	if ip == "" {
		ip = "unknown"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if until, hit := l.locked[ip]; hit {
		if now.Before(until) {
			return false, until.Sub(now)
		}
		delete(l.locked, ip)
		delete(l.attempts, ip)
	}
	// prune window
	cut := now.Add(-loginWindow)
	arr := l.attempts[ip]
	n := 0
	for _, t := range arr {
		if t.After(cut) {
			arr[n] = t
			n++
		}
	}
	arr = arr[:n]
	if len(arr) >= loginMaxAttempts {
		l.locked[ip] = now.Add(loginLockout)
		l.attempts[ip] = arr
		l.pruneLocked(now)
		return false, loginLockout
	}
	// Count this attempt before the password check so a parallel burst cannot
	// all observe a count below the limit.
	arr = append(arr, now)
	l.attempts[ip] = arr
	if len(arr) >= loginMaxAttempts {
		l.locked[ip] = now.Add(loginLockout)
	}
	l.pruneLocked(now)
	return true, 0
}

func (l *loginLimiter) pruneLocked(now time.Time) {
	if len(l.locked) < 256 && len(l.attempts) < 256 {
		return
	}
	for ip, until := range l.locked {
		if now.After(until) {
			delete(l.locked, ip)
		}
	}
	cut := now.Add(-loginWindow)
	for ip, arr := range l.attempts {
		n := 0
		for _, t := range arr {
			if t.After(cut) {
				arr[n] = t
				n++
			}
		}
		if n == 0 {
			delete(l.attempts, ip)
			continue
		}
		l.attempts[ip] = arr[:n]
	}
	for len(l.attempts) > 256 {
		oldestIP := ""
		var oldest time.Time
		for ip, arr := range l.attempts {
			if len(arr) == 0 {
				oldestIP = ip
				break
			}
			if oldestIP == "" || arr[0].Before(oldest) {
				oldestIP = ip
				oldest = arr[0]
			}
		}
		if oldestIP == "" {
			break
		}
		delete(l.attempts, oldestIP)
		delete(l.locked, oldestIP)
	}
	for len(l.locked) > 256 {
		oldestIP := ""
		var oldest time.Time
		for ip, until := range l.locked {
			if oldestIP == "" || until.Before(oldest) {
				oldestIP = ip
				oldest = until
			}
		}
		if oldestIP == "" {
			break
		}
		delete(l.locked, oldestIP)
	}
}

func (l *loginLimiter) success(ip string) {
	if ip == "" {
		ip = "unknown"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, ip)
	delete(l.locked, ip)
}
