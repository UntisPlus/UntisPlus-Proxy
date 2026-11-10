package store

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// Benchmarks for the paths that run on every timetable request and every poll.
// They are not part of the normal test run; use:
//
//	go test ./internal/store -run '^$' -bench 'BenchmarkHotPath|BenchmarkReadDuringWrite|BenchmarkApplyClassSnapshot'

func seedPerfUsers(b *testing.B, st *Store, users, classes int) {
	b.Helper()
	for i := 0; i < users; i++ {
		school := "testschool"
		if i%2 == 0 {
			school = "other"
		}
		u := &User{
			Username: fmt.Sprintf("u%05d", i),
			Password: "x",
			PersonID: int64(100000 + i),
			ClassID:  int64(i % classes),
			School:   school,
			LastSeen: time.Now(),
		}
		if err := st.UpsertUser(u); err != nil {
			b.Fatalf("seed user: %v", err)
		}
	}
}

func openPerfStore(b *testing.B) *Store {
	b.Helper()
	st, err := Open(filepath.Join(b.TempDir(), "perf.db"))
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	b.Cleanup(func() { st.Close() })
	_ = st.SetDefaultSchool("testschool")
	return st
}

func BenchmarkHotPath(b *testing.B) {
	st := openPerfStore(b)
	seedPerfUsers(b, st, 5000, 200)

	b.Run("Pool", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := st.Pool("testschool"); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("PoolContains", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := st.PoolContains("testschool", 42); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("OwnerForClass", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := st.OwnerForClass("testschool", 42); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("GetUser", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := st.GetUser("u01234"); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkReadDuringWrite approximates a timetable read while the poller holds
// a snapshot transaction. WAL plus more than one pooled connection should keep
// this close to the uncontended read cost.
func BenchmarkReadDuringWrite(b *testing.B) {
	st := openPerfStore(b)
	seedPerfUsers(b, st, 5000, 200)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			tx, err := st.db.Begin()
			if err != nil {
				return
			}
			for i := 0; i < 200; i++ {
				_, _ = tx.Exec(`INSERT OR REPLACE INTO timetable_changes
					(school,class_id,period_id,kind,start,end,subject,room,description,mod_ver)
					VALUES (?,?,?,?,?,?,?,?,?,?)`,
					"testschool", i%50, 900000+i, "CHANGED", "2026-11-10T08:00", "2026-11-10T08:45",
					"Math", "R1", "", 1)
			}
			_ = tx.Commit()
		}
	}()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := st.PoolContains("testschool", 42); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	close(stop)
	<-done
}

func snapshotRows(n int) []PeriodRow {
	rows := make([]PeriodRow, n)
	for i := range rows {
		rows[i] = PeriodRow{
			PeriodID: int64(800000 + i), Kind: "ADDED",
			Start: "2026-11-10T08:00", End: "2026-11-10T08:45",
			Subject: "Math", Room: "R1", Teacher: "T1",
		}
	}
	return rows
}

// BenchmarkApplyClassSnapshot measures the per-class write the poller performs
// every interval. It runs inside one transaction, which is why readers must not
// be serialized behind it.
func BenchmarkApplyClassSnapshot(b *testing.B) {
	st := openPerfStore(b)
	seedPerfUsers(b, st, 5000, 200)
	rows := snapshotRows(200)
	if _, err := st.ReplaceClassSnapshot("testschool", 42, rows, 1, "2026-11-10"); err != nil {
		b.Fatal(err)
	}
	ver := int64(1)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ver++
		if _, err := st.ApplyClassSnapshotWindow("testschool", 42, rows, ver, "2026-11-10", "2026-11-14", nil); err != nil {
			b.Fatal(err)
		}
	}
}
