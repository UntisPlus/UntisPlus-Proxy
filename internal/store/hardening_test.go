package store

// Regression tests for the store-side hardening pass: exhausted outbox rows are
// eventually pruned, and the element-name catalog drops names the latest master
// data snapshot no longer lists.

import (
	"testing"
	"time"
)

func TestPruneDeadOutboxRemovesOnlyOldDeadRows(t *testing.T) {
	st := openOutboxStore(t)

	// One old, exhausted delivery: dead, with its death time in the past.
	if err := st.EnqueueOutbox("s", 1, 1, "webhook", 5, []byte(`{"summary":"old"}`)); err != nil {
		t.Fatalf("enqueue old: %v", err)
	}
	old, err := st.DueOutbox(time.Now(), 10)
	if err != nil || len(old) != 1 {
		t.Fatalf("DueOutbox(old) = %v, %v", old, err)
	}
	if err := st.FailOutbox(old[0].ID, 8, "dial refused", time.Now().Add(-8*24*time.Hour), 8); err != nil {
		t.Fatalf("fail old: %v", err)
	}

	// One recent dead row that must survive the same sweep.
	if err := st.EnqueueOutbox("s", 2, 2, "webhook", 5, []byte(`{"summary":"fresh"}`)); err != nil {
		t.Fatalf("enqueue fresh: %v", err)
	}
	fresh, _ := st.DueOutbox(time.Now(), 10)
	if err := st.FailOutbox(fresh[0].ID, 8, "dial refused", time.Now(), 8); err != nil {
		t.Fatalf("fail fresh: %v", err)
	}

	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	n, err := st.PruneDeadOutbox(cutoff)
	if err != nil {
		t.Fatalf("PruneDeadOutbox: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned %d rows, want 1 (only the row dead before the cutoff)", n)
	}
	dead, _ := st.RecentDeadOutbox(10)
	if len(dead) != 1 || dead[0].ClassID != 2 {
		t.Errorf("remaining dead rows = %+v, want only the fresh one", dead)
	}
}

func TestPruneDeadOutboxLeavesPendingUntouched(t *testing.T) {
	st := openOutboxStore(t)
	if err := st.EnqueueOutbox("s", 1, 1, "webhook", 5, []byte(`{}`)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	n, err := st.PruneDeadOutbox(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("PruneDeadOutbox: %v", err)
	}
	if n != 0 {
		t.Errorf("pruned %d pending rows, want 0", n)
	}
}

// TestSaveMasterNamesPrunesRemovedElements: a rename or removal upstream must not
// leave a stale name resolvable forever.
func TestSaveMasterNamesPrunesRemovedElements(t *testing.T) {
	st := openOutboxStore(t)
	if err := st.SaveMasterNames("testschool", "TEACHER", map[int64]string{1: "Alpha", 2: "Beta"}); err != nil {
		t.Fatalf("first snapshot: %v", err)
	}
	if err := st.SaveMasterNames("testschool", "TEACHER", map[int64]string{1: "Alpha"}); err != nil {
		t.Fatalf("second snapshot: %v", err)
	}
	if id, err := st.LookupElement("testschool", "TEACHER", "Beta"); err == nil {
		t.Errorf("removed teacher still resolves to %d", id)
	}
	if id, err := st.LookupElement("testschool", "TEACHER", "Alpha"); err != nil || id != 1 {
		t.Errorf("kept teacher = %d, %v; want 1, nil", id, err)
	}
}

// TestSaveMasterNamesKeepsSharedRows: a row with an empty school is shared across
// deployments and must never be pruned by a school-scoped snapshot.
func TestSaveMasterNamesKeepsSharedRows(t *testing.T) {
	st := openOutboxStore(t)
	if _, err := st.db.Exec(`INSERT INTO element_names (school, el_type, el_id, name)
		VALUES ('', 'TEACHER', 99, 'Shared')`); err != nil {
		t.Fatalf("seed shared row: %v", err)
	}
	if err := st.SaveMasterNames("testschool", "TEACHER", map[int64]string{1: "Alpha"}); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if got := st.ElementName("testschool", "TEACHER", 99); got != "Shared" {
		t.Errorf("shared row was pruned: name = %q, want Shared", got)
	}
}
