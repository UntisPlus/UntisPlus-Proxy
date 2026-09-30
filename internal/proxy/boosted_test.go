package proxy

// Boosted raw-forwarding: a boosted user's timetable requests are served from
// the saved non-student (teacher) source accounts, with the auth block rewritten
// to that account. These tests pin the forward itself (cookie + rewritten body)
// and the graceful fallback when no teacher account has been saved yet.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"untis-proxy/internal/store"
	"untis-proxy/internal/untis"
)

const boostSourceSecret = "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"

func seedTeacherSource(t *testing.T, st *store.Store, username string, personID int64) {
	t.Helper()
	if err := st.UpsertUser(&store.User{Username: username, Method: "key", PersonType: 2, PersonID: personID, Password: boostSourceSecret}); err != nil {
		t.Fatalf("seed teacher source: %v", err)
	}
}

func TestBoostedInternServedFromTeacherSource(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	seedTeacherSource(t, st, "mrTeacher", 5009)
	seedSessionUser(t, st, "dee", 5, 7, 5000)
	if err := st.SetBoostedFlag("dee", true); err != nil {
		t.Fatalf("boost dee: %v", err)
	}

	rec := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(5009, "TEACHER", "2026-09-21", "2026-09-27", "dee")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	forwards := f.Forwards()
	if len(forwards) != 1 {
		t.Fatalf("forwards = %d, want 1 (RAW via teacher source)", len(forwards))
	}
	fw := forwards[0]
	// served from the school-level cookie only (no per-user session)
	if !strings.Contains(fw.Cookie, "schoolname=") || strings.Contains(fw.Cookie, "JSESSIONID=") {
		t.Errorf("forward cookie = %q, want schoolname-only cookie", fw.Cookie)
	}
	var sent struct {
		Params []struct {
			Auth struct {
				User string `json:"user"`
				Otp  string `json:"otp"`
			} `json:"auth"`
		} `json:"params"`
	}
	if err := json.Unmarshal(fw.Body, &sent); err != nil {
		t.Fatalf("bad forwarded body: %v", err)
	}
	if sent.Params[0].Auth.User != "mrteacher" {
		t.Errorf("rewritten auth user = %q, want teacher source mrteacher (store lowercases)", sent.Params[0].Auth.User)
	}
	if sent.Params[0].Auth.Otp != untis.TOTP(boostSourceSecret) {
		t.Errorf("rewritten otp = %q, want TOTP of teacher secret", sent.Params[0].Auth.Otp)
	}
}

func TestBoostedInternFallsBackToReconGateWithoutSource(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	// only a student account exists → no boosted source to draw from
	seedSessionUser(t, st, "dee", 5, 7, 5000)
	if err := st.SetBoostedFlag("dee", true); err != nil {
		t.Fatalf("boost dee: %v", err)
	}

	rec := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(5009, "TEACHER", "2026-09-21", "2026-09-27", "dee")))
	if code := errCode(t, rec); code != -8509 {
		t.Errorf("error code = %d, want -8509 (recon not granted)", code)
	}
	if got := len(f.Forwards()); got != 0 {
		t.Errorf("forwards = %d, want 0 (no source account)", got)
	}
}

func TestTeacherAccountPassthroughRawIntern(t *testing.T) {
	// A non-boosted teacher account behaves like stock WebUntis: any timetable
	// (even a STUDENT's) is forwarded raw through their own session.
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	seedTeacherSource(t, st, "mrTeacher", 5009)

	rec := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(5005, "STUDENT", "2026-09-21", "2026-09-27", "mrTeacher")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	forwards := f.Forwards()
	if len(forwards) != 1 {
		t.Fatalf("forwards = %d, want 1", len(forwards))
	}
	if !strings.Contains(forwards[0].Cookie, "JSESSIONID=") {
		t.Errorf("forward cookie = %q, want the teacher's real session", forwards[0].Cookie)
	}
}

func TestBoostedRESTWeeklyServedFromTeacherSource(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	seedTeacherSource(t, st, "mrTeacher", 5009)
	seedSessionUser(t, st, "dee", 5, 7, 5000)
	if err := st.SetBoostedFlag("dee", true); err != nil {
		t.Fatalf("boost dee: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet,
		"/WebUntis/api/public/timetable/weekly/data?elementType=2&elementId=5009&date=2026-09-21&school=testschool", nil)
	sess := p.sessions.New("dee", 0)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: sess.ID})
	rec := httptest.NewRecorder()
	p.handleREST(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	rest := f.Rests()
	if len(rest) != 1 {
		t.Fatalf("rest calls = %d, want 1", len(rest))
	}
	if !strings.Contains(rest[0].Cookie, "JSESSIONID=") {
		t.Errorf("rest forward cookie = %q, want teacher session", rest[0].Cookie)
	}
	if !strings.Contains(rest[0].Path, "weekly/data") {
		t.Errorf("rest forward path = %q, want weekly/data", rest[0].Path)
	}
}

func TestBoostedRESTWeeklyWithoutSourceIs403(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	seedSessionUser(t, st, "dee", 5, 7, 5000)
	if err := st.SetBoostedFlag("dee", true); err != nil {
		t.Fatalf("boost dee: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet,
		"/WebUntis/api/public/timetable/weekly/data?elementType=2&elementId=5009&date=2026-09-21&school=testschool", nil)
	sess := p.sessions.New("dee", 0)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: sess.ID})
	rec := httptest.NewRecorder()
	p.handleREST(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (teacher weekly needs recon)", rec.Code)
	}
}
