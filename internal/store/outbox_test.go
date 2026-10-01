package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openOutboxStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestEnqueueInSnapshotTransaction is the core guarantee: the delivery row is
// written by the same transaction that stamps the new class version, so a
// committed change always leaves a delivery waiting.
func TestEnqueueInSnapshotTransaction(t *testing.T) {
	st := openOutboxStore(t)

	payload := []byte(`{"summary":"10b · 1 change"}`)
	changed, err := st.ApplyClassSnapshot("s", 1,
		[]PeriodRow{{PeriodID: 100, Start: "a", End: "b", Subject: "Mathe"}}, 7, "",
		func(rows []PeriodRow, put func(string, int64, []byte) error) error {
			if len(rows) != 1 {
				t.Errorf("enqueue saw %d changed rows, want 1", len(rows))
			}
			return put("webhook", 42, payload)
		})
	if err != nil {
		t.Fatalf("ApplyClassSnapshot: %v", err)
	}
	if changed != 1 {
		t.Fatalf("changed = %d, want 1", changed)
	}
	if v := st.ClassVersion("s", 1); v != 7 {
		t.Errorf("ClassVersion = %d, want 7", v)
	}

	rows, err := st.DueOutbox(time.Now(), 10)
	if err != nil {
		t.Fatalf("DueOutbox: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("queued deliveries = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.School != "s" || got.ClassID != 1 || got.Version != 7 || got.Dest != "webhook" || got.DestID != 42 {
		t.Errorf("queued row = %+v, want school=s class=1 version=7 dest=webhook destID=42", got)
	}
	if string(got.Payload) != string(payload) {
		t.Errorf("payload = %s, want %s", got.Payload, payload)
	}
}

// TestSnapshotRollbackDropsQueuedDelivery is the other half of the guarantee:
// when the enqueue callback fails the snapshot write must roll back with it.
// A half-applied change that announced itself — or that silently swallowed a
// notification — is exactly the bug this outbox exists to prevent.
func TestSnapshotRollbackDropsQueuedDelivery(t *testing.T) {
	st := openOutboxStore(t)

	_, err := st.ApplyClassSnapshot("s", 1,
		[]PeriodRow{{PeriodID: 100, Start: "a", End: "b", Subject: "Mathe"}}, 7, "",
		func(rows []PeriodRow, put func(string, int64, []byte) error) error {
			if err := put("webhook", 42, []byte(`{"summary":"x"}`)); err != nil {
				return err
			}
			return errors.New("destination lookup blew up")
		})
	if err == nil {
		t.Fatal("ApplyClassSnapshot succeeded, want the callback error to propagate")
	}
	if v := st.ClassVersion("s", 1); v != 0 {
		t.Errorf("ClassVersion = %d after rollback, want 0", v)
	}
	snap, err := st.LoadClassSnapshot("s", 1)
	if err != nil {
		t.Fatalf("LoadClassSnapshot: %v", err)
	}
	if len(snap) != 0 {
		t.Errorf("snapshot after rollback = %+v, want empty", snap)
	}
	rows, err := st.DueOutbox(time.Now(), 10)
	if err != nil {
		t.Fatalf("DueOutbox: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("queued deliveries after rollback = %d, want 0", len(rows))
	}
}

// TestClaimOutboxIsExclusive guards the single-worker assumption: a second
// worker must not pick up a row another worker already claimed, or one change
// would be delivered twice.
func TestClaimOutboxIsExclusive(t *testing.T) {
	st := openOutboxStore(t)
	if err := st.EnqueueOutbox("s", 1, 1, "ntfy", 5, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	rows, _ := st.DueOutbox(time.Now(), 10)
	if len(rows) != 1 {
		t.Fatalf("queued = %d, want 1", len(rows))
	}

	first, err := st.ClaimOutbox(rows[0].ID)
	if err != nil || !first {
		t.Fatalf("first claim = (%v, %v), want (true, nil)", first, err)
	}
	second, err := st.ClaimOutbox(rows[0].ID)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if second {
		t.Error("second claim succeeded on an already-claimed row, delivery would be duplicated")
	}
}

// TestFailedDeliveryBacksOffThenDies pins the retry contract: a destination that
// keeps failing must stop consuming work, but stay visible for an operator.
func TestFailedDeliveryBacksOffThenDies(t *testing.T) {
	st := openOutboxStore(t)
	if err := st.EnqueueOutbox("s", 1, 1, "webhook", 5, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	rows, _ := st.DueOutbox(time.Now(), 10)
	id := rows[0].ID

	// Attempt 1 fails: the row stays pending, but not due yet.
	if err := st.FailOutbox(id, 1, "dial refused", time.Now().Add(time.Hour), 3); err != nil {
		t.Fatal(err)
	}
	due, _ := st.DueOutbox(time.Now(), 10)
	if len(due) != 0 {
		t.Errorf("row is due immediately after failure, want backoff honoured: %+v", due)
	}
	// ...but it is still there, merely deferred.
	stats, err := st.OutboxStats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 1 || stats.Dead != 0 {
		t.Errorf("stats = %+v, want 1 pending / 0 dead", stats)
	}

	// Exhaust the attempts.
	if err := st.FailOutbox(id, 3, "dial refused", time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	stats, err = st.OutboxStats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Dead != 1 || stats.Pending != 0 {
		t.Errorf("stats = %+v, want 1 dead / 0 pending", stats)
	}
	dead, err := st.RecentDeadOutbox(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(dead) != 1 {
		t.Fatalf("RecentDeadOutbox = %d rows, want 1", len(dead))
	}
	if dead[0].Attempts != 3 || dead[0].LastErr != "dial refused" {
		t.Errorf("dead row = %+v, want attempts=3 last_error=dial refused", dead[0])
	}
}

// TestFinishOutboxRemovesTheRow: a delivered row is removed rather than kept
// forever, and finishing an already-finished row is a no-op.
func TestFinishOutboxRemovesTheRow(t *testing.T) {
	st := openOutboxStore(t)
	if err := st.EnqueueOutbox("s", 1, 1, "webhook", 5, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	rows, _ := st.DueOutbox(time.Now(), 10)
	id := rows[0].ID
	if _, err := st.ClaimOutbox(id); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishOutbox(id); err != nil {
		t.Fatal(err)
	}
	due, _ := st.DueOutbox(time.Now(), 10)
	if len(due) != 0 {
		t.Errorf("queue after delivery = %d rows, want 0", len(due))
	}
	if err := st.FinishOutbox(id); err != nil {
		t.Errorf("finishing an already-finished row: %v", err)
	}
}

// TestReleaseOutboxStaleRequeuesAbandonedClaims covers the crash case: a worker
// died holding rows in 'sending', which would otherwise strand those
// notifications forever. Requeued rows must be due immediately — delaying them
// again would just repeat the delay on the next restart.
func TestReleaseOutboxStaleRequeuesAbandonedClaims(t *testing.T) {
	st := openOutboxStore(t)
	if err := st.EnqueueOutbox("s", 1, 1, "webhook", 5, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	rows, _ := st.DueOutbox(time.Now(), 10)
	id := rows[0].ID
	if _, err := st.ClaimOutbox(id); err != nil {
		t.Fatal(err)
	}
	stats, err := st.OutboxStats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sending != 1 {
		t.Fatalf("stats after claim = %+v, want 1 sending", stats)
	}

	if err := st.ReleaseOutboxStale(); err != nil {
		t.Fatal(err)
	}
	stats, err = st.OutboxStats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 1 || stats.Sending != 0 {
		t.Errorf("stats = %+v, want 1 pending / 0 sending", stats)
	}
	due, _ := st.DueOutbox(time.Now(), 10)
	if len(due) != 1 {
		t.Errorf("reclaimed row is not due: %+v", due)
	}
}

// TestOutboxIsScopedPerSchool keeps one school's backlog out of another's.
func TestOutboxIsScopedPerSchool(t *testing.T) {
	st := openOutboxStore(t)
	if err := st.EnqueueOutbox("a", 1, 1, "webhook", 5, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	// Different school, different destination id.
	if err := st.EnqueueOutbox("b", 2, 9, "ntfy", 5, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	rows, _ := st.DueOutbox(time.Now(), 10)
	if len(rows) != 2 {
		t.Fatalf("queued = %d, want 2", len(rows))
	}
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.School+"/"+r.Dest] = true
	}
	for _, want := range []string{"a/webhook", "b/ntfy"} {
		if !seen[want] {
			t.Errorf("missing queued row for %s, got %v", want, seen)
		}
	}
}
