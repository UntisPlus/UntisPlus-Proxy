package proxy

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"untis-proxy/internal/access"
	"untis-proxy/internal/store"
	"untis-proxy/internal/untis"
)

func analyticsFixture(t *testing.T) (*Proxy, *store.Store, *fakeUpstream) {
	t.Helper()
	f := &fakeUpstream{timetable: `{"result":{"periods":[{"id":9007199254740993,"date":20260921,"code":"cancelled","teachers":[{"id":42,"name":"Private Teacher"}]}]}}`}
	p, st := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st, "student", 7, 5000)
	seedTeacherSource(t, st, "source", 42)
	st.SetFeatureEnabled("", "analytics", true)
	for _, cap := range []access.Capability{access.CapAnalyticsPersonal, access.CapAnalyticsHistory, access.CapAnalyticsTeacher, access.CapAnalyticsClassHistory, access.CapAnalyticsExport} {
		if err := st.SetUserCapGrant("testschool", "student", string(cap), 1); err != nil {
			t.Fatal(err)
		}
	}
	return p, st, f
}
func analyticsReq(t *testing.T, p *Proxy, user, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/analytics/teacher-cancellations?school=testschool", strings.NewReader(body))
	if user != "" {
		r.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: p.sessions.New(user, 5000).ID})
	}
	w := httptest.NewRecorder()
	p.handleCancellationAnalytics(w, r)
	return w
}

// Exercise the same cookie issuance and report routes used by the beta smoke
// script. A dashboard administrator still needs the feature and the grant.
func TestAnalyticsAfterAuthenticatorSignIn(t *testing.T) {
	day := time.Now().UTC().AddDate(0, 0, -7).Format(time.DateOnly)
	f := &fakeUpstream{timetable: fmt.Sprintf(`{"result":{"periods":[{"id":95,"date":%q,"code":"cancelled","teachers":[{"id":42,"name":"Example Teacher"}]}]}}`, day)}
	p, st := newFakeProxyUpstream(t, f)
	const user = "analytics-admin"
	const testKey = "JBSWY3DPEHPK3PXP"
	if err := st.UpsertUser(&store.User{Username: user, School: "testschool", Admin: true, Method: "key", Password: testKey, PersonType: 5, PersonID: 7, ClassID: 5000}); err != nil {
		t.Fatal(err)
	}

	handler := p.Handler()
	login := httptest.NewRequest(http.MethodPost, "/WebUntis/jsonrpc_intern.do?school=testschool", strings.NewReader(loginBody(t, user, untis.TOTP(testKey))))
	login.Header.Set("Content-Type", "application/json")
	signedIn := httptest.NewRecorder()
	handler.ServeHTTP(signedIn, login)
	var sessionCookie *http.Cookie
	for _, cookie := range signedIn.Result().Cookies() {
		if cookie.Name == "JSESSIONID" {
			sessionCookie = cookie
			break
		}
	}
	if signedIn.Code != http.StatusOK || sessionCookie == nil || sessionCookie.Value == "" {
		t.Fatalf("authenticator login failed: HTTP %d, %s", signedIn.Code, signedIn.Body.String())
	}

	sendReport := func(cookie *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		body := fmt.Sprintf(`{"mode":"personal","from":%q,"to":%q,"anonymise":true}`, day, day)
		req := httptest.NewRequest(http.MethodPost, "/api/analytics/teacher-cancellations?school=testschool", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	assertDenied := func(label string, cookie *http.Cookie, want int) {
		t.Helper()
		rec := sendReport(cookie)
		if rec.Code != want || len(f.Forwards()) != 0 {
			t.Fatalf("%s: HTTP %d, want %d with no upstream timetable request: %s", label, rec.Code, want, rec.Body.String())
		}
	}
	assertDenied("without a session", nil, http.StatusUnauthorized)
	assertDenied("analytics disabled", sessionCookie, http.StatusForbidden)
	if err := st.SetFeatureEnabled("", "analytics", true); err != nil {
		t.Fatal(err)
	}
	assertDenied("feature enabled without a grant", sessionCookie, http.StatusForbidden)
	if err := st.SetUserCapGrant("testschool", user, string(access.CapAnalyticsPersonal), 1); err != nil {
		t.Fatal(err)
	}
	rec := sendReport(sessionCookie)
	var report analyticsResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &report) != nil ||
		!report.Complete || !report.Anonymised || report.Totals.Cancelled != 1 || report.UniqueLessons != 1 {
		t.Fatalf("authorized report: HTTP %d, %s", rec.Code, rec.Body.String())
	}
	calls := f.Forwards()
	if len(calls) != 1 || calls[0].Method != "getTimetable2017" || calls[0].Cookie != p.untis.SchoolCookie("testschool") {
		t.Fatalf("expected one upstream timetable read with the school routing cookie, got %d calls", len(calls))
	}
}

func TestAnalyticsPersonalStatsAndAnonymisedExports(t *testing.T) {
	p, _, f := analyticsFixture(t)
	w := analyticsReq(t, p, "student", `{"from":"2026-09-21","to":"2026-09-21"}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var report analyticsResponse
	if json.Unmarshal(w.Body.Bytes(), &report) != nil || report.Totals.Cancelled != 1 || !report.Complete || len(report.Teachers) != 1 {
		t.Fatalf("bad report: %s", w.Body.String())
	}
	if len(f.Forwards()) != 1 || !strings.Contains(string(f.Forwards()[0].Body), `"id":7`) {
		t.Fatal("did not use own personal identity")
	}
	for _, format := range []string{"json", "export-json", "csv-zip"} {
		w = analyticsReq(t, p, "student", `{"from":"2026-09-21","to":"2026-09-21","anonymise":true,"format":"`+format+`"}`)
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		contents := w.Body.Bytes()
		if format == "csv-zip" {
			z, err := zip.NewReader(bytes.NewReader(contents), int64(len(contents)))
			if err != nil {
				t.Fatal(err)
			}
			contents = nil
			if len(z.File) != 10 {
				t.Fatalf("missing export tables: %d", len(z.File))
			}
			for _, file := range z.File {
				reader, _ := file.Open()
				b, _ := io.ReadAll(reader)
				reader.Close()
				contents = append(contents, b...)
			}
		}
		if strings.Contains(string(contents), "Private Teacher") || strings.Contains(string(contents), `"id":"42"`) {
			t.Fatal("export leaked teacher identifier")
		}
	}
}
func TestAnalyticsScopeAndMasterSwitchNeverBypassed(t *testing.T) {
	for _, scenario := range []string{"anonymous", "master-off", "personal-deny", "export-deny", "unapproved-teacher", "foreign-school", "history-deny", "unknown-field"} {
		t.Run(scenario, func(t *testing.T) {
			p, st, f := analyticsFixture(t)
			user := "student"
			body := `{"from":"2026-09-21","to":"2026-09-21"}`
			switch scenario {
			case "anonymous":
				user = ""
			case "master-off":
				st.SetFeatureEnabled("", "analytics", false)
				st.SetFeatureEnabled("testschool", "analytics", true)
			case "personal-deny":
				st.SetUserCapGrant("testschool", user, string(access.CapAnalyticsPersonal), -1)
			case "export-deny":
				st.SetUserCapGrant("testschool", user, string(access.CapAnalyticsExport), -1)
				body = `{"from":"2026-09-21","to":"2026-09-21","format":"csv-zip"}`
			case "unapproved-teacher":
				body = `{"mode":"teacher","teacherId":42,"from":"2026-09-21","to":"2026-09-21"}`
			case "foreign-school":
				u, _ := st.GetUser(user)
				u.School = "other"
				st.UpsertUser(u)
			case "history-deny":
				st.SetUserCapGrant("testschool", user, string(access.CapAnalyticsHistory), -1)
				body = `{"from":"2020-01-01","to":"2020-01-02"}`
			case "unknown-field":
				body = `{"from":"2026-09-21","to":"2026-09-21","username":"source"}`
			}
			w := analyticsReq(t, p, user, body)
			if w.Code < 400 || len(f.Forwards()) != 0 {
				t.Fatalf("scope bypass: %d %s", w.Code, w.Body.String())
			}
		})
	}
}
func TestAnalyticsApprovedTeacherAndClassHistoryGaps(t *testing.T) {
	p, st, f := analyticsFixture(t)
	scope := store.AnalyticsAccess{TeacherIDs: []int64{42}, Profiles: []store.AnalyticsProfile{{ID: "history", Segments: []store.AnalyticsSegment{{ClassID: 5000, From: "2026-09-21", To: "2026-09-21"}}}}}
	if err := st.SetAnalyticsAccess("testschool", "student", scope); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"mode":"teacher","teacherId":42,"from":"2026-09-21","to":"2026-09-21"}`, `{"mode":"class-history","profileId":"history","from":"2026-09-21","to":"2026-09-22"}`} {
		w := analyticsReq(t, p, "student", body)
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		var response analyticsResponse
		json.Unmarshal(w.Body.Bytes(), &response)
		if strings.Contains(body, "class-history") && response.Complete {
			t.Fatal("unapproved history gap claimed complete")
		}
	}
	for _, call := range f.Forwards() {
		if call.Method != "getTimetable2017" || !strings.Contains(string(call.Body), `"user":"source"`) {
			t.Fatal("analytics used unexpected upstream operation")
		}
	}
}
func TestAnalyticsPartialInvalidAndRevokedReports(t *testing.T) {
	p, st, f := analyticsFixture(t)
	f.setTimetableBody(`{"error":{"code":-8504}}`)
	w := analyticsReq(t, p, "student", `{"from":"2026-09-21","to":"2026-09-21"}`)
	var response analyticsResponse
	json.Unmarshal(w.Body.Bytes(), &response)
	if w.Code != 200 || response.Complete || response.Totals.NotCancelledRate != nil {
		t.Fatalf("upstream failure became zero-success: %s", w.Body.String())
	}
	f.setTimetableBody(`{"result":{"periods":[]}}`)
	for _, body := range []string{`{"from":"bad","to":"2026-09-21"}`, `{"from":"2020-01-01","to":"2026-09-21"}`, `{"from":"2026-09-22","to":"2026-09-21"}`, `{"from":"2026-09-21","to":"2026-09-21","format":"html"}`} {
		if analyticsReq(t, p, "student", body).Code != 400 {
			t.Fatalf("invalid request accepted %s", body)
		}
	}
	f.mu.Lock()
	f.timetableDelay = 100 * time.Millisecond
	f.mu.Unlock()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- analyticsReq(t, p, "student", `{"from":"2026-09-21","to":"2026-09-21"}`) }()
	deadline := time.Now().Add(2 * time.Second)
	for len(f.Forwards()) < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	st.SetFeatureEnabled("", "analytics", false)
	if result := <-done; result.Code != 403 {
		t.Fatalf("revoked report returned: %d", result.Code)
	}
}
func TestAnalyticsCSVFormulaAndPreciseIDs(t *testing.T) {
	p, _, _ := analyticsFixture(t)
	raw := []byte(`{"result":{"periods":[{"id":9007199254740993,"date":20260921,"teachers":[{"id":9007199254740993,"name":"=bad"}]}]}}`)
	lessons, err := p.parseAnalyticsLessons("testschool", raw, "2026-09-21", "2026-09-21")
	if err != nil || lessons[0].Teachers[0].ID != "9007199254740993" {
		t.Fatal("large ID corrupted")
	}
	if analyticsCSVSafe("=bad") != "'=bad" {
		t.Fatal("CSV formula not escaped")
	}
}

func TestAnalyticsUnavailableWindowReasons(t *testing.T) {
	for _, tt := range []struct {
		name, reply, reason string
		clearCredential     bool
	}{
		{"no saved key", "", "source_auth_unavailable", true},
		{"upstream error", `{"error":{"code":-8500,"message":"private upstream information"}}`, "upstream_rejected", false},
		{"missing periods", `{"result":{}}`, "invalid_timetable_response", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p, st, f := analyticsFixture(t)
			if tt.clearCredential {
				u, err := st.GetUser("student")
				if err != nil || u == nil {
					t.Fatalf("load test student: %v", err)
				}
				u.Password = ""
				if err = st.UpsertUser(u); err != nil {
					t.Fatal(err)
				}
			} else {
				f.setTimetableBody(tt.reply)
			}
			w := analyticsReq(t, p, "student", `{"from":"2026-09-21","to":"2026-09-21"}`)
			var out analyticsResponse
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Complete ||
				len(out.Windows) != 1 || out.Windows[0].Status != "unavailable" || out.Windows[0].Reason != tt.reason ||
				strings.Contains(w.Body.String(), "private upstream information") {
				t.Fatalf("want safe unavailable reason %s, got HTTP %d: %s", tt.reason, w.Code, w.Body.String())
			}
			if tt.clearCredential && f.TimetableCalls() != 0 {
				t.Fatal("called upstream timetable without a saved key")
			}
		})
	}
}

func TestAnalyticsReadsUpstreamTimetableShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"nested timetable with teacher elements", `{"result":{"timetable":{"periods":[
			{"id":1,"startDateTime":"2026-09-21T08:00:00","endDateTime":"2026-09-21T08:45:00","code":"CANCELLED","elements":[{"id":42,"type":"TEACHER"},{"id":18,"type":"ROOM"}]}
		]}}}`},
		{"mobile ttData with te", `{"result":{"ttData":[
			{"id":1,"date":20260921,"startTime":800,"endTime":845,"code":"cancelled","te":[{"id":42,"longname":"Example Teacher"}]}
		],"masterDataTimestamp":0,"timetableTimestamp":0}}`},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			p, _, f := analyticsFixture(t)
			f.setTimetableBody(tt.body)
			w := analyticsReq(t, p, "student", `{"from":"2026-09-21","to":"2026-09-21","anonymise":true}`)
			var report analyticsResponse
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &report) != nil ||
				!report.Complete || report.UniqueLessons != 1 || report.Totals.Scheduled != 1 ||
				report.Totals.Cancelled != 1 || len(report.Teachers) != 1 {
				t.Fatalf("timetable shape was not counted: HTTP %d %s", w.Code, w.Body.String())
			}
		})
	}
}

// The modern mobile response marks cancellations in is and ties cover
// teachers to scheduled staff through the original element ID.
func TestAnalyticsCountsModernCancellationsAndCover(t *testing.T) {
	raw := `{"result":{"masterData":{"teachers":[{"id":42,"name":"Original Name"},{"id":43,"name":"Cover Name"}],
 "subjects":[{"id":18,"longName":"Maths"}]},"timetable":{"periods":[
 {"id":1,"startDateTime":"2026-09-21T08:00:00","endDateTime":"2026-09-21T08:45:00","is":["CANCELLED","IRREGULAR"],"elements":[{"type":"TEACHER","id":0,"orgId":42},{"type":"SUBJECT","id":18}]},
 {"id":2,"startDateTime":"2026-09-21T09:00:00","endDateTime":"2026-09-21T09:45:00","is":["CANCELLED"],"elements":[{"type":"TEACHER","id":42},{"type":"SUBJECT","id":18}]},
 {"id":3,"startDateTime":"2026-09-21T10:00:00","endDateTime":"2026-09-21T10:45:00","is":["CANCELLED"],"elements":[{"type":"TEACHER","id":42},{"type":"SUBJECT","id":18}]},
 {"id":4,"startDateTime":"2026-09-21T10:15:00","endDateTime":"2026-09-21T11:00:00","is":["IRREGULAR"],"elements":[{"type":"TEACHER","id":43,"orgId":42},{"type":"SUBJECT","id":18}],"teachers":[{"id":43,"name":"Cover Name"}]},
 {"id":5,"startDateTime":"2026-09-21T08:45:00","endDateTime":"2026-09-21T09:00:00","elements":[{"type":"TEACHER","id":42}]}
 ]}}}`
	p, _, f := analyticsFixture(t)
	f.setTimetableBody(raw)
	for _, anonymous := range []bool{false, true} {
		body := fmt.Sprintf(`{"from":"2026-09-21","to":"2026-09-21","anonymise":%t}`, anonymous)
		w := analyticsReq(t, p, "student", body)
		var report analyticsResponse
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &report) != nil || !report.Complete ||
			report.Totals.Cancelled != 3 || report.CancelledLessons != 3 || report.CoveredLessons != 1 || report.ClashingLessons != 1 ||
			len(report.MissedTeachers) != 1 || len(report.MissedSubjects) != 1 || len(report.CoverTeachers) != 1 || len(report.Changes) != 4 {
			t.Fatalf("incorrect lesson-change report: HTTP %d %s", w.Code, w.Body.String())
		}
		if report.MissedTeachers[0].Lessons != 4 || report.MissedSubjects[0].Cancelled != 3 || report.CoverTeachers[0].Lessons != 1 ||
			report.Changes[0].Clash != "no_overlap_observed" || report.Changes[2].Clash != "overlap_observed" ||
			len(report.Changes[3].Covers) != 1 || report.Changes[3].Covers[0].Teacher.Name != report.CoverTeachers[0].Name ||
			report.Changes[3].Covers[0].Original.Name != report.MissedTeachers[0].Name {
			t.Fatalf("incorrect attribution or overlap: %s", w.Body.String())
		}
		if anonymous {
			if strings.Contains(w.Body.String(), "Original Name") || strings.Contains(w.Body.String(), "Cover Name") ||
				strings.Contains(w.Body.String(), "Maths") || report.Changes[3].Covers[0].Teacher.ID != "" ||
				report.Changes[3].Covers[0].Original.ID != "" || report.MissedSubjects[0].ID != "" {
				t.Fatal("anonymous lesson changes leaked names or identifiers")
			}
		} else if report.MissedTeachers[0].Name != "Original Name" || report.MissedSubjects[0].Name != "Maths" ||
			report.CoverTeachers[0].Name != "Cover Name" {
			t.Fatal("master-data names were not resolved")
		}
	}
}

func TestAnalyticsFetchesSingleDayWithNormalMobileWindow(t *testing.T) {
	p, _, f := analyticsFixture(t)
	f.mu.Lock()
	f.rejectSingleDayTimetable = true
	f.timetable = `{"result":{"timetable":{"periods":[
	  {"id":1,"date":20260921,"is":["CANCELLED"],"elements":[{"type":"TEACHER","id":42}]},
	  {"id":2,"date":20260922,"is":["CANCELLED"],"elements":[{"type":"TEACHER","id":43}]}
	]}}}`
	f.mu.Unlock()
	w := analyticsReq(t, p, "student", `{"from":"2026-09-21","to":"2026-09-21"}`)
	var report analyticsResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &report) != nil ||
		!report.Complete || report.CancelledLessons != 1 || report.UniqueLessons != 1 || len(report.Teachers) != 1 ||
		len(report.Changes) != 1 || report.Changes[0].Date != "2026-09-21" ||
		len(report.Windows) != 1 || report.Windows[0].To != "2026-09-21" || report.Windows[0].UpstreamCode != nil {
		t.Fatalf("unexpected single-day report: HTTP %d, %s", w.Code, w.Body.String())
	}
	calls := f.Forwards()
	if len(calls) != 1 || !strings.Contains(string(calls[0].Body), `"startDate":"2026-09-21"`) ||
		!strings.Contains(string(calls[0].Body), `"endDate":"2026-09-27"`) {
		t.Fatal("expected one upstream read using a seven-day window")
	}
}

func TestAnalyticsPersonalWeeklyWindowsPreserveObservedLessons(t *testing.T) {
	p, _, f := analyticsFixture(t)
	f.mu.Lock()
	f.rejectTimetableFrom = "2026-10-19"
	f.rejectSingleDayTimetable = true
	f.timetable = `{"result":{"periods":[
	  {"id":1,"date":20260921,"code":"cancelled","teachers":[{"id":42}]},
	  {"id":2,"date":20260928,"code":"cancelled","teachers":[{"id":42}]},
	  {"id":3,"date":20261026,"code":"cancelled","teachers":[{"id":42}]},
	  {"id":4,"date":20261027,"code":"cancelled","teachers":[{"id":42}]},
	  {"id":5,"date":20261025,"code":"cancelled","teachers":[{"id":42}]}
	]}}`
	f.mu.Unlock()
	w := analyticsReq(t, p, "student", `{"from":"2026-09-21","to":"2026-10-26"}`)
	var report analyticsResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &report) != nil ||
		report.Complete || report.CancelledLessons != 3 || report.UniqueLessons != 3 || len(report.Changes) != 3 ||
		report.Changes[1].Date != "2026-09-28" || report.Changes[2].Date != "2026-10-26" || len(report.Windows) != 6 {
		t.Fatalf("unexpected partial report: HTTP %d, %s", w.Code, w.Body.String())
	}
	const rejected = 4
	if gap := report.Windows[rejected]; gap.Status != "unavailable" || gap.UpstreamCode == nil || *gap.UpstreamCode != -8998 {
		t.Fatalf("expected upstream rejection for the preceding week, got %+v", gap)
	}
	calls := f.Forwards()
	if len(calls) != 6 {
		t.Fatalf("expected six upstream reads, got %d", len(calls))
	}
	wantStarts := []string{"2026-09-21", "2026-09-28", "2026-10-05", "2026-10-12", "2026-10-19", "2026-10-26"}
	wantEnds := []string{"2026-09-27", "2026-10-04", "2026-10-11", "2026-10-18", "2026-10-25", "2026-10-26"}
	for i, call := range calls {
		readStart := wantStarts[i]
		if i == len(calls)-1 {
			readStart = "2026-10-20"
		}
		if !strings.Contains(string(call.Body), `"startDate":"`+readStart+`"`) ||
			!strings.Contains(string(call.Body), `"endDate":"`+wantEnds[i]+`"`) {
			t.Fatalf("upstream read %d should span %s through %s", i, readStart, wantEnds[i])
		}
		window := report.Windows[i]
		if window.From != wantStarts[i] || window.To != wantEnds[i] || (i != rejected && window.Status != "observed") {
			t.Fatalf("unexpected report window %d: %+v", i, window)
		}
	}
}

func TestAnalyticsDoesNotRetryUpstreamAuthRejection(t *testing.T) {
	p, _, f := analyticsFixture(t)
	f.setTimetableBody(`{"error":{"code":-8504,"message":"private sign-in detail"}}`)
	w := analyticsReq(t, p, "student", `{"from":"2026-09-21","to":"2026-09-21"}`)
	var report analyticsResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &report) != nil ||
		report.Complete || len(report.Windows) != 1 || report.Windows[0].UpstreamCode == nil ||
		*report.Windows[0].UpstreamCode != -8504 || f.TimetableCalls() != 1 ||
		strings.Contains(w.Body.String(), "private sign-in detail") {
		t.Fatalf("unexpected rejection: HTTP %d, %s", w.Code, w.Body.String())
	}
}

func TestAnalyticsRoomUsesOnlySignedInTeacher(t *testing.T) {
	p, st, f := analyticsFixture(t)
	seedTeacherSource(t, st, "room-caller", 77)
	if err := st.SetUserCapGrant("testschool", "room-caller", string(access.CapAnalyticsTeacher), 1); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertElementNames("testschool", "ROOM", map[int64]string{169: "14-05"}); err != nil {
		t.Fatal(err)
	}
	body := `{"mode":"room","room":"14-05","from":"2026-09-21","to":"2026-09-28"}`
	if w := analyticsReq(t, p, "student", body); w.Code != http.StatusForbidden || f.TimetableCalls() != 0 {
		t.Fatal("student could borrow a teacher source for room analytics")
	}
	w := analyticsReq(t, p, "room-caller", body)
	var report analyticsResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &report) != nil ||
		report.Source != "room" || report.Room != "14-05" || !report.Complete || report.CancelledLessons != 1 || len(report.Windows) != 2 {
		t.Fatalf("unexpected room analytics: HTTP %d %s", w.Code, w.Body.String())
	}
	calls := f.Forwards()
	if len(calls) != 2 {
		t.Fatalf("expected two weekly room reads, got %d", len(calls))
	}
	for _, call := range calls {
		got := string(call.Body)
		if !strings.Contains(got, `"user":"room-caller"`) || !strings.Contains(got, `"type":"ROOM"`) ||
			!strings.Contains(got, `"id":169`) || strings.Contains(got, `"user":"source"`) {
			t.Fatal("room analytics did not read upstream as the signed-in teacher")
		}
	}
	if err := st.SetUserCapGrant("testschool", "room-caller", string(access.CapTimetableUpstreamView), -1); err != nil {
		t.Fatal(err)
	}
	if w := analyticsReq(t, p, "room-caller", body); w.Code != http.StatusForbidden || f.TimetableCalls() != 2 {
		t.Fatal("room analytics ignored upstream timetable deny")
	}
}

func TestAnalyticsPersonalOTPUsesCallerIdentityWithoutSavedKey(t *testing.T) {
	p, st, f := analyticsFixture(t)
	u, err := st.GetUser("student")
	if err != nil || u == nil {
		t.Fatalf("load student: %v", err)
	}
	u.Password = ""
	if err := st.UpsertUser(u); err != nil {
		t.Fatal(err)
	}

	for _, body := range []string{
		`{"mode":"teacher","teacherId":42,"from":"2026-09-21","to":"2026-09-21","personalOtp":"012345"}`,
		`{"mode":"class-history","from":"2026-09-21","to":"2026-09-21","personalOtp":"012345"}`,
		`{"from":"2026-09-21","to":"2026-09-21","personalOtp":"abc123"}`,
		`{"from":"2026-09-21","to":"2026-10-22","personalOtp":"012345"}`,
	} {
		if w := analyticsReq(t, p, "student", body); w.Code != http.StatusBadRequest || f.TimetableCalls() != 0 {
			t.Fatalf("invalid OTP request reached upstream: HTTP %d, %s", w.Code, w.Body.String())
		}
	}

	w := analyticsReq(t, p, "student", `{"from":"2026-09-21","to":"2026-09-21","personalOtp":"012345"}`)
	var report analyticsResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &report) != nil ||
		!report.Complete || report.CancelledLessons != 1 || strings.Contains(w.Body.String(), "012345") {
		t.Fatalf("unexpected OTP report: HTTP %d, %s", w.Code, w.Body.String())
	}
	calls := f.Forwards()
	var forwarded struct {
		Params []struct {
			ID   int64
			Type string
			Auth struct {
				User string
				OTP  int
			}
		}
	}
	if len(calls) != 1 || json.Unmarshal(calls[0].Body, &forwarded) != nil || len(forwarded.Params) != 1 {
		t.Fatal("expected one upstream timetable read")
	}
	params := forwarded.Params[0]
	if params.ID != 7 || params.Type != "STUDENT" || params.Auth.User != "student" || params.Auth.OTP != 12345 {
		t.Fatal("expected the caller identity and the original six-digit code")
	}
	saved, _, err := st.SchoolCredential("testschool", "student")
	if err != nil || saved != "" {
		t.Fatal("the report stored its one-time code")
	}
	if updated, err := st.GetUser("student"); err != nil || updated == nil || updated.Password != "" {
		t.Fatal("the report modified the legacy replay credential")
	}
}
