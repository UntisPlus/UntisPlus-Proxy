package proxy

// §3/§4 – getTimetable2017 and info-center gate tests.
// The proxy self-authenticates requests via the auth block, so most perm
// checks (write editor flag, pool membership, recon access) must run before
// any upstream call. These tests assert the JSON-RPC error codes and — for the
// passing cases — that the request actually reached the fake upstream with the
// right cookie and (where applicable) a rewritten owner auth block.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"untis-proxy/internal/store"
	"untis-proxy/internal/untis"
)

func internBody(method string, params ...map[string]any) string {
	ps := make([]any, 0, len(params))
	for _, pr := range params {
		ps = append(ps, pr)
	}
	b, _ := json.Marshal(map[string]any{"id": "t", "jsonrpc": "2.0", "method": method, "params": ps})
	return string(b)
}

func internReq(t *testing.T, p *Proxy, queryM, user, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/WebUntis/jsonrpc_intern.do?m="+queryM+"&school=testschool", strings.NewReader(body))
	if user != "" {
		sess := p.sessions.New(user, 0)
		req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: sess.ID})
	}
	rec := httptest.NewRecorder()
	p.handleJSONRPCIntern(rec, req)
	return rec
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) int {
	t.Helper()
	var resp struct {
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json-rpc error body: %v (body=%s)", err, rec.Body.String())
	}
	return resp.Error.Code
}

func authMap(user string) map[string]any {
	return map[string]any{"user": user, "otp": "123456"}
}

// ttParam builds one getTimetable2017 param object; auth is merged into the
// same object (as the real app sends it), not appended as a second param.
func ttParam(id int64, typ, start, end string, authUser string) map[string]any {
	m := map[string]any{"id": id, "type": typ, "startDate": start, "endDate": end}
	if authUser != "" {
		m["auth"] = authMap(authUser)
	}
	return m
}

// TestIntern_MissingParamsIs8502: a getTimetable2017 request without any
// params must be rejected with -8502, never a panic or 5xx.
func TestIntern_MissingParamsIs8502(t *testing.T) {
	p, _ := newFakeProxyUpstream(t, &fakeUpstream{})
	rec := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 error carrier (body %s)", rec.Code, rec.Body.String())
	}
	if code := errCode(t, rec); code != -8502 {
		t.Errorf("error code = %d, want -8502", code)
	}
}

// TestIntern_NoIdentityIsNotLoggedIn: no auth block and no session cookie must
// produce -8520.
func TestIntern_NoIdentityIsNotLoggedIn(t *testing.T) {
	p, _ := newFakeProxyUpstream(t, &fakeUpstream{})
	rec := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		map[string]any{"id": 5000, "type": "CLASS", "startDate": "2026-09-21", "endDate": "2026-09-27"}))
	if code := errCode(t, rec); code != -8520 {
		t.Errorf("error code = %d, want -8520", code)
	}
}

// TestIntern_UnknownAuthUserIsNotLoggedIn: an auth block naming a user that
// has no local account must be rejected the same way.
func TestIntern_UnknownAuthUserIsNotLoggedIn(t *testing.T) {
	p, _ := newFakeProxyUpstream(t, &fakeUpstream{})
	rec := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(5000, "CLASS", "2026-09-21", "2026-09-27", "ghost")))
	if code := errCode(t, rec); code != -8520 {
		t.Errorf("error code = %d, want -8520", code)
	}
}

// TestIntern_OwnClassForwardedToUpstream: a student asking for their OWN class
// is forwarded raw (upstream validates), and the target-school schoolname
// cookie must ride along (it used to be dropped → -8500 invalid schoolname).
func TestIntern_OwnClassForwardedToUpstream(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", PersonType: 5, PersonID: 7, ClassID: 5000}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	rec := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(5000, "CLASS", "2026-09-21", "2026-09-27", "dee")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	forwards := f.Forwards()
	if len(forwards) != 1 {
		t.Fatalf("forwards = %d, want 1", len(forwards))
	}
	if fw := forwards[0]; !strings.Contains(fw.Cookie, "schoolname=") {
		t.Errorf("forward cookie = %q, want schoolname cookie", fw.Cookie)
	}
	var sent struct {
		Params []struct {
			Auth struct {
				User string `json:"user"`
			} `json:"auth"`
		} `json:"params"`
	}
	if err := json.Unmarshal(forwards[0].Body, &sent); err != nil {
		t.Fatalf("bad forwarded body: %v", err)
	}
	if len(sent.Params) != 1 || sent.Params[0].Auth.User != "dee" {
		t.Errorf("forwarded auth user = %+v, want dee (echoed verbatim)", sent.Params)
	}
}

// TestIntern_ForeignClassNotPooledIs8509: a student asking about a class that
// is neither their own nor pooled must get -8509 before any upstream call.
func TestIntern_ForeignClassNotPooledIs8509(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", PersonType: 5, PersonID: 7, ClassID: 5000}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	rec := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(9999, "CLASS", "2026-09-21", "2026-09-27", "dee")))
	if code := errCode(t, rec); code != -8509 {
		t.Errorf("error code = %d, want -8509", code)
	}
	if got := len(f.Forwards()); got != 0 {
		t.Errorf("forwards = %d, want 0 (gated before upstream)", got)
	}
}

// TestIntern_PooledClassRewrittenToOwner: a student asking about a pooled
// class they do not attend must be answered via the class owner — the auth
// block is rewritten to the owner's replayable account before forwarding.
func TestIntern_PooledClassRewrittenToOwner(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	secret := "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
	if err := st.UpsertUser(&store.User{Username: "owen", Method: "key", PersonType: 5, PersonID: 100, ClassID: 5000, Password: secret}); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	// The requester must be in a DIFFERENT class: a student asking about their
	// own class is forwarded untouched (upstream validates that auth block
	// itself), and the owner rewrite under test only happens for a pooled class
	// they do not attend.
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", PersonType: 5, PersonID: 7, ClassID: 6001}); err != nil {
		t.Fatalf("seed requester: %v", err)
	}

	rec := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(5000, "CLASS", "2026-09-21", "2026-09-27", "dee")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	forwards := f.Forwards()
	if len(forwards) != 1 {
		t.Fatalf("forwards = %d, want 1", len(forwards))
	}
	if fw := forwards[0]; !strings.Contains(fw.Cookie, "schoolname=") {
		t.Errorf("forward cookie = %q, want schoolname cookie", fw.Cookie)
	}
	var sent struct {
		Params []struct {
			Auth struct {
				User string `json:"user"`
				Otp  string `json:"otp"`
			} `json:"auth"`
		} `json:"params"`
	}
	if err := json.Unmarshal(forwards[0].Body, &sent); err != nil {
		t.Fatalf("bad forwarded body: %v", err)
	}
	if got := sent.Params[0].Auth.User; got != "owen" {
		t.Errorf("forwarded auth user = %q, want owner owen", got)
	}
	if got := sent.Params[0].Auth.Otp; got != untis.TOTP(secret) {
		t.Errorf("forwarded otp = %q, want derived TOTP of owner secret", got)
	}
}

// TestIntern_InfoCenterWriteGatedForBasic: a self-auth write method from a
// non-editor user must be rejected with -32601, never touching upstream.
func TestIntern_InfoCenterWriteGatedForBasic(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", PersonType: 5, PersonID: 7, ClassID: 5000}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	rec := internReq(t, p, "setAbsence", "", internBody("setAbsence",
		map[string]any{"userId": 7, "auth": authMap("dee")}))
	if code := errCode(t, rec); code != -32601 {
		t.Errorf("error code = %d, want -32601 (method not allowed)", code)
	}
	if got := len(f.Forwards()); got != 0 {
		t.Errorf("forwards = %d, want 0 (gated before upstream)", got)
	}
}

// TestIntern_InfoCenterEditorWriteForwarded: with the editor flag the same
// write method goes through to upstream (and carries the schoolname cookie).
func TestIntern_InfoCenterEditorWriteForwarded(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", PersonType: 5, PersonID: 7, ClassID: 5000}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := st.SetPerm("dee", store.FeatureEditor, true); err != nil {
		t.Fatalf("grant editor: %v", err)
	}

	rec := internReq(t, p, "setAbsence", "", internBody("setAbsence",
		map[string]any{"userId": 7, "auth": authMap("dee")}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	forwards := f.Forwards()
	if len(forwards) != 1 {
		t.Fatalf("forwards = %d, want 1", len(forwards))
	}
	if fw := forwards[0]; !strings.Contains(fw.Cookie, "schoolname=") {
		t.Errorf("forward cookie = %q, want schoolname cookie", fw.Cookie)
	}
}

// TestIntern_InfoCenterReadForwardedForBasic: reading your own absences is
// allowed even for a basic user — the request is forwarded with the cookie.
func TestIntern_InfoCenterReadForwardedForBasic(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", PersonType: 5, PersonID: 7, ClassID: 5000}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	rec := internReq(t, p, "getAbsence", "", internBody("getAbsence",
		map[string]any{"userId": 7, "auth": authMap("dee")}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	forwards := f.Forwards()
	if len(forwards) != 1 {
		t.Fatalf("forwards = %d, want 1", len(forwards))
	}
	if fw := forwards[0]; !strings.Contains(fw.Cookie, "schoolname=") {
		t.Errorf("forward cookie = %q, want schoolname cookie", fw.Cookie)
	}
}

// TestIntern_InfoCenterNoSessionNoAuthIs8520: no auth block and no session on
// an info-center method must be rejected with -8520.
func TestIntern_InfoCenterNoSessionNoAuthIs8520(t *testing.T) {
	p, _ := newFakeProxyUpstream(t, &fakeUpstream{})
	rec := internReq(t, p, "getAbsence", "", internBody("getAbsence"))
	if code := errCode(t, rec); code != -8520 {
		t.Errorf("error code = %d, want -8520", code)
	}
}

// TestIntern_ClassScopedReadRunsAsTeacher: the absence editor's data source
// (getPeriodData2017) only answers with roster, class register and absences for
// a teacher identity, so an editor's request has to be replayed as the boosted
// teacher. Without it the app gets an empty student list and a blank screen.
func TestIntern_ClassScopedReadRunsAsTeacher(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	secret := "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
	if err := st.UpsertUser(&store.User{Username: "owen", Method: "key", PersonType: 2, Password: secret}); err != nil {
		t.Fatalf("seed teacher: %v", err)
	}
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", PersonType: 5, PersonID: 7, ClassID: 5000}); err != nil {
		t.Fatalf("seed requester: %v", err)
	}
	if err := st.SetPerm("dee", store.FeatureEditor, true); err != nil {
		t.Fatalf("grant editor: %v", err)
	}

	rec := internReq(t, p, "getPeriodData2017", "", internBody("getPeriodData2017",
		map[string]any{"ttIds": []int{6883118}, "auth": authMap("dee")}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	forwards := f.Forwards()
	if len(forwards) != 1 {
		t.Fatalf("forwards = %d, want 1", len(forwards))
	}
	var sent struct {
		Params []struct {
			Auth struct {
				User string `json:"user"`
				Otp  string `json:"otp"`
			} `json:"auth"`
		} `json:"params"`
	}
	if err := json.Unmarshal(forwards[0].Body, &sent); err != nil {
		t.Fatalf("bad forwarded body: %v", err)
	}
	if got := sent.Params[0].Auth.User; got != "owen" {
		t.Errorf("forwarded auth user = %q, want teacher owen", got)
	}
	if got := sent.Params[0].Auth.Otp; got != untis.TOTP(secret) {
		t.Errorf("forwarded otp = %q, want derived TOTP of teacher secret", got)
	}
}

// TestIntern_ClassScopedReadStaysPersonalForBasic: without the editor flag the
// same method keeps running as the requester — nobody is lifted into a teacher
// account they were not granted.
func TestIntern_ClassScopedReadStaysPersonalForBasic(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	if err := st.UpsertUser(&store.User{Username: "owen", Method: "key", PersonType: 2, Password: "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"}); err != nil {
		t.Fatalf("seed teacher: %v", err)
	}
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", PersonType: 5, PersonID: 7, ClassID: 5000}); err != nil {
		t.Fatalf("seed requester: %v", err)
	}

	rec := internReq(t, p, "getPeriodData2017", "", internBody("getPeriodData2017",
		map[string]any{"ttIds": []int{6883118}, "auth": authMap("dee")}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	forwards := f.Forwards()
	if len(forwards) != 1 {
		t.Fatalf("forwards = %d, want 1", len(forwards))
	}
	var sent struct {
		Params []struct {
			Auth struct {
				User string `json:"user"`
			} `json:"auth"`
		} `json:"params"`
	}
	if err := json.Unmarshal(forwards[0].Body, &sent); err != nil {
		t.Fatalf("bad forwarded body: %v", err)
	}
	if got := sent.Params[0].Auth.User; got != "dee" {
		t.Errorf("forwarded auth user = %q, want own account dee", got)
	}
}

// TestIntern_OwnDataStaysPersonalForEditor: the routing is scoped to the
// class-scoped editor methods, so an editor's own timetable still runs on his
// own account (this is what the boost already did for pooled classes, and
// personal data must not change with the editor flag).
func TestIntern_OwnDataStaysPersonalForEditor(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	if err := st.UpsertUser(&store.User{Username: "owen", Method: "key", PersonType: 2, Password: "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"}); err != nil {
		t.Fatalf("seed teacher: %v", err)
	}
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", PersonType: 5, PersonID: 7, ClassID: 5000}); err != nil {
		t.Fatalf("seed requester: %v", err)
	}
	if err := st.SetPerm("dee", store.FeatureEditor, true); err != nil {
		t.Fatalf("grant editor: %v", err)
	}

	rec := internReq(t, p, "getAbsence", "", internBody("getAbsence",
		map[string]any{"userId": 7, "auth": authMap("dee")}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	forwards := f.Forwards()
	if len(forwards) != 1 {
		t.Fatalf("forwards = %d, want 1", len(forwards))
	}
	var sent struct {
		Params []struct {
			Auth struct {
				User string `json:"user"`
			} `json:"auth"`
		} `json:"params"`
	}
	if err := json.Unmarshal(forwards[0].Body, &sent); err != nil {
		t.Fatalf("bad forwarded body: %v", err)
	}
	if got := sent.Params[0].Auth.User; got != "dee" {
		t.Errorf("forwarded auth user = %q, want own account dee", got)
	}
}

// TestPublicJSONRPC_OwnDataIs32601: the public jsonrpc.do endpoint deliberately
// does not implement the intern methods — getOwnData must answer -32601 locally.
func TestPublicJSONRPC_OwnDataIs32601(t *testing.T) {
	f := &fakeUpstream{}
	p, _ := newFakeProxyUpstream(t, f)
	req := httptest.NewRequest(http.MethodPost, "/WebUntis/jsonrpc.do", strings.NewReader(internBody("getOwnData")))
	rec := httptest.NewRecorder()
	p.handleJSONRPC(rec, req)
	if code := errCode(t, rec); code != -32601 {
		t.Errorf("error code = %d, want -32601", code)
	}
	if got := len(f.Public()); got != 0 {
		t.Errorf("upstream public calls = %d, want 0", got)
	}
}

// TestPublicJSONRPC_NoSessionIs8520: other methods on the public endpoint need
// a session; without one the proxy answers -8520 instead of forwarding.
func TestPublicJSONRPC_NoSessionIs8520(t *testing.T) {
	f := &fakeUpstream{}
	p, _ := newFakeProxyUpstream(t, f)
	req := httptest.NewRequest(http.MethodPost, "/WebUntis/jsonrpc.do", strings.NewReader(internBody("getAppName")))
	rec := httptest.NewRecorder()
	p.handleJSONRPC(rec, req)
	if code := errCode(t, rec); code != -8520 {
		t.Errorf("error code = %d, want -8520", code)
	}
	if got := len(f.Public()); got != 0 {
		t.Errorf("upstream public calls = %d, want 0", got)
	}
}
