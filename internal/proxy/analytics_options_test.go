package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"untis-proxy/internal/access"
	"untis-proxy/internal/store"
)

func TestAnalyticsDiscoveryAcceptsLargeTeacherCatalog(t *testing.T) {
	p, st, upstream := analyticsFixture(t)
	ids := make([]int64, 537)
	for i := range ids {
		ids[i] = int64(10000 + i)
	}
	if err := st.SetAnalyticsAccess("testschool", "student", store.AnalyticsAccess{TeacherIDs: ids}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/analytics/options?school=testschool", nil)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: p.sessions.New("student", 5000).ID})
	w := httptest.NewRecorder()
	p.Handler().ServeHTTP(w, req)
	var options struct{ Targets []analyticsOption }
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &options) != nil {
		t.Fatalf("large catalog unavailable: HTTP %d", w.Code)
	}
	count := 0
	for _, target := range options.Targets {
		if target.Mode == "teacher" {
			count++
		}
	}
	if count != len(ids) || upstream.TimetableCalls() != 0 {
		t.Fatalf("expected %d approved teachers with no timetable reads, got %d", len(ids), count)
	}
}

func TestAnalyticsOptionsAndReportsRecheckTargetScopes(t *testing.T) {
	p, st, upstream := analyticsFixture(t)
	const caller = "report-caller"
	seedSessionUser(t, st, caller, 5, 71, 0)
	seedSessionUser(t, st, "shared-class-owner", 5, 72, 6000)
	if err := st.SetAdmin(caller, true); err != nil {
		t.Fatal(err)
	}
	for _, cap := range []access.Capability{access.CapAnalyticsPersonal, access.CapAnalyticsTeacher,
		access.CapAnalyticsStudent, access.CapAnalyticsClassHistory, access.CapTimetableClassPooledView,
		access.CapTimetableTeacherReconstructedView} {
		if err := st.SetUserCapGrant("testschool", caller, string(cap), 1); err != nil {
			t.Fatal(err)
		}
	}
	scope := store.AnalyticsAccess{TeacherIDs: []int64{42}, RoomIDs: []int64{161}, StudentIDs: []int64{73}, ClassIDs: []int64{6000}}
	if err := st.SetAnalyticsAccess("testschool", caller, scope); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAnalyticsYears("testschool", []store.AnalyticsYear{{Name: "2026/2027", From: "2026-08-01", To: "2027-07-31"}}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/analytics/options?school=testschool", nil)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: p.sessions.New(caller, 0).ID})
	list := httptest.NewRecorder()
	p.Handler().ServeHTTP(list, req)
	var options struct {
		Targets     []analyticsOption
		SchoolYears []store.AnalyticsYear
	}
	if list.Code != http.StatusOK || json.Unmarshal(list.Body.Bytes(), &options) != nil ||
		len(options.SchoolYears) != 1 || len(options.Targets) != 5 || upstream.TimetableCalls() != 0 {
		t.Fatalf("unexpected target options: HTTP %d %s", list.Code, list.Body.String())
	}
	for _, test := range []struct{ body, source, forwarded string }{
		{`{"mode":"student","studentId":73,"schoolYear":"2026/2027","from":"2026-09-21","to":"2026-09-27"}`, "saved-teacher", `"type":"STUDENT"`},
		{`{"mode":"class","classId":6000,"dataSource":"shared-class","from":"2026-09-21","to":"2026-09-27"}`, "shared-class", `"id":6000`},
		{`{"mode":"teacher","teacherId":42,"dataSource":"reconstruction","from":"2026-09-21","to":"2026-09-27"}`, "reconstruction", `"type":"CLASS"`},
		{`{"mode":"room","room":"161","from":"2026-09-21","to":"2026-09-27"}`, "saved-teacher", `"type":"ROOM"`},
	} {
		before := len(upstream.Forwards())
		report := analyticsReq(t, p, caller, test.body)
		calls := upstream.Forwards()[before:]
		if report.Code != http.StatusOK || !strings.Contains(report.Body.String(), `"dataSource":"`+test.source+`"`) || len(calls) == 0 ||
			!strings.Contains(string(calls[0].Body), test.forwarded) {
			t.Fatalf("wrong source for %s: HTTP %d %s", test.body, report.Code, report.Body.String())
		}
	}
	scope.StudentIDs = nil
	if err := st.SetAnalyticsAccess("testschool", caller, scope); err != nil {
		t.Fatal(err)
	}
	before := upstream.TimetableCalls()
	for _, body := range []string{
		`{"mode":"student","studentId":73,"from":"2026-09-21","to":"2026-09-27"}`,
		`{"mode":"class","classId":6001,"from":"2026-09-21","to":"2026-09-27"}`,
	} {
		if w := analyticsReq(t, p, caller, body); w.Code != http.StatusForbidden || upstream.TimetableCalls() != before {
			t.Fatalf("unapproved selection reached upstream: HTTP %d %s", w.Code, w.Body.String())
		}
	}
}
