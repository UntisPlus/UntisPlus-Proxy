package proxy

// §8 – Change detection tests (handleTimetableChanges).
// Each test seeds the DB directly and calls the HTTP handler, asserting
// the exact JSON fields returned or the HTTP status code.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"untis-proxy/internal/store"
)

// changesRequest calls handleTimetableChanges with the given query string.
func changesRequest(t *testing.T, p *Proxy, user string, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/timetable/changes?"+query, nil)
	sess := p.sessions.New(user, 0)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: sess.ID})
	rec := httptest.NewRecorder()
	p.handleTimetableChanges(rec, req)
	return rec
}

// seedChange inserts a single period row directly into the store.
func seedChange(t *testing.T, st *store.Store, school string, classID, version int64, rows []store.PeriodRow) {
	t.Helper()
	n, err := st.ReplaceClassSnapshot(school, classID, rows, version, "")
	if err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	_ = n
}

// TestChanges_Unauthenticated: without a session the endpoint must return 401.
func TestChanges_Unauthenticated(t *testing.T) {
	p, _ := newTestProxy(t)
	req := httptest.NewRequest(http.MethodGet, "/api/timetable/changes?classId=5000&since=0", nil)
	rec := httptest.NewRecorder()
	p.handleTimetableChanges(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code=%d, want 401", rec.Code)
	}
}

// TestChanges_ForeignClass: a student may not poll a different class's changes.
func TestChanges_ForeignClass(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", ClassID: 5000, PersonType: 5}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	// Requesting classId 9999 (not user's class) must be forbidden.
	rec := changesRequest(t, p, "dee", "classId=9999&since=0")
	if rec.Code != http.StatusForbidden {
		t.Errorf("foreign class: code=%d, want 403", rec.Code)
	}
}

// TestChanges_NotModifiedWhenNoNewChanges: if since equals the current
// version and there are no newer rows, the endpoint must return 304.
func TestChanges_NotModifiedWhenNoNewChanges(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", ClassID: 5000, PersonType: 5}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	// Seed version 5.
	seedChange(t, st, "testschool", 5000, 5, []store.PeriodRow{
		{PeriodID: 1, Kind: "ADDED", Subject: "M", Start: "2026-09-07 08:00", End: "2026-09-07 09:00"},
	})
	// since=5 matches current version → 304.
	rec := changesRequest(t, p, "dee", "classId=5000&since=5")
	if rec.Code != http.StatusNotModified {
		t.Errorf("up-to-date: code=%d, want 304", rec.Code)
	}
}

// TestChanges_ReturnsChangesWhenSinceIsOld: if since < current version the
// response must include the changed rows and the current version.
func TestChanges_ReturnsChangesWhenSinceIsOld(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", ClassID: 5000, PersonType: 5}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	seedChange(t, st, "testschool", 5000, 7, []store.PeriodRow{
		{PeriodID: 10, Kind: "ADDED", Subject: "M", Start: "2026-09-07 08:00", End: "2026-09-07 09:00"},
	})
	rec := changesRequest(t, p, "dee", "classId=5000&since=0")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var out struct {
		Changes []map[string]any `json:"changes"`
		Current int64            `json:"current"`
		Since   int64            `json:"since"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if out.Current != 7 {
		t.Errorf("current = %d, want 7", out.Current)
	}
	if len(out.Changes) == 0 {
		t.Errorf("changes = empty, want at least 1 row")
	}
	if out.Since != 0 {
		t.Errorf("since = %d, want 0", out.Since)
	}
}

// TestChanges_DefaultsToUsersClass: when no classId query parameter is sent
// the handler must fall back to the session user's own class.
func TestChanges_DefaultsToUsersClass(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", ClassID: 5000, PersonType: 5}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	seedChange(t, st, "testschool", 5000, 3, []store.PeriodRow{
		{PeriodID: 20, Kind: "ADDED", Subject: "E", Start: "2026-09-08 10:00", End: "2026-09-08 11:00"},
	})
	// No classId in query; must use user's class.
	rec := changesRequest(t, p, "dee", "since=0")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var out struct {
		ClassID int64            `json:"classId"`
		Changes []map[string]any `json:"changes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if out.ClassID != 5000 {
		t.Errorf("classId = %d, want 5000 (user default)", out.ClassID)
	}
}

// TestChanges_Since0AlwaysReturnsSnapshot: since=0 must return all stored
// rows (the full snapshot) when the class has any changes.
func TestChanges_Since0AlwaysReturnsSnapshot(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "sam", Method: "key", ClassID: 5000, PersonType: 5}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	seedChange(t, st, "testschool", 5000, 2, []store.PeriodRow{
		{PeriodID: 1, Kind: "ADDED", Subject: "A", Start: "2026-09-01 08:00", End: "2026-09-01 09:00"},
		{PeriodID: 2, Kind: "ADDED", Subject: "B", Start: "2026-09-02 08:00", End: "2026-09-02 09:00"},
	})
	rec := changesRequest(t, p, "sam", "classId=5000&since=0")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Changes []map[string]any `json:"changes"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Changes) != 2 {
		t.Errorf("changes len = %d, want 2 (full snapshot)", len(out.Changes))
	}
}

// TestChanges_VersionBumpsOnNewData: after a second snapshot write the
// current version must increase and the newly changed rows must appear.
func TestChanges_VersionBumpsOnNewData(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "sam", Method: "key", ClassID: 5001, PersonType: 5}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	// First snapshot at version 1.
	seedChange(t, st, "testschool", 5001, 1, []store.PeriodRow{
		{PeriodID: 1, Kind: "ADDED", Subject: "A", Start: "2026-09-01 08:00", End: "2026-09-01 09:00"},
	})
	// Second snapshot at version 2: subject changed.
	seedChange(t, st, "testschool", 5001, 2, []store.PeriodRow{
		{PeriodID: 1, Kind: "CHANGED", Subject: "A-CHANGED", Start: "2026-09-01 08:00", End: "2026-09-01 09:00"},
	})
	rec := changesRequest(t, p, "sam", "classId=5001&since=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Current int64            `json:"current"`
		Changes []map[string]any `json:"changes"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Current != 2 {
		t.Errorf("current = %d, want 2 after second snapshot", out.Current)
	}
	if len(out.Changes) == 0 {
		t.Errorf("changes empty after subject change")
	}
}

// TestChanges_NotModifiedForAClientThatNeverSendsSinceEvents guards the 304
// decision against the event counter.
//
// eventVersion was added to this endpoint so an app could notice that one of its
// own events changed. It was initially folded into the not-modified test as
// `events == sinceEvents`, with sinceEvents defaulting to 0 when absent — so any
// student who had at least one event had eventVersion >= 1, could never match 0,
// and every poll from every existing app build got a full 200 body forever
// instead of the 304 it had been getting. The class-change tests did not catch it
// because they seed no events, and a student's eventVersion of 0 still matches.
//
// The rule is now that the counter only takes part when the client opted in by
// sending sinceEvents; a client that never sends it behaves exactly as it did
// before events existed.
func TestChanges_NotModifiedForAClientThatNeverSendsSinceEvents(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", ClassID: 5000, PersonType: 5}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	seedChange(t, st, "testschool", 5000, 5, []store.PeriodRow{
		{PeriodID: 1, Kind: "ADDED", Subject: "M", Start: "2026-09-07 08:00", End: "2026-09-07 09:00"},
	})
	// One event, so this student's eventVersion is 1.
	if _, err := st.CreateStudentEvent("testschool", "dee", store.NewStudentEvent{
		Date: "2026-10-01", StartTime: "14:00", EndTime: "15:00", Title: "Technik",
	}, "adm"); err != nil {
		t.Fatalf("CreateStudentEvent: %v", err)
	}
	if v, err := st.StudentEventVersion("testschool", "dee"); err != nil || v != 1 {
		t.Fatalf("eventVersion = %d (%v), want 1", v, err)
	}

	rec := changesRequest(t, p, "dee", "classId=5000&since=5")
	if rec.Code != http.StatusNotModified {
		t.Errorf("a client that never sends sinceEvents got %d, want 304 (body %s)", rec.Code, rec.Body.String())
	}

	// And a second poll is still a 304, not a one-off.
	if rec := changesRequest(t, p, "dee", "classId=5000&since=5"); rec.Code != http.StatusNotModified {
		t.Errorf("second poll got %d, want 304", rec.Code)
	}
}

// TestChanges_EventVersionStillNotifiesAnOptedInClient is the other half: a
// client that does send sinceEvents must still be told about its own events, or
// the fix above would have quietly disabled the feature.
func TestChanges_EventVersionStillNotifiesAnOptedInClient(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", ClassID: 5000, PersonType: 5}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	seedChange(t, st, "testschool", 5000, 5, []store.PeriodRow{
		{PeriodID: 1, Kind: "ADDED", Subject: "M", Start: "2026-09-07 08:00", End: "2026-09-07 09:00"},
	})
	if _, err := st.CreateStudentEvent("testschool", "dee", store.NewStudentEvent{
		Date: "2026-10-01", StartTime: "14:00", EndTime: "15:00", Title: "Technik",
	}, "adm"); err != nil {
		t.Fatalf("CreateStudentEvent: %v", err)
	}

	// Up to date on both counters: 304.
	if rec := changesRequest(t, p, "dee", "classId=5000&since=5&sinceEvents=1"); rec.Code != http.StatusNotModified {
		t.Errorf("opted-in, up-to-date client got %d, want 304", rec.Code)
	}
	// A client whose event counter is behind gets 200 even with no class change.
	if rec := changesRequest(t, p, "dee", "classId=5000&since=5&sinceEvents=0"); rec.Code != http.StatusOK {
		t.Errorf("opted-in client behind on events got %d, want 200", rec.Code)
	}
	// An event appeared since: 200 even though the class has not changed.
	if _, err := st.CreateStudentEvent("testschool", "dee", store.NewStudentEvent{
		Date: "2026-10-08", StartTime: "14:00", EndTime: "15:00", Title: "Technik",
	}, "adm"); err != nil {
		t.Fatalf("second CreateStudentEvent: %v", err)
	}
	if rec := changesRequest(t, p, "dee", "classId=5000&since=5&sinceEvents=1"); rec.Code != http.StatusOK {
		t.Errorf("a new event produced %d, want 200", rec.Code)
	}
}
