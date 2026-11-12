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

// Persister stores sessions across a restart. It is an interface so the session
// package does not depend on the SQLite store; the store's methods happen to
// satisfy it.
type Persister interface {
	SaveSession(id, username string, classID int64, expires time.Time) error
	DeleteSession(id string) error
}

type Manager struct {
	mu        sync.RWMutex
	sessions  map[string]*Session
	ttl       time.Duration
	persister Persister
}

func NewManager(ttl time.Duration) *Manager {
	m := &Manager{sessions: make(map[string]*Session), ttl: ttl}
	go m.gc()
	return m
}

// SetPersister attaches durable storage. It is called once during startup,
// before the HTTP server accepts requests.
func (m *Manager) SetPersister(p Persister) {
	m.mu.Lock()
	m.persister = p
	m.mu.Unlock()
}

// Restore loads sessions persisted by an earlier process. Only unexpired ones
// are admitted; an ID that is already live keeps the existing entry.
func (m *Manager) Restore(sessions []Session) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range sessions {
		s := sessions[i]
		if s.ID == "" || !s.Expires.After(now) {
			continue
		}
		if _, exists := m.sessions[s.ID]; exists {
			continue
		}
		copied := s
		m.sessions[s.ID] = &copied
	}
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
	p := m.persister
	m.mu.Unlock()
	if p != nil {
		// A failed write only costs a re-login after a restart; it must never
		// fail the login that just succeeded in memory.
		_ = p.SaveSession(s.ID, s.Username, s.ClassID, s.Expires)
	}
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
	p := m.persister
	m.mu.Unlock()
	if p != nil {
		_ = p.DeleteSession(id)
	}
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
	now := time.Now()
	var expired []string
	for id, s := range m.sessions {
		if now.After(s.Expires) {
			delete(m.sessions, id)
			expired = append(expired, id)
		}
	}
	p := m.persister
	m.mu.Unlock()
	if p != nil {
		for _, id := range expired {
			_ = p.DeleteSession(id)
		}
	}
}
