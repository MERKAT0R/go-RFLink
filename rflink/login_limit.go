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
	l.attempts[ip] = arr
	if len(arr) >= loginMaxAttempts {
		l.locked[ip] = now.Add(loginLockout)
		return false, loginLockout
	}
	return true, 0
}

func (l *loginLimiter) fail(ip string) {
	if ip == "" {
		ip = "unknown"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.attempts[ip] = append(l.attempts[ip], time.Now())
	if len(l.attempts[ip]) >= loginMaxAttempts {
		l.locked[ip] = time.Now().Add(loginLockout)
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
