/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

const (
	sessionCookieName = "gorflink_session"
	sessionTTL        = 72 * time.Hour
	maxSessions       = 16
)

type session struct {
	ID        string
	CreatedAt time.Time
	ExpiresAt time.Time
	IP        string
}

type sessionStore struct {
	mu   sync.Mutex
	byID map[string]*session
}

func newSessionStore() *sessionStore {
	s := &sessionStore{byID: make(map[string]*session)}
	go s.reaper()
	return s
}

func (s *sessionStore) reaper() {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for range t.C {
		s.mu.Lock()
		now := time.Now()
		for id, sess := range s.byID {
			if now.After(sess.ExpiresAt) {
				delete(s.byID, id)
			}
		}
		s.mu.Unlock()
	}
}

func (s *sessionStore) Create(ip string) (*session, error) {
	id, err := randomSessionID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	sess := &session{
		ID:        id,
		CreatedAt: now,
		ExpiresAt: now.Add(sessionTTL),
		IP:        ip,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Evict oldest if over limit
	for len(s.byID) >= maxSessions {
		var oldestID string
		var oldestTime time.Time
		first := true
		for id, ss := range s.byID {
			if first || ss.CreatedAt.Before(oldestTime) {
				oldestID = id
				oldestTime = ss.CreatedAt
				first = false
			}
		}
		if oldestID != "" {
			delete(s.byID, oldestID)
			log.Info("session evicted (limit)", "session_id_prefix", oldestID[:8], "limit", maxSessions)
		} else {
			break
		}
	}
	s.byID[id] = sess
	return sess, nil
}

func (s *sessionStore) Get(id string) *session {
	if id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok {
		return nil
	}
	if time.Now().After(sess.ExpiresAt) {
		delete(s.byID, id)
		return nil
	}
	return sess
}

func (s *sessionStore) Revoke(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[id]; !ok {
		return false
	}
	delete(s.byID, id)
	return true
}

func (s *sessionStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byID)
}

func (s *sessionStore) RevokeAllExcept(keepID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id := range s.byID {
		if id == keepID {
			continue
		}
		delete(s.byID, id)
		n++
	}
	return n
}

func (s *sessionStore) List() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	out := make([]map[string]any, 0, len(s.byID))
	for id, sess := range s.byID {
		if now.After(sess.ExpiresAt) {
			continue
		}
		prefix := id
		if len(prefix) > 8 {
			prefix = prefix[:8]
		}
		out = append(out, map[string]any{
			"id":         id,
			"id_prefix":  prefix,
			"ip":         sess.IP,
			"created_at": sess.CreatedAt.Format(time.RFC3339),
			"expires_at": sess.ExpiresAt.Format(time.RFC3339),
		})
	}
	return out
}

func randomSessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, sess *session) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sess.ID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
		Expires:  sess.ExpiresAt,
		MaxAge:   int(time.Until(sess.ExpiresAt).Seconds()),
	})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	})
}

func sessionIDFromRequest(r *http.Request) string {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c == nil {
		return ""
	}
	return c.Value
}
