package store

// Tests for student_events: the proxy-owned per-student events an admin authors.
// The properties that matter are isolation (never another student's), range
// correctness at the boundaries, and the revision counter an .ics client relies on.

import (
	"testing"
	"time"
)

func sampleEvent() NewStudentEvent {
	return NewStudentEvent{
		Date: "2026-10-01", StartTime: "14:00", EndTime: "15:00",
		Title: "Technik", Subject: "Mathe", Room: "R12", Teacher: "Mr Smith",
		Description: "bring the workbook",
	}
}

func TestStudentEventCreateRoundTrip(t *testing.T) {
	st := openTestStore(t)
	ev, err := st.CreateStudentEvent("testschool", "dee", sampleEvent(), "bob")
	if err != nil {
		t.Fatalf("CreateStudentEvent: %v", err)
	}
	if ev.ID <= 0 {
		t.Errorf("id = %d, want a real row id", ev.ID)
	}
	if ev.Revision != 1 {
		t.Errorf("revision = %d, want 1 for a new event", ev.Revision)
	}
	if ev.Username != "dee" || ev.CreatedBy != "bob" {
		t.Errorf("username = %q, createdBy = %q", ev.Username, ev.CreatedBy)
	}
	if ev.CreatedAt.IsZero() || ev.UpdatedAt.IsZero() {
		t.Error("timestamps were not set")
	}

	got, ok, err := st.StudentEventByID("testschool", ev.ID)
	if err != nil || !ok {
		t.Fatalf("StudentEventByID: %v (found %v)", err, ok)
	}
	if got != ev {
		t.Errorf("stored event differs from what was returned\n got: %+v\nwant: %+v", got, ev)
	}
}

func TestStudentEventOptionalFieldsDefaultToEmpty(t *testing.T) {
	st := openTestStore(t)
	ev, err := st.CreateStudentEvent("testschool", "dee", NewStudentEvent{
		Date: "2026-10-01", StartTime: "14:00", EndTime: "15:00", Title: "Technik",
	}, "")
	if err != nil {
		t.Fatalf("CreateStudentEvent: %v", err)
	}
	got, _, err := st.StudentEventByID("testschool", ev.ID)
	if err != nil {
		t.Fatalf("StudentEventByID: %v", err)
	}
	if got.Subject != "" || got.Room != "" || got.Teacher != "" || got.Description != "" || got.CreatedBy != "" {
		t.Errorf("unset fields did not read back empty: %+v", got)
	}
}

func TestStudentEventNormalizesUsername(t *testing.T) {
	st := openTestStore(t)
	// Written with padding and capitals, read back with neither.
	if _, err := st.CreateStudentEvent("testschool", "  DeE ", sampleEvent(), "bob"); err != nil {
		t.Fatalf("CreateStudentEvent: %v", err)
	}
	for _, username := range []string{"dee", "DEE", "  DeE  "} {
		evs, err := st.StudentEventsForRange("testschool", username, "2026-01-01", "2027-01-01")
		if err != nil {
			t.Fatalf("StudentEventsForRange(%q): %v", username, err)
		}
		if len(evs) != 1 {
			t.Errorf("StudentEventsForRange(%q) returned %d, want 1", username, len(evs))
		}
	}
}

func TestStudentEventIsolatedPerStudent(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.CreateStudentEvent("testschool", "dee", sampleEvent(), "bob"); err != nil {
		t.Fatalf("CreateStudentEvent: %v", err)
	}
	// A different student in the same class must see nothing.
	for _, username := range []string{"sam", "dee2", ""} {
		evs, err := st.StudentEventsForRange("testschool", username, "2026-01-01", "2027-01-01")
		if err != nil {
			t.Fatalf("StudentEventsForRange(%q): %v", username, err)
		}
		if len(evs) != 0 {
			t.Errorf("%q sees %d of dee's events: %+v", username, len(evs), evs)
		}
	}
}

func TestStudentEventIsolatedPerSchool(t *testing.T) {
	st := openTestStore(t)
	ev, err := st.CreateStudentEvent("testschool", "dee", sampleEvent(), "bob")
	if err != nil {
		t.Fatalf("CreateStudentEvent: %v", err)
	}
	// Same id, different school: not found rather than found-and-checked.
	if _, found, err := st.StudentEventByID("otherschool", ev.ID); err != nil {
		t.Fatalf("StudentEventByID: %v", err)
	} else if found {
		t.Error("an event was visible from another school")
	}
	if deleted, err := st.DeleteStudentEvent("otherschool", ev.ID); err != nil {
		t.Fatalf("DeleteStudentEvent: %v", err)
	} else if deleted {
		t.Error("an event was deleted from another school")
	}
	evs, err := st.StudentEventsForRange("otherschool", "dee", "2026-01-01", "2027-01-01")
	if err != nil {
		t.Fatalf("StudentEventsForRange: %v", err)
	}
	if len(evs) != 0 {
		t.Errorf("another school served %d events: %+v", len(evs), evs)
	}
	// And the original is untouched by the attempt.
	if _, found, err := st.StudentEventByID("testschool", ev.ID); err != nil || !found {
		t.Errorf("the original event went missing: found %v, err %v", found, err)
	}
}

func TestStudentEventsForRangeIsHalfOpen(t *testing.T) {
	st := openTestStore(t)
	for _, date := range []string{"2026-09-30", "2026-10-01", "2026-10-02", "2026-10-03"} {
		ev := sampleEvent()
		ev.Date = date
		if _, err := st.CreateStudentEvent("testschool", "dee", ev, "bob"); err != nil {
			t.Fatalf("CreateStudentEvent(%s): %v", date, err)
		}
	}
	// [2026-10-01, 2026-10-03) is the two middle days. A caller walking days must
	// not see the boundary date twice.
	evs, err := st.StudentEventsForRange("testschool", "dee", "2026-10-01", "2026-10-03")
	if err != nil {
		t.Fatalf("StudentEventsForRange: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(evs), evs)
	}
	if evs[0].Date != "2026-10-01" || evs[1].Date != "2026-10-02" {
		t.Errorf("got dates %s and %s, want 2026-10-01 and 2026-10-02", evs[0].Date, evs[1].Date)
	}
}

func TestStudentEventsForRangeIsOrderedByDateThenTime(t *testing.T) {
	st := openTestStore(t)
	for _, tc := range []struct{ date, start, title string }{
		{"2026-10-02", "09:00", "later day"},
		{"2026-10-01", "15:00", "afternoon"},
		{"2026-10-01", "08:00", "morning"},
	} {
		ev := sampleEvent()
		ev.Date, ev.StartTime, ev.Title = tc.date, tc.start, tc.title
		if _, err := st.CreateStudentEvent("testschool", "dee", ev, "bob"); err != nil {
			t.Fatalf("CreateStudentEvent: %v", err)
		}
	}
	evs, err := st.StudentEventsForRange("testschool", "dee", "2026-01-01", "2027-01-01")
	if err != nil {
		t.Fatalf("StudentEventsForRange: %v", err)
	}
	want := []string{"morning", "afternoon", "later day"}
	if len(evs) != len(want) {
		t.Fatalf("got %d events, want %d", len(evs), len(want))
	}
	for i, title := range want {
		if evs[i].Title != title {
			t.Errorf("event %d = %q, want %q (ordered by date then start time)", i, evs[i].Title, title)
		}
	}
}

func TestStudentEventPartialUpdateLeavesOtherFieldsAlone(t *testing.T) {
	st := openTestStore(t)
	ev, err := st.CreateStudentEvent("testschool", "dee", sampleEvent(), "bob")
	if err != nil {
		t.Fatalf("CreateStudentEvent: %v", err)
	}
	room := "R99"
	got, ok, err := st.UpdateStudentEvent("testschool", ev.ID, StudentEventPatch{Room: &room})
	if err != nil || !ok {
		t.Fatalf("UpdateStudentEvent: %v (found %v)", err, ok)
	}
	if got.Room != "R99" {
		t.Errorf("room = %q, want R99", got.Room)
	}
	// Everything not in the patch survives. This is the property that lets an admin
	// change one field without reading the row first.
	if got.Title != "Technik" || got.Subject != "Mathe" || got.Teacher != "Mr Smith" ||
		got.Description != "bring the workbook" || got.Date != "2026-10-01" ||
		got.StartTime != "14:00" || got.EndTime != "15:00" || got.CreatedBy != "bob" {
		t.Errorf("the patch overwrote fields it did not mention: %+v", got)
	}
	if got.CreatedAt != ev.CreatedAt {
		t.Errorf("createdAt changed on edit: %v -> %v", ev.CreatedAt, got.CreatedAt)
	}
}

func TestStudentEventUpdateBumpsRevision(t *testing.T) {
	st := openTestStore(t)
	ev, err := st.CreateStudentEvent("testschool", "dee", sampleEvent(), "bob")
	if err != nil {
		t.Fatalf("CreateStudentEvent: %v", err)
	}
	// An .ics client compares SEQUENCE to decide whether a VEVENT changed, so the
	// revision must rise on every edit even inside the same second as the last one.
	room := "R1"
	for i := 2; i <= 4; i++ {
		next := room
		got, ok, err := st.UpdateStudentEvent("testschool", ev.ID, StudentEventPatch{Room: &next})
		if err != nil || !ok {
			t.Fatalf("UpdateStudentEvent: %v (found %v)", err, ok)
		}
		if got.Revision != int64(i) {
			t.Errorf("after %d edits revision = %d, want %d", i-1, got.Revision, i)
		}
		room = next + "x"
	}
}

func TestStudentEventUpdateOfMissingRowIsNotAnError(t *testing.T) {
	st := openTestStore(t)
	title := "x"
	// Two admins editing the same event: the second finds it gone. That converges
	// on "not found" rather than failing the request.
	if _, ok, err := st.UpdateStudentEvent("testschool", 999999, StudentEventPatch{Title: &title}); err != nil {
		t.Errorf("UpdateStudentEvent on a missing row returned %v, want no error", err)
	} else if ok {
		t.Error("UpdateStudentEvent reported a missing row as found")
	}
}

func TestStudentEventUpdateIsScopedToSchool(t *testing.T) {
	st := openTestStore(t)
	ev, err := st.CreateStudentEvent("testschool", "dee", sampleEvent(), "bob")
	if err != nil {
		t.Fatalf("CreateStudentEvent: %v", err)
	}
	title := "hijacked"
	if _, ok, err := st.UpdateStudentEvent("otherschool", ev.ID, StudentEventPatch{Title: &title}); err != nil {
		t.Fatalf("UpdateStudentEvent: %v", err)
	} else if ok {
		t.Error("an event was editable from another school")
	}
	got, ok, err := st.StudentEventByID("testschool", ev.ID)
	if err != nil || !ok {
		t.Fatalf("StudentEventByID: %v (found %v)", err, ok)
	}
	if got.Title != "Technik" {
		t.Errorf("the cross-school update took effect: %q", got.Title)
	}
}

func TestStudentEventDeleteIsIdempotent(t *testing.T) {
	st := openTestStore(t)
	ev, err := st.CreateStudentEvent("testschool", "dee", sampleEvent(), "bob")
	if err != nil {
		t.Fatalf("CreateStudentEvent: %v", err)
	}
	deleted, err := st.DeleteStudentEvent("testschool", ev.ID)
	if err != nil || !deleted {
		t.Fatalf("DeleteStudentEvent: deleted %v, err %v", deleted, err)
	}
	// A client retrying after a lost response must converge, not fail.
	deleted, err = st.DeleteStudentEvent("testschool", ev.ID)
	if err != nil {
		t.Errorf("second delete returned %v, want no error", err)
	}
	if deleted {
		t.Error("second delete claimed to remove a row")
	}
	evs, err := st.StudentEventsForRange("testschool", "dee", "2026-01-01", "2027-01-01")
	if err != nil {
		t.Fatalf("StudentEventsForRange: %v", err)
	}
	if len(evs) != 0 {
		t.Errorf("the event survived the delete: %+v", evs)
	}
}

func TestStudentEventDeleteLeavesOtherEventsAlone(t *testing.T) {
	st := openTestStore(t)
	first, err := st.CreateStudentEvent("testschool", "dee", sampleEvent(), "bob")
	if err != nil {
		t.Fatalf("CreateStudentEvent: %v", err)
	}
	second, err := st.CreateStudentEvent("testschool", "dee", sampleEvent(), "bob")
	if err != nil {
		t.Fatalf("CreateStudentEvent: %v", err)
	}
	third, err := st.CreateStudentEvent("testschool", "sam", sampleEvent(), "bob")
	if err != nil {
		t.Fatalf("CreateStudentEvent: %v", err)
	}
	if _, err := st.DeleteStudentEvent("testschool", first.ID); err != nil {
		t.Fatalf("DeleteStudentEvent: %v", err)
	}
	if _, ok, err := st.StudentEventByID("testschool", second.ID); err != nil || !ok {
		t.Errorf("the second event went missing: found %v, err %v", ok, err)
	}
	if _, ok, err := st.StudentEventByID("testschool", third.ID); err != nil || !ok {
		t.Errorf("another student's event was deleted: found %v, err %v", ok, err)
	}
}

func TestStudentEventsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/test.db"
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	created, err := st.CreateStudentEvent("testschool", "dee", sampleEvent(), "bob")
	if err != nil {
		t.Fatalf("CreateStudentEvent: %v", err)
	}
	room := "R7"
	if _, _, err := st.UpdateStudentEvent("testschool", created.ID, StudentEventPatch{Room: &room}); err != nil {
		t.Fatalf("UpdateStudentEvent: %v", err)
	}
	st.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	got, ok, err := reopened.StudentEventByID("testschool", created.ID)
	if err != nil || !ok {
		t.Fatalf("after reopen: found %v, err %v", ok, err)
	}
	if got.Room != "R7" || got.Revision != 2 || got.Title != "Technik" || got.CreatedBy != "bob" {
		t.Errorf("event did not survive the restart intact: %+v", got)
	}
	if !got.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("createdAt = %v, want %v", got.CreatedAt, created.CreatedAt)
	}
	if got.UpdatedAt.Before(created.CreatedAt) {
		t.Errorf("updatedAt %v predates createdAt %v", got.UpdatedAt, created.CreatedAt)
	}
}

func TestStudentEventTimestampIsWholeSeconds(t *testing.T) {
	st := openTestStore(t)
	ev, err := st.CreateStudentEvent("testschool", "dee", sampleEvent(), "bob")
	if err != nil {
		t.Fatalf("CreateStudentEvent: %v", err)
	}
	// The stamp is stored as unix seconds, so a nanosecond component would not
	// survive the round trip and the value returned by the write would disagree
	// with the value read back.
	if ev.CreatedAt.Nanosecond() != 0 {
		t.Errorf("createdAt carries sub-second precision: %v", ev.CreatedAt)
	}
	got, _, err := st.StudentEventByID("testschool", ev.ID)
	if err != nil {
		t.Fatalf("StudentEventByID: %v", err)
	}
	if !got.CreatedAt.Equal(ev.CreatedAt) {
		t.Errorf("createdAt read back as %v, want the returned %v", got.CreatedAt, ev.CreatedAt)
	}
}

func TestStudentEventsOnEmptyStore(t *testing.T) {
	st := openTestStore(t)
	evs, err := st.StudentEventsForRange("testschool", "nobody", "2026-01-01", "2027-01-01")
	if err != nil {
		t.Fatalf("StudentEventsForRange: %v", err)
	}
	if len(evs) != 0 {
		t.Errorf("got %d events, want none", len(evs))
	}
	all, err := st.StudentEvents("testschool", "nobody")
	if err != nil {
		t.Fatalf("StudentEvents: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("StudentEvents got %d, want none", len(all))
	}
}

func TestListStudentEventsSpansSchools(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.CreateStudentEvent("schoolA", "dee", sampleEvent(), "bob"); err != nil {
		t.Fatalf("CreateStudentEvent: %v", err)
	}
	other := sampleEvent()
	other.Title = "at B"
	if _, err := st.CreateStudentEvent("schoolB", "dee", other, "bob"); err != nil {
		t.Fatalf("CreateStudentEvent: %v", err)
	}
	// The admin search needs to find a student's events without knowing the
	// school, so this read is intentionally the only one not school-scoped.
	evs, err := st.ListStudentEvents("dee")
	if err != nil {
		t.Fatalf("ListStudentEvents: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("got %d events across schools, want 2: %+v", len(evs), evs)
	}
	if evs[0].School == evs[1].School {
		t.Errorf("both events report school %q", evs[0].School)
	}
	if sam, err := st.ListStudentEvents("sam"); err != nil {
		t.Fatalf("ListStudentEvents: %v", err)
	} else if len(sam) != 0 {
		t.Errorf("another student sees %d of dee's events: %+v", len(sam), sam)
	}
}

func TestStudentEventCountsAreScopedBySchoolAndStudent(t *testing.T) {
	st := openTestStore(t)
	for _, tc := range []struct {
		school, user string
		n            int
	}{
		{"schoolA", "dee", 2},
		{"schoolA", "sam", 1},
		{"schoolB", "dee", 1},
	} {
		for i := 0; i < tc.n; i++ {
			if _, err := st.CreateStudentEvent(tc.school, tc.user, sampleEvent(), "bob"); err != nil {
				t.Fatalf("CreateStudentEvent: %v", err)
			}
		}
	}
	counts, err := st.StudentEventCounts()
	if err != nil {
		t.Fatalf("StudentEventCounts: %v", err)
	}
	for key, want := range map[string]int{"schoolA|dee": 2, "schoolA|sam": 1, "schoolB|dee": 1} {
		if counts[key] != want {
			t.Errorf("counts[%q] = %d, want %d (all: %v)", key, counts[key], want, counts)
		}
	}
	if len(counts) != 3 {
		t.Errorf("got %d entries, want 3: %v", len(counts), counts)
	}
	// Deleting is reflected, so the dashboard does not drift from reality.
	evs, err := st.StudentEvents("schoolA", "dee")
	if err != nil || len(evs) == 0 {
		t.Fatalf("StudentEvents: %v", err)
	}
	if _, err := st.DeleteStudentEvent("schoolA", evs[0].ID); err != nil {
		t.Fatalf("DeleteStudentEvent: %v", err)
	}
	counts, err = st.StudentEventCounts()
	if err != nil {
		t.Fatalf("StudentEventCounts: %v", err)
	}
	if counts["schoolA|dee"] != 1 {
		t.Errorf("after delete counts = %d, want 1", counts["schoolA|dee"])
	}
}

func TestStudentEventTextIsPreserved(t *testing.T) {
	st := openTestStore(t)
	// Free text an admin types, including quotes and newlines that would break a
	// naive concatenation into the ICS DESCRIPTION.
	ev := sampleEvent()
	ev.Title = `Technik "Mathe" / Tutor`
	ev.Description = "line one\nline two\ttabbed \\ backslash"
	created, err := st.CreateStudentEvent("testschool", "dee", ev, "bob")
	if err != nil {
		t.Fatalf("CreateStudentEvent: %v", err)
	}
	got, _, err := st.StudentEventByID("testschool", created.ID)
	if err != nil {
		t.Fatalf("StudentEventByID: %v", err)
	}
	if got.Title != ev.Title || got.Description != ev.Description {
		t.Errorf("text was altered\n title: %q\n desc:   %q", got.Title, got.Description)
	}
}

func TestStudentEventTimeIsStoredVerbatim(t *testing.T) {
	st := openTestStore(t)
	// The store does not parse or reformat times: it holds what the admin chose,
	// so a time it does not recognise cannot silently become a different one.
	for _, tc := range []struct{ start, end string }{
		{"00:00", "23:59"},
		{"8:30", "9:15"},
		{"14:00", "14:00"},
		{"", ""},
	} {
		ev := sampleEvent()
		ev.StartTime, ev.EndTime = tc.start, tc.end
		created, err := st.CreateStudentEvent("testschool", "dee", ev, "bob")
		if err != nil {
			t.Fatalf("CreateStudentEvent: %v", err)
		}
		got, _, err := st.StudentEventByID("testschool", created.ID)
		if err != nil {
			t.Fatalf("StudentEventByID: %v", err)
		}
		if got.StartTime != tc.start || got.EndTime != tc.end {
			t.Errorf("stored %q-%q, want %q-%q", got.StartTime, got.EndTime, tc.start, tc.end)
		}
	}
}

func TestStudentEventCreatedAtIsRecent(t *testing.T) {
	st := openTestStore(t)
	before := time.Now().Truncate(time.Second)
	ev, err := st.CreateStudentEvent("testschool", "dee", sampleEvent(), "bob")
	if err != nil {
		t.Fatalf("CreateStudentEvent: %v", err)
	}
	if ev.CreatedAt.Before(before) || ev.CreatedAt.After(time.Now()) {
		t.Errorf("createdAt = %v, want between %v and now", ev.CreatedAt, before)
	}
}
