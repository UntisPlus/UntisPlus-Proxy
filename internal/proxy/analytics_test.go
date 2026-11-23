package proxy

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"untis-proxy/internal/access"
	"untis-proxy/internal/store"
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
			if len(z.File) != 5 {
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
