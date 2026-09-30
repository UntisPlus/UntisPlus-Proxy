package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The calendar/subscription rows the admin dashboard lists live in the same
// database as the real accounts, so a test that opened the production file
// would leave rows behind that look exactly like real subscriptions. The guard
// in Open makes that impossible; these tests pin the behaviour so it cannot be
// dropped by accident.

// TestOpenRefusesRepoRelativePath: the mistake that matters is passing a
// repo-relative "data/untis.db" or the bare "untis.db" default from a test.
func TestOpenRefusesRepoRelativePath(t *testing.T) {
	// Resolve the real database from this package's location rather than
	// hardcoding an absolute path. The check only means anything if it points at
	// the actual file, and that path moves every time the checkout is renamed or
	// cloned somewhere else. A test run always has the package directory as its
	// working directory, so two levels up is the repository root.
	live, err := filepath.Abs(filepath.Join("..", "..", "data", "untis.db"))
	if err != nil {
		t.Fatalf("resolve live database path: %v", err)
	}
	for _, path := range []string{"data/untis.db", "untis.db", live} {
		st, err := Open(path)
		if err == nil {
			st.Close()
			t.Errorf("Open(%q) succeeded under test, want refusal", path)
			continue
		}
		if !strings.Contains(err.Error(), "temporary directory") {
			t.Errorf("Open(%q) error = %v, want it to mention the temporary-directory requirement", path, err)
		}
	}
}

// TestOpenRefusesPathOutsideTempDir: a temp file somewhere else on disk is
// also refused — the rule is "use t.TempDir()", not merely "not the repo".
func TestOpenRefusesPathOutsideTempDir(t *testing.T) {
	// A path directly in the user's home is outside the temp root, so it must be
	// refused even though it is a plausible-looking location.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	if strings.HasPrefix(home, os.TempDir()) {
		t.Skip("home is inside the temp dir, cannot construct the case")
	}
	path := filepath.Join(home, ".untis-proxy-should-not-exist.db")
	st, err := Open(path)
	if err == nil {
		st.Close()
		os.Remove(path)
		t.Errorf("Open(%q) succeeded under test, want refusal", path)
	}
}

// TestOpenAllowsTempDir: the guard must not break the normal test path.
func TestOpenAllowsTempDir(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open in t.TempDir() = %v, want success", err)
	}
	defer st.Close()
	if err := st.SetDefaultSchool("testschool"); err != nil {
		t.Errorf("SetDefaultSchool: %v", err)
	}
}
