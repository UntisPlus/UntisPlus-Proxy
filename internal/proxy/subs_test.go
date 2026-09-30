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
