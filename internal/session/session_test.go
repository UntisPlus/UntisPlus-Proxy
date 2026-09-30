package session

import (
	"testing"
	"time"
)

func TestManagerNewAndGet(t *testing.T) {
	m := NewManager(5 * time.Minute)
	s := m.New("dee", 5000)
	if s.ID == "" || len(s.ID) != 32 {
		t.Fatalf("session id = %q, want 32 hex chars", s.ID)
	}
	if s.Username != "dee" || s.ClassID != 5000 {
		t.Errorf("session = %+v, want dee/5000", s)
	}
	if !s.Expires.After(time.Now()) {
		t.Errorf("expires = %v, want future", s.Expires)
	}
	if got := m.Get(s.ID); got != s {
		t.Errorf("Get returned %+v, want %+v", got, s)
	}
}

func TestManagerNewGeneratesUniqueIDs(t *testing.T) {
	m := NewManager(time.Minute)
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		id := m.New("u", int64(i)).ID
		if seen[id] {
			t.Fatalf("duplicate session id %q", id)
		}
		seen[id] = true
	}
}

func TestManagerGetUnknownAndExpired(t *testing.T) {
	m := NewManager(time.Nanosecond)
	if got := m.Get("nope"); got != nil {
		t.Fatalf("Get(unknown) = %+v, want nil", got)
	}
	s := m.New("dee", 0)
	s.Expires = time.Now().Add(-time.Second)
	if got := m.Get(s.ID); got != nil {
		t.Fatalf("Get(expired) = %+v, want nil", got)
	}
}

func TestManagerDelete(t *testing.T) {
	m := NewManager(time.Minute)
	s := m.New("dee", 0)
	m.Delete(s.ID)
	if got := m.Get(s.ID); got != nil {
		t.Fatalf("Get after Delete = %+v, want nil", got)
	}
	// deleting a missing id must not panic
	m.Delete("missing")
}

func TestManagerSweepExpired(t *testing.T) {
	m := NewManager(time.Minute)
	a := m.New("a", 0)
	b := m.New("b", 0)
	a.Expires = time.Now().Add(-time.Minute) // expired
	m.sweepExpired()
	if m.Get(a.ID) != nil {
		t.Error("expired session a survived sweep")
	}
	if m.Get(b.ID) == nil {
		t.Error("live session b was swept")
	}
}

func TestManagerBackgroundGC(t *testing.T) {
	// The background goroutine must not leak or panic across several sweep
	// intervals; force a sweep and confirm live sessions survive.
	m := NewManager(time.Minute)
	live := m.New("dee", 0)
	old := m.New("old", 0)
	old.Expires = time.Now().Add(-time.Hour)
	m.sweepExpired()
	m.sweepExpired()
	if m.Get(live.ID) == nil {
		t.Error("live session lost")
	}
	if m.Get(old.ID) != nil {
		t.Error("expired session not collected")
	}
}

func TestManagerConcurrentAccess(t *testing.T) {
	m := NewManager(time.Minute)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			m.New("w", int64(i))
		}
	}()
	for i := 0; i < 100; i++ {
		m.New("r", int64(i))
		m.sweepExpired()
	}
	<-done
}
