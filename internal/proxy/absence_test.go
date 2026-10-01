package proxy

// Absence enrichment: derived metadata, the student's private note, and the
// boundaries around both.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"untis-proxy/internal/store"
)

// absenceResponse is the shape recorded in the Phase 0 probe: one excused absence
// for student 7 in class 5000, 10:00-10:45, reason id 3.
const absenceResponse = `{"jsonrpc":"2.0","id":"upstream","result":{"absences":[
	{"id":300001,"startDateTime":"2026-10-01T10:00","endDateTime":"2026-10-01T10:45",
	 "klasseId":5000,"absenceReasonId":3,"absenceReason":"3","excused":true,
	 "text":"Doctor","excuse":{"date":"2026-09-30","excuseStatusId":1,"id":9,"number":"AB-1","text":"see doctor"},
	 "studentId":7,"owner":false,"studentOfAge":true}],
	"version":2}}`

func absenceProxy(t *testing.T) (*Proxy, *store.Store) {
	t.Helper()
	p, st := newTestProxy(t)
	for _, u := range []struct {
		name    string
		id      int64
		classID int64
	}{{"dee", 7, 5000}, {"sam", 8, 5000}} {
		if err := st.UpsertUser(&store.User{Username: u.name, School: "testschool",
			Method: "key", Password: "x", PersonType: 5, PersonID: u.id, ClassID: u.classID}); err != nil {
			t.Fatalf("seed %s: %v", u.name, err)
		}
	}
	return p, st
}

// firstAbsenceAt decodes the first entry of the absence list held under key.
func firstAbsenceAt(t *testing.T, recBody, key string) jsonObject {
	t.Helper()
	result := decodeResult(t, []byte(recBody))
	list := jsonArray(result[key])
	if len(list) == 0 {
		t.Fatalf("no absences under %q in response: %s", key, recBody)
	}
	return list[0]
}

// firstAbsence decodes the first entry of the standard `absences` list.
func firstAbsence(t *testing.T, recBody string) jsonObject {
	t.Helper()
	return firstAbsenceAt(t, recBody, "absences")
}

// TestAbsenceDerivedMetadata: the readable fields arrive without losing the
// upstream ones. `text` is the teacher's comment and `excuse.text` is upstream's
// excuse — neither may be touched or reused for the student's own note.
func TestAbsenceDerivedMetadata(t *testing.T) {
	p, _ := absenceProxy(t)
	out := p.enrichAbsenceResponse([]byte(absenceResponse), "testschool", "dee")
	abs := firstAbsence(t, string(out))

	if abs["text"] != "Doctor" {
		t.Errorf("the teacher's comment was altered: %v", abs["text"])
	}
	excuse, ok := abs["excuse"].(jsonObject)
	if !ok || excuse["text"] != "see doctor" {
		t.Errorf("upstream excuse was altered: %v", abs["excuse"])
	}
	if abs["id"] == nil || abs["studentId"] == nil || abs["klasseId"] == nil {
		t.Errorf("upstream identity fields went missing: %v", abs)
	}
	derived, ok := abs["derived"].(jsonObject)
	if !ok {
		t.Fatalf("no derived block: %v", abs)
	}
	if derived["date"] != "2026-10-01" {
		t.Errorf("derived date = %v, want 2026-10-01", derived["date"])
	}
	if derived["weekday"] != "Thursday" {
		t.Errorf("derived weekday = %v, want Thursday", derived["weekday"])
	}
	if derived["classId"] == nil {
		t.Errorf("derived classId is missing: %v", derived)
	}
	// No masterData cached in this test, so the name and reason cannot resolve.
	// They must be absent rather than empty strings.
	if _, present := derived["className"]; present {
		t.Errorf("a class name was invented with no catalogue: %v", derived)
	}
	if _, present := derived["reason"]; present {
		t.Errorf("a reason was invented with no catalogue: %v", derived)
	}
}

// TestAbsenceNoteIsAttachedToItsOwner is the core of the feature: the note the
// student wrote comes back on their own absence.
func TestAbsenceNoteIsAttachedToItsOwner(t *testing.T) {
	p, st := absenceProxy(t)
	if _, err := st.SetAbsenceNote("testschool", "dee", 300001, "bring workbook"); err != nil {
		t.Fatalf("SetAbsenceNote: %v", err)
	}
	abs := firstAbsence(t, string(p.enrichAbsenceResponse([]byte(absenceResponse), "testschool", "dee")))
	if abs["note"] != "bring workbook" {
		t.Errorf("note = %v, want the student's own note", abs["note"])
	}
	if abs["noteUpdatedAt"] == nil {
		t.Error("noteUpdatedAt is missing")
	}
}

// TestAbsenceNoteNeverCrossesStudents: the note is keyed by viewer, so another
// student's identical absence carries nothing.
func TestAbsenceNoteNeverCrossesStudents(t *testing.T) {
	p, st := absenceProxy(t)
	if _, err := st.SetAbsenceNote("testschool", "dee", 300001, "private"); err != nil {
		t.Fatalf("SetAbsenceNote: %v", err)
	}
	abs := firstAbsence(t, string(p.enrichAbsenceResponse([]byte(absenceResponse), "testschool", "sam")))
	if _, present := abs["note"]; present {
		t.Errorf("dee's note leaked to another student: %v", abs["note"])
	}
	if _, present := abs["noteUpdatedAt"]; present {
		t.Errorf("noteUpdatedAt leaked to another student: %v", abs["noteUpdatedAt"])
	}
}

// TestAbsenceNoteSkippedForAnotherStudentID is the guard that makes the privacy
// property structural. The viewer's note exists, but the entry is about a
// different student, so nothing at all is attached — not even derived metadata
// that was resolved for the wrong person.
func TestAbsenceNoteSkippedForAnotherStudentID(t *testing.T) {
	p, st := absenceProxy(t)
	if _, err := st.SetAbsenceNote("testschool", "dee", 300001, "private"); err != nil {
		t.Fatalf("SetAbsenceNote: %v", err)
	}
	// Same absence id, but studentId 8 while the viewer is 7.
	other := strings.Replace(absenceResponse, `"studentId":7`, `"studentId":8`, 1)
	abs := firstAbsence(t, string(p.enrichAbsenceResponse([]byte(other), "testschool", "dee")))
	for _, field := range []string{"note", "noteUpdatedAt", "derived"} {
		if _, present := abs[field]; present {
			t.Errorf("%s was attached to another student's absence: %v", field, abs[field])
		}
	}
}

// TestAbsenceNoteSkippedWithoutStudentID: no studentId means the entry cannot be
// attributed to the viewer, and an unattributable record gets no private text.
func TestAbsenceNoteSkippedWithoutStudentID(t *testing.T) {
	p, st := absenceProxy(t)
	if _, err := st.SetAbsenceNote("testschool", "dee", 300001, "private"); err != nil {
		t.Fatalf("SetAbsenceNote: %v", err)
	}
	anonymous := strings.Replace(absenceResponse, `"studentId":7,`, ``, 1)
	abs := firstAbsence(t, string(p.enrichAbsenceResponse([]byte(anonymous), "testschool", "dee")))
	if _, present := abs["note"]; present {
		t.Errorf("a note was attached to an absence with no studentId: %v", abs["note"])
	}
}

// TestAbsenceEnrichmentPassesThroughOnBadInput: an unreadable or unexpected
// response must reach the app exactly as upstream sent it.
func TestAbsenceEnrichmentPassesThroughOnBadInput(t *testing.T) {
	p, _ := absenceProxy(t)
	for _, raw := range []string{
		`not json`,
		``,
		`{"jsonrpc":"2.0","id":"x"}`,
		`{"jsonrpc":"2.0","id":"x","result":null}`,
		`{"jsonrpc":"2.0","id":"x","error":{"code":-1,"message":"no"},"result":{"absences":[{"id":1,"startDateTime":"2026-10-01T10:00"}]}}`,
		`{"jsonrpc":"2.0","id":"x","result":{"absences":"nope"}}`,
		`{"jsonrpc":"2.0","id":"x","result":{"absences":[{"text":"no id or start"}]}}`,
		`{"jsonrpc":"2.0","id":"x","result":{"totally":"different"}}`,
	} {
		if got := string(p.enrichAbsenceResponse([]byte(raw), "testschool", "dee")); got != raw {
			t.Errorf("response altered for %s\n got: %s\nwant: %s", raw, got, raw)
		}
	}
}

// TestAbsenceListFoundUnderAnotherKey: the key name is not something the proxy
// can rely on, so a differently named array is still recognised by its contents.
func TestAbsenceListFoundUnderAnotherKey(t *testing.T) {
	p, _ := absenceProxy(t)
	raw := `{"jsonrpc":"2.0","id":"x","result":{"someNewName":[
		{"id":300001,"startDateTime":"2026-10-01T10:00","studentId":7,"klasseId":5000}]}}`
	abs := firstAbsenceAt(t, string(p.enrichAbsenceResponse([]byte(raw), "testschool", "dee")), "someNewName")
	derived, ok := abs["derived"].(jsonObject)
	if !ok || derived["date"] != "2026-10-01" {
		t.Errorf("a recognisable absence list under an unknown key was not enriched: %v", abs)
	}
}

// TestAbsenceListFoundWhenNested: a per-day wrapper is walked one level deep.
func TestAbsenceListFoundWhenNested(t *testing.T) {
	p, _ := absenceProxy(t)
	raw := `{"jsonrpc":"2.0","id":"x","result":{"days":[
		{"date":"2026-10-01","absences":[
			{"id":300001,"startDateTime":"2026-10-01T10:00","studentId":7,"klasseId":5000}]}]}}`
	result := decodeResult(t, p.enrichAbsenceResponse([]byte(raw), "testschool", "dee"))
	days := jsonArray(result["days"])
	if len(days) != 1 {
		t.Fatalf("days = %v", result["days"])
	}
	abs := jsonArray(days[0]["absences"])[0]
	derived, ok := abs["derived"].(jsonObject)
	if !ok || derived["date"] != "2026-10-01" {
		t.Errorf("a nested absence list was not enriched: %v", abs)
	}
}

// TestAbsenceEmptyListUntouched: an absence-free range is the common case and
// must not be reshaped into something the app has to special-case.
func TestAbsenceEmptyListUntouched(t *testing.T) {
	p, _ := absenceProxy(t)
	raw := `{"jsonrpc":"2.0","id":"x","result":{"absences":[]}}`
	if got := string(p.enrichAbsenceResponse([]byte(raw), "testschool", "dee")); got != raw {
		t.Errorf("an empty absence list was rewritten:\n got: %s\nwant: %s", got, raw)
	}
}

// TestAbsenceNonObjectEntriesPreserved: a stray entry must survive enrichment
// rather than be filtered out of the array.
func TestAbsenceNonObjectEntriesPreserved(t *testing.T) {
	p, _ := absenceProxy(t)
	raw := `{"jsonrpc":"2.0","id":"x","result":{"absences":[
		{"id":300001,"startDateTime":"2026-10-01T10:00","studentId":7},
		"stray", 42, null]}}`
	result := decodeResult(t, p.enrichAbsenceResponse([]byte(raw), "testschool", "dee"))
	if got := len(result["absences"].([]any)); got != 4 {
		t.Errorf("array has %d entries after enrichment, want 4 (nothing may be dropped)", got)
	}
}

// TestAbsenceSubjectOnlyWhenUnambiguous is the honesty rule: a whole-day absence
// covers several lessons and has no single subject, so none is reported. Only a
// lesson-scoped absence names one.
func TestAbsenceSubjectOnlyWhenUnambiguous(t *testing.T) {
	p, st := absenceProxy(t)
	seedDay(t, st, "2026-10-01", []store.PeriodRow{
		{Start: "2026-10-01 08:00", End: "2026-10-01 08:45", Subject: "Deutsch"},
		{Start: "2026-10-01 10:00", End: "2026-10-01 10:45", Subject: "Mathematik 3"},
		{Start: "2026-10-01 14:00", End: "2026-10-01 14:45", Subject: "Sport"},
	})

	// Exactly one lesson overlapped -> reported.
	one := strings.Replace(absenceResponse, `"2026-10-01T10:00","endDateTime":"2026-10-01T10:45"`,
		`"2026-10-01T10:00","endDateTime":"2026-10-01T10:45"`, 1)
	derived := derivedOf(t, p.enrichAbsenceResponse([]byte(one), "testschool", "dee"))
	if derived["subject"] != "Mathematik 3" {
		t.Errorf("a lesson-scoped absence lost its subject: %v", derived)
	}

	// A whole-day absence covers three lessons -> no subject, never the first one.
	allday := strings.Replace(absenceResponse,
		`"startDateTime":"2026-10-01T10:00","endDateTime":"2026-10-01T10:45"`,
		`"startDateTime":"2026-10-01T00:00","endDateTime":"2026-10-01T23:59"`, 1)
	derived = derivedOf(t, p.enrichAbsenceResponse([]byte(allday), "testschool", "dee"))
	if _, present := derived["subject"]; present {
		t.Errorf("a whole-day absence was given a single subject: %v", derived["subject"])
	}
	if derived["date"] != "2026-10-01" {
		t.Errorf("the rest of the derived block was lost: %v", derived)
	}
}

// TestAbsenceSubjectUnknownForOldDates: the snapshot only keeps the polling
// window, so an absence older than it has no subject rather than a wrong one.
func TestAbsenceSubjectUnknownForOldDates(t *testing.T) {
	p, st := absenceProxy(t)
	seedDay(t, st, "2026-10-01", []store.PeriodRow{
		{Start: "2026-10-01 10:00", End: "2026-10-01 10:45", Subject: "Mathematik 3"},
	})
	old := strings.Replace(absenceResponse,
		`"startDateTime":"2026-10-01T10:00","endDateTime":"2026-10-01T10:45"`,
		`"startDateTime":"2019-03-04T10:00","endDateTime":"2019-03-04T10:45"`, 1)
	derived := derivedOf(t, p.enrichAbsenceResponse([]byte(old), "testschool", "dee"))
	if _, present := derived["subject"]; present {
		t.Errorf("an absence outside the snapshot window was given a subject: %v", derived["subject"])
	}
	if derived["date"] != "2019-03-04" {
		t.Errorf("date was not derived for an old absence: %v", derived)
	}
}

// TestAbsenceSubjectUsesAbsenceClassNotViewerClass: an absence can name a class
// other than the viewer's own, and the lesson is looked up in that class.
func TestAbsenceSubjectUsesAbsenceClassNotViewerClass(t *testing.T) {
	p, st := absenceProxy(t)
	// dee is in 5000; the absence names 6000, which is seeded instead.
	seedDayClass(t, st, 6000, "2026-10-01", []store.PeriodRow{
		{Start: "2026-10-01 10:00", End: "2026-10-01 10:45", Subject: "Ethik"},
	})
	raw := strings.Replace(absenceResponse, `"klasseId":5000`, `"klasseId":6000`, 1)
	derived := derivedOf(t, p.enrichAbsenceResponse([]byte(raw), "testschool", "dee"))
	if derived["subject"] != "Ethik" {
		t.Errorf("subject was not resolved from the absence's own class: %v", derived)
	}
}

func derivedOf(t *testing.T, raw []byte) jsonObject {
	t.Helper()
	abs := firstAbsence(t, string(raw))
	derived, ok := abs["derived"].(jsonObject)
	if !ok {
		t.Fatalf("no derived block: %v", abs)
	}
	return derived
}

// TestAbsenceEnrichmentNeedsAViewer: with nobody to attribute the response to,
// nothing is added.
func TestAbsenceEnrichmentNeedsAViewer(t *testing.T) {
	p, st := absenceProxy(t)
	if _, err := st.SetAbsenceNote("testschool", "dee", 300001, "private"); err != nil {
		t.Fatalf("SetAbsenceNote: %v", err)
	}
	for _, viewer := range []string{"", "nobody-here"} {
		if got := string(p.enrichAbsenceResponse([]byte(absenceResponse), "testschool", viewer)); got != absenceResponse {
			t.Errorf("viewer %q was decorated anyway:\n got: %s", viewer, got)
		}
	}
	if got := string(p.enrichAbsenceResponse([]byte(absenceResponse), "", "dee")); got != absenceResponse {
		t.Errorf("an empty school was decorated anyway:\n got: %s", got)
	}
}

// TestAbsenceEnrichmentNotOnOtherMethods: enrichment is scoped to the student's
// own absence list, so nothing else grows a note.
func TestAbsenceEnrichmentNotOnOtherMethods(t *testing.T) {
	p, st := absenceProxy(t)
	if _, err := st.SetAbsenceNote("testschool", "dee", 300001, "private"); err != nil {
		t.Fatalf("SetAbsenceNote: %v", err)
	}
	for _, m := range []string{"getTimetable2017", "getPeriodData2017", "getHomeWork2017", "getAbsence", "getUserData2017"} {
		if absenceCarryingMethod(m) {
			t.Errorf("%s should not be an absence-carrying method", m)
		}
		if got := string(p.enrichPersonalResponse([]byte(absenceResponse), "testschool", m, "dee")); got != absenceResponse {
			t.Errorf("method %s was enriched:\n got: %s", m, got)
		}
	}
	if !absenceCarryingMethod("getStudentAbsences2017") {
		t.Error("getStudentAbsences2017 must be an absence-carrying method")
	}
	if !absenceCarryingMethod("GETSTUDENTABSENCES2017") {
		t.Error("method matching should be case-insensitive")
	}
}

// --- the note endpoints ---

func TestAbsenceNotesRequireSession(t *testing.T) {
	p, _ := absenceProxy(t)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		req, err := http.NewRequest(method, "/api/absence/notes", strings.NewReader(`{"absenceKey":1,"note":"x"}`))
		if err != nil {
			t.Fatal(err)
		}
		rec := httptestRecorder()
		p.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a session = %d, want 401", method, rec.Code)
		}
	}
}

func TestAbsenceNoteWriteAndRead(t *testing.T) {
	p, _ := absenceProxy(t)
	rec := sessionedRequest(t, p, "dee", http.MethodPost, "/api/absence/notes",
		`{"absenceKey":300001,"note":"  bring workbook  "}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("write failed: %d %s", rec.Code, rec.Body.String())
	}
	var written absenceNotePayload
	if err := json.Unmarshal(rec.Body.Bytes(), &written); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if written.Note != "bring workbook" {
		t.Errorf("note was not trimmed: %q", written.Note)
	}
	if written.UpdatedAt == "" {
		t.Error("updatedAt is missing")
	}

	read := sessionedRequest(t, p, "dee", http.MethodGet, "/api/absence/notes", "")
	var out struct {
		Notes []absenceNotePayload `json:"notes"`
	}
	if err := json.Unmarshal(read.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if len(out.Notes) != 1 || out.Notes[0].Note != "bring workbook" {
		t.Errorf("read back %s", read.Body.String())
	}
}

// TestAbsenceNoteIsSessionScoped: a username in the body must not move the write
// to another student.
func TestAbsenceNoteIsSessionScoped(t *testing.T) {
	p, st := absenceProxy(t)
	rec := sessionedRequest(t, p, "dee", http.MethodPost, "/api/absence/notes",
		`{"absenceKey":300001,"note":"mine","username":"sam"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("write failed: %s", rec.Body.String())
	}
	sam, err := st.AbsenceNotes("testschool", "sam")
	if err != nil {
		t.Fatalf("AbsenceNotes: %v", err)
	}
	if len(sam) != 0 {
		t.Errorf("a note was written for another student: %v", sam)
	}
	dee, err := st.AbsenceNotes("testschool", "dee")
	if err != nil {
		t.Fatalf("AbsenceNotes: %v", err)
	}
	if len(dee) != 1 {
		t.Errorf("the note was not written for the session user: %v", dee)
	}
}

// TestAbsenceNoteEmptyClears: posting an empty note deletes rather than storing a
// blank, so the two representations cannot disagree.
func TestAbsenceNoteEmptyClears(t *testing.T) {
	p, st := absenceProxy(t)
	if _, err := st.SetAbsenceNote("testschool", "dee", 300001, "something"); err != nil {
		t.Fatalf("SetAbsenceNote: %v", err)
	}
	for i := 0; i < 2; i++ {
		rec := sessionedRequest(t, p, "dee", http.MethodPost, "/api/absence/notes",
			`{"absenceKey":300001,"note":"   "}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("clear %d failed: %s", i, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"note":null`) {
			t.Errorf("clear %d did not report a null note: %s", i, rec.Body.String())
		}
	}
	notes, err := st.AbsenceNotes("testschool", "dee")
	if err != nil {
		t.Fatalf("AbsenceNotes: %v", err)
	}
	if len(notes) != 0 {
		t.Errorf("the note survived clearing: %v", notes)
	}
}

func TestAbsenceNoteValidation(t *testing.T) {
	p, st := absenceProxy(t)
	if _, err := st.SetAbsenceNote("testschool", "dee", 300001, "keep"); err != nil {
		t.Fatalf("SetAbsenceNote: %v", err)
	}
	cases := []struct {
		body string
		want int
	}{
		{`{"note":"no key"}`, http.StatusBadRequest},
		{`{"absenceKey":0,"note":"zero key"}`, http.StatusBadRequest},
		{`{"absenceKey":-1,"note":"negative key"}`, http.StatusBadRequest},
		{`not json`, http.StatusBadRequest},
		{`{"absenceKey":300001,"note":"` + strings.Repeat("x", maxAbsenceNoteLen+1) + `"}`, http.StatusBadRequest},
	}
	for _, c := range cases {
		rec := sessionedRequest(t, p, "dee", http.MethodPost, "/api/absence/notes", c.body)
		if rec.Code != c.want {
			t.Errorf("body %s = %d, want %d", c.body, rec.Code, c.want)
		}
	}
	// None of the rejected bodies may have disturbed the stored note.
	notes, err := st.AbsenceNotes("testschool", "dee")
	if err != nil {
		t.Fatalf("AbsenceNotes: %v", err)
	}
	if len(notes) != 1 || notes[300001].Note != "keep" {
		t.Errorf("a rejected write changed stored state: %v", notes)
	}
}

// TestAbsenceNotesArePerSchool: the same absence id in two schools is two notes.
func TestAbsenceNotesArePerSchool(t *testing.T) {
	_, st := absenceProxy(t)
	if _, err := st.SetAbsenceNote("testschool", "dee", 300001, "here"); err != nil {
		t.Fatalf("SetAbsenceNote: %v", err)
	}
	other, err := st.AbsenceNotes("otherschool", "dee")
	if err != nil {
		t.Fatalf("AbsenceNotes: %v", err)
	}
	if len(other) != 0 {
		t.Errorf("a note leaked into another school: %v", other)
	}
}

// TestAbsenceNoteNotesAreSorted: map order must not decide the response order.
func TestAbsenceNoteNotesAreSorted(t *testing.T) {
	p, st := absenceProxy(t)
	for _, k := range []int64{900, 100, 500} {
		if _, err := st.SetAbsenceNote("testschool", "dee", k, "n"); err != nil {
			t.Fatal(err)
		}
	}
	read := sessionedRequest(t, p, "dee", http.MethodGet, "/api/absence/notes", "")
	var out struct {
		Notes []absenceNotePayload `json:"notes"`
	}
	if err := json.Unmarshal(read.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	for i := 1; i < len(out.Notes); i++ {
		if out.Notes[i-1].AbsenceKey >= out.Notes[i].AbsenceKey {
			t.Fatalf("notes are not sorted by key: %+v", out.Notes)
		}
	}
}

// TestAbsenceNotesOnEmptyStore: a student who never wrote a note gets a valid
// empty list, not an error.
func TestAbsenceNotesOnEmptyStore(t *testing.T) {
	p, _ := absenceProxy(t)
	read := sessionedRequest(t, p, "sam", http.MethodGet, "/api/absence/notes", "")
	if read.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", read.Code, read.Body.String())
	}
	if !strings.Contains(read.Body.String(), `"notes":[]`) {
		t.Errorf("expected an empty list, got %s", read.Body.String())
	}
}

// --- helpers ---

// seedDay seeds a class snapshot for one date, so subject matching has periods to
// match against.
func seedDay(t *testing.T, st *store.Store, date string, rows []store.PeriodRow) {
	t.Helper()
	seedDayClass(t, st, 5000, date, rows)
}

func seedDayClass(t *testing.T, st *store.Store, classID int64, date string, rows []store.PeriodRow) {
	t.Helper()
	for i := range rows {
		rows[i].PeriodID = int64(i + 1)
	}
	// dropRemovedBefore is far in the past so nothing seeded here is pruned.
	if _, err := st.ReplaceClassSnapshot("testschool", classID, rows, 1, "2000-01-01"); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
}

func httptestRecorder() *httptest.ResponseRecorder {
	return httptest.NewRecorder()
}

// --- end to end through the JSON-RPC dispatch ---

// TestAbsenceInternEnrichedForStudent: the normal path — the session owner reads
// their own absences and gets their own note back.
func TestAbsenceInternEnrichedForStudent(t *testing.T) {
	f := &fakeUpstream{internReply: absenceResponse}
	p, st := newFakeProxyUpstream(t, f)
	seedSessionUser(t, st, "dee", 5, 7, 5000)
	if _, err := st.SetAbsenceNote("testschool", "dee", 300001, "bring workbook"); err != nil {
		t.Fatalf("SetAbsenceNote: %v", err)
	}
	body := `{"jsonrpc":"2.0","id":"t","method":"getStudentAbsences2017","params":[` +
		`{"startDate":"2026-10-01","endDate":"2026-10-31","includeExcused":true,"includeUnExcused":true,` +
		`"auth":{"user":"dee"}}]}`
	rec := internReq(t, p, "getStudentAbsences2017", "", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	abs := firstAbsence(t, rec.Body.String())
	if abs["note"] != "bring workbook" {
		t.Errorf("the student's note did not reach their own absence response: %s", rec.Body.String())
	}
	if _, ok := abs["derived"]; !ok {
		t.Errorf("derived metadata is missing: %s", rec.Body.String())
	}
	// The include flags have to survive the forward, or upstream answers with an
	// empty list that looks exactly like "no absences".
	fw := f.Forwards()
	if len(fw) != 1 {
		t.Fatalf("forwards = %d, want 1", len(fw))
	}
	for _, want := range []string{"includeExcused", "includeUnExcused"} {
		if !strings.Contains(string(fw[0].Body), want) {
			t.Errorf("the forward dropped %s: %s", want, fw[0].Body)
		}
	}
}

// TestAbsenceInternNeverDecoratedForTeacher is the end-to-end privacy guard. An
// editor opening the class register gets getPeriodData2017 replayed as a boosted
// teacher, and that response must carry no student note.
func TestAbsenceInternNeverDecoratedForTeacher(t *testing.T) {
	classRegister := `{"jsonrpc":"2.0","id":"upstream","result":{"absences":[
		{"id":300001,"startDateTime":"2026-10-01T10:00","studentId":7,"klasseId":5000}]}}`
	f := &fakeUpstream{internReply: classRegister}
	p, st := newFakeProxyUpstream(t, f)
	seedTeacherSource(t, st, "mrTeacher", 5009)
	seedSessionUser(t, st, "dee", 5, 7, 5000)
	if err := st.SetBoostedFlag("dee", true); err != nil {
		t.Fatalf("boost dee: %v", err)
	}
	if err := st.SetPerm("dee", store.FeatureEditor, true); err != nil {
		t.Fatalf("grant editor: %v", err)
	}
	if _, err := st.SetAbsenceNote("testschool", "dee", 300001, "private"); err != nil {
		t.Fatalf("SetAbsenceNote: %v", err)
	}
	// Precondition: this really is a rewritten, teacher-run request.
	if p.classScopedOwner("testschool", "dee", "getPeriodData2017") == nil {
		t.Fatal("precondition: getPeriodData2017 should resolve to the teacher source")
	}
	rec := internReq(t, p, "getPeriodData2017", "dee",
		internBody("getPeriodData2017", map[string]any{"ttId": 5005, "ttType": 2}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "private") {
		t.Errorf("a student's private note appeared in a teacher's response: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"note"`) {
		t.Errorf("a note field appeared in a teacher's response: %s", rec.Body.String())
	}
}

// TestAbsenceInternNotEnrichedWithoutIdentity: no session and no auth block means
// the proxy cannot attribute the response, so nothing personal is added.
func TestAbsenceInternNotEnrichedWithoutIdentity(t *testing.T) {
	f := &fakeUpstream{internReply: absenceResponse}
	p, st := newFakeProxyUpstream(t, f)
	seedSessionUser(t, st, "dee", 5, 7, 5000)
	if _, err := st.SetAbsenceNote("testschool", "dee", 300001, "private"); err != nil {
		t.Fatalf("SetAbsenceNote: %v", err)
	}
	rec := internReq(t, p, "getStudentAbsences2017", "", internBody("getStudentAbsences2017"))
	if code := errCode(t, rec); code != -8520 {
		t.Errorf("error code = %d, want -8520 (not logged in)", code)
	}
	if strings.Contains(rec.Body.String(), "private") {
		t.Errorf("a note was attached to an unauthenticated response: %s", rec.Body.String())
	}
	if len(f.Forwards()) != 0 {
		t.Error("an unauthenticated request was forwarded upstream")
	}
}
