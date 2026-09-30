package proxy

// §5 – REST API gate and forward tests (handleREST).
// The REST weekly endpoint serves students their own class via a pooled owner,
// gates teacher/room/subject reconstruction behind recon, and denies write
// verbs unless the session user (or a Bearer token) is authorized. All gates
// must fire before any upstream call.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"untis-proxy/internal/store"
)

func restReq(t *testing.T, p *Proxy, user, method, path string, body string, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	var req *http.Request
	if rd != nil {
		req = httptest.NewRequest(method, path, rd)
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if user != "" {
		sess := p.sessions.New(user, 0)
		req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: sess.ID})
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	p.handleREST(rec, req)
	return rec
}

// TestREST_WeeklyUnauthenticatedForbidden: the weekly timetable needs a
// session; anonymous requests are 403.
func TestREST_WeeklyUnauthenticatedForbidden(t *testing.T) {
	f := &fakeUpstream{}
	p, _ := newFakeProxyUpstream(t, f)
	rec := restReq(t, p, "", http.MethodGet, "/WebUntis/api/public/timetable/weekly/data?elementType=1&elementId=5000&date=2026-09-21", "", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if got := len(f.Rests()); got != 0 {
		t.Errorf("upstream rest calls = %d, want 0", got)
	}
}

// TestREST_WeeklyReconGateForBasicStudent: a plain student may not fetch a
// teacher's weekly timetable unless recon access is granted.
func TestREST_WeeklyReconGateForBasicStudent(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	if err := st.UpsertUser(&store.User{Username: "sam", Method: "key", PersonType: 5, PersonID: 7, ClassID: 5000}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	rec := restReq(t, p, "sam", http.MethodGet, "/WebUntis/api/public/timetable/weekly/data?elementType=2&elementId=5009&date=2026-09-21", "", "")
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (recon gate)", rec.Code)
	}
	if got := len(f.Rests()); got != 0 {
		t.Errorf("upstream rest calls = %d, want 0", got)
	}
}

// TestREST_WeeklyNonPooledClassForbidden: a weekly request for a class that is
// not in the pool must be denied before any upstream session is used.
func TestREST_WeeklyNonPooledClassForbidden(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	if err := st.UpsertUser(&store.User{Username: "sam", Method: "key", PersonType: 5, PersonID: 7, ClassID: 5000}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	rec := restReq(t, p, "sam", http.MethodGet, "/WebUntis/api/public/timetable/weekly/data?elementType=1&elementId=9999&date=2026-09-21", "", "")
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if got := len(f.Rests()); got != 0 {
		t.Errorf("upstream rest calls = %d, want 0", got)
	}
}

// TestREST_WriteNonEditorForbidden: a write verb with a basic session user is
// denied with 403 before reaching upstream.
func TestREST_WriteNonEditorForbidden(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	if err := st.UpsertUser(&store.User{Username: "sam", Method: "key", PersonType: 5, PersonID: 7, ClassID: 5000}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	rec := restReq(t, p, "sam", http.MethodPost, "/WebUntis/api/public/absence/change", `{"id":1}`, "")
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if got := len(f.Rests()); got != 0 {
		t.Errorf("upstream rest calls = %d, want 0", got)
	}
}

// TestREST_BearerWriteForbidden: a Bearer-authenticated write verb has no
// session identity and must be denied.
func TestREST_BearerWriteForbidden(t *testing.T) {
	f := &fakeUpstream{}
	p, _ := newFakeProxyUpstream(t, f)
	rec := restReq(t, p, "", http.MethodPut, "/WebUntis/api/public/absence/change", `{"id":1}`, "tok")
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if got := len(f.Rests()); got != 0 {
		t.Errorf("upstream rest calls = %d, want 0", got)
	}
}

// TestREST_BearerReadForwarded: GETs (own data) are allowed Bearer-only and
// forwarded as-is to the real API with the Authorization header.
func TestREST_BearerReadForwarded(t *testing.T) {
	f := &fakeUpstream{restBody: `{"data":{"result":{"data":{"elementPeriods":{}}}}}`}
	p, _ := newFakeProxyUpstream(t, f)
	rec := restReq(t, p, "", http.MethodGet, "/WebUntis/api/public/absence?userId=7", "", "tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	rests := f.Rests()
	if len(rests) != 1 {
		t.Fatalf("upstream rest calls = %d, want 1", len(rests))
	}
	if got := rests[0].AuthHdr; got != "Bearer tok" {
		t.Errorf("authorization header = %q, want Bearer tok forwarded", got)
	}
	if !strings.EqualFold(rec.Body.String(), f.restBody) {
		t.Errorf("response body = %q, want raw upstream %q", rec.Body.String(), f.restBody)
	}
}

// TestREST_WeeklyPooledClassForwarded: a student fetching a pooled class's
// weekly timetable gets it served through the pool owner's real session and
// the exact upstream body is returned.
func TestREST_WeeklyPooledClassForwarded(t *testing.T) {
	f := &fakeUpstream{restBody: `{"data":{"result":{"data":{"elementPeriods":{"5000":[{}]}}}}}`}
	p, st := newFakeProxyUpstream(t, f)
	if err := st.UpsertUser(&store.User{Username: "owen", Method: "key", PersonType: 5, PersonID: 100, ClassID: 5000, Password: "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"}); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	if err := st.UpsertUser(&store.User{Username: "sam", Method: "key", PersonType: 5, PersonID: 7, ClassID: 5000}); err != nil {
		t.Fatalf("seed student: %v", err)
	}

	rec := restReq(t, p, "sam", http.MethodGet, "/WebUntis/api/public/timetable/weekly/data?elementType=1&elementId=5000&date=2026-09-21", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	rests := f.Rests()
	if len(rests) != 1 {
		t.Fatalf("upstream rest calls = %d, want 1", len(rests))
	}
	if !strings.Contains(rests[0].Path, "elementId=5000") {
		t.Errorf("upstream rest path = %q, want elementId=5000", rests[0].Path)
	}
	var got, want map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	_ = json.Unmarshal([]byte(f.restBody), &want)
	if len(got) != len(want) {
		t.Errorf("response %v ≠ upstream %v", got, want)
	}
}
