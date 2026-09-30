package session

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

type Session struct {
	ID       string
	Username string
	ClassID  int64
	Expires  time.Time
}

type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	ttl      time.Duration
}

func NewManager(ttl time.Duration) *Manager {
	m := &Manager{sessions: make(map[string]*Session), ttl: ttl}
	go m.gc()
	return m
}

func (m *Manager) New(username string, classID int64) *Session {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	s := &Session{
		ID:       hex.EncodeToString(b),
		Username: username,
		ClassID:  classID,
		Expires:  time.Now().Add(m.ttl),
	}
	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()
	return s
}

func (m *Manager) Get(id string) *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := m.sessions[id]
	if s == nil || time.Now().After(s.Expires) {
		return nil
	}
	return s
}

func (m *Manager) Delete(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
}

func (m *Manager) gc() {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for range t.C {
		m.sweepExpired()
	}
}

// sweepExpired removes expired sessions. Safe to call concurrently with the
// background GC; exposed so tests can drive it deterministically.
func (m *Manager) sweepExpired() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for id, s := range m.sessions {
		if now.After(s.Expires) {
			delete(m.sessions, id)
		}
	}
}
