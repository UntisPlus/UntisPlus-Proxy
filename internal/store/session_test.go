package store

import (
	"testing"
	"time"
)

func TestSessionRoundTrip(t *testing.T) {
	st := openTestStore(t)
	expires := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := st.SaveSession("sid-1", "Dee", 5000, expires); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	sessions, err := st.LoadSessions(time.Now())
	if err != nil {
		t.Fatalf("LoadSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %v, want one", sessions)
	}
	got := sessions[0]
	if got.ID != "sid-1" || got.Username != "dee" || got.ClassID != 5000 {
		t.Errorf("session = %+v, want sid-1/dee/5000", got)
	}
	if !got.Expires.Equal(expires) {
		t.Errorf("expires = %v, want %v", got.Expires, expires)
	}

	if err := st.DeleteSession("sid-1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	sessions, err = st.LoadSessions(time.Now())
	if err != nil {
		t.Fatalf("LoadSessions after delete: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("sessions after delete = %v, want none", sessions)
	}
}

func TestLoadSessionsDropsExpired(t *testing.T) {
	st := openTestStore(t)
	if err := st.SaveSession("old", "dee", 0, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	sessions, err := st.LoadSessions(time.Now())
	if err != nil {
		t.Fatalf("LoadSessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expired session was restored: %v", sessions)
	}
	// A second load must not see it either: LoadSessions pruned it.
	if sessions, err = st.LoadSessions(time.Now()); err != nil || len(sessions) != 0 {
		t.Errorf("second LoadSessions = %v, %v; want empty", sessions, err)
	}
}

func TestClassPeriodsBetween(t *testing.T) {
	st := openTestStore(t)
	rows := [][2]string{
		{"2026-10-01 08:00", "2026-10-01 08:45"},
		{"2026-10-01T10:00", "2026-10-01T10:45"},
		{"2026-10-02 08:00", "2026-10-02 08:45"},
		{"2026-10-03 08:00", "2026-10-03 08:45"},
	}
	for i, r := range rows {
		if _, err := st.db.Exec(`INSERT INTO timetable_changes
			(school, class_id, period_id, kind, start, end, subject, room, teacher, description, mod_ver)
			VALUES (?, ?, ?, 'UNCHANGED', ?, ?, 'Mathe', '', '', '', 1)`,
			"testschool", 5000, i+1, r[0], r[1]); err != nil {
			t.Fatalf("insert period: %v", err)
		}
	}
	if _, err := st.db.Exec(`INSERT INTO timetable_changes
		(school, class_id, period_id, kind, start, end, subject, room, teacher, description, mod_ver)
		VALUES ('other', 5000, 99, 'UNCHANGED', '2026-10-01 08:00', '2026-10-01 08:45', 'X', '', '', '', 1)`); err != nil {
		t.Fatalf("insert other-school period: %v", err)
	}

	got, err := st.ClassPeriodsBetween("testschool", 5000, "2026-10-01", "2026-10-02")
	if err != nil {
		t.Fatalf("ClassPeriodsBetween: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("periods = %d, want the two on the 1st and one on the 2nd (%v)", len(got), got)
	}
	if got[0].Start != "2026-10-01 08:00" || got[2].Start != "2026-10-02 08:00" {
		t.Errorf("periods not in start order or wrong window: %+v", got)
	}
	none, err := st.ClassPeriodsBetween("testschool", 5000, "2026-10-04", "2026-10-05")
	if err != nil {
		t.Fatalf("empty range: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("empty range returned %v", none)
	}
}
