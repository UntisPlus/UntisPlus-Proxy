package store

// Tests for the private absence notes: the keys, the isolation between students
// and schools, and the note/clear round trip.

import "testing"

func TestAbsenceNoteRoundTrip(t *testing.T) {
	st := openTestStore(t)
	at, err := st.SetAbsenceNote("testschool", "dee", 300001, "bring workbook")
	if err != nil {
		t.Fatalf("SetAbsenceNote: %v", err)
	}
	if at.IsZero() {
		t.Error("SetAbsenceNote returned a zero timestamp")
	}
	notes, err := st.AbsenceNotes("testschool", "dee")
	if err != nil {
		t.Fatalf("AbsenceNotes: %v", err)
	}
	got, ok := notes[300001]
	if !ok {
		t.Fatalf("note 300001 is missing from %v", notes)
	}
	if got.Note != "bring workbook" {
		t.Errorf("note = %q, want %q", got.Note, "bring workbook")
	}
	// The returned stamp is the stored one, so a client sees the same updatedAt
	// immediately after writing as it does on a later read.
	if !got.UpdatedAt.Equal(at) {
		t.Errorf("stored %v != returned %v", got.UpdatedAt, at)
	}
	if got.Key != 300001 {
		t.Errorf("Key = %d, want 300001", got.Key)
	}
}

func TestAbsenceNoteIsolatedPerUser(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.SetAbsenceNote("testschool", "dee", 300001, "private"); err != nil {
		t.Fatal(err)
	}
	sam, err := st.AbsenceNotes("testschool", "sam")
	if err != nil {
		t.Fatalf("AbsenceNotes: %v", err)
	}
	if len(sam) != 0 {
		t.Errorf("dee's note leaked to sam: %v", sam)
	}
}

func TestAbsenceNoteIsolatedPerSchool(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.SetAbsenceNote("testschool", "dee", 300001, "here"); err != nil {
		t.Fatal(err)
	}
	other, err := st.AbsenceNotes("otherschool", "dee")
	if err != nil {
		t.Fatalf("AbsenceNotes: %v", err)
	}
	if len(other) != 0 {
		t.Errorf("a note leaked into another school: %v", other)
	}
}

// TestAbsenceNoteNormalizesUsername: two spellings of one student must be one
// note, or the same person ends up with a duplicate they can never see.
func TestAbsenceNoteNormalizesUsername(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.SetAbsenceNote("testschool", "Dee", 300001, "n"); err != nil {
		t.Fatal(err)
	}
	lower, err := st.AbsenceNotes("testschool", "dee")
	if err != nil {
		t.Fatalf("AbsenceNotes: %v", err)
	}
	if len(lower) != 1 {
		t.Errorf("case-insensitive lookup returned %d notes, want 1", len(lower))
	}
	// Overwriting via a different spelling updates the same row.
	if _, err := st.SetAbsenceNote("testschool", "DEE", 300001, "edited"); err != nil {
		t.Fatal(err)
	}
	again, err := st.AbsenceNotes("testschool", "dee")
	if err != nil {
		t.Fatalf("AbsenceNotes: %v", err)
	}
	if len(again) != 1 || again[300001].Note != "edited" {
		t.Errorf("a differently-cased write did not update the same row: %v", again)
	}
}

func TestAbsenceNoteSetIsIdempotent(t *testing.T) {
	st := openTestStore(t)
	for i := 0; i < 4; i++ {
		if _, err := st.SetAbsenceNote("testschool", "dee", 300001, "n"); err != nil {
			t.Fatalf("set %d: %v", i, err)
		}
	}
	notes, err := st.AbsenceNotes("testschool", "dee")
	if err != nil {
		t.Fatalf("AbsenceNotes: %v", err)
	}
	if len(notes) != 1 {
		t.Errorf("four writes produced %d notes, want 1", len(notes))
	}
}

func TestAbsenceNoteClear(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.SetAbsenceNote("testschool", "dee", 300001, "n"); err != nil {
		t.Fatal(err)
	}
	// Clearing twice must not error: the endpoint has to converge on retry.
	for i := 0; i < 3; i++ {
		if err := st.ClearAbsenceNote("testschool", "dee", 300001); err != nil {
			t.Fatalf("clear %d: %v", i, err)
		}
	}
	notes, err := st.AbsenceNotes("testschool", "dee")
	if err != nil {
		t.Fatalf("AbsenceNotes: %v", err)
	}
	if len(notes) != 0 {
		t.Errorf("notes survived clearing: %v", notes)
	}
}

// TestAbsenceNoteClearKeepsOtherNotes: a targeted clear must not wipe the
// student's whole set.
func TestAbsenceNoteClearKeepsOtherNotes(t *testing.T) {
	st := openTestStore(t)
	for _, k := range []int64{300001, 300002, 300003} {
		if _, err := st.SetAbsenceNote("testschool", "dee", k, "n"); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.ClearAbsenceNote("testschool", "dee", 300002); err != nil {
		t.Fatalf("clear: %v", err)
	}
	notes, err := st.AbsenceNotes("testschool", "dee")
	if err != nil {
		t.Fatalf("AbsenceNotes: %v", err)
	}
	if len(notes) != 2 {
		t.Fatalf("got %d notes after a targeted clear, want 2: %v", len(notes), notes)
	}
	if _, ok := notes[300002]; ok {
		t.Error("the cleared note is still present")
	}
}

// TestAbsenceNoteLargeKey: absence ids are the storage key and travel as int64.
func TestAbsenceNoteLargeKey(t *testing.T) {
	st := openTestStore(t)
	const bigKey = int64(9007199254740993)
	if _, err := st.SetAbsenceNote("testschool", "dee", bigKey, "n"); err != nil {
		t.Fatalf("SetAbsenceNote: %v", err)
	}
	notes, err := st.AbsenceNotes("testschool", "dee")
	if err != nil {
		t.Fatalf("AbsenceNotes: %v", err)
	}
	if _, ok := notes[bigKey]; !ok {
		t.Errorf("large key did not round-trip: %v", notes)
	}
}

func TestAbsenceNotesOnEmptyStore(t *testing.T) {
	st := openTestStore(t)
	notes, err := st.AbsenceNotes("testschool", "dee")
	if err != nil {
		t.Fatalf("AbsenceNotes on an empty store: %v", err)
	}
	if len(notes) != 0 {
		t.Errorf("notes = %v, want none", notes)
	}
	if err := st.ClearAbsenceNote("testschool", "dee", 300001); err != nil {
		t.Errorf("clearing an absent note on an empty store: %v", err)
	}
}

// TestAbsenceNoteTextIsPreserved: a note may contain anything the student types,
// including quotes and newlines, and must come back byte for byte.
func TestAbsenceNoteTextIsPreserved(t *testing.T) {
	st := openTestStore(t)
	for _, text := range []string{
		`quotes " and ' and \ backslash`,
		"line one\nline two\ttabbed",
		"ä ö ü ß — emoji 🎒",
		"   leading and trailing   ",
	} {
		if _, err := st.SetAbsenceNote("testschool", "dee", 300001, text); err != nil {
			t.Fatalf("set %q: %v", text, err)
		}
		notes, err := st.AbsenceNotes("testschool", "dee")
		if err != nil {
			t.Fatalf("AbsenceNotes: %v", err)
		}
		if got := notes[300001].Note; got != text {
			t.Errorf("note round-trip changed the text:\n got: %q\nwant: %q", got, text)
		}
	}
}

// --- ClassPeriodsOnDate ---

func TestClassPeriodsOnDate(t *testing.T) {
	st := openTestStore(t)
	rows := []PeriodRow{
		{PeriodID: 1, Start: "2026-10-01 08:00", End: "2026-10-01 08:45", Subject: "Deutsch"},
		{PeriodID: 2, Start: "2026-10-01 10:00", End: "2026-10-01 10:45", Subject: "Mathematik 3"},
		{PeriodID: 3, Start: "2026-10-02 08:00", End: "2026-10-02 08:45", Subject: "Sport"},
	}
	if _, err := st.ReplaceClassSnapshot("testschool", 5000, rows, 1, "2000-01-01"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := st.ClassPeriodsOnDate("testschool", 5000, "2026-10-01")
	if err != nil {
		t.Fatalf("ClassPeriodsOnDate: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d periods, want 2", len(got))
	}
	// Ordered by start time, so the caller can take the first of a day.
	if got[0].Subject != "Deutsch" || got[1].Subject != "Mathematik 3" {
		t.Errorf("periods are not in start order: %+v", got)
	}

	none, err := st.ClassPeriodsOnDate("testschool", 5000, "2019-03-04")
	if err != nil {
		t.Fatalf("ClassPeriodsOnDate for an unknown date: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("a date outside the snapshot returned %d periods", len(none))
	}
}

// TestClassPeriodsOnDateIsolated: another class's or school's periods must not
// be attributed to this one, or a wrong subject appears on a student's record.
func TestClassPeriodsOnDateIsolated(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.ReplaceClassSnapshot("testschool", 5000, []PeriodRow{
		{PeriodID: 1, Start: "2026-10-01 08:00", End: "2026-10-01 08:45", Subject: "Deutsch"},
	}, 1, "2000-01-01"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReplaceClassSnapshot("testschool", 6000, []PeriodRow{
		{PeriodID: 1, Start: "2026-10-01 08:00", End: "2026-10-01 08:45", Subject: "Ethik"},
	}, 1, "2000-01-01"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReplaceClassSnapshot("otherschool", 5000, []PeriodRow{
		{PeriodID: 1, Start: "2026-10-01 08:00", End: "2026-10-01 08:45", Subject: "Fremdsprache"},
	}, 1, "2000-01-01"); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		school  string
		classID int64
		want    string
	}{
		{"testschool", 5000, "Deutsch"},
		{"testschool", 6000, "Ethik"},
		{"otherschool", 5000, "Fremdsprache"},
	}
	for _, c := range cases {
		got, err := st.ClassPeriodsOnDate(c.school, c.classID, "2026-10-01")
		if err != nil {
			t.Fatalf("%s/%d: %v", c.school, c.classID, err)
		}
		if len(got) != 1 || got[0].Subject != c.want {
			t.Errorf("%s/%d returned %+v, want subject %s", c.school, c.classID, got, c.want)
		}
	}
}

func TestClassPeriodsOnDateEmpty(t *testing.T) {
	st := openTestStore(t)
	got, err := st.ClassPeriodsOnDate("testschool", 9999, "2026-10-01")
	if err != nil {
		t.Fatalf("ClassPeriodsOnDate on an empty store: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d periods, want none", len(got))
	}
}

// TestClassPeriodsOnDateOnlyMatchesThatDate: the lookup is by date prefix, so a
// period on another day must not be returned. An absence on the wrong day then
// finds no lesson, which is the safe direction — no subject rather than the wrong
// one.
func TestClassPeriodsOnDateOnlyMatchesThatDate(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.ReplaceClassSnapshot("testschool", 5000, []PeriodRow{
		{PeriodID: 1, Start: "2026-10-01 10:00", End: "2026-10-01 10:45", Subject: "Mathe"},
		{PeriodID: 2, Start: "2026-10-02 10:00", End: "2026-10-02 10:45", Subject: "Deutsch"},
	}, 1, "2000-01-01"); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	first, err := st.ClassPeriodsOnDate("testschool", 5000, "2026-10-01")
	if err != nil {
		t.Fatalf("ClassPeriodsOnDate: %v", err)
	}
	if len(first) != 1 || first[0].Subject != "Mathe" {
		t.Errorf("2026-10-01 returned %v, want just Mathe", first)
	}
	second, err := st.ClassPeriodsOnDate("testschool", 5000, "2026-10-02")
	if err != nil {
		t.Fatalf("ClassPeriodsOnDate: %v", err)
	}
	if len(second) != 1 || second[0].Subject != "Deutsch" {
		t.Errorf("2026-10-02 returned %v, want just Deutsch", second)
	}
	none, err := st.ClassPeriodsOnDate("testschool", 5000, "2026-10-03")
	if err != nil {
		t.Fatalf("ClassPeriodsOnDate: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("a day with no lessons returned %v, want none", none)
	}
}

// TestClassPeriodsOnDateReturnsPeriodsInOrder: an absence spanning several lessons
// is ambiguous regardless of order, but a stable order keeps the overlap scan
// deterministic and its outcome reproducible.
func TestClassPeriodsOnDateReturnsPeriodsInOrder(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.ReplaceClassSnapshot("testschool", 5000, []PeriodRow{
		{PeriodID: 1, Start: "2026-10-01 11:00", End: "2026-10-01 11:45", Subject: "Sport"},
		{PeriodID: 2, Start: "2026-10-01 08:00", End: "2026-10-01 08:45", Subject: "Deutsch"},
		{PeriodID: 3, Start: "2026-10-01 10:00", End: "2026-10-01 10:45", Subject: "Mathe"},
	}, 1, "2000-01-01"); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	rows, err := st.ClassPeriodsOnDate("testschool", 5000, "2026-10-01")
	if err != nil {
		t.Fatalf("ClassPeriodsOnDate: %v", err)
	}
	want := []string{"Deutsch", "Mathe", "Sport"}
	if len(rows) != len(want) {
		t.Fatalf("got %d periods, want %d", len(rows), len(want))
	}
	for i, subject := range want {
		if rows[i].Subject != subject {
			t.Errorf("period %d = %q, want %q (ordered by start)", i, rows[i].Subject, subject)
		}
	}
}

// TestAbsenceNotesIgnoresAnUnknownSchoolAndUser: a read that matches nothing must
// report an empty set, not an error, so a student with no notes sees a normal
// empty response rather than a failure.
func TestAbsenceNotesIgnoresAnUnknownSchoolAndUser(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.SetAbsenceNote("testschool", "dee", 300001, "mine"); err != nil {
		t.Fatalf("SetAbsenceNote: %v", err)
	}
	for _, tc := range []struct{ school, user string }{
		{"testschool", "sam"},
		{"otherschool", "dee"},
		{"", "dee"},
		{"testschool", ""},
	} {
		notes, err := st.AbsenceNotes(tc.school, tc.user)
		if err != nil {
			t.Errorf("AbsenceNotes(%q, %q): %v", tc.school, tc.user, err)
			continue
		}
		if len(notes) != 0 {
			t.Errorf("AbsenceNotes(%q, %q) returned %v, want none", tc.school, tc.user, notes)
		}
	}
}
