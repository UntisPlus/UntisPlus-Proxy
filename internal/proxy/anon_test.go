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

	"untis-proxy/internal/store"
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
	// v2: anonymous class timetable access requires upstream-exposed/detected (or
	// explicitly allowed) stock class target and classes feature.
	if err := st.UpsertSchoolAnonProfile(&store.SchoolAnonProfile{
		School:  "testschool",
		LoginOK: true,
		Source:  "override",
		ProfileJSON: store.EncodeAnonProfilePayload(store.AnonProfilePayload{
			ClassesDisplayable: []int64{5000},
			Capabilities:       map[string]bool{"classes": true},
		}),
		DetectedAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("seed anon profile: %v", err)
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

// TestAnonymousPooledClassServedFromOwner: v2 anonymous sessions never receive
// proxy-pooled/reconstructed supplements. A pooled class request is forwarded
// upstream as #anonymous# (stock behavior), not served through a pooled owner.
func TestAnonymousPooledClassServedFromOwner(t *testing.T) {
	f := &fakeUpstream{}
	f.setTimetable(t, []map[string]any{{
		"id":            1,
		"startDateTime": "2026-11-09T08:00:00Z",
		"endDateTime":   "2026-11-09T08:45:00Z",
	}})
	p, st := newFakeProxyUpstream(t, f)
	seedSessionUser(t, st, "owner", 5, 7, 5000)
	// Legacy toggle must not enable proxy-pooled supplements for anonymous in v2.
	// Anonymous stock class forwarding requires classes feature/target exposure.
	if err := st.SetAnonymousFeature("classes", true); err != nil {
		t.Fatalf("enable anonymous classes: %v", err)
	}
	if err := st.UpsertSchoolAnonProfile(&store.SchoolAnonProfile{
		School:  "testschool",
		LoginOK: true,
		Source:  "override",
		ProfileJSON: store.EncodeAnonProfilePayload(store.AnonProfilePayload{
			ClassesDisplayable: []int64{5000},
			Capabilities:       map[string]bool{"classes": true},
		}),
		DetectedAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("seed anon profile: %v", err)
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

	// Forwarded upstream as anonymous (stock), never as pooled owner.
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
	if len(sent.Params) != 1 || sent.Params[0].Auth.User != "#anonymous#" {
		t.Fatalf("forwarded auth user = %+v, want #anonymous#", sent.Params)
	}
	if u, _ := st.GetUser("#anonymous#"); u != nil {
		t.Fatalf("anonymous class read persisted a user row: %+v", u)
	}
}

// TestAnonymousMasterDataPromotesPooledClasses: v2 anonymous sessions never
// receive proxy-pooled supplements in masterData; displayable flags stay as
// upstream exposed them (stock), regardless of legacy anonymous feature toggles.
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
	if got[5000] {
		t.Fatalf("pooled class 5000 must not be promoted for anonymous in v2: %v", got)
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

// TestAnonymousPooledClassRequiresFeatureAndCoverage: v2 anonymous sessions
// never receive proxy-pooled supplements; requests are forwarded upstream as
// #anonymous# (stock) only when classes feature and exposed target allow it.
// Pool/recon coverage is irrelevant for anonymous stock forwarding.
func TestAnonymousPooledClassRequiresFeatureAndCoverage(t *testing.T) {
	start := time.Now().Format("2006-01-02")
	end := time.Now().AddDate(0, 0, 6).Format("2006-01-02")
	short := time.Now().AddDate(0, 0, 3).Format("2006-01-02")

	cases := []struct {
		name        string
		classes     bool
		allowTarget bool
		horizon     string
		endDate     string
		wantOK      bool
	}{
		{name: "feature off", classes: false, allowTarget: true, horizon: end, endDate: end, wantOK: false},
		{name: "never scanned", classes: true, allowTarget: true, horizon: "", endDate: end, wantOK: true},
		{name: "scan ends before request", classes: true, allowTarget: true, horizon: short, endDate: end, wantOK: true},
		{name: "covered", classes: true, allowTarget: true, horizon: end, endDate: end, wantOK: true},
		{name: "covered with compact date", classes: true, allowTarget: true, horizon: end, endDate: strings.ReplaceAll(end, "-", ""), wantOK: true},
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
			if err := st.SetAnonymousFeature("classes", tc.classes); err != nil {
				t.Fatalf("set classes feature: %v", err)
			}
			payload := store.AnonProfilePayload{Capabilities: map[string]bool{"classes": tc.classes}}
			if tc.allowTarget {
				payload.ClassesDisplayable = []int64{5000}
			}
			if err := st.UpsertSchoolAnonProfile(&store.SchoolAnonProfile{
				School:      "testschool",
				LoginOK:     true,
				Source:      "override",
				ProfileJSON: store.EncodeAnonProfilePayload(payload),
				DetectedAt:  time.Now(),
				ExpiresAt:   time.Now().Add(time.Hour),
			}); err != nil {
				t.Fatalf("seed anon profile: %v", err)
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

			forwards := f.Forwards()
			if !tc.wantOK {
				if code := errCode(t, rec); code != -8509 {
					t.Fatalf("timetable error code = %d, want -8509 (body %s)", code, rec.Body.String())
				}
				if len(forwards) != 0 {
					t.Fatalf("upstream forwards = %d, want 0 (denied by proxy)", len(forwards))
				}
				return
			}
			if code := errCode(t, rec); code != 0 {
				t.Fatalf("timetable error code = %d, body %s", code, rec.Body.String())
			}
			if len(forwards) != 1 {
				t.Fatalf("upstream forwards = %d, want 1 (body %s)", len(forwards), rec.Body.String())
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
			authUser := ""
			if len(sent.Params) == 1 {
				authUser = sent.Params[0].Auth.User
			}
			if authUser != "#anonymous#" {
				t.Fatalf("forwarded auth user = %q, want anonymous (v2 stock behavior)", authUser)
			}
		})
	}
}
