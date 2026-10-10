package store

// Tests for the proxy-local absence register on hand-authored events. None of
// this is ever written to Untis, so correctness rests on the store alone: the
// derived id must be stable enough for the app to delete with, one student's
// absence must converge on one row, and the admin window must respect its
// boundaries exactly.

import "testing"

func TestEventAbsenceIDIsStableAndInSpace(t *testing.T) {
	a := EventAbsenceID("appt-key", "dee")
	if a != EventAbsenceID("appt-key", "dee") {
		t.Error("the id is not stable for the same appointment and student")
	}
	if a == EventAbsenceID("appt-key", "sam") {
		t.Error("two students on one appointment share an id")
	}
	if a == EventAbsenceID("other-key", "dee") {
		t.Error("two appointments share an id")
	}
	if a > eventAbsenceIDSpace || a <= eventAbsenceIDSpace-1_000_000_000 {
		t.Errorf("id = %d, want it inside the synthetic absence space", a)
	}
	// The fold of the username must not matter: the app names the account, the
	// proxy stores it folded, and the derived id has to agree either way.
	if EventAbsenceID("appt-key", "Dee") != EventAbsenceID("appt-key", "dee") {
		t.Error("the id changes with the case of the username")
	}
}

func TestEventAbsenceRoundTripAndUpsertConverges(t *testing.T) {
	st := openTestStore(t)
	key := "appt-key"
	if _, err := st.SetEventAbsence("testschool", key, "2026-10-01", "dee", "T14:00", "T15:00", "tech1"); err != nil {
		t.Fatalf("SetEventAbsence: %v", err)
	}
	if _, err := st.SetEventAbsence("testschool", key, "2026-10-01", "dee", "T14:30", "T15:30", "tech2"); err != nil {
		t.Fatalf("SetEventAbsence upsert: %v", err)
	}
	rows, err := st.EventAbsences("testschool", key)
	if err != nil {
		t.Fatalf("EventAbsences: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want one after an upsert: %+v", len(rows), rows)
	}
	if rows[0].StartTime != "T14:30" || rows[0].CreatedBy != norm("tech2") {
		t.Errorf("row = %+v, want the latest window and author", rows[0])
	}

	byID, ok, err := st.EventAbsenceByID("testschool", EventAbsenceID(key, "dee"))
	if err != nil || !ok {
		t.Fatalf("EventAbsenceByID: %v (found %v)", err, ok)
	}
	if byID.Username != eventKey("dee") {
		t.Errorf("username = %q, want the folded account", byID.Username)
	}

	found, err := st.DeleteEventAbsence("testschool", key, "dee")
	if err != nil || !found {
		t.Fatalf("DeleteEventAbsence: %v (found %v)", err, found)
	}
	if rows, _ := st.EventAbsences("testschool", key); len(rows) != 0 {
		t.Errorf("got %d rows after delete, want none", len(rows))
	}
	// Deleting an already-gone row is not an error: a retry after a dropped
	// response must converge on the same state.
	if found, err := st.DeleteEventAbsence("testschool", key, "dee"); err != nil || found {
		t.Errorf("second delete = %v, %v; want false, nil", found, err)
	}
}

func TestAllEventAbsencesRespectsWindow(t *testing.T) {
	st := openTestStore(t)
	for _, d := range []string{"2026-09-30", "2026-10-01", "2026-10-02", "2026-10-03"} {
		if _, err := st.SetEventAbsence("testschool", "key-"+d, d, "dee", "T14:00", "T15:00", "tech1"); err != nil {
			t.Fatalf("seed %s: %v", d, err)
		}
	}
	if _, err := st.SetEventAbsence("otherschool", "key-x", "2026-10-01", "dee", "T14:00", "T15:00", "tech1"); err != nil {
		t.Fatalf("seed other school: %v", err)
	}
	rows, err := st.AllEventAbsences("testschool", "2026-10-01", "2026-10-02")
	if err != nil {
		t.Fatalf("AllEventAbsences: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want the two inside [10-01, 10-02]: %+v", len(rows), rows)
	}
	if rows[0].Date != "2026-10-01" || rows[1].Date != "2026-10-02" {
		t.Errorf("dates = %q, %q; want ascending and inclusive", rows[0].Date, rows[1].Date)
	}
}

func TestEventAbsenceCheckedRoundTrip(t *testing.T) {
	st := openTestStore(t)
	checked, err := st.EventAbsenceChecked("testschool", "appt-key")
	if err != nil {
		t.Fatalf("EventAbsenceChecked: %v", err)
	}
	if checked {
		t.Error("an appointment nobody submitted reports checked")
	}
	if _, err := st.SetEventAbsenceChecked("testschool", "appt-key", "tech1"); err != nil {
		t.Fatalf("SetEventAbsenceChecked: %v", err)
	}
	if checked, _ := st.EventAbsenceChecked("testschool", "appt-key"); !checked {
		t.Error("the submitted appointment was not reported checked")
	}
}

func TestEventAbsenceLogRoundTripAndWindow(t *testing.T) {
	st := openTestStore(t)
	for i, d := range []string{"2026-10-01", "2026-10-02", "2026-10-03"} {
		if err := st.LogEventAbsenceChange("testschool", "key-"+d, d, "dee", "created", "14:00–15:00", "tech1"); err != nil {
			t.Fatalf("log %d: %v", i, err)
		}
	}
	if err := st.LogEventAbsenceChange("otherschool", "key-x", "2026-10-02", "dee", "deleted", "", "tech1"); err != nil {
		t.Fatalf("log other school: %v", err)
	}
	rows, err := st.EventAbsenceLog("testschool", "2026-10-01", "2026-10-02")
	if err != nil {
		t.Fatalf("EventAbsenceLog: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d entries, want the two inside the window: %+v", len(rows), rows)
	}
	// Newest first, so the dashboard reads a fresh change at the top.
	if rows[0].Date != "2026-10-02" || rows[1].Date != "2026-10-01" {
		t.Errorf("order = %q, %q; want newest first", rows[0].Date, rows[1].Date)
	}
	if rows[0].Action != "created" || rows[0].Actor != "tech1" || rows[0].Username != eventKey("dee") {
		t.Errorf("entry = %+v, want the stored action, actor and folded student", rows[0])
	}
}
