package main

import (
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
