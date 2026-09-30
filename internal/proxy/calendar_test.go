package proxy

// §7 – Calendar token API tests.
// Each test covers one distinct calendar token scenario:
//   - Student creates a personal token (own personId only)
//   - Student cannot create for another person
//   - Class token requires pool membership
//   - Teacher/room/subject token requires recon or boosted perm
//   - Token is idempotent (second call returns same token)
//   - Token revocation returns 404 from the ICS endpoint
//   - Days clamping (0 → 30, >365 → 365)
//   - Default timezone falls back to Europe/Berlin
//   - Zero-target or multi-target requests are rejected

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"untis-proxy/internal/store"
)

// calRequest fires handleCalendarToken with a session for user.
func calRequest(t *testing.T, p *Proxy, user string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/calendar/token", strings.NewReader(body))
	sess := p.sessions.New(user, 0)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: sess.ID})
	rec := httptest.NewRecorder()
	p.handleCalendarToken(rec, req)
	return rec
}

// TestCalendar_PersonalTokenCreated: a student with a valid personId can
// create a personal calendar token in one POST call.
func TestCalendar_PersonalTokenCreated(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", PersonID: 5005, DisplayName: "Test Student"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	rec := calRequest(t, p, "dee", `{"personId":5005}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create personal token: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if out["token"] == "" || out["token"] == nil {
		t.Errorf("token field missing: %v", out)
	}
	if out["type"] != "student" {
		t.Errorf("type = %v, want student", out["type"])
	}
	if out["url"] == nil || !strings.HasSuffix(out["url"].(string), ".ics") {
		t.Errorf("url = %v, want .ics suffix", out["url"])
	}
}

// TestCalendar_PersonalTokenIdempotent: a second POST for the same personId
// must return the same token value (not create a duplicate).
func TestCalendar_PersonalTokenIdempotent(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", PersonID: 5005}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	var first, second string
	for _, call := range []int{1, 2} {
		rec := calRequest(t, p, "dee", `{"personId":5005}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d: code=%d body=%s", call, rec.Code, rec.Body.String())
		}
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		tok, _ := out["token"].(string)
		if call == 1 {
			first = tok
		} else {
			second = tok
		}
	}
	if first == "" || first != second {
		t.Errorf("idempotent: first=%q second=%q (must be equal)", first, second)
	}
}

// TestCalendar_PersonalToken_ForeignPersonForbidden: a student must not be
// able to create a token for another student's personId.
func TestCalendar_PersonalToken_ForeignPersonForbidden(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", PersonID: 5005}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	rec := calRequest(t, p, "dee", `{"personId":9999}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("foreign personId: code=%d, want 403", rec.Code)
	}
}

// TestCalendar_ClassToken_NotInPoolForbidden: creating a class token for a
// class that is not in the pool must be rejected.
func TestCalendar_ClassToken_NotInPoolForbidden(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", ClassID: 5000}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	// classId 9999 is not in the pool.
	rec := calRequest(t, p, "dee", `{"classId":9999}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("unpooled class: code=%d, want 403", rec.Code)
	}
}

// TestCalendar_ClassToken_InPoolAllowed: a class that IS in the pool must
// get a token successfully.
func TestCalendar_ClassToken_InPoolAllowed(t *testing.T) {
	p, st := newTestProxy(t)
	// seed a student whose class (5000) lands in the pool.
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", PersonType: 5, PersonID: 5005, ClassID: 5000, School: "testschool"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	rec := calRequest(t, p, "dee", `{"classId":5000}`)
	if rec.Code != http.StatusOK {
		t.Errorf("pooled class: code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
}

// TestCalendar_TeacherToken_RequiresReconOrBoosted: a plain student must be
// forbidden from creating a teacher calendar token.
func TestCalendar_TeacherToken_RequiresReconOrBoosted(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "plain", Method: "key", PersonID: 1, ClassID: 5000}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	rec := calRequest(t, p, "plain", `{"teacherId":99}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("plain student teacher token: code=%d, want 403", rec.Code)
	}
}

// TestCalendar_TeacherToken_WithRecon: a user with recon permission can
// create a teacher calendar token.
func TestCalendar_TeacherToken_WithRecon(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "reconuser", Method: "key", PersonID: 2, ClassID: 5000}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := st.SetReconOverride("reconuser", "", true); err != nil {
		t.Fatalf("grant recon: %v", err)
	}

	rec := calRequest(t, p, "reconuser", `{"teacherId":99}`)
	if rec.Code != http.StatusOK {
		t.Errorf("recon teacher token: code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["type"] != "teacher" {
		t.Errorf("type = %v, want teacher", out["type"])
	}
}

// TestCalendar_DaysDefault: if days is 0 or not set, the response must
// report days=30.
func TestCalendar_DaysDefault(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", PersonID: 5005}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	rec := calRequest(t, p, "dee", `{"personId":5005}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["days"].(float64) != 30 {
		t.Errorf("days = %v, want 30 (default)", out["days"])
	}
}

// TestCalendar_DaysClamped: days > 365 must be clamped to 365.
func TestCalendar_DaysClamped(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "dee2", Method: "key", PersonID: 4000}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	rec := calRequest(t, p, "dee2", `{"personId":4000,"days":999}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["days"].(float64) != 365 {
		t.Errorf("days = %v, want 365 (clamped)", out["days"])
	}
}

// TestCalendar_DefaultTimezone: if no timezone is specified the response must
// contain Europe/Berlin.
func TestCalendar_DefaultTimezone(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "tzuser", Method: "key", PersonID: 5000}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	rec := calRequest(t, p, "tzuser", `{"personId":5000}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["timezone"] != "Europe/Berlin" {
		t.Errorf("timezone = %v, want Europe/Berlin", out["timezone"])
	}
}

// TestCalendar_CustomTimezone: an explicitly-set timezone must be echoed back.
func TestCalendar_CustomTimezone(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "nycuser", Method: "key", PersonID: 6000}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	rec := calRequest(t, p, "nycuser", `{"personId":6000,"timezone":"America/New_York"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["timezone"] != "America/New_York" {
		t.Errorf("timezone = %v, want America/New_York", out["timezone"])
	}
}

// TestCalendar_ZeroTargets_BadRequest: a request with no target element must
// return 400.
func TestCalendar_ZeroTargets_BadRequest(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", PersonID: 5005}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	rec := calRequest(t, p, "dee", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("zero targets: code=%d, want 400", rec.Code)
	}
}

// TestCalendar_MultipleTargets_BadRequest: specifying two targets at once must
// return 400.
func TestCalendar_MultipleTargets_BadRequest(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", PersonID: 5005, ClassID: 5000}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	rec := calRequest(t, p, "dee", `{"personId":5005,"classId":5000}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("multiple targets: code=%d, want 400", rec.Code)
	}
}

// TestCalendar_Unauthenticated: a request with no session must return 401.
func TestCalendar_Unauthenticated(t *testing.T) {
	p, _ := newTestProxy(t)
	req := httptest.NewRequest(http.MethodPost, "/api/calendar/token", strings.NewReader(`{"personId":1}`))
	rec := httptest.NewRecorder()
	p.handleCalendarToken(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated: code=%d, want 401", rec.Code)
	}
}

// TestCalendar_MethodNotAllowed: GET must be rejected with 405.
func TestCalendar_MethodNotAllowed(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", PersonID: 5005}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/calendar/token", nil)
	sess := p.sessions.New("dee", 0)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: sess.ID})
	rec := httptest.NewRecorder()
	p.handleCalendarToken(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: code=%d, want 405", rec.Code)
	}
}

// TestCalendar_ICS_UnknownTokenIs404: requesting an ICS for a non-existent
// token must return 404.
func TestCalendar_ICS_UnknownTokenIs404(t *testing.T) {
	p, _ := newTestProxy(t)
	req := httptest.NewRequest(http.MethodGet, "/api/calendar/doesnotexist.ics", nil)
	req.SetPathValue("token", "doesnotexist.ics")
	rec := httptest.NewRecorder()
	p.handleCalendarICS(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown token: code=%d, want 404", rec.Code)
	}
}

// TestCalendar_ICS_RendersCalendarWithVEVENT: a valid class token must render
// a VCALENDAR feed that contains the class name and a VEVENT per period (data
// reconstructed from the pooled class owner's upstream timetable).
func TestCalendar_ICS_RendersCalendarWithVEVENT(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	if err := st.UpsertUser(&store.User{Username: "owen", Method: "key", PersonType: 5, PersonID: 100, ClassID: 5000, Password: "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"}); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	if err := st.CreateClassToken(&store.ClassToken{School: "testschool", Token: "caltok", ElementType: "CLASS", ElementID: 5000, Days: 7}); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	start := time.Now().Add(24 * time.Hour).Format("2006-01-02T15:04Z07:00")
	end := time.Now().Add(25 * time.Hour).Format("2006-01-02T15:04Z07:00")
	f.setTimetable(t, []map[string]any{
		{"id": 1, "startDateTime": start, "endDateTime": end,
			"elements": []any{map[string]any{"type": "SUBJECT", "id": 1}}},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/calendar/caltok.ics", nil)
	req.SetPathValue("token", "caltok.ics")
	rec := httptest.NewRecorder()
	p.handleCalendarICS(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/calendar") {
		t.Errorf("content-type = %q, want text/calendar", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"BEGIN:VCALENDAR", "END:VCALENDAR", "BEGIN:VEVENT", "END:VEVENT", "X-WR-CALNAME:Untis class-5000"} {
		if !strings.Contains(body, want) {
			t.Errorf("ICS missing %q in:\n%s", want, body)
		}
	}
}

// TestCalendar_ICS_StampAndSequence: every VEVENT must carry the RFC 5545
// mandatory DTSTAMP plus a SEQUENCE taken from the class change counter, so
// subscribers notice a moved lesson that keeps its UID.
func TestCalendar_ICS_StampAndSequence(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	if err := st.UpsertUser(&store.User{Username: "owen", Method: "key", PersonType: 5, PersonID: 100, ClassID: 5000, Password: "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"}); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	if err := st.CreateClassToken(&store.ClassToken{School: "testschool", Token: "seqtok", ElementType: "CLASS", ElementID: 5000, Days: 7}); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	// Two class changes -> version 2.
	if _, err := st.ReplaceClassSnapshot("testschool", 5000, []store.PeriodRow{
		{PeriodID: 1, Start: "2026-01-05T08:00:00", End: "2026-01-05T08:45:00", Subject: "Mathe", Room: "R1", Teacher: "Müller"},
	}, 1, ""); err != nil {
		t.Fatalf("snapshot v1: %v", err)
	}
	if _, err := st.ReplaceClassSnapshot("testschool", 5000, []store.PeriodRow{
		{PeriodID: 1, Start: "2026-01-05T09:00:00", End: "2026-01-05T09:45:00", Subject: "Mathe", Room: "R2", Teacher: "Müller"},
	}, 2, ""); err != nil {
		t.Fatalf("snapshot v2: %v", err)
	}
	if got := st.ClassVersion("testschool", 5000); got != 2 {
		t.Fatalf("class version = %d, want 2", got)
	}

	f.setTimetable(t, []map[string]any{
		{"id": 1, "startDateTime": time.Now().Add(24 * time.Hour).Format("2006-01-02T15:04Z07:00"),
			"endDateTime": time.Now().Add(25 * time.Hour).Format("2006-01-02T15:04Z07:00")},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/calendar/seqtok.ics", nil)
	req.SetPathValue("token", "seqtok.ics")
	rec := httptest.NewRecorder()
	p.handleCalendarICS(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "SEQUENCE:2\r\n") {
		t.Errorf("ICS missing SEQUENCE:2 in:\n%s", body)
	}
	events := strings.Count(body, "BEGIN:VEVENT")
	stamps := strings.Count(body, "DTSTAMP:")
	if events == 0 {
		t.Fatalf("no VEVENT rendered in:\n%s", body)
	}
	if stamps != events {
		t.Errorf("DTSTAMP count = %d, want %d (one per VEVENT) in:\n%s", stamps, events, body)
	}
	if _, err := time.Parse("20060102T150405Z", firstValue(body, "DTSTAMP:")); err != nil {
		t.Errorf("DTSTAMP not a UTC timestamp: %v", err)
	}
}

// TestCalendar_ICS_SequenceFallsBackToSchoolVersion: a class that was never
// polled has no version of its own, so the feed falls back to the school-wide
// change counter rather than pinning SEQUENCE to 0 forever.
func TestCalendar_ICS_SequenceFallsBackToSchoolVersion(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	if err := st.UpsertUser(&store.User{Username: "owen", Method: "key", PersonType: 5, PersonID: 100, ClassID: 5000, Password: "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"}); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	if err := st.CreateClassToken(&store.ClassToken{School: "testschool", Token: "fallbacktok", ElementType: "CLASS", ElementID: 5000, Days: 7}); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	// Some other class in the same school is on version 5.
	if _, err := st.ReplaceClassSnapshot("testschool", 777, []store.PeriodRow{
		{PeriodID: 1, Start: "2026-01-05T08:00:00", End: "2026-01-05T08:45:00", Subject: "Deutsch"},
	}, 5, ""); err != nil {
		t.Fatalf("snapshot class 777: %v", err)
	}
	if got := st.SchoolVersion("testschool"); got != 5 {
		t.Fatalf("school version = %d, want 5", got)
	}

	f.setTimetable(t, []map[string]any{
		{"id": 1, "startDateTime": time.Now().Add(24 * time.Hour).Format("2006-01-02T15:04Z07:00"),
			"endDateTime": time.Now().Add(25 * time.Hour).Format("2006-01-02T15:04Z07:00")},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/calendar/fallbacktok.ics", nil)
	req.SetPathValue("token", "fallbacktok.ics")
	rec := httptest.NewRecorder()
	p.handleCalendarICS(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "SEQUENCE:5\r\n") {
		t.Errorf("unpolled class feed should use school version 5, got:\n%s", body)
	}
}

// firstValue returns the value of the first "key<value>\r\n" line in body.
func firstValue(body, key string) string {
	for _, line := range strings.Split(body, "\r\n") {
		if v, ok := strings.CutPrefix(line, key); ok {
			return v
		}
	}
	return ""
}

// TestCalendar_ICS_FileDownloadName: the ICS response advertises the filename
// for the subscription so clients can name the calendar sensibly.
func TestCalendar_ICS_FileDownloadName(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	if err := st.UpsertUser(&store.User{Username: "owen", Method: "key", PersonType: 5, PersonID: 100, ClassID: 5000, Password: "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"}); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	if err := st.CreateClassToken(&store.ClassToken{School: "testschool", Token: "caltok2", ElementType: "CLASS", ElementID: 5000, Days: 7}); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/calendar/caltok2.ics", nil)
	req.SetPathValue("token", "caltok2.ics")
	rec := httptest.NewRecorder()
	p.handleCalendarICS(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if disp := rec.Header().Get("Content-Disposition"); !strings.Contains(disp, "untis-5000.ics") {
		t.Errorf("content-disposition = %q, want untis-5000.ics", disp)
	}
}
