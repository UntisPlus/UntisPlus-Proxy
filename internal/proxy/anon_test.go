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
	"time"
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

	if err := st.SetSchoolAnonymous("testschool", false); err != nil {
		t.Fatalf("disable anonymous: %v", err)
	}
	f.mu.Lock()
	f.loginAuth = nil
	f.mu.Unlock()
	blocked := httptest.NewRequest(http.MethodPost,
		"/WebUntis/jsonrpc_intern.do?m=getUserData2017&school=testschool",
		strings.NewReader(loginBody(t, "#anonymous#", "0")))
	blockedRec := httptest.NewRecorder()
	p.handleJSONRPCIntern(blockedRec, blocked)
	if code := errCode(t, blockedRec); code != -8504 {
		t.Fatalf("disabled anonymous login error = %d, want -8504 (body %s)", code, blockedRec.Body.String())
	}
	f.mu.Lock()
	auth = f.loginAuth
	f.mu.Unlock()
	if auth != nil {
		t.Fatalf("disabled anonymous login still reached upstream with %v", auth)
	}
}

// TestAnonymousClassTimetableForwardedRaw is the regression test for the
// reported failure: anonymous login succeeds, but the following class
// getTimetable2017 used to fail with -8520 because keyLogin had not persisted
// "#anonymous#" and the handler required store.GetUser to succeed. The class
// below is not in the pool, so it keeps the raw upstream path.
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

	// Disabling anonymous closes both the login and the raw forwarding path: an
	// already-issued anonymous session must not keep reading timetables.
	if err := st.SetSchoolAnonymous("testschool", false); err != nil {
		t.Fatalf("disable anonymous: %v", err)
	}
	disabled := internReq(t, p, "getTimetable2017", "",
		internBody("getTimetable2017", param))
	if code := errCode(t, disabled); code != -8509 {
		t.Fatalf("disabled anonymous timetable error = %d, want -8509 (body %s)", code, disabled.Body.String())
	}
	if got := f.TimetableCalls(); got != 2 {
		t.Fatalf("disabled anonymous request reached upstream (%d timetable calls, want 2)", got)
	}
}

// TestAnonymousPooledClassServedFromOwner: a pooled class is served through the
// pool's student owner even when upstream's own #anonymous# account has no
// timetable right. This is what makes a school that hides its classes from
// #anonymous# work once anonymous login is enabled, without turning anonymous
// into an account.
func TestAnonymousPooledClassServedFromOwner(t *testing.T) {
	f := &fakeUpstream{}
	f.setTimetable(t, []map[string]any{{
		"id":            1,
		"startDateTime": "2026-11-09T08:00:00Z",
		"endDateTime":   "2026-11-09T08:45:00Z",
	}})
	p, st := newFakeProxyUpstream(t, f)
	seedSessionUser(t, st, "owner", 5, 7, 5000)
	// The fallback is opt-in and only serves what recon has actually covered.
	if err := st.SetAnonymousFeature("classes", true); err != nil {
		t.Fatalf("enable anonymous classes: %v", err)
	}
	start := time.Now().Format("2006-01-02")
	end := time.Now().AddDate(0, 0, 6).Format("2006-01-02")
	p.stateFor("testschool").recon.setScanHorizon(5000, end)

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

	req := httptest.NewRequest(http.MethodPost,
		"/WebUntis/jsonrpc_intern.do?m=getTimetable2017&school=testschool",
		strings.NewReader(internBody("getTimetable2017",
			ttParam(5000, "CLASS", start, end, "#anonymous#"))))
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: sid})
	rec := httptest.NewRecorder()
	p.handleJSONRPCIntern(rec, req)
	if code := errCode(t, rec); code != 0 {
		t.Fatalf("pooled class returned error code %d, body %s", code, rec.Body.String())
	}
	if got := len(cachedPeriods(t, rec)); got != 1 {
		t.Fatalf("periods = %d, want 1 (body %s)", got, rec.Body.String())
	}

	// The class must be fetched as its pooled owner, never as #anonymous#: the
	// whole point is that upstream would deny the anonymous account.
	var sent struct {
		Params []struct {
			Auth struct {
				User string `json:"user"`
			} `json:"auth"`
		} `json:"params"`
	}
	forwards := f.Forwards()
	if len(forwards) != 1 {
		t.Fatalf("upstream forwards = %d, want 1", len(forwards))
	}
	if err := json.Unmarshal(forwards[0].Body, &sent); err != nil {
		t.Fatalf("decode forwarded body: %v", err)
	}
	if len(sent.Params) != 1 || sent.Params[0].Auth.User != "owner" {
		t.Fatalf("forwarded auth user = %+v, want owner", sent.Params)
	}
	if u, _ := st.GetUser("#anonymous#"); u != nil {
		t.Fatalf("anonymous class read persisted a user row: %+v", u)
	}
}

// TestAnonymousMasterDataPromotesPooledClasses: upstream may hide every class
// from #anonymous#, which left the app with an empty picker. The login rewrite
// must expose the pooled classes while keeping whatever upstream already
// exposes to the anonymous account.
func TestAnonymousMasterDataPromotesPooledClasses(t *testing.T) {
	f := &fakeUpstream{}
	f.userDataBody = `{"jsonrpc":"2.0","id":"upstream","result":{"masterData":{"klassen":[
		{"id":5000,"name":"5a","displayable":false},
		{"id":6000,"name":"6b","displayable":false},
		{"id":7000,"name":"7c","displayable":true}]}}}`
	p, st := newFakeProxyUpstream(t, f)
	seedSessionUser(t, st, "owner", 5, 7, 5000)
	if err := st.SetAnonymousFeature("classes", true); err != nil {
		t.Fatalf("enable anonymous classes: %v", err)
	}
	p.stateFor("testschool").recon.setScanHorizon(5000, reconHorizon())

	login := httptest.NewRequest(http.MethodPost,
		"/WebUntis/jsonrpc_intern.do?m=getUserData2017&school=testschool",
		strings.NewReader(loginBody(t, "#anonymous#", "0")))
	rec := httptest.NewRecorder()
	p.handleJSONRPCIntern(rec, login)
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous login status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Result struct {
			MasterData struct {
				Klassen []struct {
					ID          int64 `json:"id"`
					Displayable bool  `json:"displayable"`
				} `json:"klassen"`
			} `json:"masterData"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode login body: %v", err)
	}
	got := map[int64]bool{}
	for _, k := range resp.Result.MasterData.Klassen {
		got[k.ID] = k.Displayable
	}
	if !got[5000] {
		t.Fatalf("pooled class 5000 not displayable: %v", got)
	}
	if got[6000] {
		t.Fatalf("non-pooled class 6000 became displayable: %v", got)
	}
	if !got[7000] {
		t.Fatalf("upstream-displayable class 7000 was hidden: %v", got)
	}
}

// TestAnonymousMasterDataStaysStockByDefault: without the classes capability
// anonymous must look exactly like upstream, so a pooled class that upstream
// hides is not promoted.
func TestAnonymousMasterDataStaysStockByDefault(t *testing.T) {
	f := &fakeUpstream{}
	f.userDataBody = `{"jsonrpc":"2.0","id":"upstream","result":{"masterData":{"klassen":[
		{"id":5000,"name":"5a","displayable":false},
		{"id":7000,"name":"7c","displayable":true}]}}}`
	p, st := newFakeProxyUpstream(t, f)
	seedSessionUser(t, st, "owner", 5, 7, 5000)
	p.stateFor("testschool").recon.setScanHorizon(5000, reconHorizon())

	login := httptest.NewRequest(http.MethodPost,
		"/WebUntis/jsonrpc_intern.do?m=getUserData2017&school=testschool",
		strings.NewReader(loginBody(t, "#anonymous#", "0")))
	rec := httptest.NewRecorder()
	p.handleJSONRPCIntern(rec, login)
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous login status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Result struct {
			MasterData struct {
				Klassen []struct {
					ID          int64 `json:"id"`
					Displayable bool  `json:"displayable"`
				} `json:"klassen"`
			} `json:"masterData"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode login body: %v", err)
	}
	for _, k := range resp.Result.MasterData.Klassen {
		if k.ID == 5000 && k.Displayable {
			t.Fatal("pooled class was promoted while anonymous classes is off")
		}
		if k.ID == 7000 && !k.Displayable {
			t.Fatal("upstream-displayable class was hidden")
		}
	}
}

// TestAnonymousPooledClassRequiresFeatureAndCoverage pins the two conditions
// for the pooled-class fallback. Either one missing must leave the request on
// the raw upstream path rather than serving it through the pool.
func TestAnonymousPooledClassRequiresFeatureAndCoverage(t *testing.T) {
	start := time.Now().Format("2006-01-02")
	end := time.Now().AddDate(0, 0, 6).Format("2006-01-02")
	short := time.Now().AddDate(0, 0, 3).Format("2006-01-02")

	cases := []struct {
		name      string
		classes   bool
		horizon   string
		endDate   string
		wantOwner bool
	}{
		{name: "feature off", classes: false, horizon: end, endDate: end},
		{name: "never scanned", classes: true, horizon: "", endDate: end},
		{name: "scan ends before request", classes: true, horizon: short, endDate: end},
		{name: "covered", classes: true, horizon: end, endDate: end, wantOwner: true},
		{name: "covered with compact date", classes: true, horizon: end, endDate: strings.ReplaceAll(end, "-", ""), wantOwner: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeUpstream{}
			f.setTimetable(t, []map[string]any{{
				"id":            1,
				"startDateTime": start + "T08:00:00Z",
				"endDateTime":   start + "T08:45:00Z",
			}})
			p, st := newFakeProxyUpstream(t, f)
			seedSessionUser(t, st, "owner", 5, 7, 5000)
			if tc.classes {
				if err := st.SetAnonymousFeature("classes", true); err != nil {
					t.Fatalf("enable classes: %v", err)
				}
			}
			if tc.horizon != "" {
				p.stateFor("testschool").recon.setScanHorizon(5000, tc.horizon)
			}

			login := httptest.NewRequest(http.MethodPost,
				"/WebUntis/jsonrpc_intern.do?m=getUserData2017&school=testschool",
				strings.NewReader(loginBody(t, "#anonymous#", "0")))
			loginRec := httptest.NewRecorder()
			p.handleJSONRPCIntern(loginRec, login)
			if loginRec.Code != http.StatusOK {
				t.Fatalf("anonymous login status = %d, body %s", loginRec.Code, loginRec.Body.String())
			}
			sid := findCookie(loginRec, "JSESSIONID")

			req := httptest.NewRequest(http.MethodPost,
				"/WebUntis/jsonrpc_intern.do?m=getTimetable2017&school=testschool",
				strings.NewReader(internBody("getTimetable2017",
					ttParam(5000, "CLASS", start, tc.endDate, "#anonymous#"))))
			req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: sid})
			rec := httptest.NewRecorder()
			p.handleJSONRPCIntern(rec, req)
			if code := errCode(t, rec); code != 0 {
				t.Fatalf("timetable error code = %d, body %s", code, rec.Body.String())
			}

			var sent struct {
				Params []struct {
					Auth struct {
						User string `json:"user"`
					} `json:"auth"`
				} `json:"params"`
			}
			forwards := f.Forwards()
			if len(forwards) != 1 {
				t.Fatalf("upstream forwards = %d, want 1 (body %s)", len(forwards), rec.Body.String())
			}
			if err := json.Unmarshal(forwards[0].Body, &sent); err != nil {
				t.Fatalf("decode forwarded body: %v", err)
			}
			authUser := ""
			if len(sent.Params) == 1 {
				authUser = sent.Params[0].Auth.User
			}
			want := "#anonymous#"
			if tc.wantOwner {
				want = "owner"
			}
			if authUser != want {
				t.Fatalf("forwarded auth user = %q, want %q", authUser, want)
			}
		})
	}
}
