package store

import (
	"database/sql"
	"errors"
	"time"
)

const accountDefaultSchool = "account.default-school"

var ErrAccountSchool = errors.New("account is unavailable in this school")

// SetDefaultSchool binds pre-multi-school rows at startup. An enrollment
// can predate the user's first login, so it may have only a secrets row.
func (s *Store) SetDefaultSchool(school string) error {
	if school == "" {
		return ErrAccountSchool
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO settings (key,value) VALUES (?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, accountDefaultSchool, school); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE users SET school=? WHERE school=''`, school); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE secrets SET school=COALESCE(
		(SELECT school FROM users WHERE username=secrets.username), ?) WHERE school=''`, school); err != nil {
		return err
	}
	return tx.Commit()
}

// SaveLoginCredential binds a verified upstream username (globally unique
// in this schema) to one school, even for an OTP-only login without a secret.
// Only call this after authentication. The single write serializes concurrent
// first logins, enrollment and updates against the same ownership checks.
func (s *Store) SaveLoginCredential(school, username, secret, password string) error {
	username = norm(username)
	if school == "" || username == "" || username == "#anonymous#" {
		return ErrAccountSchool
	}
	res, err := s.db.Exec(`INSERT INTO secrets (username, secret, password, updated_at, school)
		SELECT ?,?,?,?,? WHERE NOT EXISTS (
			SELECT 1 FROM users WHERE username=? COLLATE NOCASE AND school<>?
		)
		ON CONFLICT(username) DO UPDATE SET
			secret=CASE WHEN excluded.secret<>'' THEN excluded.secret ELSE secrets.secret END,
			password=CASE WHEN excluded.password<>'' THEN excluded.password ELSE secrets.password END,
			school=excluded.school, updated_at=excluded.updated_at
		WHERE COALESCE(NULLIF(secrets.school,''), (SELECT NULLIF(school,'') FROM users WHERE username=excluded.username),
			(SELECT value FROM settings WHERE key=?))=excluded.school`,
		username, secret, password, time.Now().Unix(), school, username, school, accountDefaultSchool)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrAccountSchool
	}
	return nil
}

// SchoolCredential reads only replay material owned by the requested school.
// Unbound rows written by older administrative tools use the recorded user
// school or, for enrollment-only rows, the configured legacy default.
func (s *Store) SchoolCredential(school, username string) (secret, password string, err error) {
	if school == "" {
		return "", "", ErrAccountSchool
	}
	err = s.db.QueryRow(`SELECT s.secret, s.password FROM secrets s
		LEFT JOIN users u ON u.username=s.username
		WHERE s.username=? AND (u.username IS NULL OR u.school=?)
		AND COALESCE(NULLIF(s.school,''), NULLIF(u.school,''), (SELECT value FROM settings WHERE key=?))=?`,
		norm(username), school, accountDefaultSchool, school).Scan(&secret, &password)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return secret, password, err
}
