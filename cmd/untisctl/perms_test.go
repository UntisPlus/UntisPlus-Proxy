package main

import (
	"path/filepath"
	"testing"

	"untis-proxy/internal/store"
)

// TestTechnikerIsGrantableFromCLI pins M2: an operator managing permissions from
// untisctl must be able to provision the Technik feature, and only per-user,
// because the aggregate pools several students' private events.
func TestTechnikerIsGrantableFromCLI(t *testing.T) {
	for _, alias := range []string{"techniker", "technik"} {
		typ, ok := reconTypes[alias]
		if !ok {
			t.Fatalf("reconTypes has no %q alias", alias)
		}
		if typ != store.FeatureTechniker {
			t.Errorf("reconTypes[%q] = %q, want %q", alias, typ, store.FeatureTechniker)
		}
	}
	if !perUserOnly[store.FeatureTechniker] {
		t.Error("techniker must be per-user only, not a global switch")
	}
}

// TestDefaultPermsCLI pins that the default permission set is fully manageable
// from untisctl, independently of any individual user.
func TestDefaultPermsCLI(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// The shipped default is recon on.
	defaults, err := st.DefaultPerms()
	if err != nil || !defaults[store.FeatureRecon] {
		t.Fatalf("shipped defaults = (%v, %v), want recon on", defaults, err)
	}

	cmdDefaultPerms(st, []string{"clear"})
	defaults, _ = st.DefaultPerms()
	for _, feature := range defaultPermOrder {
		if defaults[feature] {
			t.Fatalf("after clear, %s still defaulted on", feature)
		}
	}

	cmdDefaultPerms(st, []string{"grant", "boosted"})
	cmdDefaultPerms(st, []string{"grant", "editor"})
	defaults, _ = st.DefaultPerms()
	if !defaults[store.FeatureBoosted] || !defaults[store.FeatureEditor] {
		t.Fatalf("defaults after grants = %v, want boosted+editor", defaults)
	}

	cmdDefaultPerms(st, []string{"revoke", "editor"})
	defaults, _ = st.DefaultPerms()
	if defaults[store.FeatureEditor] {
		t.Fatal("revoke editor did not remove it from the defaults")
	}

	// Granting recon must keep the recon/boosted exclusion.
	cmdDefaultPerms(st, []string{"grant", "recon"})
	defaults, _ = st.DefaultPerms()
	if !defaults[store.FeatureRecon] || defaults[store.FeatureBoosted] {
		t.Fatalf("defaults after recon = %v, want recon only", defaults)
	}
}

// TestAnonymousCLI covers the deployment-wide capability toggles, including the
// new pooled-class fallback and its singular alias.
func TestAnonymousCLI(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cmdAnonymous(st, []string{"grant", "classes"})
	if on, _ := st.AnonymousFeature("classes"); !on {
		t.Fatal("anonymous classes stayed off after grant")
	}
	cmdAnonymous(st, []string{"revoke", "class"})
	if on, _ := st.AnonymousFeature("classes"); on {
		t.Fatal("anonymous classes stayed on after revoke")
	}

	cmdAnonymous(st, []string{"grant", "ntfy"})
	cmdAnonymous(st, []string{"grant", "webhooks"})
	cmdAnonymous(st, []string{"grant", "changes"})
	features, err := st.AnonymousFeatures()
	if err != nil {
		t.Fatal(err)
	}
	for _, feature := range []string{"ntfy", "webhooks", "changes"} {
		if !features[feature] {
			t.Fatalf("anonymous %s stayed off after grant", feature)
		}
	}
	if features["classes"] {
		t.Fatal("granting other capabilities turned classes back on")
	}
}
