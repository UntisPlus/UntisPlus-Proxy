package store

// Opt-in retention tests for student-authored state. Both tables are deleted by
// age only: a row inside the window must survive, so enabling retention cannot
// erase this term's work while the operator is looking at it.

import (
	"testing"
	"time"
)

func TestPruneHomeworkDoneRemovesOnlyOldRows(t *testing.T) {
	st := openOutboxStore(t)
	now := time.Now()
	if _, err := st.SetHomeworkDone("s", "alice", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetHomeworkDone("s", "alice", 2); err != nil {
		t.Fatal(err)
	}
	cutoff := now.Add(-24 * time.Hour)
	// Backdate the first flag past the cutoff; the second stays current.
	if _, err := st.db.Exec(`UPDATE homework_done SET done_at=? WHERE hw_id=1`, cutoff.Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	n, err := st.PruneHomeworkDone(cutoff)
	if err != nil {
		t.Fatalf("PruneHomeworkDone: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned %d rows, want 1", n)
	}
	flags, err := st.HomeworkDone("s", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, old := flags[1]; old {
		t.Error("the backdated flag survived pruning")
	}
	if _, current := flags[2]; !current {
		t.Error("the current flag was pruned")
	}
}

func TestPruneAbsenceNotesRemovesOnlyOldRows(t *testing.T) {
	st := openOutboxStore(t)
	now := time.Now()
	if _, err := st.SetAbsenceNote("s", "alice", 10, "old"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetAbsenceNote("s", "alice", 11, "new"); err != nil {
		t.Fatal(err)
	}
	cutoff := now.Add(-24 * time.Hour)
	if _, err := st.db.Exec(`UPDATE absence_notes SET updated_at=? WHERE absence_key=10`, cutoff.Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	n, err := st.PruneAbsenceNotes(cutoff)
	if err != nil {
		t.Fatalf("PruneAbsenceNotes: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned %d rows, want 1", n)
	}
	notes, err := st.AbsenceNotes("s", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, old := notes[10]; old {
		t.Error("the backdated note survived pruning")
	}
	if _, current := notes[11]; !current {
		t.Error("the current note was pruned")
	}
}

// TestPruneHomeworkDoneKeepsRowsAtCutoff: the cutoff is exclusive, so a row
// written exactly at the boundary survives until the next sweep.
func TestPruneHomeworkDoneKeepsRowsAtCutoff(t *testing.T) {
	st := openOutboxStore(t)
	cutoff := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	if _, err := st.SetHomeworkDone("s", "alice", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE homework_done SET done_at=? WHERE hw_id=1`, cutoff.Unix()); err != nil {
		t.Fatal(err)
	}
	n, err := st.PruneHomeworkDone(cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("pruned %d rows at the exact cutoff, want 0", n)
	}
}
