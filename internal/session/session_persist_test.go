package session

import (
	"sync"
	"testing"
	"time"
)

// fakePersister records what the manager writes and deletes so persistence can
// be asserted without a database.
type fakePersister struct {
	mu     sync.Mutex
	saved  map[string]Session
	delete []string
}

func newFakePersister() *fakePersister {
	return &fakePersister{saved: map[string]Session{}}
}

func (f *fakePersister) SaveSession(id, username string, classID int64, expires time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved[id] = Session{ID: id, Username: username, ClassID: classID, Expires: expires}
	return nil
}

func (f *fakePersister) DeleteSession(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.saved, id)
	f.delete = append(f.delete, id)
	return nil
}

func (f *fakePersister) snapshot() map[string]Session {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]Session, len(f.saved))
	for k, v := range f.saved {
		out[k] = v
	}
	return out
}

func TestManagerPersistsNewAndDelete(t *testing.T) {
	p := newFakePersister()
	m := NewManager(time.Hour)
	m.SetPersister(p)

	s := m.New("dee", 5000)
	saved := p.snapshot()
	got, ok := saved[s.ID]
	if !ok {
		t.Fatalf("session %q was not persisted", s.ID)
	}
	if got.Username != "dee" || got.ClassID != 5000 {
		t.Errorf("persisted = %+v, want dee/5000", got)
	}

	m.Delete(s.ID)
	if _, still := p.snapshot()[s.ID]; still {
		t.Errorf("session %q survived Delete in the persister", s.ID)
	}
}

func TestManagerRestoreOnlyLiveSessions(t *testing.T) {
	m := NewManager(time.Hour)
	live := Session{ID: "live", Username: "dee", ClassID: 5000, Expires: time.Now().Add(time.Hour)}
	dead := Session{ID: "dead", Username: "old", Expires: time.Now().Add(-time.Minute)}
	m.Restore([]Session{live, dead})

	if got := m.Get("live"); got == nil || got.Username != "dee" {
		t.Errorf("live session = %+v, want dee", got)
	}
	if got := m.Get("dead"); got != nil {
		t.Errorf("expired session restored: %+v", got)
	}
}

func TestManagerSweepPersistsExpiredDeletion(t *testing.T) {
	p := newFakePersister()
	m := NewManager(time.Hour)
	m.SetPersister(p)
	old := m.New("old", 0)
	m.sessions[old.ID].Expires = time.Now().Add(-time.Minute)
	m.sweepExpired()
	if _, ok := p.snapshot()[old.ID]; ok {
		t.Errorf("expired session %q survived the sweep in the persister", old.ID)
	}
}

type blockingPersister struct {
	*fakePersister
	entered chan string
	release chan struct{}
}

func (p *blockingPersister) SaveSession(id, username string, classID int64, expires time.Time) error {
	p.entered <- id
	<-p.release
	return p.fakePersister.SaveSession(id, username, classID, expires)
}

func TestCreationAndRevocationSerializePersistence(t *testing.T) {
	p := &blockingPersister{fakePersister: newFakePersister(), entered: make(chan string, 1), release: make(chan struct{})}
	m := NewManager(time.Hour)
	defer m.Close()
	m.SetPersister(p)
	created := make(chan struct{})
	go func() { m.New("person", 0); close(created) }()
	id := <-p.entered
	deleted := make(chan struct{})
	go func() { m.Delete(id); close(deleted) }()
	close(p.release)
	<-created
	<-deleted
	if m.Get(id) != nil {
		t.Fatal("revoked session remains in memory")
	}
	if _, ok := p.snapshot()[id]; ok {
		t.Fatal("revoked session was persisted after deletion")
	}
}
