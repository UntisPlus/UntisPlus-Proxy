package proxy

// Public /WebUntis/jsonrpc.do tests for the local authenticate/logout methods:
// the -8502 gate, the full password-login happy path (store upsert, cookies,
// forwarded authenticate call) and upstream-error pass-through.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func publicJSONRPCReq(t *testing.T, p *Proxy, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/WebUntis/jsonrpc.do?school=testschool", strings.NewReader(body))
	rec := httptest.NewRecorder()
	p.handleJSONRPC(rec, req)
	return rec
}

func TestAuthenticateGateEmptyCredentialsIs8502(t *testing.T) {
	f := &fakeUpstream{}
	p, _ := newFakeProxyUpstream(t, f)
	rec := publicJSONRPCReq(t, p, `{"jsonrpc":"2.0","id":"a","method":"authenticate","params":{"user":"","password":""}}`)
	if code := errCode(t, rec); code != -8502 {
		t.Errorf("error code = %d, want -8502", code)
	}
	if got := len(f.Public()); got != 0 {
		t.Errorf("upstream calls = %d, want 0 (gated locally)", got)
	}
}

func TestAuthenticateHappyPath(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	rec := publicJSONRPCReq(t, p, `{"jsonrpc":"2.0","id":"a","method":"authenticate","params":{"user":"owen","password":"pw","client":"test"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	var out struct {
		ID     json.RawMessage `json:"id"`
		Result struct {
			SessionID  string `json:"sessionId"`
			PersonType int64  `json:"personType"`
			PersonID   int64  `json:"personId"`
			KlasseID   int64  `json:"klasseId"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if out.Result.SessionID == "" {
		t.Errorf("sessionId = %q, want minted internal session", out.Result.SessionID)
	}
	if out.Result.PersonType != 5 || out.Result.PersonID != 5005 || out.Result.KlasseID != 5000 {
		t.Errorf("result = %+v, want personType 5 / personId 5005 / klasseId 5000", out.Result)
	}
	if string(out.ID) != `"a"` {
		t.Errorf("echoed id = %s, want a", out.ID)
	}

	// the account must now exist in the store with the password method
	u, err := st.GetUser("owen")
	if err != nil || u == nil {
		t.Fatalf("user owen not persisted: %v", err)
	}
	if u.Method != "password" || u.PersonID != 5005 || u.ClassID != 5000 {
		t.Errorf("persisted user = %+v, want password/5005/5000", u)
	}

	// the real authenticate call must have been forwarded with the credentials
	// (the handler also fires an async upstream logout we don't care about here,
	// so filter for the authenticate forward specifically)
	pubs := f.Public()
	if len(pubs) == 0 {
		t.Fatalf("no public upstream calls forwarded")
	}
	found := false
	for _, pub := range pubs {
		if strings.Contains(string(pub.Body), `"method":"authenticate"`) &&
			strings.Contains(string(pub.Body), `"password":"pw"`) {
			found = true
		}
	}
	if !found {
		t.Errorf("authenticate forward missing/incorrect: bodies = %s", string(pubs[0].Body))
	}

	// internal session cookie set
	var sets []string
	for _, ck := range rec.Result().Header["Set-Cookie"] {
		sets = append(sets, ck)
	}
	if !strings.Contains(strings.Join(sets, "|"), "JSESSIONID") {
		t.Errorf("no JSESSIONID Set-Cookie: %v", sets)
	}
}

func TestAuthenticateUpstreamErrorPassedThroughRaw(t *testing.T) {
	f := &fakeUpstream{authenticateBody: `{"jsonrpc":"2.0","id":"u","error":{"code":-8501,"message":"wrong password"}}`}
	p, _ := newFakeProxyUpstream(t, f)
	rec := publicJSONRPCReq(t, p, `{"jsonrpc":"2.0","id":"a","method":"authenticate","params":{"user":"owen","password":"nope"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with raw error body", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "wrong password") {
		t.Errorf("raw upstream error not mirrored: %s", rec.Body.String())
	}
}

func TestLogoutDeletesSession(t *testing.T) {
	p, _ := newTestProxy(t)
	s := p.sessions.New("dee", 0)
	req := httptest.NewRequest(http.MethodPost, "/WebUntis/jsonrpc.do", strings.NewReader(`{"jsonrpc":"2.0","id":"l","method":"logout","params":{}}`))
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: s.ID})
	rec := httptest.NewRecorder()
	p.handleJSONRPC(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"result":{}`) {
		t.Errorf("logout result = %s, want {} result", rec.Body.String())
	}
	if p.sessions.Get(s.ID) != nil {
		t.Error("session still alive after logout")
	}
}

func TestExtractMethod(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"method":"getTimetable2017"}`, "getTimetable2017"},
		{`{"jsonrpc":"2.0","method":"logout"}`, "logout"},
		{`not-json`, ""},
		{`{}`, ""},
	}
	for _, c := range cases {
		if got := extractMethod([]byte(c.body)); got != c.want {
			t.Errorf("extractMethod(%q) = %q, want %q", c.body, got, c.want)
		}
	}
}
