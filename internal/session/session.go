package session

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

type Session struct {
	ID       string
	Username string
	ClassID  int64
	UserID   int64
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
	stop      chan struct{}
	closeOnce sync.Once
}

func NewManager(ttl time.Duration) *Manager {
	m := &Manager{sessions: make(map[string]*Session), ttl: ttl, stop: make(chan struct{})}
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
	if _, err := rand.Read(b); err != nil {
		return nil
	}
	s := &Session{
		ID:       hex.EncodeToString(b),
		Username: username,
		ClassID:  classID,
		Expires:  time.Now().Add(m.ttl),
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.persister
	if binder, ok := p.(interface{ SessionUserID(string) (int64, error) }); ok {
		id, err := binder.SessionUserID(username)
		if err != nil || (id == 0 && username != "#anonymous#") {
			return nil
		}
		s.UserID = id
	}
	m.sessions[s.ID] = s
	if p != nil {
		// Bound storage must still recognize the account before issuing a
		// session. Legacy persisters retain their best-effort behavior.
		if bound, ok := p.(interface {
			SaveBoundSession(string, string, int64, int64, time.Time) error
		}); ok {
			if err := bound.SaveBoundSession(s.ID, s.Username, s.ClassID, s.UserID, s.Expires); err != nil {
				delete(m.sessions, s.ID)
				return nil
			}
		} else {
			_ = p.SaveSession(s.ID, s.Username, s.ClassID, s.Expires)
		}
	}
	copied := *s
	return &copied
}

func (m *Manager) Get(id string) *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := m.sessions[id]
	if s == nil || time.Now().After(s.Expires) {
		return nil
	}
	copied := *s
	return &copied
}

func (m *Manager) Delete(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
	p := m.persister
	if p != nil {
		_ = p.DeleteSession(id)
	}
}

func (m *Manager) gc() {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			m.sweepExpired()
		case <-m.stop:
			return
		}
	}
}

// sweepExpired removes expired sessions. Safe to call concurrently with the
// background GC; exposed so tests can drive it deterministically.
func (m *Manager) sweepExpired() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	var expired []string
	for id, s := range m.sessions {
		if now.After(s.Expires) {
			delete(m.sessions, id)
			expired = append(expired, id)
		}
	}
	p := m.persister
	if p != nil {
		for _, id := range expired {
			_ = p.DeleteSession(id)
		}
	}
}

// Close stops maintenance without revoking durable sessions.
func (m *Manager) Close() { m.closeOnce.Do(func() { close(m.stop) }) }

// DeleteUser revokes all live sessions after account deletion.
func (m *Manager) DeleteUser(username string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		if strings.EqualFold(strings.TrimSpace(username), s.Username) {
			delete(m.sessions, id)
			if m.persister != nil {
				_ = m.persister.DeleteSession(id)
			}
		}
	}
}
