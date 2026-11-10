package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"untis-proxy/internal/store"
)

// countingNtfy records every notification body so outage tests can assert how
// many alerts were sent and what they said.
type countingNtfy struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (c *countingNtfy) server(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		var msg map[string]any
		_ = json.Unmarshal(b, &msg)
		c.mu.Lock()
		c.bodies = append(c.bodies, msg)
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (c *countingNtfy) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

func (c *countingNtfy) last() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		return nil
	}
	return c.bodies[len(c.bodies)-1]
}

// TestSchoolGateBlocksAndAllows covers the public gate: a blocked school is
// refused even before a handler runs, unblocking restores access, and the
// allow-new switch rejects unknown schools without touching the default.
func TestSchoolGateBlocksAndAllows(t *testing.T) {
	p, st := newTestProxy(t)
	h := p.Handler()

	if err := st.UpsertSchool("other"); err != nil {
		t.Fatalf("register school: %v", err)
	}
	if err := st.SetSchoolBlocked("other", true); err != nil {
		t.Fatalf("block school: %v", err)
	}

	req := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	const body = `{"jsonrpc":"2.0","id":1,"method":"getTimetable"}`

	if rec := req("/WebUntis/jsonrpc.do?school=other", body); rec.Code != http.StatusForbidden {
		t.Fatalf("blocked school status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}

	if err := st.SetSchoolBlocked("other", false); err != nil {
		t.Fatalf("unblock school: %v", err)
	}
	if rec := req("/WebUntis/jsonrpc.do?school=other", body); rec.Code == http.StatusForbidden {
		t.Fatalf("unblocked school still refused: %s", rec.Body.String())
	}

	// Unknown schools auto-register by default.
	if rec := req("/WebUntis/jsonrpc.do?school=brandnew", body); rec.Code == http.StatusForbidden {
		t.Fatalf("new school refused while auto-registration is on: %s", rec.Body.String())
	}
	if err := st.SetAllowNewSchools(false); err != nil {
		t.Fatalf("disable allow-new: %v", err)
	}
	if rec := req("/WebUntis/jsonrpc.do?school=brandnew2", body); rec.Code != http.StatusForbidden {
		t.Fatalf("unknown school status = %d, want 403 when allow-new is off (body %s)", rec.Code, rec.Body.String())
	}

	// The configured default school must remain reachable, otherwise an admin
	// could lock the deployment out of itself.
	if !p.schoolAdmitted("testschool") {
		t.Fatal("default school was not admitted")
	}
}

// TestSchoolGateUsesSessionSchool: an existing session from a school that gets
// blocked is cut off even when the request carries no ?school= parameter.
func TestSchoolGateUsesSessionSchool(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "otheruser", School: "other", ClassID: 7}); err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	if err := st.SetSchoolBlocked("other", true); err != nil {
		t.Fatalf("block school: %v", err)
	}
	s := p.sessions.New("otheruser", 7)
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: s.ID})
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("blocked session status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestOutageAlertsDebounceAndRecovery walks one subscription through a sustained
// outage and recovery: no alert on a transient failure, one down alert after the
// debounce, no duplicates, then one recovery alert.
func TestOutageAlertsDebounceAndRecovery(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertSchool("testschool"); err != nil {
		t.Fatalf("register school: %v", err)
	}
	if err := st.UpsertUser(&store.User{Username: "student", School: "testschool", ClassID: 11}); err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	rec := &countingNtfy{}
	server := rec.server(t)
	if _, err := st.AddOutageNtfy(&store.OutageNtfy{
		School: "testschool", Topic: "ops", BaseURL: server, Enabled: true,
	}); err != nil {
		t.Fatalf("add outage subscription: %v", err)
	}

	base := time.Now()
	p.stateFor("testschool")
	setPoll := func(lastPoll, lastOK time.Time) {
		s := p.stateFor("testschool")
		s.pollMu.Lock()
		s.lastPoll, s.lastPollOK = lastPoll, lastOK
		s.pollMu.Unlock()
	}
	setPoll(base, base.Add(-time.Minute))
	if down, _ := p.schoolPollOutage("testschool", base); !down {
		t.Fatal("failed poll was not reported as an outage")
	}

	tr := newOutageAlertTracker()
	p.runOutageAlerts(base, tr)
	p.runOutageAlerts(base.Add(2*time.Minute), tr)
	if rec.count() != 0 {
		t.Fatalf("sent %d alerts before the debounce window", rec.count())
	}
	p.runOutageAlerts(base.Add(4*time.Minute), tr)
	if rec.count() != 1 {
		t.Fatalf("sent %d alerts after the debounce, want 1", rec.count())
	}
	msg := rec.last()
	if !strings.Contains(msg["title"].(string), "outage") {
		t.Errorf("down title = %q, want an outage title", msg["title"])
	}
	if !strings.Contains(msg["message"].(string), "testschool") {
		t.Errorf("down message = %q, want the school name", msg["message"])
	}
	p.runOutageAlerts(base.Add(5*time.Minute), tr)
	if rec.count() != 1 {
		t.Fatalf("duplicate down alert sent: %d notifications", rec.count())
	}

	// A successful poll after the failed attempt clears the outage.
	setPoll(base.Add(5*time.Minute), base.Add(6*time.Minute))
	p.runOutageAlerts(base.Add(7*time.Minute), tr)
	if rec.count() != 2 {
		t.Fatalf("sent %d notifications, want 2 (down + recovery)", rec.count())
	}
	if got := rec.last()["title"].(string); !strings.Contains(got, "recovered") {
		t.Errorf("recovery title = %q", got)
	}
}

// TestOutageAlertsAllScope: the global subscription fires when any monitored
// school is down and names it; a blocked school is not monitored.
func TestOutageAlertsAllScope(t *testing.T) {
	p, st := newTestProxy(t)
	for _, name := range []string{"testschool", "other"} {
		if err := st.UpsertSchool(name); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
		if err := st.UpsertUser(&store.User{Username: "u-" + name, School: name, ClassID: 12}); err != nil {
			t.Fatalf("upsert user for %s: %v", name, err)
		}
	}
	rec := &countingNtfy{}
	server := rec.server(t)
	if _, err := st.AddOutageNtfy(&store.OutageNtfy{Topic: "all", BaseURL: server, Enabled: true}); err != nil {
		t.Fatalf("add global subscription: %v", err)
	}

	now := time.Now()
	for _, name := range []string{"testschool", "other"} {
		s := p.stateFor(name)
		s.pollMu.Lock()
		s.lastPoll, s.lastPollOK = now, now.Add(-time.Minute)
		s.pollMu.Unlock()
	}
	down, detail := p.outageCondition("", now)
	if !down || !strings.Contains(detail, "testschool") || !strings.Contains(detail, "other") {
		t.Fatalf("overall outage = (%v, %q), want both schools named", down, detail)
	}

	if err := st.SetSchoolBlocked("other", true); err != nil {
		t.Fatalf("block other: %v", err)
	}
	down, detail = p.outageCondition("", now)
	if !down || strings.Contains(detail, "other") {
		t.Fatalf("blocked school still in overall outage: (%v, %q)", down, detail)
	}
}

// TestAdminOutageNtfyAPI covers the dashboard/API surface: create, list, test
// and delete an outage subscription.
func TestAdminOutageNtfyAPI(t *testing.T) {
	p, _ := newTestProxy(t)
	rec := &countingNtfy{}
	server := rec.server(t)

	school := jsqString("testschool")
	body := `{"school":` + school + `,"topic":"ops","baseUrl":` + jsqString(server) + `}`
	res := adminRequest(t, p, "bob", http.MethodPost, "/admin/outage-ntfy", body)
	if res.Code != http.StatusOK {
		t.Fatalf("create status = %d (body %s)", res.Code, res.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &created); err != nil {
		t.Fatalf("create response: %v", err)
	}
	id := int64(created["id"].(float64))

	res = adminRequest(t, p, "bob", http.MethodGet, "/admin/outage-ntfy", "")
	var listed struct {
		Subscriptions []struct {
			ID    int64  `json:"id"`
			Scope string `json:"scope"`
			Topic string `json:"topic"`
		} `json:"subscriptions"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &listed); err != nil {
		t.Fatalf("list response: %v", err)
	}
	if len(listed.Subscriptions) != 1 || listed.Subscriptions[0].Scope != "testschool" {
		t.Fatalf("list = %+v", listed.Subscriptions)
	}

	res = adminRequest(t, p, "bob", http.MethodPost, "/admin/outage-ntfy/"+strconv.FormatInt(id, 10)+"/test", "")
	var sent map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &sent); err != nil {
		t.Fatalf("test response: %v", err)
	}
	if sent["ok"] != true || rec.count() != 1 {
		t.Fatalf("test notification = %v, sent %d", sent, rec.count())
	}

	res = adminRequest(t, p, "bob", http.MethodDelete, "/admin/outage-ntfy/"+strconv.FormatInt(id, 10), "")
	if !strings.Contains(res.Body.String(), `"deleted":true`) {
		t.Fatalf("delete response = %s", res.Body.String())
	}
}

// TestAdminSchoolAccessAPI covers the dashboard controls for blocking and the
// allow-new switch.
func TestAdminSchoolAccessAPI(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertSchool("other"); err != nil {
		t.Fatalf("register school: %v", err)
	}
	res := adminRequest(t, p, "bob", http.MethodPost, "/admin/schools/other/block", "")
	if !strings.Contains(res.Body.String(), `"blocked":true`) {
		t.Fatalf("block response = %s", res.Body.String())
	}
	info, _ := st.SchoolInfo("other")
	if info == nil || !info.Blocked {
		t.Fatalf("school not blocked: %+v", info)
	}

	res = adminRequest(t, p, "bob", http.MethodPost, "/admin/schools/other/unblock", "")
	if !strings.Contains(res.Body.String(), `"blocked":false`) {
		t.Fatalf("unblock response = %s", res.Body.String())
	}

	res = adminRequest(t, p, "bob", http.MethodPost, "/admin/schools/settings", `{"allowNew":false}`)
	if !strings.Contains(res.Body.String(), `"allowNew":false`) {
		t.Fatalf("settings response = %s", res.Body.String())
	}
	if allow, _ := st.AllowNewSchools(); allow {
		t.Fatal("allow-new still enabled after the dashboard switch")
	}

	res = adminRequest(t, p, "bob", http.MethodGet, "/admin/schools", "")
	if !strings.Contains(res.Body.String(), `"allowNew":false`) {
		t.Fatalf("schools response does not report the switch: %s", res.Body.String())
	}
}

// jsqString returns a JSON string literal for tests that build request bodies.
func jsqString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
