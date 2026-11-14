package proxy

// Anonymous sessions are a live upstream read-only login but deliberately not a
// stored proxy user. These tests pin the two halves of that contract: the login
// must not persist a placeholder account or forward a placeholder replay key,
// and the following getTimetable2017 must be forwarded raw instead of failing
// at the user lookup with -8520.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestKeyLoginAnonymousSkipsReplayKey: a stale placeholder secret stored for
// "#anonymous#" must not be sent upstream as an auth key, and the login must not
// create a user row.
func TestKeyLoginAnonymousSkipsReplayKey(t *testing.T) {
	f := &fakeUntis{}
	p, st, _ := newFakeProxy(t, f)
	placeholder := strings.Repeat("A", 24)
	if err := st.UpsertSecret("#anonymous#", placeholder); err != nil {
		t.Fatal(err)
	}
	p.setReplayKey("#anonymous#", placeholder)

	req := httptest.NewRequest(http.MethodPost,
		"/WebUntis/jsonrpc_intern.do?m=getUserData2017&school=testschool",
		strings.NewReader(loginBody(t, "#anonymous#", "0")))
	rec := httptest.NewRecorder()
	p.handleJSONRPCIntern(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous login status = %d, body %s", rec.Code, rec.Body.String())
	}
	f.mu.Lock()
	auth := f.loginAuth
	f.mu.Unlock()
	if got := auth["key"]; got != nil && got != "" {
		t.Errorf("upstream key = %v, want empty (anonymous is not a replayable account)", got)
	}
	if u, _ := st.GetUser("#anonymous#"); u != nil {
		t.Fatalf("anonymous login persisted a user row: %+v", u)
	}
}

// TestAnonymousClassTimetableForwardedRaw is the regression test for the
// reported failure: anonymous login succeeds, but the following class
// getTimetable2017 used to fail with -8520 because keyLogin had not persisted
// "#anonymous#" and the handler required store.GetUser to succeed.
func TestAnonymousClassTimetableForwardedRaw(t *testing.T) {
	f := &fakeUpstream{}
	f.setTimetable(t, []map[string]any{{
		"id":            1,
		"startDateTime": "2026-11-09T08:00:00Z",
		"endDateTime":   "2026-11-09T08:45:00Z",
	}})
	p, st := newFakeProxyUpstream(t, f)

	login := httptest.NewRequest(http.MethodPost,
		"/WebUntis/jsonrpc_intern.do?m=getUserData2017&school=testschool",
		strings.NewReader(loginBody(t, "#anonymous#", "0")))
	loginRec := httptest.NewRecorder()
	p.handleJSONRPCIntern(loginRec, login)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("anonymous login status = %d, body %s", loginRec.Code, loginRec.Body.String())
	}
	sid := findCookie(loginRec, "JSESSIONID")
	if sid == "" {
		t.Fatal("anonymous login did not set a proxy session cookie")
	}

	param := ttParam(5000, "CLASS", "2026-11-09", "2026-11-15", "#anonymous#")
	rec := internReq(t, p, "getTimetable2017", "",
		internBody("getTimetable2017", param))
	if rec.Code != http.StatusOK {
		t.Fatalf("timetable status = %d, body %s", rec.Code, rec.Body.String())
	}
	if code := errCode(t, rec); code != 0 {
		t.Fatalf("timetable returned error code %d, body %s", code, rec.Body.String())
	}
	if got := len(cachedPeriods(t, rec)); got != 1 {
		t.Fatalf("periods = %d, want 1 (body %s)", got, rec.Body.String())
	}

	// A cookie-only client (no auth block) must work too: the proxy session
	// still names #anonymous#, even though there is no user row to look up.
	cookieReq := httptest.NewRequest(http.MethodPost,
		"/WebUntis/jsonrpc_intern.do?m=getTimetable2017&school=testschool",
		strings.NewReader(internBody("getTimetable2017",
			map[string]any{"id": 5000, "type": "CLASS", "startDate": "2026-11-09", "endDate": "2026-11-15"})))
	cookieReq.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: sid})
	cookieRec := httptest.NewRecorder()
	p.handleJSONRPCIntern(cookieRec, cookieReq)
	if code := errCode(t, cookieRec); code != 0 {
		t.Fatalf("cookie-only timetable returned error code %d, body %s", code, cookieRec.Body.String())
	}

	forwards := f.Forwards()
	if len(forwards) != 2 {
		t.Fatalf("upstream forwards = %d, want 2 (body %s)", len(forwards), rec.Body.String())
	}
	var sent struct {
		Params []struct {
			Auth struct {
				User string `json:"user"`
			} `json:"auth"`
		} `json:"params"`
	}
	if err := json.Unmarshal(forwards[0].Body, &sent); err != nil {
		t.Fatalf("decode forwarded body: %v", err)
	}
	if len(sent.Params) != 1 || sent.Params[0].Auth.User != "#anonymous#" {
		t.Fatalf("forwarded auth user = %+v, want #anonymous#", sent.Params)
	}
	if u, _ := st.GetUser("#anonymous#"); u != nil {
		t.Fatalf("timetable persisted an anonymous user row: %+v", u)
	}
}
