package store

// Backups. The database holds every account secret, calendar token and
// notification subscription, so a copy has to be taken with SQLite's own
// consistent-snapshot machinery (VACUUM INTO) rather than by copying the file:
// with WAL journaling a plain file copy can miss committed pages that still live
// in the -wal sidecar.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// BackupTo writes a defragmented, self-contained copy of the database to path.
// It is safe to run against a live server: WAL readers are not blocked, and the
// output is a single file with no -wal/-shm sidecar, so it can be copied off the
// host or restored by hand.
//
// It refuses to overwrite an existing file, because silently clobbering a backup
// is exactly the failure a backup command must not have.
func (s *Store) BackupTo(path string) (int64, error) {
	if strings.TrimSpace(path) == "" {
		return 0, errors.New("backup: destination path is empty")
	}
	if _, err := os.Stat(path); err == nil {
		return 0, fmt.Errorf("backup: %s already exists", path)
	} else if !os.IsNotExist(err) {
		return 0, fmt.Errorf("backup: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return 0, fmt.Errorf("backup: create %s: %w", dir, err)
		}
	}
	// VACUUM INTO takes no bound parameters, so the path is quoted as an
	// identifier; doubling embedded quotes is SQLite's escape for them.
	if _, err := s.db.Exec(`VACUUM INTO "` + strings.ReplaceAll(path, `"`, `""`) + `"`); err != nil {
		return 0, fmt.Errorf("backup: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("backup: %w", err)
	}
	return info.Size(), nil
}

// BackupSuffix returns the timestamp suffix used for automatic backup file
// names, so operators can recognise (and prune) them.
func BackupSuffix(now time.Time) string {
	return now.UTC().Format("20060102T150405Z")
}
