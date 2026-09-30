package main

// untisctl backup tests. pruneBackups deletes files, so its blast radius is
// pinned down here: only the automatic backup names of the given database may
// ever be removed.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPruneBackupsKeepsNewestAndLeavesOthersAlone(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "untis.db")
	keep := map[string]string{
		"untis.db.untis-backup-20260101T000000Z.db":  "old backup",
		"untis.db.untis-backup-20260202T000000Z.db":  "backup",
		"untis.db.untis-backup-20260303T000000Z.db":  "backup",
		"other.db.untis-backup-20260101T000000Z.db":  "another database",
		"untis.db.backup-manual.db":                  "hand-made copy",
		"untis.db.untis-backup-20260404T000000Z.tmp": "partial write",
		"notes.txt": "unrelated",
	}
	for name, body := range keep {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "untis.db.untis-backup-20260505T000000Z.db"), 0o750); err != nil {
		t.Fatalf("seed dir: %v", err)
	}

	pruneBackups(db, 2)

	// Four candidates match the naming scheme (the fifth is a directory), and
	// the two newest file backups are kept.
	gone := []string{"untis.db.untis-backup-20260101T000000Z.db"}
	for _, name := range gone {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s should have been pruned", name)
		}
	}
	for name, body := range keep {
		if contains(gone, name) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			if os.IsNotExist(err) && name == "untis.db.untis-backup-20260505T000000Z.db" {
				continue
			}
			t.Errorf("%s: %v", name, err)
			continue
		}
		if string(b) != body {
			t.Errorf("%s was modified: %q", name, b)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "untis.db.untis-backup-20260505T000000Z.db")); err != nil {
		t.Errorf("a directory named like a backup should be left alone: %v", err)
	}
}

func TestPruneBackupsBelowCountIsNoop(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "untis.db")
	name := "untis.db.untis-backup-20260101T000000Z.db"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("only"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	pruneBackups(db, 5)
	if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
		t.Errorf("backup was pruned even though it is under the limit: %v", err)
	}
	pruneBackups(db, 0)
	if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
		t.Errorf("--keep 0 must keep everything: %v", err)
	}
}

func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{2048, "2.0 KiB"},
		{5 * 1024 * 1024, "5.0 MiB"},
	} {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
