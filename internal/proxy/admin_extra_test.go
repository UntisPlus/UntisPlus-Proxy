package proxy

// adminPerms / adminPool / adminRecon and the per-user permission mutation
// endpoints (/admin/users/{u}). These are what the dashboard uses to grant
// boosted/editor/recon flags and inspect the pool.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"untis-proxy/internal/store"
)

func TestAdminPermsListing(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.SetPerm("dee", store.FeatureBoosted, true); err != nil {
		t.Fatalf("grant: %v", err)
	}
	rec := adminRequest(t, p, "bob", http.MethodGet, "/admin/perms", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Perms []map[string]any `json:"perms"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	found := false
	for _, r := range out.Perms {
		if r["Username"] == "dee" && r["Feature"] == store.FeatureBoosted && r["Allowed"] == true {
			found = true
		}
	}
	if !found {
		t.Errorf("boosted perm for dee missing: %s", rec.Body.String())
	}
}

func TestAdminPoolListing(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", PersonType: 5, PersonID: 7, ClassID: 5000, ClassName: "10aR", Password: "replayable"}); err != nil {
		t.Fatalf("seed class: %v", err)
	}
	rec := adminRequest(t, p, "bob", http.MethodGet, "/admin/pool", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rec.Code, rec.Body.String())
	}
	var out struct {
		School string `json:"school"`
		Pool   []struct {
			ID    int64  `json:"id"`
			Name  string `json:"name"`
			Owner string `json:"owner"`
		} `json:"pool"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if len(out.Pool) != 1 || out.Pool[0].ID != 5000 || out.Pool[0].Name != "10aR" || out.Pool[0].Owner != "dee" {
		t.Errorf("pool = %+v, want single 5000/10aR owned by dee", out.Pool)
	}
}

func TestAdminPermMutation(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	rec := adminRequest(t, p, "bob", http.MethodPost, "/admin/users/dee", `{"feature":"boosted","allowed":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("boost grant code = %d (body %s)", rec.Code, rec.Body.String())
	}
	if ok, _ := st.HasPerm("dee", store.FeatureBoosted); !ok {
		t.Error("dee should hold boosted after grant")
	}

	rec = adminRequest(t, p, "bob", http.MethodPost, "/admin/users/dee", `{"feature":"editor","allowed":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("editor grant code = %d", rec.Code)
	}
	if p.isEditor("dee") != true {
		t.Error("dee should be an editor after grant")
	}

	rec = adminRequest(t, p, "bob", http.MethodPost, "/admin/users/dee", `{"feature":"recon","allowed":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("recon grant code = %d", rec.Code)
	}
	if ok, _ := st.ReconAccess("dee", "TEACHER"); !ok {
		t.Error("dee should have recon access after grant")
	}

	// revoke
	rec = adminRequest(t, p, "bob", http.MethodPost, "/admin/users/dee", `{"feature":"editor","allowed":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("editor revoke code = %d", rec.Code)
	}
	if p.isEditor("dee") == true {
		t.Error("dee should not be an editor after revoke")
	}
}

func TestAdminReconCounts(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.SaveReconElements("testschool", map[string][]int64{
		"TEACHER": {5009, 224},
		"ROOM":    {169},
		"SUBJECT": {},
	}); err != nil {
		t.Fatalf("seed recon elements: %v", err)
	}
	rec := adminRequest(t, p, "bob", http.MethodGet, "/admin/recon", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rec.Code, rec.Body.String())
	}
	var out struct {
		School   string           `json:"school"`
		Teachers int64            `json:"teachers"`
		Rooms    int64            `json:"rooms"`
		Counts   map[string]int64 `json:"counts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v (body=%s)", err, rec.Body.String())
	}
	if out.School != "testschool" {
		t.Errorf("school = %q, want testschool", out.School)
	}
	if out.Teachers != 2 {
		t.Errorf("teachers = %d, want 2 (5009, 224)", out.Teachers)
	}
	if out.Rooms != 1 {
		t.Errorf("rooms = %d, want 1 (169)", out.Rooms)
	}
	if out.Counts["TEACHER"] != 2 {
		t.Errorf("counts[TEACHER] = %d, want 2", out.Counts["TEACHER"])
	}
	if out.Counts["SUBJECT"] != 0 {
		t.Errorf("counts[SUBJECT] = %d, want 0 (seeded empty set)", out.Counts["SUBJECT"])
	}
}

func TestAdminUsersListingShowsPermStatus(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.SetPerm("dee", store.FeatureBoosted, true); err != nil {
		t.Fatalf("grant boosted: %v", err)
	}
	if err := st.SetPerm("dee", store.FeatureEditor, true); err != nil {
		t.Fatalf("grant editor: %v", err)
	}
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	rec := adminRequest(t, p, "bob", http.MethodGet, "/admin/users", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"boosted":true`) || !strings.Contains(rec.Body.String(), `"editor":true`) {
		t.Errorf("user listing missing perm badges: %s", rec.Body.String())
	}
}
