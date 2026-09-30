package store

// Backup tests: a backup must be a complete, restorable copy of a live
// database, and it must never destroy an existing file.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBackupToWritesRestorableCopy(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.SetAdmin("owen", true); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	if err := st.CreateClassToken(&ClassToken{Token: "tok1", School: "testschool", ClassID: 5000, Days: 30}); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	dst := filepath.Join(dir, "backups", "copy.db")
	size, err := st.BackupTo(dst)
	if err != nil {
		t.Fatalf("BackupTo: %v", err)
	}
	if size <= 0 {
		t.Errorf("backup size = %d, want > 0", size)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	if info.Size() != size {
		t.Errorf("reported size %d, file is %d bytes", size, info.Size())
	}
	// A VACUUM INTO result is self-contained: no WAL sidecar may be required.
	for _, sidecar := range []string{dst + "-wal", dst + "-shm"} {
		if _, err := os.Stat(sidecar); err == nil {
			t.Errorf("backup left a %s sidecar behind", filepath.Base(sidecar))
		}
	}

	restored, err := Open(dst)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer restored.Close()
	tok, err := restored.ClassTokenByToken("tok1")
	if err != nil || tok == nil {
		t.Fatalf("token missing from backup: %v", err)
	}
	if tok.ClassID != 5000 {
		t.Errorf("token class = %d, want 5000", tok.ClassID)
	}
}

func TestBackupToRefusesToClobber(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	dst := filepath.Join(dir, "copy.db")
	if err := os.WriteFile(dst, []byte("precious"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if _, err := st.BackupTo(dst); err == nil {
		t.Fatal("BackupTo overwrote an existing file")
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read existing file: %v", err)
	}
	if string(b) != "precious" {
		t.Errorf("existing file was modified: %q", b)
	}
}

func TestBackupToRejectsEmptyPath(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.BackupTo("  "); err == nil {
		t.Fatal("BackupTo accepted an empty path")
	}
}

func TestBackupSuffixIsSortable(t *testing.T) {
	early := BackupSuffix(mustTime(t, "2026-09-27T10:00:00Z"))
	late := BackupSuffix(mustTime(t, "2026-09-28T10:00:00Z"))
	if early >= late {
		t.Errorf("suffixes do not sort chronologically: %s >= %s", early, late)
	}
	if early != "20260927T100000Z" {
		t.Errorf("suffix = %s, want 20260927T100000Z", early)
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time: %v", err)
	}
	return v
}
