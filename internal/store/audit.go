package store

import (
	"database/sql"
	"time"
)

// AdminAudit is one recorded administrative action. It is append-only: an entry
// answers who changed what and when, and is never edited or removed by the
// application.
type AdminAudit struct {
	ID     int64
	At     time.Time
	Actor  string
	School string
	Action string
	Target string
	Detail string
}

// LogAdminAudit appends one action to the audit trail. Callers must pass the
// authenticated actor; a value from the request body would let a caller forge
// the trail.
func (s *Store) LogAdminAudit(actor, school, action, target, detail string) error {
	_, err := s.db.Exec(`INSERT INTO admin_audit (at, actor, school, action, target, detail)
		VALUES (?,?,?,?,?,?)`, time.Now().Unix(), actor, school, action, target, detail)
	return err
}

// AdminAuditEntries returns the newest audit entries first, optionally scoped to
// one school. limit is clamped so a dashboard request cannot ask for the whole
// trail at once.
func (s *Store) AdminAuditEntries(school string, limit int) ([]AdminAudit, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	q := `SELECT id, at, actor, school, action, target, detail FROM admin_audit`
	var args []any
	if school != "" {
		q += ` WHERE school=?`
		args = append(args, school)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminAudit
	for rows.Next() {
		var e AdminAudit
		var at int64
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.School, &e.Action, &e.Target, &e.Detail); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// CountAdminAudit reports how many entries exist, optionally for one school. It
// is used by tests and by a status card without paying for the full list.
func (s *Store) CountAdminAudit(school string) (int, error) {
	q := `SELECT COUNT(*) FROM admin_audit`
	var args []any
	if school != "" {
		q += ` WHERE school=?`
		args = append(args, school)
	}
	var n int
	err := s.db.QueryRow(q, args...).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return n, err
}
