package proxy

// Audit trail tests. The trail is append-only and must be attributed to the
// authenticated actor, never to a value from the request body, and it must never
// carry a live credential.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func auditEntries(t *testing.T, p *Proxy) []map[string]any {
	t.Helper()
	rec := adminRequest(t, p, "bob", http.MethodGet, "/admin/audit", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/audit = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad audit json: %v (%s)", err, rec.Body.String())
	}
	return out.Entries
}

func TestAuditRecordsMutationsWithAuthenticatedActor(t *testing.T) {
	p, _ := newTestProxy(t)
	rec := adminRequest(t, p, "bob", http.MethodPost, "/admin/users/alice",
		`{"feature":"editor","allowed":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("mutation = %d: %s", rec.Code, rec.Body.String())
	}

	entries := auditEntries(t, p)
	if len(entries) == 0 {
		t.Fatal("no audit entries after a permission change")
	}
	if got := entries[0]["actor"]; got != "bob" {
		t.Errorf("actor = %v, want the authenticated admin", got)
	}
	if got := entries[0]["action"]; got != "user.perm" {
		t.Errorf("action = %v, want user.perm", got)
	}
	if got := entries[0]["target"]; got != "alice" {
		t.Errorf("target = %v, want alice", got)
	}
	if got, _ := entries[0]["detail"].(string); !strings.Contains(got, "editor=true") {
		t.Errorf("detail = %q, want the value that was set", got)
	}
}

// TestAuditNeverStoresCalendarToken: a token is a bearer credential. The audit
// row names the element instead, so reading the trail cannot hand someone a live
// subscription URL.
func TestAuditNeverStoresCalendarToken(t *testing.T) {
	p, _ := newTestProxy(t)
	rec := adminRequest(t, p, "bob", http.MethodPost, "/admin/tokens",
		`{"elementType":"CLASS","elementId":5000}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create token = %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.Token == "" {
		t.Fatalf("cannot read created token: %v (%s)", err, rec.Body.String())
	}

	entries := auditEntries(t, p)
	found := false
	for _, e := range entries {
		if e["action"] == "token.create" {
			found = true
			if e["target"] != "CLASS 5000" {
				t.Errorf("target = %v, want the element name, not the token", e["target"])
			}
		}
		for _, v := range e {
			if s, _ := v.(string); strings.Contains(s, created.Token) {
				t.Errorf("audit entry contains the live token: %v", e)
			}
		}
	}
	if !found {
		t.Error("token creation was not audited")
	}
}

// TestAuditWebhookKeepsOnlyHost: a chat bridge puts its access token in the URL
// path, so the trail may record the destination host but never the path. The
// same rule protects Discord/Slack/Teams secrets from anyone who can read the
// audit log.
func TestAuditWebhookKeepsOnlyHost(t *testing.T) {
	p, _ := newTestProxy(t)
	rec := adminRequest(t, p, "bob", http.MethodPost, "/admin/webhooks",
		`{"school":"testschool","classId":5000,"url":"https://hooks.example.org/services/T000/B000/live-secret","secret":"s"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create webhook = %d: %s", rec.Code, rec.Body.String())
	}

	entries := auditEntries(t, p)
	found := false
	for _, e := range entries {
		if e["action"] != "webhook.create" {
			continue
		}
		found = true
		if got := e["detail"]; got != "https://hooks.example.org" {
			t.Errorf("webhook audit detail = %v, want scheme and host only", got)
		}
		for _, v := range e {
			if s, _ := v.(string); strings.Contains(s, "live-secret") {
				t.Errorf("audit entry leaks the webhook path secret: %v", e)
			}
		}
	}
	if !found {
		t.Error("webhook creation was not audited")
	}
}

// TestAuditIsScopedBySchool: the dashboard can ask for one school's trail, and a
// global entry (settings) must not appear in a school-scoped query.
func TestAuditIsScopedBySchool(t *testing.T) {
	p, _ := newTestProxy(t)
	adminRequest(t, p, "bob", http.MethodPost, "/admin/schools/settings", `{"allowNew":false}`)
	adminRequest(t, p, "bob", http.MethodPost, "/admin/schools/other/block", `{}`)

	rec := adminRequest(t, p, "bob", http.MethodGet, "/admin/audit?school=other", "")
	var out struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) == 0 {
		t.Fatal("the school-scoped trail is empty")
	}
	for _, e := range out.Entries {
		if e["school"] != "other" {
			t.Errorf("school-scoped audit returned %v", e)
		}
	}
}
