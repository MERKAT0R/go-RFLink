/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// rawHub fans out serial lines to WebSocket subscribers.
type rawHub struct {
	mu   sync.Mutex
	subs map[chan rawLineEntry]struct{}
}

func newRawHub() *rawHub {
	return &rawHub{subs: make(map[chan rawLineEntry]struct{})}
}

func (h *rawHub) subscribe() chan rawLineEntry {
	ch := make(chan rawLineEntry, 32)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *rawHub) unsubscribe(ch chan rawLineEntry) {
	h.mu.Lock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
	h.mu.Unlock()
}

func (h *rawHub) broadcast(entry rawLineEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- entry:
		default:
			// slow consumer — drop line for this client
		}
	}
}

const wsMaxReadBytes = 4096

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true // non-browser clients
		}
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" {
			return false
		}
		return strings.EqualFold(u.Host, r.Host)
	},
}

func (h *httpAPIServer) handleRawWS(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Error("websocket handler panic recovered",
				"path", r.URL.Path,
				"panic", fmt.Sprintf("%v", rec),
			)
		}
	}()
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Auth: GUI session cookie, or an API token with the read scope.
	sid, token, ok := h.wsCredentials(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Debug("websocket upgrade failed", "err", err)
		return
	}
	defer func() {
		_ = conn.Close()
	}()
	conn.SetReadLimit(wsMaxReadBytes)

	sub := h.rawHub.subscribe()
	defer h.rawHub.unsubscribe(sub)

	// Reader: detect client disconnect / pings. Frames are discarded.
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = recover() }()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	_ = conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_ = conn.WriteJSON(map[string]any{"type": "hello", "buffer": h.app.publisher.RecentRawLines()})

	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if !h.wsStillAllowed(sid, token) {
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case entry, ok := <-sub:
			if !ok {
				return
			}
			if !h.wsStillAllowed(sid, token) {
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteJSON(map[string]any{"type": "line", "at": entry.At, "device": entry.Device, "message": entry.Message}); err != nil {
				return
			}
		}
	}
}

// wsCredentials accepts a live GUI session or a read-scoped API token.
// Query tokens are not accepted: they land in proxy logs.
func (h *httpAPIServer) wsCredentials(r *http.Request) (sid, token string, ok bool) {
	if sid = sessionIDFromRequest(r); sid != "" && h.app.sessions.Get(sid) != nil {
		return sid, "", true
	}
	got := r.Header.Get("X-API-Token")
	if got == "" {
		auth := r.Header.Get("Authorization")
		if len(auth) >= 7 && strings.EqualFold(auth[:7], "bearer ") {
			got = strings.TrimSpace(auth[7:])
		}
	}
	if got != "" {
		if tok, valid := h.app.opts.validAPIToken(got); valid && tok.hasScope("read") {
			return "", got, true
		}
	}
	return "", "", false
}

func (h *httpAPIServer) wsStillAllowed(sid, token string) bool {
	if sid != "" {
		return h.app.sessions.Get(sid) != nil
	}
	if token == "" {
		return false
	}
	tok, ok := h.app.opts.validAPIToken(token)
	return ok && tok.hasScope("read")
}

func subtleConstantTimeEq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
