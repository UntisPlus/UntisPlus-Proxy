package proxy

// Retention is destructive and opt-in, so the default path must be a no-op even
// when the sweep runs. The store has the row-level cutoff tests; this checks the
// proxy switch and that the sweep deletes both tables once enabled.

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"untis-proxy/internal/session"
	"untis-proxy/internal/store"
	"untis-proxy/internal/untis"
)

// newRetentionProxy opens a proxy over a database whose path the test can reach,
// so it can backdate rows the public store API writes with "now".
func newRetentionProxy(t *testing.T) (*Proxy, *store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	p := New(st, untis.New(untis.Config{Server: "school.example.com", School: "testschool"}),
		session.NewManager(5*time.Minute), Options{School: "testschool"})
	return p, st, path
}

func TestPruneRetentionDoesNothingWhenDisabled(t *testing.T) {
	old := retentionAge
	t.Cleanup(func() { retentionAge = old })
	SetRetentionAge(0)

	p, st, _ := newRetentionProxy(t)
	if _, err := st.SetHomeworkDone("testschool", "alice", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetAbsenceNote("testschool", "alice", 10, "keep me"); err != nil {
		t.Fatal(err)
	}

	p.pruneRetention(time.Now())

	if n, err := st.HomeworkDoneCount("testschool", "alice"); err != nil || n != 1 {
		t.Errorf("homework flags = %d, %v; want the row kept while retention is off", n, err)
	}
	if notes, err := st.AbsenceNotes("testschool", "alice"); err != nil || len(notes) != 1 {
		t.Errorf("absence notes = %d, %v; want the note kept while retention is off", len(notes), err)
	}
}

func TestPruneRetentionRemovesRowsOlderThanAge(t *testing.T) {
	old := retentionAge
	t.Cleanup(func() { retentionAge = old })

	p, st, path := newRetentionProxy(t)
	if _, err := st.SetHomeworkDone("testschool", "alice", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetAbsenceNote("testschool", "alice", 10, "old"); err != nil {
		t.Fatal(err)
	}

	// Backdate both rows into the previous term. The columns keep whole seconds,
	// so pruning a row written "now" with a tiny age would prove nothing.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer db.Close()
	oldAt := time.Now().Add(-2 * time.Hour).Unix()
	if _, err := db.Exec(`UPDATE homework_done SET done_at=? WHERE hw_id=1`, oldAt); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE absence_notes SET updated_at=? WHERE absence_key=10`, oldAt); err != nil {
		t.Fatal(err)
	}

	SetRetentionAge(time.Hour)
	p.pruneRetention(time.Now())

	if n, err := st.HomeworkDoneCount("testschool", "alice"); err != nil || n != 0 {
		t.Errorf("homework flags = %d, %v; want 0 after pruning", n, err)
	}
	if notes, err := st.AbsenceNotes("testschool", "alice"); err != nil || len(notes) != 0 {
		t.Errorf("absence notes = %d, %v; want 0 after pruning", len(notes), err)
	}
}
