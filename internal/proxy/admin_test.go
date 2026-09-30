package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"untis-proxy/internal/session"
	"untis-proxy/internal/store"
	"untis-proxy/internal/untis"
)

func newTestProxy(t *testing.T) (*Proxy, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.SetAdmin("bob", true); err != nil {
		t.Fatalf("set admin: %v", err)
	}
	_ = st.SetDefaultSchool("testschool")
	p := New(st, untis.New(untis.Config{Server: "school.example.com", School: "testschool"}), session.NewManager(5*time.Minute), Options{School: "testschool"})
	return p, st
}

func adminRequest(t *testing.T, p *Proxy, user string, method, path string, body string) *httptest.ResponseRecorder {
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
	p.handleAdmin(rec, req)
	return rec
}

func TestAdminStatus(t *testing.T) {
	p, st := newTestProxy(t)
	_, _ = st.AddWebhook(&store.Webhook{School: "testschool", URL: "https://e/h", Enabled: true})
	_, _ = st.AddNtfyTopic(&store.NtfyTopic{School: "testschool", Topic: "schoollife", Enabled: true})
	rec := adminRequest(t, p, "bob", http.MethodGet, "/admin/status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if out["admins"].(float64) != 1 || out["webhooks"].(float64) != 1 || out["ntfyTopics"].(float64) != 1 {
		t.Errorf("unexpected status summary: %v", out)
	}
}

func TestAdminForbiddenForNonAdmin(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "mallory", Method: "key"}); err != nil {
		t.Fatalf("add user: %v", err)
	}
	rec := adminRequest(t, p, "mallory", http.MethodGet, "/admin/status", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status code = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestAdminUnauthorizedWithoutSession(t *testing.T) {
	p, _ := newTestProxy(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/status", nil)
	rec := httptest.NewRecorder()
	p.handleAdmin(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status code = %d, want 401", rec.Code)
	}
}

func TestAdminUsersCRUD(t *testing.T) {
	p, st := newTestProxy(t)
	rec := adminRequest(t, p, "bob", http.MethodPost, "/admin/users", `{"username":"alice","admin":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create user code = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	rec = adminRequest(t, p, "bob", http.MethodGet, "/admin/users", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list users code = %d", rec.Code)
	}
	var out struct {
		Users []map[string]any `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	found := false
	for _, u := range out.Users {
		if u["username"] == "alice" {
			found = true
		}
	}
	if !found {
		t.Fatalf("alice missing from listing: %s", rec.Body.String())
	}
	// set admin
	rec = adminRequest(t, p, "bob", http.MethodPost, "/admin/users/alice", `{"admin":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set admin code = %d (body %s)", rec.Code, rec.Body.String())
	}
	if ok, _ := st.IsAdmin("alice"); !ok {
		t.Fatalf("alice should be admin after promotion")
	}
	// delete alice
	rec = adminRequest(t, p, "bob", http.MethodDelete, "/admin/users/alice", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete code = %d", rec.Code)
	}
	if u, _ := st.GetUser("alice"); u != nil {
		t.Fatalf("alice should be deleted")
	}
}

func TestAdminSchools(t *testing.T) {
	p, _ := newTestProxy(t)
	rec := adminRequest(t, p, "bob", http.MethodPost, "/admin/schools/otherschool", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("register school code = %d", rec.Code)
	}
	rec = adminRequest(t, p, "bob", http.MethodGet, "/admin/schools", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list schools code = %d", rec.Code)
	}
	var out struct {
		Schools []map[string]any `json:"schools"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if len(out.Schools) != 1 || out.Schools[0]["name"] != "otherschool" {
		t.Fatalf("expected otherschool, got %s", rec.Body.String())
	}
	if _, ok := out.Schools[0]["addedAt"].(float64); !ok {
		t.Fatalf("addedAt should be epoch seconds: %v", out.Schools[0])
	}
}

func TestMultiSchoolAutoRegisterAndPoolIsolation(t *testing.T) {
	p, st := newTestProxy(t)

	// 1. First use of an unknown school auto-registers it (no manual step).
	p.stateFor("otherschool")
	schools, err := st.ListSchools()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, s := range schools {
		names[s.Name] = true
	}
	if !names["otherschool"] {
		t.Fatalf("stateFor('otherschool') did not auto-register the school: %v", schools)
	}

	// 2. Per-school pools are isolated: otherschool's class never leaks into
	// testschool's pool (and vice-versa); a school='' account (pre-multi-school)
	// matches any school.
	if err := st.UpsertUser(&store.User{Username: "a1", PersonType: 5, PersonID: 1, ClassID: 101, School: "testschool"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertUser(&store.User{Username: "a2", PersonType: 5, PersonID: 2, ClassID: 102, School: "otherschool"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertUser(&store.User{Username: "a3", PersonType: 5, PersonID: 3, ClassID: 200, School: ""}); err != nil {
		t.Fatal(err)
	}

	testschool, err := st.Pool("testschool")
	if err != nil {
		t.Fatal(err)
	}
	otherschool, err := st.Pool("otherschool")
	if err != nil {
		t.Fatal(err)
	}
	herSchools := map[int64]bool{}
	for _, c := range testschool {
		herSchools[c.ID] = true
	}
	neSchools := map[int64]bool{}
	for _, c := range otherschool {
		neSchools[c.ID] = true
	}
	if !herSchools[101] || !herSchools[200] {
		t.Errorf("testschool pool = %v, want classes 101 + 200", testschool)
	}
	if herSchools[102] {
		t.Errorf("testschool pool = %v, but otherschool class 102 leaked in", testschool)
	}
	if !neSchools[102] || !neSchools[200] {
		t.Errorf("otherschool pool = %v, want classes 102 + 200", otherschool)
	}
	if neSchools[101] {
		t.Errorf("otherschool pool = %v, but testschool class 101 leaked in", otherschool)
	}

	// 3. stateFor is idempotent and creation is reflected on /admin/schools.
	_ = p.stateFor("otherschool")
	rec := adminRequest(t, p, "bob", http.MethodGet, "/admin/schools", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list schools code = %d", rec.Code)
	}
	var out struct {
		Schools []map[string]any `json:"schools"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	found := false
	for _, s := range out.Schools {
		if s["name"] == "otherschool" {
			found = true
		}
	}
	if !found {
		t.Fatalf("otherschool missing from /admin/schools: %s", rec.Body.String())
	}
}

func TestAdminWebhooksAndNtfy(t *testing.T) {
	p, _ := newTestProxy(t)
	rec := adminRequest(t, p, "bob", http.MethodPost, "/admin/webhooks", `{"school":"testschool","classId":5000,"url":"https://e/h","secret":"s"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("add webhook code = %d (body %s)", rec.Code, rec.Body.String())
	}
	rec = adminRequest(t, p, "bob", http.MethodPost, "/admin/ntfy", `{"school":"testschool","classId":0,"topic":"schoollife","baseUrl":"https://ntfy.example.org/"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("add ntfy code = %d (body %s)", rec.Code, rec.Body.String())
	}
	rec = adminRequest(t, p, "bob", http.MethodGet, "/admin/webhooks", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list webhooks code = %d", rec.Code)
	}
	var wh struct {
		Webhooks []map[string]any `json:"webhooks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &wh); err != nil {
		t.Fatalf("bad webhook json: %v", err)
	}
	if len(wh.Webhooks) != 1 || wh.Webhooks[0]["classId"].(float64) != 5000 {
		t.Fatalf("unexpected webhooks: %s", rec.Body.String())
	}
	rec = adminRequest(t, p, "bob", http.MethodGet, "/admin/ntfy", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list ntfy code = %d", rec.Code)
	}
	var nf struct {
		Topics []map[string]any `json:"topics"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &nf); err != nil {
		t.Fatalf("bad ntfy json: %v", err)
	}
	if len(nf.Topics) != 1 {
		t.Fatalf("unexpected ntfy topics: %s", rec.Body.String())
	}
	// A trailing slash on the server override is trimmed on save, so the
	// dashboard can't list two rows that are really the same subscription.
	if got, _ := nf.Topics[0]["baseUrl"].(string); got != "https://ntfy.example.org" {
		t.Errorf("stored baseUrl = %q, want the trailing slash trimmed", got)
	}
	// delete both
	rec = adminRequest(t, p, "bob", http.MethodDelete, "/admin/webhooks/1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete webhook code = %d", rec.Code)
	}
	rec = adminRequest(t, p, "bob", http.MethodDelete, "/admin/ntfy/1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete ntfy code = %d", rec.Code)
	}
}

func TestAdminDashboardServesHTML(t *testing.T) {
	p, _ := newTestProxy(t)
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	s := p.sessions.New("bob", 0)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: s.ID})
	rec := httptest.NewRecorder()
	p.handleAdminDashboard(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard code = %d, want 200", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q, want text/html", ct)
	}
	if len(rec.Body.String()) < 1000 {
		t.Fatalf("dashboard HTML suspiciously small: %d bytes", len(rec.Body.String()))
	}
}

func TestAdminTokensExposeCreatorAndElement(t *testing.T) {
	p, st := newTestProxy(t)
	// student token: element name resolved from the store via the person
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", PersonID: 5005, DisplayName: "Test Student"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := st.CreateClassToken(&store.ClassToken{
		Token: "creator-probe", School: "testschool", PersonID: 5005,
		ElementType: "STUDENT", ElementID: 5005, Timezone: "Europe/Berlin",
		Days: 30, CreatedAt: 12345, CreatedBy: "bob",
	}); err != nil {
		t.Fatalf("create token: %v", err)
	}
	rec := adminRequest(t, p, "bob", http.MethodGet, "/admin/tokens", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("tokens code = %d (body %s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Tokens []map[string]any `json:"tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	for _, x := range out.Tokens {
		if x["token"] != "creator-probe" {
			continue
		}
		if x["createdBy"] != "bob" {
			t.Errorf("createdBy = %v, want bob", x["createdBy"])
		}
		if x["createdAt"].(float64) != 12345 {
			t.Errorf("createdAt = %v, want 12345", x["createdAt"])
		}
		if x["elementName"] != "Test Student" {
			t.Errorf("elementName = %v, want Test Student (store-backed)", x["elementName"])
		}
		return
	}
	t.Fatalf("probe token missing from listing: %s", rec.Body.String())
}

func TestAdminSearch(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.SaveMasterNames("testschool", "TEACHER", map[int64]string{112: "A. Hartley"}); err != nil {
		t.Fatalf("seed teacher: %v", err)
	}
	if err := st.UpsertUser(&store.User{Username: "kenji", Method: "key", ClassID: 5000, ClassName: "10aR"}); err != nil {
		t.Fatalf("seed class: %v", err)
	}
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", PersonID: 5005, PersonType: 5, DisplayName: "Test Student"}); err != nil {
		t.Fatalf("seed student: %v", err)
	}
	rec := adminRequest(t, p, "bob", http.MethodGet, "/admin/search?school=testschool&q=hart", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("search code = %d (body %s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	foundTeacher := false
	for _, r := range out.Results {
		if r["type"] == "TEACHER" && r["id"].(float64) == 112 && r["name"] == "A. Hartley" {
			foundTeacher = true
		}
	}
	if !foundTeacher {
		t.Fatalf("teacher A. Hartley missing from search: %s", rec.Body.String())
	}
	// type filter: only classes
	rec = adminRequest(t, p, "bob", http.MethodGet, "/admin/search?school=testschool&type=CLASS&q=10a", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("search code = %d", rec.Code)
	}
	out = struct {
		Results []map[string]any `json:"results"`
	}{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if len(out.Results) != 1 || out.Results[0]["id"].(float64) != 5000 {
		t.Fatalf("CLASS search = %s, want only 10aR/5000", rec.Body.String())
	}
	// student source: accounts with a person ID are searchable (by display name)
	rec = adminRequest(t, p, "bob", http.MethodGet, "/admin/search?school=testschool&q=Test", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("search code = %d", rec.Code)
	}
	out = struct {
		Results []map[string]any `json:"results"`
	}{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	foundStudent := false
	for _, r := range out.Results {
		if r["type"] == "STUDENT" && r["id"].(float64) == 5005 && r["name"] == "Test Student" {
			foundStudent = true
		}
	}
	if !foundStudent {
		t.Fatalf("student Test Student missing from search: %s", rec.Body.String())
	}
}

func TestAdminTokenCreateEditRevoke(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", PersonID: 5005, DisplayName: "Test Student"}); err != nil {
		t.Fatalf("seed student: %v", err)
	}
	rec := adminRequest(t, p, "bob", http.MethodPost, "/admin/tokens", `{"school":"testschool","elementType":"STUDENT","elementId":5005,"days":30}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create code = %d (body %s)", rec.Code, rec.Body.String())
	}
	var created struct {
		Token string `json:"token"`
		Name  string `json:"name"`
		Days  int    `json:"days"`
		URL   string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if created.Token == "" || created.Name != "Test Student" || created.Days != 30 {
		t.Fatalf("unexpected create response: %s", rec.Body.String())
	}
	if !strings.HasPrefix(created.URL, "http") || !strings.HasSuffix(created.URL, "/api/calendar/"+created.Token+".ics") {
		t.Fatalf("ics url = %q, want /api/calendar/<token>.ics", created.URL)
	}
	// edit days
	rec = adminRequest(t, p, "bob", http.MethodPost, "/admin/tokens/"+created.Token, `{"days":90}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit code = %d (body %s)", rec.Code, rec.Body.String())
	}
	// listing reflects the change, and existing element names resolve
	rec = adminRequest(t, p, "bob", http.MethodGet, "/admin/tokens", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list code = %d", rec.Code)
	}
	var list struct {
		Tokens []map[string]any `json:"tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	for _, x := range list.Tokens {
		if x["token"] != created.Token {
			continue
		}
		if x["days"].(float64) != 90 {
			t.Errorf("days = %v, want 90 after edit", x["days"])
		}
		if x["elementName"] != "Test Student" {
			t.Errorf("elementName = %v, want Test Student", x["elementName"])
		}
	}
	// revoke
	rec = adminRequest(t, p, "bob", http.MethodDelete, "/admin/tokens/"+created.Token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke code = %d", rec.Code)
	}
	if tok, _ := st.ClassTokenByToken(created.Token); tok != nil {
		t.Fatal("token still present after revoke")
	}
}

func TestAdminWebhookTestButton(t *testing.T) {
	p, st := newTestProxy(t)
	received := make(chan map[string]any, 1)
	sig := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		received <- m
		sig <- r.Header.Get("X-Untis-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	id, _ := st.AddWebhook(&store.Webhook{School: "testschool", ClassID: 5000, URL: srv.URL + "/h", Secret: "sekrit", Enabled: true})
	rec := adminRequest(t, p, "bob", http.MethodPost, "/admin/webhooks/"+strconv.FormatInt(id, 10)+"/test", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("test code = %d (body %s)", rec.Code, rec.Body.String())
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
	body := <-received
	if body["event"] != "test" {
		t.Errorf("event = %v, want test", body["event"])
	}
	if !strings.HasPrefix(<-sig, "sha256=") {
		t.Errorf("signature missing in test webhook delivery")
	}
}

func TestAdminNtfyTestButtonUsesPerTopicBase(t *testing.T) {
	p, st := newTestProxy(t)
	gotTopic := make(chan string, 1)
	gotErr := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		w.WriteHeader(http.StatusOK)
		gotTopic <- r.URL.Path
	}))
	defer srv.Close()

	id, err := st.AddNtfyTopic(&store.NtfyTopic{School: "testschool", ClassID: 0, Topic: "probe-topic", BaseURL: srv.URL, Enabled: true})
	if err != nil {
		t.Fatalf("add topic: %v", err)
	}
	rec := adminRequest(t, p, "bob", http.MethodPost, "/admin/ntfy/"+strconv.FormatInt(id, 10)+"/test", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("test code = %d (body %s)", rec.Code, rec.Body.String())
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
	select {
	case path := <-gotTopic:
		// The per-topic base_url override is honoured, but the JSON envelope is
		// posted to that server's root: ntfy stores a body sent to /<topic> as
		// the message text verbatim, which is the raw-JSON bug.
		if path != "/" {
			t.Errorf("published to %q, want the per-topic server root \"/\"", path)
		}
	case err := <-gotErr:
		t.Fatalf("publish error: %v", err)
	}
}
