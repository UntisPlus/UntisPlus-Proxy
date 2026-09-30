package untis

// Direct unit tests for the upstream client: session caching, key/password
// login, logout eviction, TOTP and the JSON-RPC forward. Each test runs
// against a throwaway TLS server that records exactly what the client sent, so
// a break in login wiring shows up as a wrong cookie/bodies assertion rather
// than a network error or a live-API flake.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// fakeLoginServer stands in for the WebUntis backend.
type fakeLoginServer struct {
	mu sync.Mutex

	keyLoginCalls int
	authCalls     int
	logoutCalls   int
	jsonrpcCalls  int

	lastKeyBody  []byte
	lastAuthBody []byte
	lastCookie   string

	// keyLoginBody overrides the getUserData2017 response (error bodies let
	// tests hit the failure paths). keyLoginNoCookie drops the Set-Cookie.
	keyLoginBody     string
	keyLoginNoCookie bool
	// authBody overrides the authenticate response. empty = success.
	authBody string
	// jsonrpcStatus/Body cover the plain JSONRPC() forward path.
	jsonrpcStatus int
	jsonrpcBody   string
}

func (f *fakeLoginServer) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	cookie := r.Header.Get("Cookie")

	switch {
	case strings.HasPrefix(r.URL.Path, "/WebUntis/jsonrpc_intern.do"):
		var req struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		f.mu.Lock()
		f.keyLoginCalls++
		f.lastKeyBody = body
		f.lastCookie = cookie
		b := f.keyLoginBody
		noCookie := f.keyLoginNoCookie
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !noCookie {
			w.Header().Set("Set-Cookie", "JSESSIONID=kidsid"+strings.Repeat("x", f.keyLoginCalls%3)+"; Path=/")
		}
		if b == "" {
			b = `{"jsonrpc":"2.0","id":"upstream","result":{}}`
		}
		_, _ = w.Write([]byte(b))
	case strings.HasPrefix(r.URL.Path, "/WebUntis/jsonrpc.do"):
		var req struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		status := http.StatusOK
		b := ""
		f.mu.Lock()
		f.lastCookie = cookie
		switch req.Method {
		case "authenticate":
			f.authCalls++
			f.lastAuthBody = body
			if f.authBody != "" {
				b = f.authBody
			} else {
				b = `{"jsonrpc":"2.0","id":"upstream","result":{"sessionId":"authsid","personType":5,"personId":5005,"klasseId":5000}}`
			}
		case "logout":
			f.logoutCalls++
			b = `{"jsonrpc":"2.0","id":"upstream","result":{}}`
		default:
			f.jsonrpcCalls++
			status = f.jsonrpcStatus
			b = f.jsonrpcBody
		}
		f.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		if b == "" {
			b = `{"jsonrpc":"2.0","id":"upstream","result":{}}`
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(b))
	default:
		http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
	}
}

func (f *fakeLoginServer) counts() (keyLogin, auth, logout, jsonrpc int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.keyLoginCalls, f.authCalls, f.logoutCalls, f.jsonrpcCalls
}

func (f *fakeLoginServer) last() (cookie string, keyBody, authBody []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastCookie, f.lastKeyBody, f.lastAuthBody
}

func newFakeLogin(t *testing.T, f *fakeLoginServer) *Client {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "https://")
	return New(Config{Server: host, School: "testschool", HTTPClient: srv.Client()})
}

func mustUpstreamError(t *testing.T, err error) *UpstreamError {
	t.Helper()
	if err == nil {
		t.Fatal("expected UpstreamError, got nil")
	}
	ue, ok := err.(*UpstreamError)
	if !ok {
		t.Fatalf("err = %T %v, want *UpstreamError", err, err)
	}
	return ue
}

func TestTOTPFormatAndValidity(t *testing.T) {
	secret := "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
	a := TOTP(secret)
	b := TOTP(secret)
	if a != b {
		t.Errorf("TOTP not stable within a time step: %q vs %q", a, b)
	}
	if len(a) != 6 {
		t.Errorf("TOTP length = %d, want 6", len(a))
	}
	for _, r := range a {
		if r < '0' || r > '9' {
			t.Errorf("TOTP = %q, wants digits only", a)
			break
		}
	}
	if got := TOTP("not-a-base32-secret!"); got != "000000" {
		t.Errorf("invalid secret TOTP = %q, want 000000", got)
	}
}

func TestKeyLoginBuildsCookieWithSchoolname(t *testing.T) {
	f := &fakeLoginServer{}
	c := newFakeLogin(t, f)
	cookie, body, err := c.KeyLogin("testschool", "dee", "123456", 1700000000000, nil)
	if err != nil {
		t.Fatalf("KeyLogin: %v", err)
	}
	if !strings.HasPrefix(cookie, "JSESSIONID=kidsid") {
		t.Errorf("cookie = %q, want JSESSIONID from Set-Cookie header", cookie)
	}
	if !strings.Contains(cookie, "schoolname=_dGVzdHNjaG9vbA==") {
		t.Errorf("cookie = %q, want schoolname=_dGVzdHNjaG9vbA==", cookie)
	}
	if len(body) == 0 {
		t.Fatal("KeyLogin returned empty body")
	}
	_, keyBody, _ := f.last()
	var sent struct {
		Method string `json:"method"`
		Params []struct {
			Auth struct {
				User       string `json:"user"`
				Otp        string `json:"otp"`
				ClientTime int64  `json:"clientTime"`
			} `json:"auth"`
		} `json:"params"`
	}
	if err := json.Unmarshal(keyBody, &sent); err != nil {
		t.Fatalf("bad login body: %v", err)
	}
	if sent.Method != "getUserData2017" {
		t.Errorf("method = %q, want getUserData2017", sent.Method)
	}
	auth := sent.Params[0].Auth
	if auth.User != "dee" || auth.Otp != "123456" || auth.ClientTime == 0 {
		t.Errorf("auth sent = %+v, want dee/123456/epoch-ms", auth)
	}
}

func TestKeyLoginForwardsExtraAuthFields(t *testing.T) {
	f := &fakeLoginServer{}
	c := newFakeLogin(t, f)
	_, _, err := c.KeyLogin("testschool", "dee", "123456", 0, map[string]any{"key": "K-XYZ"})
	if err != nil {
		t.Fatalf("KeyLogin: %v", err)
	}
	_, keyBody, _ := f.last()
	if !strings.Contains(string(keyBody), "K-XYZ") {
		t.Errorf("extraAuth field not forwarded: %s", keyBody)
	}
}

func TestKeyLoginUpstreamErrorCarriesRaw(t *testing.T) {
	f := &fakeLoginServer{keyLoginBody: `{"jsonrpc":"2.0","id":"u","error":{"code":-8506,"message":"banned"}}`}
	c := newFakeLogin(t, f)
	_, _, err := c.KeyLogin("testschool", "dee", "123456", 0, nil)
	ue := mustUpstreamError(t, err)
	if ue.Code != -8506 || !strings.Contains(string(ue.Raw), "banned") {
		t.Errorf("upstream error = %+v, want -8506 with raw body", ue)
	}
}

func TestKeyLoginNoCookieMeansBadCredentials(t *testing.T) {
	f := &fakeLoginServer{keyLoginNoCookie: true}
	c := newFakeLogin(t, f)
	_, _, err := c.KeyLogin("testschool", "dee", "123456", 0, nil)
	ue := mustUpstreamError(t, err)
	if ue.Code != -8504 {
		t.Errorf("error code = %d, want -8504 bad credentials", ue.Code)
	}
}

func TestPasswordLoginBuildsSessionCookie(t *testing.T) {
	f := &fakeLoginServer{}
	c := newFakeLogin(t, f)
	cookie, res, err := c.PasswordLogin("testschool", "dee", "pw", "untis-proxy")
	if err != nil {
		t.Fatalf("PasswordLogin: %v", err)
	}
	if !strings.HasPrefix(cookie, "JSESSIONID=authsid") {
		t.Errorf("cookie = %q, want JSESSIONID=authsid", cookie)
	}
	if !strings.Contains(cookie, "schoolname=") {
		t.Errorf("cookie = %q, want schoolname suffix", cookie)
	}
	if res["personType"].(float64) != 5 {
		t.Errorf("auth result = %v, want personType 5", res)
	}
	_, _, authBody := f.last()
	var sent struct {
		Method string `json:"method"`
		Params struct {
			User     string `json:"user"`
			Password string `json:"password"`
			Client   string `json:"client"`
		} `json:"params"`
	}
	if err := json.Unmarshal(authBody, &sent); err != nil {
		t.Fatalf("bad authenticate body: %v", err)
	}
	if sent.Params.User != "dee" || sent.Params.Password != "pw" {
		t.Errorf("authenticate params = %+v", sent.Params)
	}
}

func TestPasswordLoginErrorCarriesRaw(t *testing.T) {
	f := &fakeLoginServer{authBody: `{"jsonrpc":"2.0","id":"u","error":{"code":-8501,"message":"wrong password"}}`}
	c := newFakeLogin(t, f)
	_, _, err := c.PasswordLogin("testschool", "dee", "nope", "untis-proxy")
	ue := mustUpstreamError(t, err)
	if ue.Code != -8501 {
		t.Errorf("error code = %d, want -8501", ue.Code)
	}
}

func TestPasswordLoginMissingSessionIdIsBadCredentials(t *testing.T) {
	f := &fakeLoginServer{authBody: `{"jsonrpc":"2.0","id":"u","result":{"personType":5}}`}
	c := newFakeLogin(t, f)
	_, _, err := c.PasswordLogin("testschool", "dee", "pw", "untis-proxy")
	ue := mustUpstreamError(t, err)
	if ue.Code != -8504 {
		t.Errorf("error code = %d, want -8504", ue.Code)
	}
}

func TestSessionCachesAndDivergesByMethod(t *testing.T) {
	f := &fakeLoginServer{}
	c := newFakeLogin(t, f)
	secret := "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"

	key1, err := c.Session("testschool", "dee", secret, "key")
	if err != nil {
		t.Fatalf("Session(key) #1: %v", err)
	}
	key2, err := c.Session("testschool", "dee", secret, "key")
	if err != nil {
		t.Fatalf("Session(key) #2: %v", err)
	}
	if key1 != key2 {
		t.Errorf("cached Session returned different cookies %q vs %q", key1, key2)
	}

	// password for the same user must not share the key session cache entry
	pw1, err := c.Session("testschool", "dee", "pw", "password")
	if err != nil {
		t.Fatalf("Session(password): %v", err)
	}
	kl, auth, _, _ := f.counts()
	if kl != 1 {
		t.Errorf("key logins = %d, want 1 (second call cached)", kl)
	}
	if auth != 1 {
		t.Errorf("password logins = %d, want 1", auth)
	}
	if strings.Contains(pw1, "authsid") == false || strings.HasPrefix(pw1, "JSESSIONID=") == false {
		t.Errorf("password cookie = %q, want session cookie", pw1)
	}
}

func TestSessionKeyWithoutSecretFails(t *testing.T) {
	f := &fakeLoginServer{}
	c := newFakeLogin(t, f)
	if _, err := c.Session("testschool", "dee", "", "key"); err == nil {
		t.Fatal("expected error for empty key secret")
	}
	if kl, _, _, _ := f.counts(); kl != 0 {
		t.Errorf("key logins = %d, want 0 (failed before upstream)", kl)
	}
}

func TestFreshSessionBreaksCache(t *testing.T) {
	f := &fakeLoginServer{}
	c := newFakeLogin(t, f)
	secret := "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
	if _, err := c.Session("testschool", "dee", secret, "key"); err != nil {
		t.Fatalf("Session: %v", err)
	}
	if _, err := c.FreshSession("testschool", "dee", secret, "key"); err != nil {
		t.Fatalf("FreshSession: %v", err)
	}
	if _, err := c.Session("testschool", "dee", secret, "key"); err != nil {
		t.Fatalf("Session after FreshSession: %v", err)
	}
	if kl, _, _, _ := f.counts(); kl != 2 {
		t.Errorf("key logins = %d, want 2 (FreshSession forces re-login, then cache holds)", kl)
	}
}

func TestLogoutPostsAndEvictsCachedSession(t *testing.T) {
	f := &fakeLoginServer{}
	c := newFakeLogin(t, f)
	secret := "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
	cookie, err := c.Session("testschool", "dee", secret, "key")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	c.Logout("testschool", cookie)
	_, _, logout, _ := f.counts()
	if logout != 1 {
		t.Errorf("logout calls = %d, want 1", logout)
	}
	// the eviction must force a brand-new login rather than reusing the cache
	if _, err := c.Session("testschool", "dee", secret, "key"); err != nil {
		t.Fatalf("Session after Logout: %v", err)
	}
	if kl, _, _, _ := f.counts(); kl != 2 {
		t.Errorf("key logins = %d, want 2 after logout eviction", kl)
	}
}

func TestJSONRPCForwardsBodyAndCookie(t *testing.T) {
	f := &fakeLoginServer{}
	c := newFakeLogin(t, f)
	body := []byte(`{"jsonrpc":"2.0","id":"x","method":"getAppName","params":{}}`)
	b, err := c.JSONRPC("testschool", "JSESSIONID=abc", body)
	if err != nil {
		t.Fatalf("JSONRPC: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("JSONRPC returned empty body")
	}
	cookie, _, _ := f.last()
	if !strings.Contains(cookie, "JSESSIONID=abc") {
		t.Errorf("forwarded cookie = %q, want JSESSIONID=abc", cookie)
	}
}

func TestJSONRPCErrorsOnNon200(t *testing.T) {
	f := &fakeLoginServer{jsonrpcStatus: http.StatusBadGateway, jsonrpcBody: `oops`}
	c := newFakeLogin(t, f)
	_, err := c.JSONRPC("testschool", "JSESSIONID=abc", []byte(`{}`))
	if err == nil {
		t.Fatal("expected status error, got nil")
	}
	if !regexp.MustCompile(`502`).MatchString(err.Error()) {
		t.Errorf("error = %v, want upstream status 502", err)
	}
}
