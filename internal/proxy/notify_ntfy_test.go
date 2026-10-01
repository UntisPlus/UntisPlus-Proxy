package proxy

// ntfy publishing tests. The regression these guard against: publishing the JSON
// envelope to /<topic> instead of the server root. ntfy accepts that call with
// HTTP 200 and then stores the whole envelope as the message text, so every
// notification arrives as raw JSON and the digest is invisible to the reader.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"untis-proxy/internal/store"
)

// recordingNtfy stands in for an ntfy server: it records the path and body it
// was called with so a test can assert where the envelope went.
type recordingNtfy struct {
	path string
	body string
	hdr  http.Header
}

func (r *recordingNtfy) server(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.path = req.URL.Path
		r.body = string(b)
		r.hdr = req.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	SetNtfyBase(srv.URL)
	t.Cleanup(func() { SetNtfyBase("https://ntfy.sh") })
}

// TestNtfyPostGoesToServerRoot: the envelope must be posted to the root url.
// ntfy only parses JSON at the root; at /<topic> it stores the body verbatim.
func TestNtfyPostGoesToServerRoot(t *testing.T) {
	rec := &recordingNtfy{}
	rec.server(t)

	if _, err := ntfyPost(&store.NtfyTopic{Topic: "untis"}, map[string]any{
		"topic": "untis", "title": "t", "message": "m",
	}); err != nil {
		t.Fatalf("ntfyPost: %v", err)
	}
	if rec.path != "/" {
		t.Errorf("posted to %q, want the server root \"/\" (a /<topic> path is stored as plain text)", rec.path)
	}
	if ct := rec.hdr.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(rec.body), &sent); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, rec.body)
	}
	if sent["topic"] != "untis" {
		t.Errorf("topic = %v, want untis", sent["topic"])
	}
}

// TestNtfyPostHonoursPerTopicServer: a topic's own base_url override wins over
// the global default.
func TestNtfyPostHonoursPerTopicServer(t *testing.T) {
	rec := &recordingNtfy{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rec.path = req.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if _, err := ntfyPost(&store.NtfyTopic{Topic: "untis", BaseURL: srv.URL + "/"}, map[string]any{
		"topic": "untis", "message": "m",
	}); err != nil {
		t.Fatalf("ntfyPost: %v", err)
	}
	if rec.path != "/" {
		t.Errorf("posted to %q, want the override's root \"/\"", rec.path)
	}
}

// TestTestNtfyPostsToRoot: the dashboard's "send test" button must not
// reintroduce the topic-url form.
func TestTestNtfyPostsToRoot(t *testing.T) {
	rec := &recordingNtfy{}
	rec.server(t)

	status, err := (&Proxy{}).testNtfy(&store.NtfyTopic{Topic: "untis"})
	if err != nil {
		t.Fatalf("testNtfy: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if rec.path != "/" {
		t.Errorf("posted to %q, want the server root \"/\"", rec.path)
	}
	if got := rec.body; !json.Valid([]byte(got)) {
		t.Errorf("body is not JSON: %s", got)
	}
}

// TestTestNtfyCarriesTitleAndTags: the test notification must exercise the same
// fields a real one does, so the dashboard button proves the notification
// renders rather than only proving the socket is open.
func TestTestNtfyCarriesTitleAndTags(t *testing.T) {
	rec := &recordingNtfy{}
	rec.server(t)

	if _, err := (&Proxy{}).testNtfy(&store.NtfyTopic{School: "testschool", Topic: "untis"}); err != nil {
		t.Fatalf("testNtfy: %v", err)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(rec.body), &sent); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if title, _ := sent["title"].(string); !strings.Contains(title, "untis") {
		t.Errorf("title = %q, want it to name the topic", title)
	}
	tags, _ := sent["tags"].([]any)
	if len(tags) == 0 {
		t.Error("tags missing, want the test to exercise tags like a real notification")
	}
	if msg, _ := sent["message"].(string); strings.Contains(msg, "{") {
		t.Errorf("message looks like raw JSON: %q", msg)
	}
}

// TestPublicURLFollowsConfig: the notification click target must come from
// -public-base, not a hostname baked into the binary. Hardcoding one
// deployment's domain leaks it to every reader of the repo and sends other
// operators' users to somebody else's site.
func TestPublicURLFollowsConfig(t *testing.T) {
	old := publicBase
	t.Cleanup(func() { publicBase = old })

	SetPublicBase("")
	if got := publicURL("/api/x"); got != "/api/x" {
		t.Errorf("unset publicBase: got %q, want the bare path", got)
	}

	SetPublicBase("https://proxy.example.org/")
	if got := publicURL("/api/x"); got != "https://proxy.example.org/api/x" {
		t.Errorf("got %q, want https://proxy.example.org/api/x", got)
	}
	// An empty value must not clobber a configured one.
	SetPublicBase("")
	if got := publicURL("/api/x"); got != "https://proxy.example.org/api/x" {
		t.Errorf("empty SetPublicBase overwrote the configured base: %q", got)
	}
}

// TestPublishNtfyClickUsesPublicBase: the delivered payload must carry the
// configured host.
func TestPublishNtfyClickUsesPublicBase(t *testing.T) {
	old := publicBase
	t.Cleanup(func() { publicBase = old })
	SetPublicBase("https://proxy.example.org")

	rec := &recordingNtfy{}
	rec.server(t)

	if err := (&Proxy{}).publishNtfyOnce(&store.NtfyTopic{Topic: "t"}, "school", 1234,
		changeDigest{Title: "class 12x", Summary: "1 added", Lines: []string{"Thu 01.10. 09:50–10:35 · Mathe"}, Changed: 1, Added: 1}); err != nil {
		t.Fatalf("publishNtfyOnce: %v", err)
	}

	var sent map[string]any
	if err := json.Unmarshal([]byte(rec.body), &sent); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	click, _ := sent["click"].(string)
	if !strings.HasPrefix(click, "https://proxy.example.org/") {
		t.Errorf("click = %q, want it built from -public-base", click)
	}
	// A prefix check is the whole assertion: anything derived from another host
	// would not start with the configured public base. Naming a real host here
	// would re-commit the very value this test exists to keep out of the
	// source, so the check stays generic.
	if strings.Contains(click, "webuntis.com") {
		t.Errorf("click = %q points upstream instead of at the public base", click)
	}
}

// the dashboard list what looks like two subscriptions to the same server.
func TestCleanNtfyBase(t *testing.T) {
	cases := map[string]string{
		"https://ntfy.example.org/":     "https://ntfy.example.org",
		"https://ntfy.example.org":      "https://ntfy.example.org",
		"https://ntfy.example.org///":   "https://ntfy.example.org",
		"  https://ntfy.example.org/  ": "https://ntfy.example.org",
		"https://ntfy.example.org/a/b/": "https://ntfy.example.org/a/b",
		"":                              "", // empty still means "use the default"
		"   ":                           "",
	}
	for in, want := range cases {
		if got := cleanNtfyBase(in); got != want {
			t.Errorf("cleanNtfyBase(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAdminDashboardDefinesTestNtfy: the ntfy table's test button calls
// testNtfy(...) from admin.html. The function was never defined there, so the
// button threw a ReferenceError and did nothing at all.
func TestAdminDashboardDefinesTestNtfy(t *testing.T) {
	b, err := os.ReadFile("static/admin.html")
	if err != nil {
		t.Fatalf("read dashboard: %v", err)
	}
	html := string(b)
	if !strings.Contains(html, "onclick=\"testNtfy(") {
		t.Fatal("the ntfy test button no longer calls testNtfy, update this test")
	}
	if !strings.Contains(html, "async function testNtfy(") {
		t.Error("admin.html calls testNtfy() but never defines it: the button is dead")
	}
}
