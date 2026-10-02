package proxy

// Failure-path coverage: what happens when the database is unreachable.
//
// Every store call in the personal features has one of two failure modes, and
// they must not be confused. A read that enriches a response (notes, homework
// flags, absence metadata) failing must cost the enrichment and nothing else —
// the app still gets its data. A write the user explicitly asked for failing must
// say so, because silently reporting success for a note that was not saved is
// worse than an error.

import (
	"database/sql"
	"encoding/json"
	"math"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"untis-proxy/internal/session"
	"untis-proxy/internal/store"
	"untis-proxy/internal/untis"
)

// storeProxyWithPath opens a store at a known path, so a test can open a second
// connection to the same file and damage it.
func storeProxyWithPath(t *testing.T) (*Proxy, *store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.SetDefaultSchool("testschool"); err != nil {
		t.Fatalf("set default school: %v", err)
	}
	if err := st.UpsertUser(&store.User{Username: "dee", School: "testschool", Method: "key",
		Password: "x", PersonType: 5, PersonID: 7, ClassID: 5000}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return New(st, untis.New(untis.Config{Server: "school.example.com", School: "testschool"}),
		session.NewManager(5*time.Minute), Options{School: "testschool"}), st, path
}

// dropTable removes one table behind the store's back. A closed store cannot
// reach the code paths under test: with no database the session lookup fails
// first and the request is refused as unauthenticated, which is correct but never
// gets as far as the note or flag query. Dropping the single table those
// endpoints read isolates the failure to the call being tested.
func dropTable(t *testing.T, path, table string) {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatalf("open second connection: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`DROP TABLE ` + table); err != nil {
		t.Fatalf("drop %s: %v", table, err)
	}
}

// closedStoreProxy returns a proxy whose database has been closed, which is how a
// test makes every store call fail.
func closedStoreProxy(t *testing.T) *Proxy {
	t.Helper()
	p, st := absenceProxy(t)
	st.Close()
	return p
}

// TestEnrichmentPassesThroughWhenTheStoreIsClosed: with no database there is no
// note and no identity to check against, so the upstream response is handed back
// untouched rather than half-decorated.
func TestEnrichmentPassesThroughWhenTheStoreIsClosed(t *testing.T) {
	p := closedStoreProxy(t)
	if got := string(p.enrichAbsenceResponse([]byte(absenceResponse), "testschool", "dee")); got != absenceResponse {
		t.Errorf("absence: response altered on a store failure\n got: %s\nwant: %s", got, absenceResponse)
	}
	if got := string(p.enrichHomeWorkResponse([]byte(oneHomework), "testschool", "dee")); got != oneHomework {
		t.Errorf("homework: response altered on a store failure\n got: %s\nwant: %s", got, oneHomework)
	}
}

// TestEnrichmentNeedsAViewerAndSchool: with no identity to key on, there is
// nothing to attach, so the response is left alone.
func TestEnrichmentNeedsAViewerAndSchool(t *testing.T) {
	p, _ := absenceProxy(t)
	for _, tc := range []struct{ school, viewer string }{{"testschool", ""}, {"", "dee"}, {"", ""}} {
		if got := string(p.enrichAbsenceResponse([]byte(absenceResponse), tc.school, tc.viewer)); got != absenceResponse {
			t.Errorf("absence school=%q viewer=%q: response altered", tc.school, tc.viewer)
		}
		if got := string(p.enrichHomeWorkResponse([]byte(oneHomework), tc.school, tc.viewer)); got != oneHomework {
			t.Errorf("homework school=%q viewer=%q: response altered", tc.school, tc.viewer)
		}
	}
}

// TestAbsenceSubjectsSkipWhatCannotBeResolved: a group is keyed by class and
// date, so entries without a usable id, date or class are dropped before any
// query rather than producing a query that can only fail.
func TestAbsenceSubjectsSkipWhatCannotBeResolved(t *testing.T) {
	p, _ := absenceProxy(t)
	user := &store.User{PersonID: 7, ClassID: 5000}
	got := p.absenceSubjects("testschool", user, []jsonObject{
		// No id: it cannot be keyed in the result map.
		{"startDateTime": "2026-10-01T10:00", "klasseId": json.Number("5000")},
		// Unparseable date: no group to query.
		{"id": json.Number("1"), "startDateTime": "nonsense", "klasseId": json.Number("5000")},
		// No class anywhere: nothing to look the timetable up by.
		{"id": json.Number("2"), "startDateTime": "2026-10-01T10:00"},
		{"id": json.Number("3"), "startDateTime": "2026-10-01T10:00", "klasseId": json.Number("0")},
		// Well formed, but the class has no periods on that date.
		{"id": json.Number("4"), "startDateTime": "2026-10-01T10:00", "klasseId": json.Number("5000")},
	})
	if len(got) != 0 {
		t.Errorf("subjects = %v, want none", got)
	}
}

// TestAbsenceSubjectsFallsBackToTheViewersClass: an absence with no klasseId of
// its own still belongs to the viewer's class, so its lesson can be found.
func TestAbsenceSubjectsFallsBackToTheViewersClass(t *testing.T) {
	p, st := absenceProxy(t)
	seedDay(t, st, "2026-10-01", []store.PeriodRow{
		{Start: "2026-10-01 10:00", End: "2026-10-01 10:45", Subject: "Mathe"},
	})
	user := &store.User{PersonID: 7, ClassID: 5000}
	got := p.absenceSubjects("testschool", user, []jsonObject{
		{"id": json.Number("1"), "startDateTime": "2026-10-01T10:00", "endDateTime": "2026-10-01T10:45"},
	})
	if got[1] != "Mathe" {
		t.Errorf("subjects = %v, want the viewer's own class to supply Mathe", got)
	}
}

// TestAbsenceSubjectsSkipsAnotherClassesAbsence: an absence carrying its own
// class id is looked up in that class, not the viewer's, so a student who is also
// shown another class's absence gets that class's lesson.
func TestAbsenceSubjectsSkipsAnotherClassesAbsence(t *testing.T) {
	p, st := absenceProxy(t)
	seedDayClass(t, st, 6000, "2026-10-01", []store.PeriodRow{
		{Start: "2026-10-01 10:00", End: "2026-10-01 10:45", Subject: "Sport"},
	})
	user := &store.User{PersonID: 7, ClassID: 5000}
	got := p.absenceSubjects("testschool", user, []jsonObject{
		{"id": json.Number("1"), "startDateTime": "2026-10-01T10:00",
			"endDateTime": "2026-10-01T10:45", "klasseId": json.Number("6000")},
		{"id": json.Number("2"), "startDateTime": "2026-10-01T10:00",
			"endDateTime": "2026-10-01T10:45"},
	})
	if got[1] != "Sport" {
		t.Errorf("subjects = %v, want the absence's own class to supply Sport", got)
	}
	if _, present := got[2]; present {
		t.Errorf("a subject was claimed for the viewer's class, which has no periods: %v", got[2])
	}
}

// TestAbsenceSubjectsSurvivesAStoreFailure: the timetable lookup is an
// optimisation, not the feature. Losing it costs the subject and nothing else.
func TestAbsenceSubjectsSurvivesAStoreFailure(t *testing.T) {
	p := closedStoreProxy(t)
	user := &store.User{PersonID: 7, ClassID: 5000}
	got := p.absenceSubjects("testschool", user, []jsonObject{
		{"id": json.Number("1"), "startDateTime": "2026-10-01T10:00", "endDateTime": "2026-10-01T10:45"},
	})
	if len(got) != 0 {
		t.Errorf("subjects = %v, want none when the lookup fails", got)
	}
}

// TestAbsentStoreRefusesEverySession: with no database the session cannot be
// resolved to a user, so the personal endpoints refuse rather than acting on an
// empty identity. This is the closed-store path, and it is why the per-table
// failures below are tested by dropping one table instead.
func TestAbsentStoreRefusesEverySession(t *testing.T) {
	p := closedStoreProxy(t)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/absence/notes", ""},
		{http.MethodPost, "/api/absence/notes", `{"absenceKey":300001,"note":"x"}`},
		{http.MethodGet, "/api/homework/flags", ""},
		{http.MethodPost, "/api/homework/done", `{"homeworkId":1001,"done":true}`},
	} {
		rec := sessionedRequest(t, p, "dee", tc.method, tc.path, tc.body)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401 (body %s)", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

// TestAbsenceNoteEndpointsReportStoreFailures: an explicit write that cannot be
// saved must be reported, not acknowledged.
func TestAbsenceNoteEndpointsReportStoreFailures(t *testing.T) {
	p, _, path := storeProxyWithPath(t)
	dropTable(t, path, "absence_notes")
	for _, tc := range []struct{ method, body string }{
		{http.MethodGet, ""},
		{http.MethodPost, `{"absenceKey":300001,"note":"x"}`},
		{http.MethodPost, `{"absenceKey":300001,"note":""}`},
	} {
		rec := sessionedRequest(t, p, "dee", tc.method, "/api/absence/notes", tc.body)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s %q = %d, want 500 (body %s)", tc.method, tc.body, rec.Code, rec.Body.String())
		}
	}
}

// TestHomeworkEndpointsReportStoreFailures: the same rule as for notes — a write
// the user asked for and did not get must not be reported as saved.
func TestHomeworkEndpointsReportStoreFailures(t *testing.T) {
	p, _, path := storeProxyWithPath(t)
	dropTable(t, path, "homework_done")
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/homework/flags", ""},
		{http.MethodPost, "/api/homework/done", `{"homeworkId":1001,"done":true}`},
		{http.MethodPost, "/api/homework/done", `{"homeworkId":1001,"done":false}`},
	} {
		rec := sessionedRequest(t, p, "dee", tc.method, tc.path, tc.body)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s %s %q = %d, want 500 (body %s)", tc.method, tc.path, tc.body, rec.Code, rec.Body.String())
		}
	}
}

// TestHomeworkDoneRejectsOtherMethods: the route is POST-only, so this guards
// the handler's own check rather than the mux's.
func TestHomeworkDoneRejectsOtherMethods(t *testing.T) {
	p, _ := absenceProxy(t)
	for _, method := range []string{http.MethodGet, http.MethodDelete, http.MethodPut} {
		req, err := http.NewRequest(method, "/api/homework/done",
			strings.NewReader(`{"homeworkId":1001,"done":true}`))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		rec := httptestRecorder()
		p.handleHomeworkDone(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d, want 405", method, rec.Code)
		}
	}
}

// TestHomeworkDoneRejectsIncompleteBodies: without both fields the request is
// ambiguous, and guessing would write a flag the user never asked for.
func TestHomeworkDoneRejectsIncompleteBodies(t *testing.T) {
	p, st := absenceProxy(t)
	for _, tc := range []struct{ name, body string }{
		{"no homeworkId", `{"done":true}`},
		{"zero homeworkId", `{"homeworkId":0,"done":true}`},
		{"negative homeworkId", `{"homeworkId":-1,"done":true}`},
		{"no done", `{"homeworkId":1001}`},
		{"done is not a bool", `{"homeworkId":1001,"done":"yes"}`},
		{"not json", `{`},
	} {
		rec := sessionedRequest(t, p, "dee", http.MethodPost, "/api/homework/done", tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body %s)", tc.name, rec.Code, rec.Body.String())
		}
	}
	if done, err := st.HomeworkDone("testschool", "dee"); err != nil {
		t.Fatalf("HomeworkDone: %v", err)
	} else if len(done) != 0 {
		t.Errorf("a rejected request wrote %d flags: %v", len(done), done)
	}
}

// TestHomeworkDoneNeedsASession: a flag belongs to somebody, so the write needs
// an identity rather than a username in the body.
func TestHomeworkDoneNeedsASession(t *testing.T) {
	p, _ := absenceProxy(t)
	req, err := http.NewRequest(http.MethodPost, "/api/homework/done",
		strings.NewReader(`{"homeworkId":1001,"done":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	rec := httptestRecorder()
	p.handleHomeworkDone(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 without a session", rec.Code)
	}
}

// TestEnrichJSONReturnsInputWhenDecorationCannotBeEncoded: a decorator that puts
// something unmarshalable into the result must not produce a truncated or
// half-written response. The untouched bytes are the only safe answer.
func TestEnrichJSONReturnsInputWhenDecorationCannotBeEncoded(t *testing.T) {
	raw := `{"jsonrpc":"2.0","id":"x","result":{"a":1}}`
	got := enrichJSON([]byte(raw), func(result jsonObject) { result["bad"] = math.Inf(1) })
	if string(got) != raw {
		t.Errorf("an unencodable decoration altered the response\n got: %s\nwant: %s", got, raw)
	}
}

// TestEnrichmentSurvivesAnUnreadableNoteTable: a failing note query must cost the
// note, not the response. The derived metadata is computed from the response
// itself, so it must still arrive — this is the one place the two are separated,
// and it is what the comment in enrichAbsenceResponse claims.
func TestEnrichmentSurvivesAnUnreadableNoteTable(t *testing.T) {
	p, _, path := storeProxyWithPath(t)
	dropTable(t, path, "absence_notes")

	out := p.enrichAbsenceResponse([]byte(absenceResponse), "testschool", "dee")
	if string(out) == absenceResponse {
		t.Fatal("the response came back untouched; the derived block should not depend on the notes")
	}
	abs := firstAbsence(t, string(out))
	derived, ok := abs["derived"].(jsonObject)
	if !ok || derived["date"] != "2026-10-01" {
		t.Errorf("derived = %v, want it to survive an unreadable note table", abs["derived"])
	}
	if _, present := abs["note"]; present {
		t.Errorf("a note was invented with no note table: %v", abs["note"])
	}
	if abs["text"] != "Doctor" {
		t.Errorf("the teacher's comment was lost: %v", abs["text"])
	}
}

// TestEnrichmentNeedsAReadableUserForAbsencesOnly: absence enrichment compares
// each entry's studentId against the session user's person id, so it needs a
// readable user row. Homework does not: its flags are keyed by the session
// username, which the dispatcher already resolved, so a missing users table costs
// absences their derived block and leaves homework untouched. The asymmetry is
// deliberate — absence checks identity independently, homework trusts a key it
// was handed.
func TestEnrichmentNeedsAReadableUserForAbsencesOnly(t *testing.T) {
	p, _, path := storeProxyWithPath(t)
	dropTable(t, path, "users")

	if got := string(p.enrichAbsenceResponse([]byte(absenceResponse), "testschool", "dee")); got != absenceResponse {
		t.Errorf("absence: response altered without a readable user\n got: %s", got)
	}
	homework, err := p.store.HomeworkDone("testschool", "dee")
	if err != nil {
		t.Fatalf("HomeworkDone: %v", err)
	}
	if len(homework) != 0 {
		t.Fatalf("precondition: expected no flags")
	}
	if got := string(p.enrichHomeWorkResponse([]byte(oneHomework), "testschool", "dee")); !strings.Contains(got, `"done":false`) {
		t.Errorf("homework: flags should still be reported without a readable user row, got %s", got)
	}
}

// TestAbsenceSubjectsSkipAViewerWithNoClass: with no class on the user and none on
// the absence there is nothing to look a timetable up by, so no subject is claimed
// rather than a lookup against a class that does not exist.
func TestAbsenceSubjectsSkipAViewerWithNoClass(t *testing.T) {
	p, st := absenceProxy(t)
	seedDayClass(t, st, 5000, "2026-10-01", []store.PeriodRow{
		{Start: "2026-10-01 10:00", End: "2026-10-01 10:45", Subject: "Mathe"},
	})
	got := p.absenceSubjects("testschool", &store.User{PersonID: 7}, []jsonObject{
		{"id": json.Number("1"), "startDateTime": "2026-10-01T10:00", "endDateTime": "2026-10-01T10:45"},
	})
	if len(got) != 0 {
		t.Errorf("subjects = %v, want none when no class is known", got)
	}
}
