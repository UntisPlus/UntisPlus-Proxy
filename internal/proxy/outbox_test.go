package proxy

// Behaviour of the durable delivery outbox at the proxy layer: what a queued
// change does when a destination is broken, missing, or one of several.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"untis-proxy/internal/session"
	"untis-proxy/internal/store"
	"untis-proxy/internal/untis"
)

// outboxProxy builds a proxy whose class 5000 has a single replayable owner, plus
// the class name cached, so change detection works without a fake upstream.
func outboxProxy(t *testing.T) (*Proxy, *store.Store) {
	t.Helper()
	p, st := newTestProxy(t)
	if err := st.SaveMasterNames("testschool", "CLASS", map[int64]string{5000: "10b"}); err != nil {
		t.Fatalf("seed class name: %v", err)
	}
	// Subscriptions here target TEACHER 100, so that name must resolve for the
	// change to match.
	if err := st.SaveMasterNames("testschool", "TEACHER", map[int64]string{100: "A. Hartley"}); err != nil {
		t.Fatalf("seed teacher name: %v", err)
	}
	seedClassOwner(t, st, "owen", 100, 5000)
	return p, st
}

func outboxChange() []store.PeriodRow {
	return []store.PeriodRow{
		{PeriodID: 10, Kind: "REMOVED", Teacher: "A. Hartley", Subject: "Mathe"},
	}
}

func outboxPrev() []store.PeriodRow {
	return []store.PeriodRow{
		{PeriodID: 10, Teacher: "A. Hartley", Subject: "Mathe"},
	}
}

// TestOneBrokenDestinationDoesNotBlockAnother is the isolation guarantee: an
// unreachable subscriber must not delay, fail, or replay the healthy one.
func TestOneBrokenDestinationDoesNotBlockAnother(t *testing.T) {
	var mu sync.Mutex
	var hits int
	goodCh := make(chan received, 8)
	goodSrv := receiver(t, goodCh)

	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		// A 500 is what a real webhook endpoint does when it is having a bad day.
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer badSrv.Close()

	p, st := outboxProxy(t)
	badID, err := st.AddWebhook(&store.Webhook{School: "testschool", ElementType: "TEACHER",
		ElementID: 100, URL: badSrv.URL, Enabled: true})
	if err != nil {
		t.Fatalf("add broken webhook: %v", err)
	}
	if _, err := st.AddWebhook(&store.Webhook{School: "testschool", ElementType: "TEACHER",
		ElementID: 100, URL: goodSrv.URL, Enabled: true}); err != nil {
		t.Fatalf("add healthy webhook: %v", err)
	}

	if err := p.deliverChange("testschool", 5000, 7, outboxChange(), outboxPrev()); err != nil {
		t.Fatalf("deliverChange: %v", err)
	}
	p.drainOutbox()

	// The healthy destination is served on the very first pass...
	got := awaitRecv(t, goodCh)
	if len(got.Body) == 0 {
		t.Error("healthy destination received an empty body")
	}
	// ...while the broken one is still queued for a retry, not lost.
	stats, err := st.OutboxStats()
	if err != nil {
		t.Fatalf("OutboxStats: %v", err)
	}
	if stats.Pending != 1 || stats.Dead != 0 {
		t.Errorf("stats = %+v, want 1 pending / 0 dead (only the broken destination should remain)", stats)
	}
	rows, err := st.DueOutbox(time.Now().Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("DueOutbox: %v", err)
	}
	if len(rows) != 1 || rows[0].DestID != badID {
		t.Errorf("remaining queue = %+v, want just the broken webhook %d", rows, badID)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Errorf("broken destination was called %d times in one drain, want exactly 1", hits)
	}
}

// TestRepeatedFailureEndsInDeadNotForever: a destination that never recovers
// must stop consuming work, but the exhaustion must remain visible.
func TestRepeatedFailureEndsInDeadNotForever(t *testing.T) {
	var mu sync.Mutex
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p, st := outboxProxy(t)
	if _, err := st.AddWebhook(&store.Webhook{School: "testschool", ElementType: "TEACHER",
		ElementID: 100, URL: srv.URL, Enabled: true}); err != nil {
		t.Fatalf("add webhook: %v", err)
	}
	if err := p.deliverChange("testschool", 5000, 7, outboxChange(), outboxPrev()); err != nil {
		t.Fatalf("deliverChange: %v", err)
	}

	// Drain far more often than the retry budget allows, stepping past the
	// backoff each time. A destination that never recovers must terminate rather
	// than retry forever.
	dead := false
	for i := 0; i < outboxMaxAttempts*3; i++ {
		p.drainOutboxDue(time.Now().Add(24 * time.Hour))
		if stats, err := st.OutboxStats(); err == nil && stats.Dead == 1 {
			dead = true
			break
		}
	}
	if !dead {
		stats, _ := st.OutboxStats()
		t.Fatalf("delivery never reached the dead state, stats = %+v", stats)
	}

	dead1, err := st.RecentDeadOutbox(10)
	if err != nil {
		t.Fatalf("RecentDeadOutbox: %v", err)
	}
	if len(dead1) != 1 || !strings.Contains(dead1[0].LastErr, "500") {
		t.Errorf("dead row = %+v, want one row carrying the failure reason", dead1)
	}
	if dead1[0].Attempts != outboxMaxAttempts {
		t.Errorf("dead row attempts = %d, want %d", dead1[0].Attempts, outboxMaxAttempts)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != outboxMaxAttempts {
		t.Errorf("destination called %d times, want %d", calls, outboxMaxAttempts)
	}
}

// TestBackoffGrowsAndIsCapped pins the retry pacing.
func TestBackoffGrowsAndIsCapped(t *testing.T) {
	if got := outboxBackoff(1); got != outboxBaseBackoff {
		t.Errorf("outboxBackoff(1) = %s, want %s", got, outboxBaseBackoff)
	}
	if got := outboxBackoff(2); got != 2*outboxBaseBackoff {
		t.Errorf("outboxBackoff(2) = %s, want %s", got, 2*outboxBaseBackoff)
	}
	for _, n := range []int{3, 8, 50, 1000} {
		if got := outboxBackoff(n); got > outboxMaxBackoff {
			t.Errorf("outboxBackoff(%d) = %s, exceeds the cap %s", n, got, outboxMaxBackoff)
		}
	}
	if got := outboxBackoff(40); got != outboxMaxBackoff {
		t.Errorf("outboxBackoff(40) = %s, want the cap %s", got, outboxMaxBackoff)
	}
}

// TestDeletedSubscriptionDropsQueuedDelivery: an operator deleting a broken
// endpoint is fixing the problem, and the backlog for it must clear rather than
// retry forever against nothing.
func TestDeletedSubscriptionDropsQueuedDelivery(t *testing.T) {
	var mu sync.Mutex
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p, st := outboxProxy(t)
	id, err := st.AddWebhook(&store.Webhook{School: "testschool", ElementType: "TEACHER",
		ElementID: 100, URL: srv.URL, Enabled: true})
	if err != nil {
		t.Fatalf("add webhook: %v", err)
	}
	if err := p.deliverChange("testschool", 5000, 7, outboxChange(), outboxPrev()); err != nil {
		t.Fatalf("deliverChange: %v", err)
	}
	p.drainOutbox() // fails once, stays queued

	if _, err := st.DeleteWebhook(id); err != nil {
		t.Fatalf("delete webhook: %v", err)
	}
	// Step past the backoff the failed attempt scheduled.
	p.drainOutboxDue(time.Now().Add(time.Hour))

	stats, err := st.OutboxStats()
	if err != nil {
		t.Fatalf("OutboxStats: %v", err)
	}
	if stats.Pending != 0 || stats.Dead != 0 {
		t.Errorf("stats = %+v, want an empty queue after the destination was deleted", stats)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("deleted destination called %d times, want 1 (the attempt before deletion)", calls)
	}
}

// TestStoredPayloadSurvivesARestart: the whole point of the outbox is that a
// queued notification survives the process going away. Reopening the same
// database must still show the delivery, with its payload intact.
func TestStoredPayloadSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.SaveMasterNames("testschool", "CLASS", map[int64]string{5000: "10b"}); err != nil {
		t.Fatalf("seed class name: %v", err)
	}
	// Subscriptions here target TEACHER 100, so that name must resolve for the
	// change to match.
	if err := st.SaveMasterNames("testschool", "TEACHER", map[int64]string{100: "A. Hartley"}); err != nil {
		t.Fatalf("seed teacher name: %v", err)
	}
	if _, err := st.AddWebhook(&store.Webhook{School: "testschool", ElementType: "TEACHER",
		ElementID: 100, URL: "https://example.invalid/hook", Enabled: true}); err != nil {
		t.Fatalf("add webhook: %v", err)
	}
	p := New(st, untis.New(untis.Config{Server: "school.example.com", School: "testschool"}), session.NewManager(5*time.Minute), Options{School: "testschool"})
	if err := p.deliverChange("testschool", 5000, 7, outboxChange(), outboxPrev()); err != nil {
		t.Fatalf("deliverChange: %v", err)
	}
	before, err := st.OutboxStats()
	if err != nil {
		t.Fatalf("OutboxStats: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	st2, err := store.Open(dir + "/test.db")
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer st2.Close()
	rows, err := st2.DueOutbox(time.Now(), 10)
	if err != nil {
		t.Fatalf("DueOutbox after restart: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("queued deliveries after restart = %d, want 1 (stats before restart %+v)", len(rows), before)
	}
	var payload struct {
		Event string `json:"event"`
	}
	if err := json.Unmarshal(rows[0].Payload, &payload); err != nil {
		t.Fatalf("stored payload did not survive a restart: %v", err)
	}
	if payload.Event == "" {
		t.Errorf("stored payload lost its content: %s", rows[0].Payload)
	}
}

// TestEmptyQueueDrainIsCheapAndSilent: the worker ticks every few seconds for
// the life of the process, so an empty queue must be a no-op, not an error path.
func TestEmptyQueueDrainIsCheapAndSilent(t *testing.T) {
	p, st := outboxProxy(t)
	if _, err := st.AddWebhook(&store.Webhook{School: "testschool", ElementType: "TEACHER",
		ElementID: 100, URL: "http://127.0.0.1:1/never", Enabled: true}); err != nil {
		t.Fatalf("add webhook: %v", err)
	}
	for i := 0; i < 3; i++ {
		p.drainOutbox()
	}
	stats, err := st.OutboxStats()
	if err != nil {
		t.Fatalf("OutboxStats: %v", err)
	}
	if stats.Pending != 0 || stats.Dead != 0 {
		t.Errorf("stats = %+v, want nothing queued when there are no changes", stats)
	}
}

// TestSendOutboxRowIgnoresUnknownDestination: a row naming a destination kind
// this build does not know about must be dropped quietly rather than retried
// forever against an unimplemented branch.
func TestSendOutboxRowIgnoresUnknownDestination(t *testing.T) {
	p, _ := outboxProxy(t)
	if err := p.sendOutboxRow(store.OutboxRow{School: "testschool", ClassID: 5000,
		Dest: "carrier-pigeon", DestID: 1, Payload: []byte(`{}`)}); err != nil {
		t.Errorf("unknown destination returned %v, want nil", err)
	}
}
