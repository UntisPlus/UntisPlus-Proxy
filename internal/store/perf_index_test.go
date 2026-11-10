package store

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// PoolContains and OwnerForClass run on every class-timetable request. They
// filter on class_id, so without a users(class_id, school) index SQLite scans
// the whole users table per request (and the table only grows). These tests pin
// the index and the two properties that matter: it is actually chosen, and the
// empty-school fallback still matches.

func openIndexTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestUsersClassSchoolIndexExists(t *testing.T) {
	st := openIndexTestStore(t)
	var name string
	err := st.db.QueryRow(`SELECT name FROM sqlite_master
		WHERE type='index' AND name='users_class_school'`).Scan(&name)
	if err != nil {
		t.Fatalf("users_class_school index missing: %v", err)
	}
}

func TestHotPathQueriesUseClassIndex(t *testing.T) {
	st := openIndexTestStore(t)
	// Populate enough rows that the planner would notice a scan is expensive.
	for i := 0; i < 2000; i++ {
		school := "testschool"
		if i%2 == 0 {
			school = "other"
		}
		u := &User{
			Username: "u" + strconv.Itoa(i),
			Password: "x",
			PersonID: int64(100000 + i),
			ClassID:  int64(i % 100),
			School:   school,
			LastSeen: time.Now(),
		}
		if err := st.UpsertUser(u); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	queries := map[string]string{
		"PoolContains": `SELECT COUNT(*) FROM users WHERE class_id=? AND (school=? OR school='')`,
		"OwnerForClass": `SELECT ` + userCols + ` FROM users
			WHERE class_id=? AND (school=? OR school='') AND password<>''
			ORDER BY last_seen DESC, id DESC LIMIT 1`,
	}
	for name, q := range queries {
		t.Run(name, func(t *testing.T) {
			rows, err := st.db.Query("EXPLAIN QUERY PLAN "+q, 42, "testschool")
			if err != nil {
				t.Fatalf("explain: %v", err)
			}
			defer rows.Close()
			plan := ""
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatalf("scan plan: %v", err)
				}
				plan += detail + "\n"
			}
			if !strings.Contains(plan, "users_class_school") {
				t.Fatalf("%s plan does not use users_class_school:\n%s", name, plan)
			}
		})
	}
}

func TestEmptySchoolStillMatchesWithIndex(t *testing.T) {
	st := openIndexTestStore(t)
	for _, u := range []*User{
		{Username: "empty-school", Password: "x", ClassID: 42, School: "", LastSeen: time.Now()},
		{Username: "right-school", Password: "x", ClassID: 42, School: "testschool", LastSeen: time.Now()},
		{Username: "other-school", Password: "x", ClassID: 42, School: "other", LastSeen: time.Now()},
	} {
		if err := st.UpsertUser(u); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	ok, err := st.PoolContains("testschool", 42)
	if err != nil || !ok {
		t.Fatalf("PoolContains = %v, %v; want true", ok, err)
	}
	owner, err := st.OwnerForClass("testschool", 42)
	if err != nil || owner == nil || owner.Username == "other-school" {
		t.Fatalf("OwnerForClass = %+v, %v; must not pick another school", owner, err)
	}
}

// TestReadProceedsDuringWrite pins the WAL reader/writer split. The poller
// holds a write transaction for every class snapshot; with a single pooled
// connection every timetable read waits behind it. The read below must finish
// while the write transaction is still open.
func TestReadProceedsDuringWrite(t *testing.T) {
	st := openIndexTestStore(t)
	if err := st.SetDefaultSchool("testschool"); err != nil {
		t.Fatalf("default school: %v", err)
	}
	if err := st.UpsertUser(&User{Username: "u1", Password: "x", ClassID: 7, School: "testschool", LastSeen: time.Now()}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tx, err := st.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT OR REPLACE INTO timetable_changes
		(school,class_id,period_id,kind,start,end,subject,room,description,mod_ver)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		"testschool", 7, 1, "ADDED", "2026-11-10T08:00", "2026-11-10T08:45", "Math", "R1", "", 1); err != nil {
		t.Fatalf("write inside tx: %v", err)
	}

	type result struct {
		ok  bool
		err error
	}
	done := make(chan result, 1)
	go func() {
		ok, err := st.PoolContains("testschool", 7)
		done <- result{ok, err}
	}()

	select {
	case r := <-done:
		if r.err != nil || !r.ok {
			t.Fatalf("read during write = %v, %v", r.ok, r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("read blocked on an open write transaction; a single pooled connection serializes timetable reads behind the poller")
	}
}
