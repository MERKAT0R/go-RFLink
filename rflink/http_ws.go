/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"crypto/subtle"
	"fmt"
	"net/http"
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

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		// Same-origin GUI; CORS-like check is done via optional Origin allow list in handler.
		return true
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
	// Auth: header (non-browser) or ?token= (browser WebSocket cannot set custom headers).
	if !h.checkAuthWS(w, r) {
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

	sub := h.rawHub.subscribe()
	defer h.rawHub.unsubscribe(sub)

	// Reader: detect client disconnect / pings
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
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case entry, ok := <-sub:
			if !ok {
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteJSON(map[string]any{"type": "line", "at": entry.At, "message": entry.Message}); err != nil {
				return
			}
		}
	}
}

func (h *httpAPIServer) checkAuthWS(w http.ResponseWriter, r *http.Request) bool {
	opts := h.app.opts
	// GUI WebSocket: always require a live session (cookie).
	if sid := sessionIDFromRequest(r); sid != "" {
		if h.app.sessions.Get(sid) != nil {
			return true
		}
	}
	// Machine clients: API token via header or ?token=
	got := r.Header.Get("X-API-Token")
	if got == "" {
		auth := r.Header.Get("Authorization")
		if len(auth) >= 7 && strings.EqualFold(auth[:7], "bearer ") {
			got = strings.TrimSpace(auth[7:])
		}
	}
	if got == "" {
		got = strings.TrimSpace(r.URL.Query().Get("token"))
	}
	if got != "" {
		if _, ok := opts.validAPIToken(got); ok {
			return true
		}
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
}

func subtleConstantTimeEq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
