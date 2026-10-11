package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"untis-proxy/internal/store"
)

func subsRequest(t *testing.T, p *Proxy, user string, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	s := p.sessions.New(user, 0)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: s.ID})
	rec := httptest.NewRecorder()

	if strings.HasPrefix(path, "/api/webhooks") {
		p.handleSubsWebhooks(rec, req)
	} else {
		p.handleSubsNtfy(rec, req)
	}
	return rec
}

// notifOptionsRequest drives GET /api/notifications/options with a session.
func notifOptionsRequest(t *testing.T, p *Proxy, user string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/notifications/options", nil)
	s := p.sessions.New(user, 0)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: s.ID})
	rec := httptest.NewRecorder()
	p.handleNotificationOptions(rec, req)
	return rec
}

// TestNotificationOptions_Visibility: the notification catalog mirrors the
// calendar one, but never offers Technik — the change poller cannot deliver it.
func TestNotificationOptions_Visibility(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", PersonType: 5, PersonID: 42, ClassID: 5000, DisplayName: "Max", School: "testschool"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := st.SetPerm("dee", store.FeatureTechniker, true); err != nil {
		t.Fatalf("grant techniker: %v", err)
	}
	calSeedMasterData(p, "testschool", calSampleMD())

	rec := notifOptionsRequest(t, p, "dee")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if classes := calOptionIDs(t, m, "class"); !classes[5000] {
		t.Errorf("classes = %v, want pooled 5000", classes)
	}
	if _, ok := m["personal"]; !ok {
		t.Errorf("personal missing: %v", m)
	}
	if _, ok := m["technik"]; ok {
		t.Errorf("technik must be absent from notification options: %v", m)
	}
}

// TestNotificationOptions_Unauthorized: no session, no catalog.
func TestNotificationOptions_Unauthorized(t *testing.T) {
	p, _ := newTestProxy(t)
	req := httptest.NewRequest(http.MethodGet, "/api/notifications/options", nil)
	rec := httptest.NewRecorder()
	p.handleNotificationOptions(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d, want 401", rec.Code)
	}
}

// TestSubsElementTargets: webhooks and ntfy accept the same element vocabulary
// as the calendar mint, gated by the same visibility rules.
func TestSubsElementTargets(t *testing.T) {
	p, st := newTestProxy(t)
	_ = st.UpsertUser(&store.User{Username: "recon", Method: "key", PersonType: 5, PersonID: 42, ClassID: 5000, School: "testschool"})
	_ = st.UpsertUser(&store.User{Username: "plain", Method: "key", PersonType: 5, PersonID: 43, ClassID: 5000, School: "testschool"})
	if err := st.SetReconOverride("recon", "", true); err != nil {
		t.Fatalf("grant recon: %v", err)
	}
	calSeedMasterData(p, "testschool", calSampleMD())

	// A recon user may target a teacher; a plain student may not.
	if rec := subsRequest(t, p, "recon", http.MethodPost, "/api/ntfy", `{"teacherId":7,"topic":"t7"}`); rec.Code != http.StatusOK {
		t.Fatalf("recon teacher ntfy = %d (body %s)", rec.Code, rec.Body.String())
	}
	if rec := subsRequest(t, p, "plain", http.MethodPost, "/api/ntfy", `{"teacherId":7,"topic":"nope"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("plain teacher ntfy = %d, want 403", rec.Code)
	}
	// A personal target resolves to the caller's own STUDENT element.
	if rec := subsRequest(t, p, "plain", http.MethodPost, "/api/webhooks", `{"personal":true,"url":"https://e/h"}`); rec.Code != http.StatusOK {
		t.Fatalf("personal webhook = %d (body %s)", rec.Code, rec.Body.String())
	}
	// A foreign personId is refused.
	if rec := subsRequest(t, p, "plain", http.MethodPost, "/api/webhooks", `{"personId":9999,"url":"https://e/h2"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("foreign personal webhook = %d, want 403", rec.Code)
	}
	// More than one target is a bad request (forbidden here, no row written).
	if rec := subsRequest(t, p, "recon", http.MethodPost, "/api/ntfy", `{"teacherId":7,"classId":5000,"topic":"x"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("multi-target ntfy = %d, want 403", rec.Code)
	}
}

// TestSubsTechnikTargetRejected: the synthetic Technik class is not a polled
// class, so a notification target for it must be refused even for a techniker.
func TestSubsTechnikTargetRejected(t *testing.T) {
	p, st := newTestProxy(t)
	_ = st.UpsertUser(&store.User{Username: "tech", Method: "key", PersonType: 5, PersonID: 42, ClassID: 5000, School: "testschool"})
	if err := st.SetPerm("tech", store.FeatureTechniker, true); err != nil {
		t.Fatalf("grant techniker: %v", err)
	}
	body := `{"classId":` + strconv.FormatInt(technikClassID, 10) + `,"topic":"tech"}`
	if rec := subsRequest(t, p, "tech", http.MethodPost, "/api/ntfy", body); rec.Code != http.StatusForbidden {
		t.Fatalf("technik ntfy target = %d, want 403", rec.Code)
	}
}

// TestSubsListOwnerScoped: the list endpoints show only the caller's own rows,
// with the target element resolved.
func TestSubsListOwnerScoped(t *testing.T) {
	p, st := newTestProxy(t)
	for _, u := range []string{"dave", "eve"} {
		_ = st.UpsertUser(&store.User{Username: u, Method: "key", PersonType: 5, PersonID: 42, ClassID: 5000, School: "testschool"})
	}
	for _, seed := range []struct{ who, url string }{{"dave", "https://e/dave"}, {"eve", "https://e/eve"}} {
		if _, err := st.AddWebhook(&store.Webhook{School: "testschool", ElementType: "CLASS", ElementID: 5000, ClassID: 5000, URL: seed.url, Enabled: true, CreatedBy: seed.who}); err != nil {
			t.Fatalf("seed webhook: %v", err)
		}
	}
	calSeedMasterData(p, "testschool", calSampleMD())

	rec := subsRequest(t, p, "dave", http.MethodGet, "/api/webhooks", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list code = %d", rec.Code)
	}
	var list struct {
		Webhooks []map[string]any `json:"webhooks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if len(list.Webhooks) != 1 || list.Webhooks[0]["url"] != "https://e/dave" {
		t.Fatalf("dave list = %s, want only his own", rec.Body.String())
	}
	if list.Webhooks[0]["elementType"] != "CLASS" {
		t.Errorf("elementType = %v, want CLASS", list.Webhooks[0]["elementType"])
	}
	if list.Webhooks[0]["createdBy"] != "dave" {
		t.Errorf("createdBy = %v, want dave", list.Webhooks[0]["createdBy"])
	}
}

func TestSubsOwnClassWebhookOnly(t *testing.T) {
	p, st := newTestProxy(t)
	// carla is a student of class 5000; no privileged flags.
	if err := st.UpsertUser(&store.User{Username: "carla", Method: "key", ClassID: 5000}); err != nil {
		t.Fatalf("add carla: %v", err)
	}
	// own class: allowed
	rec := subsRequest(t, p, "carla", http.MethodPost, "/api/webhooks", `{"classId":5000,"url":"https://e/h"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("own-class webhook code = %d (body %s)", rec.Code, rec.Body.String())
	}
	// someone else's class: forbidden
	rec = subsRequest(t, p, "carla", http.MethodPost, "/api/webhooks", `{"classId":9999,"url":"https://e/h2"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign-class webhook code = %d, want 403", rec.Code)
	}
	// school-wide: forbidden for plain student
	rec = subsRequest(t, p, "carla", http.MethodPost, "/api/webhooks", `{"classId":0,"url":"https://e/h3"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("school-wide webhook code = %d, want 403", rec.Code)
	}
	// listing: carla sees only her class hook
	rec = subsRequest(t, p, "carla", http.MethodGet, "/api/webhooks", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list code = %d", rec.Code)
	}
	var out struct {
		Webhooks []map[string]any `json:"webhooks"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Webhooks) != 1 || out.Webhooks[0]["classId"].(float64) != 5000 {
		t.Fatalf("unexpected webhooks visible to carla: %s", rec.Body.String())
	}
}

func TestSubsEditorCanSubscribeAnyPooledClass(t *testing.T) {
	p, st := newTestProxy(t)
	_ = st.UpsertUser(&store.User{Username: "caroline", Method: "key", ClassID: 5000})
	if err := st.SetPerm("caroline", store.FeatureEditor, true); err != nil {
		t.Fatalf("grant editor: %v", err)
	}
	// editor may create a school-wide topic
	rec := subsRequest(t, p, "caroline", http.MethodPost, "/api/ntfy", `{"classId":0,"topic":"schoollife"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("editor school-wide ntfy code = %d (body %s)", rec.Code, rec.Body.String())
	}
	// editor may target any pooled class
	rec = subsRequest(t, p, "caroline", http.MethodPost, "/api/ntfy", `{"classId":5000,"topic":"class5000"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("editor class ntfy code = %d (body %s)", rec.Code, rec.Body.String())
	}
	// unpooled class still forbidden even for editor
	rec = subsRequest(t, p, "caroline", http.MethodPost, "/api/ntfy", `{"classId":7777,"topic":"x"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unpooled class ntfy code = %d, want 403", rec.Code)
	}
}

func TestSubsDeleteOwnership(t *testing.T) {
	p, st := newTestProxy(t)
	_ = st.UpsertUser(&store.User{Username: "dave", Method: "key", ClassID: 5000})
	_ = st.UpsertUser(&store.User{Username: "eve", Method: "key", ClassID: 5000})
	id, err := st.AddWebhook(&store.Webhook{School: "testschool", ClassID: 5000, URL: "https://e/h", Enabled: true, CreatedBy: "dave"})
	if err != nil {
		t.Fatalf("seed webhook: %v", err)
	}
	// eve cannot delete dave's hook
	req := httptest.NewRequest(http.MethodDelete, "/api/webhooks/"+strconv.FormatInt(id, 10), nil)
	s := p.sessions.New("eve", 0)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: s.ID})
	rec := httptest.NewRecorder()
	p.handleSubsWebhooks(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("eve delete dave's hook code = %d, want 403", rec.Code)
	}
	// dave can delete his own
	req = httptest.NewRequest(http.MethodDelete, "/api/webhooks/"+strconv.FormatInt(id, 10), nil)
	s = p.sessions.New("dave", 0)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: s.ID})
	rec = httptest.NewRecorder()
	p.handleSubsWebhooks(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dave delete own hook code = %d (body %s)", rec.Code, rec.Body.String())
	}
}

func TestSubsWebhookTestRouteOwnerOnly(t *testing.T) {
	p, st := newTestProxy(t)
	_ = st.UpsertUser(&store.User{Username: "carla", Method: "key", ClassID: 5000})
	_ = st.UpsertUser(&store.User{Username: "stranger", Method: "key", ClassID: 9999})

	hits := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits <- r.Header.Get("X-Untis-Event")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rec := subsRequest(t, p, "carla", http.MethodPost, "/api/webhooks",
		`{"classId":5000,"url":"`+srv.URL+`/h","secret":"s"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create code = %d (body %s)", rec.Code, rec.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == 0 {
		t.Fatalf("create response = %s", rec.Body.String())
	}

	// owner can test
	rec = subsRequest(t, p, "carla", http.MethodPost, "/api/webhooks/"+strconv.FormatInt(created.ID, 10)+"/test", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("owner test code = %d (body %s)", rec.Code, rec.Body.String())
	}
	var out struct {
		OK     bool `json:"ok"`
		Status int  `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if !out.OK || out.Status != http.StatusOK {
		t.Fatalf("test result = %+v, want ok/200", out)
	}
	if ev := <-hits; ev != "test" {
		t.Errorf("X-Untis-Event = %q, want test", ev)
	}

	// stranger (different class, no flags) cannot test someone else's hook
	rec = subsRequest(t, p, "stranger", http.MethodPost, "/api/webhooks/"+strconv.FormatInt(created.ID, 10)+"/test", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("stranger test code = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestSubsNtfyTestRouteAndBaseUrl(t *testing.T) {
	p, st := newTestProxy(t)
	_ = st.UpsertUser(&store.User{Username: "carla", Method: "key", ClassID: 5000})
	_ = st.UpsertUser(&store.User{Username: "stranger", Method: "key", ClassID: 9999})

	gotPath := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath <- r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rec := subsRequest(t, p, "carla", http.MethodPost, "/api/ntfy",
		`{"classId":5000,"topic":"myclass","baseUrl":"`+srv.URL+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create code = %d (body %s)", rec.Code, rec.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == 0 {
		t.Fatalf("create response = %s", rec.Body.String())
	}

	// create response must not hit the global server with a wrong base
	rec = subsRequest(t, p, "carla", http.MethodPost, "/api/ntfy/"+strconv.FormatInt(created.ID, 10)+"/test", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("owner test code = %d (body %s)", rec.Code, rec.Body.String())
	}
	var out struct {
		OK     bool `json:"ok"`
		Status int  `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if !out.OK || out.Status != http.StatusOK {
		t.Fatalf("test result = %+v, want ok/200", out)
	}
	// The per-topic base is honoured, but the JSON envelope goes to that
	// server's root with the topic in the body.
	if path := <-gotPath; path != "/" {
		t.Errorf("published to %q, want the per-topic server root \"/\"", path)
	}

	rec = subsRequest(t, p, "stranger", http.MethodPost, "/api/ntfy/"+strconv.FormatInt(created.ID, 10)+"/test", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("stranger test code = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}

	// listing exposes baseUrl
	rec = subsRequest(t, p, "carla", http.MethodGet, "/api/ntfy", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list code = %d", rec.Code)
	}
	var list struct {
		Topics []map[string]any `json:"topics"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("bad list json: %v", err)
	}
	if len(list.Topics) != 1 || list.Topics[0]["baseUrl"] != srv.URL {
		t.Fatalf("unexpected topics: %s", rec.Body.String())
	}
}
