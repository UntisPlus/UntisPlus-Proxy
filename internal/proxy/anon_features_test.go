package proxy

// Anonymous self-service capabilities are deployment-wide switches layered on
// top of the per-school anonymous login. These tests pin the gate: a feature
// that is off (or whose school has the anonymous login off) keeps returning
// 401, and an enabled feature admits the synthetic "#anonymous#" user without
// ever creating a stored account. Calendar links are never available to an
// anonymous session.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"untis-proxy/internal/store"
)

func anonReq(t *testing.T, p *Proxy, method, path, body string) *http.Request {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	s := p.sessions.New("#anonymous#", 0)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: s.ID})
	return req
}

func seedPooledClass(t *testing.T, st *store.Store) {
	t.Helper()
	if err := st.UpsertUser(&store.User{
		Username: "owner", Method: "key", PersonType: 5, ClassID: 5000, School: "testschool",
	}); err != nil {
		t.Fatalf("seed pooled user: %v", err)
	}
}

func seedAnonClassTarget(t *testing.T, st *store.Store, school string, classID int64) {
	t.Helper()
	if err := st.UpsertSchoolAnonProfile(&store.SchoolAnonProfile{
		School:  school,
		LoginOK: true,
		Source:  "override",
		ProfileJSON: store.EncodeAnonProfilePayload(store.AnonProfilePayload{
			ClassesDisplayable: []int64{classID},
		}),
		DetectedAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("seed anon profile: %v", err)
	}
}

func TestAnonymousNtfyGate(t *testing.T) {
	p, st := newTestProxy(t)
	seedPooledClass(t, st)

	// Off by default: the historical 401 remains.
	rec := httptest.NewRecorder()
	p.handleSubsNtfy(rec, anonReq(t, p, http.MethodGet, "/api/ntfy?school=testschool", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled ntfy code = %d, want 401", rec.Code)
	}

	if err := st.SetAnonymousFeature("ntfy", true); err != nil {
		t.Fatal(err)
	}
	// v2: anonymous subscriptions require upstream-detected/stock class targets.
	seedAnonClassTarget(t, st, "testschool", 5000)
	rec = httptest.NewRecorder()
	p.handleSubsNtfy(rec, anonReq(t, p, http.MethodGet, "/api/ntfy?school=testschool", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("enabled ntfy list code = %d, body %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	p.handleSubsNtfy(rec, anonReq(t, p, http.MethodPost, "/api/ntfy?school=testschool",
		`{"classId":5000,"topic":"anon-feed"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous ntfy create code = %d, body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"createdBy":"#anonymous#"`) {
		t.Fatalf("anonymous ntfy create body = %s", rec.Body.String())
	}
	if u, _ := st.GetUser("#anonymous#"); u != nil {
		t.Fatalf("anonymous subscription persisted a user row: %+v", u)
	}

	// The per-school login switch wins even when the global capability is on.
	if err := st.SetSchoolAnonymous("testschool", false); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	p.handleSubsNtfy(rec, anonReq(t, p, http.MethodGet, "/api/ntfy?school=testschool", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("ntfy with anonymous login off code = %d, want 401", rec.Code)
	}
}

func TestAnonymousWebhooksGate(t *testing.T) {
	p, st := newTestProxy(t)
	seedPooledClass(t, st)

	rec := httptest.NewRecorder()
	p.handleSubsWebhooks(rec, anonReq(t, p, http.MethodGet, "/api/webhooks?school=testschool", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled webhooks code = %d, want 401", rec.Code)
	}

	if err := st.SetAnonymousFeature("webhooks", true); err != nil {
		t.Fatal(err)
	}
	// v2: anonymous subscriptions require upstream-detected/stock class targets.
	seedAnonClassTarget(t, st, "testschool", 5000)
	rec = httptest.NewRecorder()
	p.handleSubsWebhooks(rec, anonReq(t, p, http.MethodPost, "/api/webhooks?school=testschool",
		`{"classId":5000,"url":"https://example.com/hook"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous webhook create code = %d, body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"createdBy":"#anonymous#"`) {
		t.Fatalf("anonymous webhook body = %s", rec.Body.String())
	}
}

// TestAnonymousCalendarForbidden pins the security decision: a calendar link is
// a public credential that outlives the session, so an anonymous session never
// mints or manages one, even when every other capability is on.
func TestAnonymousCalendarForbidden(t *testing.T) {
	p, st := newTestProxy(t)
	seedPooledClass(t, st)
	if err := st.SetAnonymousFeature("ntfy", true); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAnonymousFeature("webhooks", true); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAnonymousFeature("changes", true); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		req  *http.Request
		call func(http.ResponseWriter, *http.Request)
	}{
		{"options", anonReq(t, p, http.MethodGet, "/api/calendar/options?school=testschool", ""), p.handleCalendarOptions},
		{"tokens", anonReq(t, p, http.MethodGet, "/api/calendar/tokens?school=testschool", ""), p.handleCalendarTokens},
		{"mint query school", anonReq(t, p, http.MethodPost, "/api/calendar/token?school=testschool", `{"classId":5000}`), p.handleCalendarToken},
		{"mint body school", anonReq(t, p, http.MethodPost, "/api/calendar/token", `{"school":"testschool","classId":5000}`), p.handleCalendarToken},
	} {
		rec := httptest.NewRecorder()
		tc.call(rec, tc.req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s code = %d, want 401 (body %s)", tc.name, rec.Code, rec.Body.String())
		}
	}
	if tokens, err := st.ListClassTokensByUser("#anonymous#"); err != nil || len(tokens) != 0 {
		t.Fatalf("anonymous calendar call created a token: %v, %v", tokens, err)
	}
}

// TestCalendarMintUsesQuerySchool guards the regression that made anonymity
// look broken: the app sends ?school= and no body school. The handler used to
// fall back to the deployment default, silently minting for the wrong school.
func TestCalendarMintUsesQuerySchool(t *testing.T) {
	p, st := newTestProxy(t)
	seedPooledClass(t, st)
	// Make the default school different from the request school so a fallback
	// cannot masquerade as correct behaviour.
	p.opts.School = "defaultschool"

	req := httptest.NewRequest(http.MethodPost, "/api/calendar/token?school=testschool",
		strings.NewReader(`{"classId":5000}`))
	sess := p.sessions.New("owner", 0)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: sess.ID})
	rec := httptest.NewRecorder()
	p.handleCalendarToken(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("calendar mint with query school code = %d, body %s", rec.Code, rec.Body.String())
	}
	var tok map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)
	if tok["school"] != "testschool" {
		t.Fatalf("calendar token school = %v, want testschool", tok["school"])
	}

	// A body that disagrees with the query is a caller bug, not a coin flip.
	req = httptest.NewRequest(http.MethodPost, "/api/calendar/token?school=testschool",
		strings.NewReader(`{"school":"defaultschool","classId":5000}`))
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: p.sessions.New("owner", 0).ID})
	rec = httptest.NewRecorder()
	p.handleCalendarToken(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("mismatched school code = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestAnonymousChangesGate(t *testing.T) {
	p, st := newTestProxy(t)
	seedPooledClass(t, st)
	// v2: anonymous class target access requires upstream-detected stock exposure.
	if err := st.UpsertSchoolAnonProfile(&store.SchoolAnonProfile{
		School:     "testschool",
		LoginOK:    true,
		Source:     "detect",
		DetectedAt: time.Now(),
		ExpiresAt:  time.Now().Add(24 * time.Hour),
		ProfileJSON: store.EncodeAnonProfilePayload(store.AnonProfilePayload{
			Capabilities:       map[string]bool{},
			ClassesDisplayable: []int64{5000},
		}),
	}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	p.handleTimetableChanges(rec, anonReq(t, p, http.MethodGet,
		"/api/timetable/changes?school=testschool&classId=5000&since=-1", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled changes code = %d, want 401", rec.Code)
	}

	if err := st.SetAnonymousFeature("changes", true); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	p.handleTimetableChanges(rec, anonReq(t, p, http.MethodGet,
		"/api/timetable/changes?school=testschool&classId=5000&since=-1", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("enabled changes code = %d, body %s", rec.Code, rec.Body.String())
	}

	// Anonymous may only follow a class exposed to anonymous upstream in v2.
	rec = httptest.NewRecorder()
	p.handleTimetableChanges(rec, anonReq(t, p, http.MethodGet,
		"/api/timetable/changes?school=testschool&classId=9999&since=-1", ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-exposed class changes code = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
}

// Notification options serve both webhook and ntfy callers, so either switch
// must open the catalog.
func TestAnonymousNotificationOptionsEitherSwitch(t *testing.T) {
	p, st := newTestProxy(t)
	seedPooledClass(t, st)
	// v2: anonymous target visibility is stock/upstream-detected only (no
	// proxy-pooled supplements). Simulate upstream exposing the pooled class.
	if err := st.UpsertSchoolAnonProfile(&store.SchoolAnonProfile{
		School:     "testschool",
		LoginOK:    true,
		Source:     "detect",
		DetectedAt: time.Now(),
		ExpiresAt:  time.Now().Add(24 * time.Hour),
		ProfileJSON: store.EncodeAnonProfilePayload(store.AnonProfilePayload{
			Capabilities:       map[string]bool{},
			ClassesDisplayable: []int64{5000},
		}),
	}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	p.handleNotificationOptions(rec, anonReq(t, p, http.MethodGet, "/api/notifications/options?school=testschool", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled notification options code = %d, want 401", rec.Code)
	}

	if err := st.SetAnonymousFeature("ntfy", true); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	p.handleNotificationOptions(rec, anonReq(t, p, http.MethodGet, "/api/notifications/options?school=testschool", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("ntfy-only notification options code = %d, body %s", rec.Code, rec.Body.String())
	}
}
