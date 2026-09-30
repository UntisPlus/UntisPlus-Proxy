package untis

// Client plumbing that the login tests do not reach: school-host resolution,
// the two PersonInfo config endpoints, the REST/raw forwarders and the error
// type's message.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

func TestExplicitServerSkipsTheSchoolSearch(t *testing.T) {
	// Configuring both School and Server pre-seeds the host cache, so the
	// school search must never be consulted (this is how the proxy points at a
	// self-hosted or proxied upstream).
	var searched bool
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		searched = true
		return jsonResponse(`{"result":{"schools":[{"loginName":"testschool","server":"resolved.webuntis.com"}]}}`), nil
	})}
	c := New(Config{Server: "configured.webuntis.com", School: "testschool", HTTPClient: hc})
	if got := c.serverFor("testschool"); got != "configured.webuntis.com" {
		t.Errorf("serverFor = %q, want the configured server", got)
	}
	if searched {
		t.Error("a configured school+server pair must not trigger a school search")
	}
}

func TestSchoolCookieIsTheNameValuePair(t *testing.T) {
	c := New(Config{Server: "s.webuntis.com", School: "testschool"})
	got := c.SchoolCookie("testschool")
	if !strings.HasPrefix(got, "schoolname=") {
		t.Errorf("SchoolCookie = %q, want a name=value pair", got)
	}
	if got != "schoolname="+c.schoolCookie("testschool") {
		t.Errorf("SchoolCookie = %q, want the same encoding the session cookie uses", got)
	}
	// an empty school falls back to the configured default
	if c.SchoolCookie("") != got {
		t.Errorf("SchoolCookie(\"\") = %q, want the configured school", c.SchoolCookie(""))
	}
	if c.SchoolCookie("") == c.SchoolCookie("zweitschule") {
		t.Error("a different school must produce a different cookie value")
	}
}

func TestUpstreamErrorMessage(t *testing.T) {
	err := &UpstreamError{Code: -8504, Message: "bad credentials"}
	if got := err.Error(); got != "upstream error -8504: bad credentials" {
		t.Errorf("Error() = %q, want the code and message", got)
	}
}

func TestResolveServerHostUsesSchoolSearch(t *testing.T) {
	var gotURL, gotBody string
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotURL = r.URL.String()
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return jsonResponse(`{"result":{"schools":[
			{"loginName":"andereschule","server":"anders.webuntis.com"},
			{"loginName":"Testschool","server":"testschool.webuntis.com"}]}}`), nil
	})}
	c := New(Config{Server: "fallback.webuntis.com", HTTPClient: hc})

	if got := c.serverFor("testschool"); got != "testschool.webuntis.com" {
		t.Errorf("serverFor = %q, want the resolved host (login match is case-insensitive)", got)
	}
	if !strings.HasPrefix(gotURL, "https://schoolsearch.webuntis.com/schoolquery2") {
		t.Errorf("school search URL = %q", gotURL)
	}
	if !strings.Contains(gotBody, `"method":"searchSchool"`) || !strings.Contains(gotBody, `"search":"testschool"`) {
		t.Errorf("school search body = %s", gotBody)
	}

	// the resolved host is cached: a second call must not hit the search API
	gotURL = ""
	if got := c.serverFor("testschool"); got != "testschool.webuntis.com" {
		t.Errorf("cached serverFor = %q", got)
	}
	if gotURL != "" {
		t.Errorf("second call re-queried the school search (%s)", gotURL)
	}
}

func TestResolveServerHostSingleMatchFallback(t *testing.T) {
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(`{"result":{"schools":[{"loginName":"sondername","server":"sonder.webuntis.com"}]}}`), nil
	})}
	c := New(Config{Server: "fallback.webuntis.com", HTTPClient: hc})
	if got := c.serverFor("x"); got != "sonder.webuntis.com" {
		t.Errorf("serverFor = %q, want the sole match", got)
	}
}

func TestResolveServerHostFallsBackOnFailure(t *testing.T) {
	t.Run("ambiguous", func(t *testing.T) {
		// two schools, neither matching: never guess
		hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(`{"result":{"schools":[
				{"loginName":"a","server":"a.webuntis.com"},
				{"loginName":"b","server":"b.webuntis.com"}]}}`), nil
		})}
		c := New(Config{Server: "fallback.webuntis.com", HTTPClient: hc})
		if got := c.serverFor("x"); got != "fallback.webuntis.com" {
			t.Errorf("serverFor = %q, want the configured fallback", got)
		}
	})
	t.Run("network-error", func(t *testing.T) {
		hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, io.ErrUnexpectedEOF
		})}
		c := New(Config{Server: "fallback.webuntis.com", HTTPClient: hc})
		if got := c.serverFor("x"); got != "fallback.webuntis.com" {
			t.Errorf("serverFor = %q, want the configured fallback", got)
		}
	})
	t.Run("garbage-json", func(t *testing.T) {
		hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(`not json`), nil
		})}
		c := New(Config{Server: "fallback.webuntis.com", HTTPClient: hc})
		if got := c.serverFor("x"); got != "fallback.webuntis.com" {
			t.Errorf("serverFor = %q, want the configured fallback", got)
		}
	})
}

// fakeAPI records every request and answers from a path-keyed body map.
type fakeAPI struct {
	mu       sync.Mutex
	requests []recordedReq
	bodies   map[string]string
	status   map[string]int
}

type recordedReq struct {
	Path   string
	Query  string
	Method string
	Body   string
	Cookie string
	Auth   string
}

func (f *fakeAPI) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, recordedReq{
		Path: r.URL.Path, Query: r.URL.RawQuery, Method: r.Method,
		Body: string(body), Cookie: r.Header.Get("Cookie"), Auth: r.Header.Get("Authorization"),
	})
	b, ok := f.bodies[r.URL.Path]
	status := f.status[r.URL.Path]
	f.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	if !ok {
		b = `{}`
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(b))
}

func (f *fakeAPI) last() recordedReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return recordedReq{}
	}
	return f.requests[len(f.requests)-1]
}

func (f *fakeAPI) find(path string) (recordedReq, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if r.Path == path {
			return r, true
		}
	}
	return recordedReq{}, false
}

func newFakeAPI(t *testing.T, f *fakeAPI) *Client {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	// the school-search lookup must not reach the network: the test TLS client
	// only trusts the fake server, so it fails and the client uses cfg.Server
	return New(Config{Server: strings.TrimPrefix(srv.URL, "https://"), School: "testschool", HTTPClient: srv.Client()})
}

func TestPersonInfoReadsBothConfigEndpoints(t *testing.T) {
	f := &fakeAPI{
		bodies: map[string]string{
			"/WebUntis/api/app/config": `{"data":{"loginServiceConfig":{"user":{
				"personId":5005,"email":"dee@testschool.example",
				"persons":[{"id":5005,"type":5,"displayName":"Dee M."}]}}}}`,
			"/WebUntis/api/daytimetable/config": `{"data":{"klasseId":5000}}`,
		},
		status: map[string]int{},
	}
	c := newFakeAPI(t, f)

	info, err := c.PersonInfo("testschool", "JSESSIONID=abc")
	if err != nil {
		t.Fatalf("PersonInfo: %v", err)
	}
	if info.PersonID != 5005 || info.PersonType != 5 || info.ClassID != 5000 {
		t.Errorf("info = %+v, want personId 5005 / type 5 / class 5000", info)
	}
	if info.Email != "dee@testschool.example" {
		t.Errorf("email = %q", info.Email)
	}
	if info.DisplayName != "Dee M." {
		t.Errorf("displayName = %q, want the first person entry's name", info.DisplayName)
	}

	// both endpoints must be requested with the session cookie
	for _, p := range []string{"/WebUntis/api/app/config", "/WebUntis/api/daytimetable/config"} {
		req, ok := f.find(p)
		if !ok {
			t.Errorf("%s was never requested", p)
			continue
		}
		if req.Cookie != "JSESSIONID=abc" {
			t.Errorf("%s cookie = %q, want the session cookie", p, req.Cookie)
		}
		if req.Method != http.MethodGet {
			t.Errorf("%s method = %s, want GET", p, req.Method)
		}
	}
}

func TestPersonInfoPrefersLoginServicePersonID(t *testing.T) {
	f := &fakeAPI{
		bodies: map[string]string{
			// loginServiceConfig reports no personId, so the persons[0] id is used
			"/WebUntis/api/app/config": `{"data":{"loginServiceConfig":{"user":{
				"persons":[{"id":77,"type":2,"displayName":"Mr teacher1"}]}}}}`,
			"/WebUntis/api/daytimetable/config": `{"data":{"klasseId":0}}`,
		},
	}
	c := newFakeAPI(t, f)
	info, err := c.PersonInfo("testschool", "JSESSIONID=abc")
	if err != nil {
		t.Fatalf("PersonInfo: %v", err)
	}
	if info.PersonID != 77 || info.PersonType != 2 {
		t.Errorf("info = %+v, want the persons[0] fallback id/type", info)
	}
	if info.ClassID != 0 {
		t.Errorf("classID = %d, want 0 when the daytimetable config reports none", info.ClassID)
	}
}

func TestPersonInfoSurvivesBrokenConfig(t *testing.T) {
	f := &fakeAPI{
		bodies: map[string]string{},
		status: map[string]int{"/WebUntis/api/app/config": http.StatusInternalServerError},
	}
	c := newFakeAPI(t, f)
	info, err := c.PersonInfo("testschool", "JSESSIONID=abc")
	if err != nil {
		t.Fatalf("PersonInfo must not fail on a broken config, got %v", err)
	}
	if *info != (PersonInfo{}) {
		t.Errorf("info = %+v, want the zero value when upstream is broken", info)
	}
}

func TestGetKlassenForwardsRawBody(t *testing.T) {
	f := &fakeAPI{
		bodies: map[string]string{"/WebUntis/jsonrpc.do": `{"result":[{"id":5000,"name":"10b"}]}`},
	}
	c := newFakeAPI(t, f)

	b, err := c.GetKlassen("testschool", "JSESSIONID=abc")
	if err != nil {
		t.Fatalf("GetKlassen: %v", err)
	}
	var out struct {
		Result []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("bad json: %v (body=%s)", err, b)
	}
	if len(out.Result) != 1 || out.Result[0].ID != 5000 {
		t.Errorf("result = %+v, want class 5000", out.Result)
	}
	req := f.last()
	if !strings.Contains(req.Body, `"method":"getKlassen"`) {
		t.Errorf("upstream body = %s, want a getKlassen call", req.Body)
	}
	if req.Cookie != "JSESSIONID=abc" {
		t.Errorf("cookie = %q, want the session cookie", req.Cookie)
	}
}

func TestRESTGetForwardsPathQueryAndCookie(t *testing.T) {
	f := &fakeAPI{
		bodies: map[string]string{"/WebUntis/api/public/timetable/weekly/data": `{"elementPeriods":{}}`},
	}
	c := newFakeAPI(t, f)

	b, status, err := c.RESTGet("testschool", "JSESSIONID=abc",
		"/WebUntis/api/public/timetable/weekly/data", "elementType=1&elementId=5000")
	if err != nil || status != http.StatusOK {
		t.Fatalf("RESTGet = (%d, %v), want (200, nil)", status, err)
	}
	if !strings.Contains(string(b), "elementPeriods") {
		t.Errorf("body = %s, want the upstream answer", b)
	}
	req := f.last()
	if req.Query != "elementType=1&elementId=5000" {
		t.Errorf("query = %q, want it forwarded verbatim", req.Query)
	}
	if req.Cookie != "JSESSIONID=abc" {
		t.Errorf("cookie = %q, want the session cookie", req.Cookie)
	}

	// an empty query must not append a bare "?"
	if _, _, err := c.RESTGet("testschool", "JSESSIONID=abc", "/WebUntis/api/public/timetable/weekly/data", ""); err != nil {
		t.Fatalf("RESTGet without query: %v", err)
	}
	if req := f.last(); strings.Contains(req.Query, "?") {
		t.Errorf("query = %q, want empty", req.Query)
	}
}

func TestRESTGetTokenUsesBearerHeader(t *testing.T) {
	f := &fakeAPI{
		bodies: map[string]string{"/WebUntis/api/restricted/data": `{"ok":true}`},
	}
	c := newFakeAPI(t, f)

	b, status, err := c.RESTGetToken("testschool", "Bearer tok123", "/WebUntis/api/restricted/data", "x=1")
	if err != nil || status != http.StatusOK {
		t.Fatalf("RESTGetToken = (%d, %v), want (200, nil)", status, err)
	}
	if !strings.Contains(string(b), `"ok":true`) {
		t.Errorf("body = %s", b)
	}
	req := f.last()
	if req.Auth != "Bearer tok123" {
		t.Errorf("Authorization = %q, want the token passed through untouched", req.Auth)
	}
	if req.Cookie != "" {
		t.Errorf("cookie = %q, want none for a token request", req.Cookie)
	}
}

func TestRawInternForwardsMethodQueryAndBody(t *testing.T) {
	f := &fakeAPI{
		bodies: map[string]string{"/WebUntis/jsonrpc_intern.do": `{"result":{"periods":[]}}`},
	}
	c := newFakeAPI(t, f)

	body := []byte(`{"id":"x","jsonrpc":"2.0","method":"getTimetable2017","params":[{"id":5000}]}`)
	b, status, hdr, err := c.RawIntern("testschool", "JSESSIONID=abc", "getTimetable2017", body)
	if err != nil || status != http.StatusOK {
		t.Fatalf("RawIntern = (%d, %v), want (200, nil)", status, err)
	}
	if !strings.Contains(string(b), `"periods"`) {
		t.Errorf("body = %s, want the upstream answer", b)
	}
	if hdr == nil {
		t.Error("RawIntern returned no response header")
	}
	req := f.last()
	if !strings.Contains(req.Query, "m=getTimetable2017") || !strings.Contains(req.Query, "school=testschool") {
		t.Errorf("query = %q, want the method and school", req.Query)
	}
	if req.Body != string(body) {
		t.Errorf("forwarded body = %s, want the caller's body verbatim", req.Body)
	}
	if req.Cookie != "JSESSIONID=abc" {
		t.Errorf("cookie = %q, want the session cookie", req.Cookie)
	}
}

func TestRawInternReportsUpstreamStatus(t *testing.T) {
	f := &fakeAPI{
		bodies: map[string]string{"/WebUntis/jsonrpc_intern.do": `{"error":{"code":-8504}}`},
		status: map[string]int{"/WebUntis/jsonrpc_intern.do": http.StatusUnauthorized},
	}
	c := newFakeAPI(t, f)

	b, status, _, err := c.RawIntern("testschool", "", "getUserData2017", []byte(`{}`))
	if err != nil {
		t.Fatalf("RawIntern: %v", err)
	}
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 passed through to the caller", status)
	}
	if !strings.Contains(string(b), "-8504") {
		t.Errorf("body = %s, want the raw upstream error", b)
	}
}
