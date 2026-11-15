package proxy

// Anonymous self-service capabilities are opt-in per school. These tests pin
// the gate: a feature that is off (or whose school has the anonymous login off)
// keeps returning 401, and an enabled feature admits the synthetic
// "#anonymous#" user without ever creating a stored account.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

func TestAnonymousNtfyGate(t *testing.T) {
	p, st := newTestProxy(t)
	seedPooledClass(t, st)

	// Off by default: the historical 401 remains.
	rec := httptest.NewRecorder()
	p.handleSubsNtfy(rec, anonReq(t, p, http.MethodGet, "/api/ntfy?school=testschool", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled ntfy code = %d, want 401", rec.Code)
	}

	if err := st.SetSchoolAnonymousFeature("testschool", "ntfy", true); err != nil {
		t.Fatal(err)
	}
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

	// The master login switch wins even when the capability is still on.
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

	if err := st.SetSchoolAnonymousFeature("testschool", "webhooks", true); err != nil {
		t.Fatal(err)
	}
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

func TestAnonymousCalendarGate(t *testing.T) {
	p, st := newTestProxy(t)
	seedPooledClass(t, st)

	rec := httptest.NewRecorder()
	p.handleCalendarOptions(rec, anonReq(t, p, http.MethodGet, "/api/calendar/options?school=testschool", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled calendar options code = %d, want 401", rec.Code)
	}

	if err := st.SetSchoolAnonymousFeature("testschool", "calendar", true); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	p.handleCalendarOptions(rec, anonReq(t, p, http.MethodGet, "/api/calendar/options?school=testschool", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("enabled calendar options code = %d, body %s", rec.Code, rec.Body.String())
	}
	var opts map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &opts)
	if classes := calOptionIDs(t, opts, "class"); !classes[5000] {
		t.Fatalf("anonymous calendar classes = %v, want pooled 5000", classes)
	}
	if _, ok := opts["personal"]; ok {
		t.Fatalf("anonymous calendar options offered a personal target: %v", opts)
	}

	rec = httptest.NewRecorder()
	p.handleCalendarToken(rec, anonReq(t, p, http.MethodPost, "/api/calendar/token",
		`{"school":"testschool","classId":5000}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous calendar mint code = %d, body %s", rec.Code, rec.Body.String())
	}
	var tok map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)
	if tok["createdBy"] != "#anonymous#" {
		t.Fatalf("anonymous calendar token = %v", tok)
	}

	if err := st.SetSchoolAnonymous("testschool", false); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	p.handleCalendarToken(rec, anonReq(t, p, http.MethodPost, "/api/calendar/token",
		`{"school":"testschool","classId":5000}`))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("calendar mint with anonymous login off code = %d, want 401", rec.Code)
	}
}

func TestAnonymousChangesGate(t *testing.T) {
	p, st := newTestProxy(t)
	seedPooledClass(t, st)

	rec := httptest.NewRecorder()
	p.handleTimetableChanges(rec, anonReq(t, p, http.MethodGet,
		"/api/timetable/changes?school=testschool&classId=5000&since=-1", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled changes code = %d, want 401", rec.Code)
	}

	if err := st.SetSchoolAnonymousFeature("testschool", "changes", true); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	p.handleTimetableChanges(rec, anonReq(t, p, http.MethodGet,
		"/api/timetable/changes?school=testschool&classId=5000&since=-1", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("enabled changes code = %d, body %s", rec.Code, rec.Body.String())
	}

	// Anonymous may only follow a class the poller maintains.
	rec = httptest.NewRecorder()
	p.handleTimetableChanges(rec, anonReq(t, p, http.MethodGet,
		"/api/timetable/changes?school=testschool&classId=9999&since=-1", ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-pooled class changes code = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
}

// Notification options serve both webhook and ntfy callers, so either switch
// must open the catalog.
func TestAnonymousNotificationOptionsEitherSwitch(t *testing.T) {
	p, st := newTestProxy(t)
	seedPooledClass(t, st)

	rec := httptest.NewRecorder()
	p.handleNotificationOptions(rec, anonReq(t, p, http.MethodGet, "/api/notifications/options?school=testschool", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled notification options code = %d, want 401", rec.Code)
	}

	if err := st.SetSchoolAnonymousFeature("testschool", "ntfy", true); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	p.handleNotificationOptions(rec, anonReq(t, p, http.MethodGet, "/api/notifications/options?school=testschool", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("ntfy-only notification options code = %d, body %s", rec.Code, rec.Body.String())
	}
}
