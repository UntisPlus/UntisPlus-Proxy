package proxy

// §6 – Permission tier tests via the /me endpoint.
// Each test creates exactly one user, exercises one specific perm combination,
// and asserts the level string returned by handleMe.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"untis-proxy/internal/store"
)

// meLevel calls handleMe with a session for username and returns the "level"
// field from the JSON response.
func meLevel(t *testing.T, p *Proxy, username string) (level string, code int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	sess := p.sessions.New(username, 0)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: sess.ID})
	rec := httptest.NewRecorder()
	p.handleMe(rec, req)
	code = rec.Code
	if code == http.StatusOK {
		var out struct {
			Level string `json:"level"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("handleMe: bad JSON: %v (body=%s)", err, rec.Body.String())
		}
		level = out.Level
	}
	return
}

// TestMe_Basic: a freshly-created user with no perms must report "basic".
func TestMe_Basic(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "plain", Method: "key"}); err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	if lvl, code := meLevel(t, p, "plain"); code != 200 || lvl != "basic" {
		t.Errorf("got level=%q code=%d, want basic/200", lvl, code)
	}
}

// TestMe_Recon: granting the recon flag must produce level "recon".
func TestMe_Recon(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "reconuser", Method: "key"}); err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	if err := st.SetReconOverride("reconuser", "", true); err != nil {
		t.Fatalf("grant recon: %v", err)
	}
	if lvl, _ := meLevel(t, p, "reconuser"); lvl != "recon" {
		t.Errorf("got %q, want recon", lvl)
	}
}

// TestMe_Boosted: granting the boosted flag must produce level "boosted".
func TestMe_Boosted(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "boostuser", Method: "key"}); err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	if err := st.SetBoostedFlag("boostuser", true); err != nil {
		t.Fatalf("grant boosted: %v", err)
	}
	if lvl, _ := meLevel(t, p, "boostuser"); lvl != "boosted" {
		t.Errorf("got %q, want boosted", lvl)
	}
}

// TestMe_Editor: granting only the editor flag (no boosted/recon) must
// produce level "editor".
func TestMe_Editor(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "editoruser", Method: "key"}); err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	if err := st.SetPerm("editoruser", store.FeatureEditor, true); err != nil {
		t.Fatalf("grant editor: %v", err)
	}
	if lvl, _ := meLevel(t, p, "editoruser"); lvl != "boosted+editor" {
		t.Errorf("got %q, want boosted+editor", lvl)
	}
}

// TestMe_BoostedPlusEditor: boosted AND editor must produce "boosted+editor".
func TestMe_BoostedPlusEditor(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "beholder", Method: "key"}); err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	if err := st.SetBoostedFlag("beholder", true); err != nil {
		t.Fatalf("grant boosted: %v", err)
	}
	if err := st.SetPerm("beholder", store.FeatureEditor, true); err != nil {
		t.Fatalf("grant editor: %v", err)
	}
	if lvl, _ := meLevel(t, p, "beholder"); lvl != "boosted+editor" {
		t.Errorf("got %q, want boosted+editor", lvl)
	}
}

// TestMe_ReconXORBoosted: granting boosted after recon must automatically
// revoke recon so the level becomes "boosted" (XOR semantics).
func TestMe_ReconXORBoosted(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "xoruser", Method: "key"}); err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	if err := st.SetReconOverride("xoruser", "", true); err != nil {
		t.Fatalf("grant recon: %v", err)
	}
	// Sanity: currently recon.
	if lvl, _ := meLevel(t, p, "xoruser"); lvl != "recon" {
		t.Fatalf("precondition: want recon, got %q", lvl)
	}
	// Upgrade to boosted → recon must be revoked.
	if err := st.SetBoostedFlag("xoruser", true); err != nil {
		t.Fatalf("grant boosted: %v", err)
	}
	if lvl, _ := meLevel(t, p, "xoruser"); lvl != "boosted" {
		t.Errorf("after boosted grant: got %q, want boosted (recon must be revoked)", lvl)
	}
}

// TestMe_RevokeAllResetsToBasic: RevokeAll must bring every user back to
// "basic" regardless of what was granted.
func TestMe_RevokeAllResetsToBasic(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "allgrant", Method: "key"}); err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	if err := st.SetBoostedFlag("allgrant", true); err != nil {
		t.Fatalf("grant boosted: %v", err)
	}
	if err := st.SetPerm("allgrant", store.FeatureEditor, true); err != nil {
		t.Fatalf("grant editor: %v", err)
	}
	if _, err := st.RevokeAll(); err != nil {
		t.Fatalf("revoke all: %v", err)
	}
	if lvl, _ := meLevel(t, p, "allgrant"); lvl != "basic" {
		t.Errorf("after RevokeAll: got %q, want basic", lvl)
	}
}

// TestMe_Unauthenticated: a request with no session cookie must return 401.
func TestMe_Unauthenticated(t *testing.T) {
	p, _ := newTestProxy(t)
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	rec := httptest.NewRecorder()
	p.handleMe(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("got %d, want 401 for no session", rec.Code)
	}
}

// TestMe_PermissionsMap: the /me response must include a "permissions" object
// with boolean keys that reflect the actual perm state.
func TestMe_PermissionsMap(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "permsmap", Method: "key"}); err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	if err := st.SetBoostedFlag("permsmap", true); err != nil {
		t.Fatalf("grant boosted: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	sess := p.sessions.New("permsmap", 0)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: sess.ID})
	rec := httptest.NewRecorder()
	p.handleMe(rec, req)

	var out struct {
		Level       string          `json:"level"`
		Permissions map[string]bool `json:"permissions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad JSON: %v (body=%s)", err, rec.Body.String())
	}
	if !out.Permissions["boosted"] {
		t.Errorf("permissions.boosted = false, want true")
	}
	if out.Permissions["recon"] {
		t.Errorf("permissions.recon = true, want false (XOR)")
	}
	if out.Permissions["editor"] {
		t.Errorf("permissions.editor = true, want false (not granted)")
	}
}
