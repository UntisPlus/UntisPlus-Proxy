package store

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// norm normalizes a username so the same account is always keyed identically
// regardless of how the client capitalizes it (usernames are case-insensitive).
func norm(username string) string {
	return strings.ToLower(strings.TrimSpace(username))
}

// userCols lists the users table columns in scan order (scanUser/scanUserRow).
const userCols = "id,username,password,method,person_id,person_type,class_id,class_name,email,display_name,created_at,last_seen,school,admin"

type User struct {
	ID          int64
	Username    string
	Password    string
	Method      string // "password" or "key"
	PersonID    int64
	PersonType  int64
	ClassID     int64
	ClassName   string
	Email       string
	DisplayName string
	School      string
	Admin       bool
	CreatedAt   time.Time
	LastSeen    time.Time
}

type Class struct {
	ID   int64
	Name string
}

// ClassToken binds an opaque calendar subscription token to a school and
// either a class (ClassID > 0) or a personal student timetable (PersonID > 0).
type ClassToken struct {
	Token       string
	School      string
	ClassID     int64
	PersonID    int64
	ElementType string
	ElementID   int64
	Timezone    string
	Days        int
	CreatedAt   int64
	LastAccess  int64
	CreatedBy   string
}

type Store struct {
	db *sql.DB
}

// underTest reports whether this process was started by `go test`. A test binary
// defines the -test.* flag set, which nothing else does.
func underTest() bool {
	return flag.Lookup("test.v") != nil
}

// Open opens (creating if needed) the sqlite database at path.
//
// Under `go test` the path must live in a temporary directory. Tests get their
// database from t.TempDir(), which is removed when the test finishes, so a test
// can neither leave rows behind for the dashboard to display nor write to the
// real database by passing a repo-relative path such as "data/untis.db". The
// check is the guard rather than a cleanup step: a test that quietly targeted
// production data would otherwise be invisible until someone noticed a stray
// calendar token in the admin UI.
func Open(path string) (*Store, error) {
	if underTest() {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		tmp, err := filepath.Abs(os.TempDir())
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(abs, tmp+string(filepath.Separator)) {
			return nil, fmt.Errorf("refusing to open %q under test: a test database must live in a temporary directory (t.TempDir()), not in %s", path, tmp)
		}
	}
	db, err := sql.Open("sqlite", path+
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT NOT NULL UNIQUE,
		password TEXT NOT NULL DEFAULT '',
		method TEXT NOT NULL DEFAULT 'password',
		person_id INTEGER NOT NULL DEFAULT 0,
		person_type INTEGER NOT NULL DEFAULT 0,
		class_id INTEGER NOT NULL DEFAULT 0,
		class_name TEXT NOT NULL DEFAULT '',
		email TEXT NOT NULL DEFAULT '',
		display_name TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL DEFAULT 0,
		last_seen INTEGER NOT NULL DEFAULT 0,
		school TEXT NOT NULL DEFAULT '',
		admin INTEGER NOT NULL DEFAULT 0
	)`)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS secrets (
		username TEXT NOT NULL PRIMARY KEY,
		secret TEXT NOT NULL,
		updated_at INTEGER NOT NULL
	)`)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS perms (
		username TEXT NOT NULL,
		feature TEXT NOT NULL,
		allowed INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (username, feature)
	)`)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS recon_elements (
		el_type TEXT NOT NULL,
		el_id INTEGER NOT NULL,
		PRIMARY KEY (el_type, el_id)
	)`)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS recon_scan (
		school TEXT NOT NULL,
		class_id INTEGER NOT NULL,
		scan_until TEXT NOT NULL,
		PRIMARY KEY (school, class_id)
	)`)
	if err != nil {
		return nil, err
	}
	// element_names is the school-wide name catalog from the upstream master
	// data. It is deliberately separate from recon_elements, which holds only
	// the elements the pooled classes actually reference: mixing them made
	// "reconstructed" mean "every element the school has", so every teacher and
	// room showed up as selectable while most of them had no data to load.
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS element_names (
		el_type TEXT NOT NULL,
		el_id INTEGER NOT NULL,
		school TEXT NOT NULL DEFAULT '',
		name TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (el_type, el_id)
	)`)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS schools (
		school TEXT NOT NULL PRIMARY KEY,
		added_at INTEGER NOT NULL DEFAULT 0,
		last_seen INTEGER NOT NULL DEFAULT 0
	)`)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS settings (
		key TEXT NOT NULL PRIMARY KEY,
		value TEXT NOT NULL DEFAULT ''
	)`)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS webhooks (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		school TEXT NOT NULL DEFAULT '',
		class_id INTEGER NOT NULL DEFAULT 0,
		url TEXT NOT NULL,
		secret TEXT NOT NULL DEFAULT '',
		enabled INTEGER NOT NULL DEFAULT 1,
		created_by TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL DEFAULT 0
	)`)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS ntfy_topics (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		school TEXT NOT NULL DEFAULT '',
		class_id INTEGER NOT NULL DEFAULT 0,
		topic TEXT NOT NULL,
		enabled INTEGER NOT NULL DEFAULT 1,
		created_by TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL DEFAULT 0
	)`)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS class_tokens (
		token TEXT NOT NULL PRIMARY KEY,
		school TEXT NOT NULL,
		class_id INTEGER NOT NULL DEFAULT 0,
		person_id INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		last_access INTEGER NOT NULL DEFAULT 0
	)`)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS timetable_versions (
		school TEXT NOT NULL,
		class_id INTEGER NOT NULL,
		version INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (school, class_id)
	)`)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS timetable_changes (
		school TEXT NOT NULL,
		class_id INTEGER NOT NULL,
		period_id INTEGER NOT NULL,
		kind TEXT NOT NULL DEFAULT 'ADDED',
		start TEXT NOT NULL DEFAULT '',
		end TEXT NOT NULL DEFAULT '',
		subject TEXT NOT NULL DEFAULT '',
		room TEXT NOT NULL DEFAULT '',
		description TEXT NOT NULL DEFAULT '',
		mod_ver INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (school, class_id, period_id)
	)`)
	if err != nil {
		return nil, err
	}
	// notification_outbox makes delivery survive a failed send. One row per
	// destination, written inside the same transaction that stamps the new
	// class version, so a change can never be committed without a delivery
	// being queued for it.
	//
	// Per-destination rows, not one row per change: a single class change fans
	// out to every matching webhook and ntfy topic, and if delivery were
	// retried as one unit then one permanently broken destination would replay
	// the whole fan-out forever — the same duplicate-notification failure the
	// re-notification flood in v1.4.4 was.
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS notification_outbox (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		school TEXT NOT NULL,
		class_id INTEGER NOT NULL,
		version INTEGER NOT NULL,
		dest TEXT NOT NULL,          -- 'webhook' or 'ntfy'
		dest_id INTEGER NOT NULL,    -- row id in webhooks / ntfy_topics
		payload TEXT NOT NULL,       -- exact bytes to send
		state TEXT NOT NULL DEFAULT 'pending', -- pending | sending | dead
		attempts INTEGER NOT NULL DEFAULT 0,
		last_error TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL DEFAULT 0,
		next_attempt_at INTEGER NOT NULL DEFAULT 0
	)`)
	if err != nil {
		return nil, err
	}
	if _, err = db.Exec(`CREATE INDEX IF NOT EXISTS notification_outbox_pending
		ON notification_outbox (state, next_attempt_at, id)`); err != nil {
		return nil, err
	}
	// homework_done is the student's own "I finished this" flag, kept by the
	// proxy rather than upstream on purpose: Untis' own `completed` field is
	// the teacher's decision for the whole class, so writing it would either
	// fail or overwrite a teacher's judgement. Keyed by the upstream homework id
	// and by viewer, so two students in the same class never see each other's
	// answers on a shared timetable URL.
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS homework_done (
		school TEXT NOT NULL,
		username TEXT NOT NULL,
		hw_id INTEGER NOT NULL,
		done_at INTEGER NOT NULL,
		PRIMARY KEY (school, username, hw_id)
	)`)
	if err != nil {
		return nil, err
	}
	// absence_notes is the student's own private note on one absence. There is no
	// upstream field for it: an absence carries `text`, which is the *teacher's*
	// comment, and `excuse.text`, which is upstream's own excuse text. Neither can
	// be reused, so the note lives here. Keyed by the upstream absence id and by
	// viewer, so a note is never visible on another student's record.
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS absence_notes (
		school TEXT NOT NULL,
		username TEXT NOT NULL,
		absence_key INTEGER NOT NULL,
		note TEXT NOT NULL DEFAULT '',
		updated_at INTEGER NOT NULL,
		PRIMARY KEY (school, username, absence_key)
	)`)
	if err != nil {
		return nil, err
	}
	// student_events is one hand-authored event on one student's schedule, created
	// by an admin. Upstream has no such concept — a Technik appointment, a study
	// group, a doctor's slot — so the row is entirely proxy-owned.
	//
	// Keyed by viewer and never by class: these events belong to a person, and a
	// class token has no viewer, so a class-wide feed must not show them. Two
	// students on the same timetable therefore never see each other's events, even
	// though the underlying lessons are shared.
	//
	// revision is a per-row counter rather than reusing updated_at, because an .ics
	// client compares SEQUENCE to decide whether a VEVENT changed, and two edits
	// within the same second would otherwise be indistinguishable to it.
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS student_events (
		event_id INTEGER PRIMARY KEY AUTOINCREMENT,
		school TEXT NOT NULL,
		username TEXT NOT NULL,
		date TEXT NOT NULL,
		start_time TEXT NOT NULL,
		end_time TEXT NOT NULL,
		title TEXT NOT NULL,
		subject TEXT NOT NULL DEFAULT '',
		room TEXT NOT NULL DEFAULT '',
		teacher TEXT NOT NULL DEFAULT '',
		description TEXT NOT NULL DEFAULT '',
		created_by TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		revision INTEGER NOT NULL DEFAULT 1
	)`)
	if err != nil {
		return nil, err
	}
	// The range read is the only query that matters for serving, and it filters on
	// school, username and date.
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS student_events_range
		ON student_events (school, username, date)`); err != nil {
		return nil, err
	}
	// A per-student counter, bumped on every edit. A poller cannot derive this from
	// the events themselves: a delete leaves no row behind, so a max(updated_at)
	// would not move and a client would never learn the event is gone. It is keyed
	// by school and username rather than shared through the class outbox, because a
	// class-visible counter would tell a classmate that someone else's schedule
	// changed.
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS student_event_versions (
		school TEXT NOT NULL,
		username TEXT NOT NULL,
		version INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (school, username)
	)`)
	if err != nil {
		return nil, err
	}
	// migration: add person_id to class_tokens for older databases
	if err := addColumnIfMissing(db, "class_tokens", "person_id", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return nil, err
	}
	// migration: element_type/element_id/timezone for calendar token generalization
	if err := addColumnIfMissing(db, "class_tokens", "element_type", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	if err := addColumnIfMissing(db, "class_tokens", "element_id", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return nil, err
	}
	if err := addColumnIfMissing(db, "class_tokens", "timezone", "TEXT NOT NULL DEFAULT 'Europe/Berlin'"); err != nil {
		return nil, err
	}
	// migration: add name column to recon_elements for CLI fuzzy lookup
	if err := addColumnIfMissing(db, "recon_elements", "name", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	// migration: add school column to recon_elements for per-school element sets
	if err := addColumnIfMissing(db, "recon_elements", "school", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	// migration: names used to live in recon_elements, which also holds the
	// reconstructed-element sets. Move the catalog into its own table and drop
	// the polluted ids so the next recon sweep rebuilds the sets from the pooled
	// classes' real periods. Without this every school element looked
	// reconstructed - and therefore selectable in the app - while almost none of
	// them had a timetable to load.
	if err := migrateElementNames(db); err != nil {
		return nil, err
	}
	// migration: add days column to class_tokens for configurable lookahead
	if err := addColumnIfMissing(db, "class_tokens", "days", "INTEGER NOT NULL DEFAULT 30"); err != nil {
		return nil, err
	}
	// migration: track who created a calendar token
	if err := addColumnIfMissing(db, "class_tokens", "created_by", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	// migration: per-topic ntfy server override (empty = global default)
	if err := addColumnIfMissing(db, "ntfy_topics", "base_url", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	// migration: add school column to users for multi-school support
	if err := addColumnIfMissing(db, "users", "school", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	// migration: add admin flag column to users
	if err := addColumnIfMissing(db, "users", "admin", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return nil, err
	}
	// migration: target element (class/student/teacher/room/subject) for ntfy
	// topics and webhooks; class_id stays for legacy class-only rows
	if err := addColumnIfMissing(db, "ntfy_topics", "element_type", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	if err := addColumnIfMissing(db, "ntfy_topics", "element_id", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return nil, err
	}
	if err := addColumnIfMissing(db, "webhooks", "element_type", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	if err := addColumnIfMissing(db, "webhooks", "element_id", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return nil, err
	}
	// migration: teacher name column in the change snapshot so teacher-targeted
	// notifications can match a changed class
	if err := addColumnIfMissing(db, "timetable_changes", "teacher", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	// backfill: set element_type/element_id on existing tokens
	if _, err := db.Exec(`UPDATE class_tokens SET element_type='CLASS', element_id=class_id WHERE class_id>0 AND element_type=''`); err != nil {
		return nil, err
	}
	if _, err := db.Exec(`UPDATE class_tokens SET element_type='STUDENT', element_id=person_id WHERE person_id>0 AND element_type=''`); err != nil {
		return nil, err
	}
	// backfill: legacy ntfy/webhook rows were class-targeted
	if _, err := db.Exec(`UPDATE ntfy_topics SET element_type='CLASS', element_id=class_id WHERE class_id>0 AND element_type=''`); err != nil {
		return nil, err
	}
	if _, err := db.Exec(`UPDATE webhooks SET element_type='CLASS', element_id=class_id WHERE class_id>0 AND element_type=''`); err != nil {
		return nil, err
	}
	st := &Store{db: db}
	if err := st.normalizeUsernames(); err != nil {
		return nil, err
	}
	return st, nil
}

// normalizeUsernames merges accounts whose usernames differ only by case into a
// single lowercase-canonical account (usernames are case-insensitive). The most
// recently active variant wins for profile data; secrets and permission flags
// are merged across all variants.
func (s *Store) normalizeUsernames() error {
	rows, err := s.db.Query(`SELECT id,username,password,method,person_id,person_type,class_id,class_name,email,display_name,created_at,last_seen
		FROM users ORDER BY last_seen DESC, id DESC`)
	if err != nil {
		return err
	}
	groups := map[string][]*User{}
	var order []string
	for rows.Next() {
		var u User
		var ca, ls int64
		if err := rows.Scan(&u.ID, &u.Username, &u.Password, &u.Method, &u.PersonID, &u.PersonType,
			&u.ClassID, &u.ClassName, &u.Email, &u.DisplayName, &ca, &ls); err != nil {
			rows.Close()
			return err
		}
		u.CreatedAt, u.LastSeen = time.Unix(ca, 0), time.Unix(ls, 0)
		key := norm(u.Username)
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], &u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	need := false
	for _, key := range order {
		g := groups[key]
		if len(g) > 1 || g[0].Username != key {
			need = true
			break
		}
	}
	if !need {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	merged := 0
	for _, key := range order {
		g := groups[key]
		if len(g) == 1 && g[0].Username == key {
			continue
		}
		// The first member is the most recently active (ORDER BY last_seen).
		best := g[0]

		// Merge secrets (newest non-empty wins) and perms (OR; boosted beats
		// recon in the impossible conflict case) across every variant.
		var sec string
		var secAt int64
		perms := map[string]bool{}
		for _, m := range g {
			var msec string
			var mat int64
			if err := s.loadSecretRow(tx, m.Username, &msec, &mat); err != nil {
				tx.Rollback()
				return err
			}
			if msec != "" && (sec == "" || mat >= secAt) {
				sec, secAt = msec, mat
			}
			if err := s.loadPerms(tx, m.Username, perms); err != nil {
				tx.Rollback()
				return err
			}
		}
		if perms[FeatureRecon] && perms[FeatureBoosted] {
			perms[FeatureRecon] = false
		}

		// Drop every variant row (user + its secret + its per-user perms).
		for _, m := range g {
			if _, err := tx.Exec(`DELETE FROM users WHERE username=?`, m.Username); err != nil {
				tx.Rollback()
				return err
			}
			if _, err := tx.Exec(`DELETE FROM secrets WHERE username=?`, m.Username); err != nil {
				tx.Rollback()
				return err
			}
			if _, err := tx.Exec(`DELETE FROM perms WHERE username=? AND username<>?`, m.Username, globalPermUser); err != nil {
				tx.Rollback()
				return err
			}
		}

		// Reinsert one canonical lowercase account with the best profile data.
		best.Username = key
		if _, err := tx.Exec(`INSERT INTO users
			(username, password, method, person_id, person_type, class_id, class_name, email, display_name, created_at, last_seen)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			best.Username, best.Password, best.Method, best.PersonID, best.PersonType,
			best.ClassID, best.ClassName, best.Email, best.DisplayName,
			best.CreatedAt.Unix(), best.LastSeen.Unix()); err != nil {
			tx.Rollback()
			return err
		}
		if sec != "" {
			if _, err := tx.Exec(`INSERT INTO secrets (username, secret, updated_at) VALUES (?,?,?)`,
				key, sec, secAt); err != nil {
				tx.Rollback()
				return err
			}
		}
		for f, v := range perms {
			allow := 0
			if v {
				allow = 1
			}
			if _, err := tx.Exec(`INSERT INTO perms (username, feature, allowed) VALUES (?,?,?)`,
				key, f, allow); err != nil {
				tx.Rollback()
				return err
			}
		}
		merged += len(g)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	log.Printf("merged %d case-insensitive duplicate account(s)", merged)
	return nil
}

// hasCaseVariant reports whether any users row's normalized username equals
// username while its stored casing differs (i.e. a case-variant already exists).
func (s *Store) hasCaseVariant(username string) bool {
	rows, err := s.db.Query(`SELECT username FROM users`)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false
		}
		if norm(name) == username {
			return true
		}
	}
	return false
}

func (s *Store) loadSecretRow(tx *sql.Tx, username string, sec *string, at *int64) error {
	*sec = ""
	*at = 0
	err := tx.QueryRow(`SELECT secret, updated_at FROM secrets WHERE username=?`, username).Scan(sec, at)
	if err == sql.ErrNoRows {
		return nil
	}
	return err
}

func (s *Store) loadPerms(tx *sql.Tx, username string, out map[string]bool) error {
	rows, err := tx.Query(`SELECT feature, allowed FROM perms WHERE username=?`, username)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var f string
		var v int
		if err := rows.Scan(&f, &v); err != nil {
			return err
		}
		out[f] = v != 0
	}
	return rows.Err()
}

// addColumnIfMissing adds a column to a table if it does not already exist.
func addColumnIfMissing(db *sql.DB, table, column, ddl string) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	_, err = db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + ddl)
	return err //nolint
}

// elementNamesMigrated keys the settings row that records the one-time split of
// the name catalog out of recon_elements.
const elementNamesMigrated = "element_names_v2"

// migrateElementNames moves every name row out of recon_elements into
// element_names and clears the polluted reconstructed-element table. It runs
// once per database; the ids come back on the next recon sweep, which rebuilds
// the sets from the pooled classes' periods.
func migrateElementNames(db *sql.DB) error {
	var v string
	if err := db.QueryRow(`SELECT value FROM settings WHERE key=?`, elementNamesMigrated).Scan(&v); err == nil && v == "1" {
		return nil
	}
	if _, err := db.Exec(`INSERT INTO element_names (school, el_type, el_id, name)
		SELECT school, el_type, el_id, name FROM recon_elements WHERE name!=''
		ON CONFLICT(el_type, el_id) DO UPDATE SET name=excluded.name, school=excluded.school`); err != nil {
		return err
	}
	// The class scan progress goes too: with an empty registry every pooled
	// class must be re-enumerated, otherwise the sweep would resume ("already
	// scanned") and leave the selectable set empty.
	if _, err := db.Exec(`DELETE FROM recon_scan`); err != nil {
		return err
	}
	if _, err := db.Exec(`DELETE FROM recon_elements`); err != nil {
		return err
	}
	_, err := db.Exec(`INSERT INTO settings (key, value) VALUES (?, '1')
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, elementNamesMigrated)
	return err
}

func (s *Store) UpsertSecret(username, secret string) error {
	username = norm(username)
	_, err := s.db.Exec(`INSERT INTO secrets (username, secret, updated_at) VALUES (?,?,?)
		ON CONFLICT(username) DO UPDATE SET secret=excluded.secret, updated_at=excluded.updated_at`,
		username, secret, time.Now().Unix())
	return err
}

func (s *Store) GetSecret(username string) (string, error) {
	var sec string
	err := s.db.QueryRow(`SELECT secret FROM secrets WHERE username=?`, norm(username)).Scan(&sec)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return sec, err
}

// globalPermUser is the special perms row username holding the global switch
// for a reconstruction element type. Per-user rows override it.
const globalPermUser = "*"

// GlobalPermUser exposes the global-switch perms username to other packages.
func GlobalPermUser() string { return globalPermUser }

// Permission feature names.
const (
	// FeatureRecon grants reconstruction of teacher/room/subject timetables
	// from the pooled class data.
	FeatureRecon = "recon"
	// FeatureBoosted grants raw forwarding of all class/teacher/room/subject
	// timetables through a saved teacher account (own personal timetable and the
	// info center still use the user's own account). Reading your own absences
	// is always allowed. It does NOT include the write methods.
	FeatureBoosted = "boosted"
	// FeatureEditor grants the absence/lesson/subject write (editing) methods
	// (set/add/update/delete/change). It implies boosted: an editor is a
	// boosted user who may also edit, a boosted user alone may only read.
	FeatureEditor = "editor"
)

// BoostFeatures are the per-user features that imply Boosted raw timetable
// access (full teacher-account forwarding). Editing is a separate flag on top,
// so "editor" means "boosted and may edit". They are mutually exclusive with
// the reconstruction flag.
var BoostFeatures = []string{FeatureBoosted, FeatureEditor}

// HasPerm reports whether a user has an explicit per-user permission row set to
// allowed. Global switches do not count.
func (s *Store) HasPerm(username, feature string) (bool, error) {
	var v int
	row := s.db.QueryRow(`SELECT allowed FROM perms WHERE username=? AND feature=?`, norm(username), feature)
	if err := row.Scan(&v); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	return v != 0, nil
}

// ReconAccess reports whether a user may use reconstruction of teacher/room/
// subject timetables. A per-user override wins if one exists; otherwise the
// global switch applies; default is deny.
func (s *Store) ReconAccess(username, elType string) (bool, error) {
	_ = elType
	var v int
	row := s.db.QueryRow(`SELECT allowed FROM perms WHERE username=? AND feature=?`, norm(username), FeatureRecon)
	if err := row.Scan(&v); err == nil {
		return v != 0, nil
	}
	row = s.db.QueryRow(`SELECT allowed FROM perms WHERE username=? AND feature=?`, globalPermUser, FeatureRecon)
	if err := row.Scan(&v); err == nil {
		return v != 0, nil
	}
	return false, nil
}

// BoostedAccess reports whether a user effectively gets Boosted raw timetable
// forwarding (they hold the boosted flag).
func (s *Store) BoostedAccess(username string) (bool, error) {
	for _, f := range BoostFeatures {
		if ok, err := s.HasPerm(username, f); err == nil && ok {
			return true, nil
		}
	}
	return false, nil
}

// EditorAccess reports whether a user holds the editor flag (absence/lesson/
// subject write methods).
func (s *Store) EditorAccess(username string) (bool, error) {
	return s.HasPerm(username, FeatureEditor)
}

// SetReconType sets the global switch for reconstruction across all users.
func (s *Store) SetReconType(elType string, allowed bool) error {
	_ = elType
	return s.setPerm(globalPermUser, FeatureRecon, allowed)
}

// SetReconOverride grants or revokes reconstruction for a single user, taking
// precedence over the global switch.
func (s *Store) SetReconOverride(username, elType string, allowed bool) error {
	_ = elType
	return s.setPerm(username, FeatureRecon, allowed)
}

// SetBoostedFlag grants or revokes the boosted (raw teacher forwarding) flag.
func (s *Store) SetBoostedFlag(username string, allowed bool) error {
	return s.setPerm(username, FeatureBoosted, allowed)
}

// SetPerm grants or revokes an arbitrary per-user permission feature.
func (s *Store) SetPerm(username, feature string, allowed bool) error {
	return s.setPerm(username, feature, allowed)
}

func (s *Store) setPerm(username, elType string, allowed bool) error {
	username = norm(username)
	// Reconstruction is mutually exclusive with Boosted: granting one side
	// revokes the other side for the same user. The global switch ("*") is not a
	// real user, so exclusion only applies to per-user overrides. Editor is
	// independent and never triggers the exclusion.
	if username != globalPermUser && allowed {
		switch elType {
		case FeatureRecon:
			// Granting reconstruction revokes boosted.
			_ = s.setPermFalse(username, FeatureBoosted)
		case FeatureBoosted:
			// Granting boosted revokes reconstruction.
			_ = s.setPermFalse(username, FeatureRecon)
		}
	}
	v := 0
	if allowed {
		v = 1
	}
	_, err := s.db.Exec(`INSERT INTO perms (username, feature, allowed) VALUES (?,?,?)
		ON CONFLICT(username, feature) DO UPDATE SET allowed=excluded.allowed`,
		username, elType, v)
	return err
}

func (s *Store) setPermFalse(username, feature string) error {
	username = norm(username)
	_, err := s.db.Exec(`INSERT INTO perms (username, feature, allowed) VALUES (?,?,0)
		ON CONFLICT(username, feature) DO UPDATE SET allowed=0`, username, feature)
	return err
}

// ClearReconOverrides removes any per-user reconstruction overrides for a
// username so they fall back to the global switches again.
func (s *Store) ClearReconOverrides(username string) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM perms WHERE username=? AND username<>?`,
		norm(username), globalPermUser)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RevokeAll clears every permission row (global switches and per-user
// overrides), returning every user to the default state of class-pool-only.
func (s *Store) RevokeAll() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM perms`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// maxAdminBootstrapKey keys the settings row remembering which usernames were
// seeded as admins from the -admin flag, so a later demotion via the API is not
// silently re-applied on restart.
const adminBootstrapKey = "admin_bootstrap"

// IsAdmin reports whether a user holds the admin flag (DB `admin` column).
func (s *Store) IsAdmin(username string) (bool, error) {
	u, err := s.GetUser(username)
	if err != nil || u == nil {
		return false, err
	}
	return u.Admin, nil
}

// SetAdmin sets the admin flag for a user, creating the row if it does not
// exist yet.
func (s *Store) SetAdmin(username string, admin bool) error {
	username = norm(username)
	u, err := s.GetUser(username)
	if err != nil {
		return err
	}
	if u == nil {
		u = &User{Username: username}
	}
	u.Admin = admin
	return s.UpsertUser(u)
}

// AdminBootstrapSeeded reports whether the -admin bootstrap list has already
// been applied for the given usernames.
func (s *Store) AdminBootstrapSeeded() (bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, adminBootstrapKey).Scan(&v)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil && v != "", err
}

// MarkAdminBootstrapSeeded records that the -admin flag list has been applied.
func (s *Store) MarkAdminBootstrapSeeded() error {
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, '1')
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, adminBootstrapKey)
	return err
}

// School is one registered upstream Untis school.
type School struct {
	Name     string
	AddedAt  time.Time
	LastSeen time.Time
}

// ListSchools returns every registered school, most recently seen first.
func (s *Store) ListSchools() ([]*School, error) {
	rows, err := s.db.Query(`SELECT school, added_at, last_seen FROM schools ORDER BY last_seen DESC, school`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*School
	for rows.Next() {
		var sc School
		var a, ls int64
		if err := rows.Scan(&sc.Name, &a, &ls); err != nil {
			return nil, err
		}
		sc.AddedAt, sc.LastSeen = time.Unix(a, 0), time.Unix(ls, 0)
		out = append(out, &sc)
	}
	return out, rows.Err()
}

// KnownSchool reports whether a school is already registered.
func (s *Store) KnownSchool(school string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM schools WHERE school=?`, school).Scan(&n)
	return n > 0, err
}

// UpsertSchool registers (or bumps last_seen for) a school.
func (s *Store) UpsertSchool(school string) error {
	_, err := s.db.Exec(`INSERT INTO schools (school, added_at, last_seen) VALUES (?,?,?)
		ON CONFLICT(school) DO UPDATE SET last_seen=excluded.last_seen`,
		school, time.Now().Unix(), time.Now().Unix())
	return err
}

// Webhook is a change-delivery webhook subscription.
type Webhook struct {
	ID          int64
	School      string
	ElementType string // "", CLASS, STUDENT, TEACHER, ROOM or SUBJECT
	ElementID   int64  // id of the target element (0 + no type = school-wide)
	ClassID     int64  // legacy class target, kept for backwards compatibility
	URL         string
	Secret      string
	Enabled     bool
	CreatedBy   string
	CreatedAt   time.Time
}

// Target resolves the element a subscription watches. It returns the school
// wide (empty) target for all-class rows created before element generalization.
func (w *Webhook) Target() (et string, eid int64) {
	if w.ElementType != "" {
		return w.ElementType, w.ElementID
	}
	if w.ClassID > 0 {
		return "CLASS", w.ClassID
	}
	return "", 0
}

// AddWebhook registers a webhook subscription.
func (s *Store) AddWebhook(w *Webhook) (int64, error) {
	en := 0
	if w.Enabled {
		en = 1
	}
	et, eid := w.Target()
	res, err := s.db.Exec(`INSERT INTO webhooks (school, element_type, element_id, class_id, url, secret, enabled, created_by, created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		w.School, et, eid, w.ClassID, w.URL, w.Secret, en, w.CreatedBy, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListWebhooks returns webhook subscriptions for a school (or all when school is "").
func (s *Store) ListWebhooks(school string) ([]*Webhook, error) {
	q := `SELECT id, school, element_type, element_id, class_id, url, secret, enabled, created_by, created_at FROM webhooks`
	var args []any
	if school != "" {
		q += ` WHERE school=?`
		args = append(args, school)
	}
	q += ` ORDER BY id`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Webhook
	for rows.Next() {
		var w Webhook
		var en, ca int64
		if err := rows.Scan(&w.ID, &w.School, &w.ElementType, &w.ElementID, &w.ClassID, &w.URL, &w.Secret, &en, &w.CreatedBy, &ca); err != nil {
			return nil, err
		}
		w.Enabled, w.CreatedAt = en != 0, time.Unix(ca, 0)
		out = append(out, &w)
	}
	return out, rows.Err()
}

// DeleteWebhook removes a webhook subscription by id.
func (s *Store) DeleteWebhook(id int64) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM webhooks WHERE id=?`, id)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// NtfyTopic is a configured ntfy push topic subscription.
type NtfyTopic struct {
	ID          int64
	School      string
	ElementType string // "", CLASS, STUDENT, TEACHER, ROOM or SUBJECT
	ElementID   int64  // id of the target element (0 + no type = school-wide)
	ClassID     int64  // legacy class target, kept for backwards compatibility
	Topic       string
	BaseURL     string // empty = use the global default ntfy server
	Enabled     bool
	CreatedBy   string
	CreatedAt   time.Time
}

// Target resolves the element a topic watches. It returns the school wide
// (empty) target for all-class rows created before element generalization.
func (n *NtfyTopic) Target() (et string, eid int64) {
	if n.ElementType != "" {
		return n.ElementType, n.ElementID
	}
	if n.ClassID > 0 {
		return "CLASS", n.ClassID
	}
	return "", 0
}

// AddNtfyTopic registers an ntfy topic subscription.
func (s *Store) AddNtfyTopic(n *NtfyTopic) (int64, error) {
	en := 0
	if n.Enabled {
		en = 1
	}
	et, eid := n.Target()
	res, err := s.db.Exec(`INSERT INTO ntfy_topics (school, element_type, element_id, class_id, topic, base_url, enabled, created_by, created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		n.School, et, eid, n.ClassID, n.Topic, n.BaseURL, en, n.CreatedBy, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListNtfyTopics returns ntfy topic subscriptions for a school (or all when school is "").
func (s *Store) ListNtfyTopics(school string) ([]*NtfyTopic, error) {
	q := `SELECT id, school, element_type, element_id, class_id, topic, base_url, enabled, created_by, created_at FROM ntfy_topics`
	var args []any
	if school != "" {
		q += ` WHERE school=?`
		args = append(args, school)
	}
	q += ` ORDER BY id`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*NtfyTopic
	for rows.Next() {
		var n NtfyTopic
		var en, ca int64
		if err := rows.Scan(&n.ID, &n.School, &n.ElementType, &n.ElementID, &n.ClassID, &n.Topic, &n.BaseURL, &en, &n.CreatedBy, &ca); err != nil {
			return nil, err
		}
		n.Enabled, n.CreatedAt = en != 0, time.Unix(ca, 0)
		out = append(out, &n)
	}
	return out, rows.Err()
}

// DeleteNtfyTopic removes an ntfy topic subscription by id.
func (s *Store) DeleteNtfyTopic(id int64) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM ntfy_topics WHERE id=?`, id)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PermRow is a single row in the perms table.
type PermRow struct {
	Username string
	Feature  string
	Allowed  bool
}

// AllPerms returns every permission row, global switches (username "*")
// first, then per-user overrides.
func (s *Store) AllPerms() ([]PermRow, error) {
	rows, err := s.db.Query(`SELECT username, feature, allowed FROM perms
		ORDER BY username='*' DESC, username, feature`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PermRow
	for rows.Next() {
		var p PermRow
		var a int
		if err := rows.Scan(&p.Username, &p.Feature, &a); err != nil {
			return nil, err
		}
		p.Allowed = a != 0
		out = append(out, p)
	}
	return out, rows.Err()
}

// SaveReconElements upserts the scanned teacher/room/subject set for a school as
// a warm-start seed for the background enumeration. It never deletes rows:
// Element ids only: the name catalog lives in element_names. Mixing the two
// made every known school element count as reconstructed (and thus selectable)
// in the app even though only a fraction have any timetable to load.
// (re)written on every login from masterData), so pruning here would wipe names
// for elements outside the currently-scanned pool.
func (s *Store) SaveReconElements(school string, elems map[string][]int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ins, err := tx.Prepare(`INSERT INTO recon_elements (school, el_type, el_id) VALUES (?,?,?)
		ON CONFLICT(el_type, el_id) DO UPDATE SET school=excluded.school`)
	if err != nil {
		return err
	}
	defer ins.Close()
	for t, ids := range elems {
		for _, id := range ids {
			if _, err := ins.Exec(school, t, id); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		tx.Rollback()
		return err
	}
	return nil
}

// UpsertElementNames records human-readable names for recon elements so the
// CLI can fuzzy-lookup teachers/rooms/subjects once a login has delivered
// masterData. Conflicts on (el_type, el_id) update the name in place.
func (s *Store) UpsertElementNames(school, elType string, names map[int64]string) error {
	if len(names) == 0 {
		return nil
	}
	stmt, err := s.db.Prepare(`INSERT INTO element_names (school, el_type, el_id, name) VALUES (?,?,?,?)
		ON CONFLICT(el_type, el_id) DO UPDATE SET name=excluded.name, school=excluded.school`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for id, name := range names {
		if name == "" {
			continue
		}
		if _, err := stmt.Exec(school, elType, id, name); err != nil {
			return err
		}
	}
	return nil
}

// LoadReconElements returns the persisted teacher/room/subject set as
// type -> [ids] for a school (rows with an unrecorded school match any).
func (s *Store) LoadReconElements(school string) (map[string][]int64, error) {
	rows, err := s.db.Query(`SELECT el_type, el_id FROM recon_elements WHERE school=? OR school=''`, school)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]int64{}
	for rows.Next() {
		var t string
		var id int64
		if err := rows.Scan(&t, &id); err != nil {
			return nil, err
		}
		out[t] = append(out[t], id)
	}
	return out, rows.Err()
}

// SaveReconScanAt records how far a class's recon scan reached. The stored
// horizon is what lets the next StartRecon resume the enumeration at that date
// instead of re-fetching the whole year from the school server.
func (s *Store) SaveReconScanAt(school string, classID int64, until string) error {
	_, err := s.db.Exec(`INSERT INTO recon_scan (school, class_id, scan_until) VALUES (?,?,?)
		ON CONFLICT(school, class_id) DO UPDATE SET scan_until=excluded.scan_until`,
		school, classID, until)
	return err
}

// ReconScan returns the recorded scan horizon per pooled class for a school.
// Classes missing from the map were never scanned.
func (s *Store) ReconScan(school string) (map[int64]string, error) {
	rows, err := s.db.Query(`SELECT class_id, scan_until FROM recon_scan WHERE school=?`, school)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var classID int64
		var until string
		if err := rows.Scan(&classID, &until); err != nil {
			return nil, err
		}
		out[classID] = until
	}
	return out, rows.Err()
}

// ClearReconScan forgets the scan progress of a school so the next StartRecon
// re-scans every pooled class from the year start (the "rescan" escape hatch
// for when new teachers/rooms must be re-enumerated).
func (s *Store) ClearReconScan(school string) error {
	_, err := s.db.Exec(`DELETE FROM recon_scan WHERE school=?`, school)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) UpsertUser(u *User) error {
	now := time.Now()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	u.Username = norm(u.Username)
	// If the canonical lowercase row does not exist yet but a differently-cased
	// variant does (e.g. a login right after a capital-cased account was seeded),
	// collapse the variants first so the upsert below re-targets the merged row
	// instead of creating yet another case-duplicate.
	var existing string
	err := s.db.QueryRow(`SELECT username FROM users WHERE username=?`, u.Username).Scan(&existing)
	if err == sql.ErrNoRows && s.hasCaseVariant(u.Username) {
		if err := s.normalizeUsernames(); err != nil {
			return err
		}
	}
	admin := 0
	if u.Admin {
		admin = 1
	}
	_, err = s.db.Exec(`INSERT INTO users
		(username, password, method, person_id, person_type, class_id, class_name, email, display_name, created_at, last_seen, school, admin)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(username) DO UPDATE SET
			password=excluded.password,
			method=excluded.method,
			person_id=excluded.person_id,
			person_type=excluded.person_type,
			class_id=excluded.class_id,
			class_name=excluded.class_name,
			email=excluded.email,
			display_name=excluded.display_name,
			school=excluded.school,
			admin=excluded.admin,
			last_seen=excluded.last_seen`,
		u.Username, u.Password, u.Method, u.PersonID, u.PersonType,
		u.ClassID, u.ClassName, u.Email, u.DisplayName,
		u.CreatedAt.Unix(), now.Unix(), u.School, admin)
	return err
}

// SetDefaultSchool backfills users whose school has not been recorded yet
// (pre-multi-school rows) with the given default school.
func (s *Store) SetDefaultSchool(school string) error {
	_, err := s.db.Exec(`UPDATE users SET school=? WHERE school=''`, school)
	return err
}

func (s *Store) Touch(username string) error {
	_, err := s.db.Exec(`UPDATE users SET last_seen=? WHERE username=?`, time.Now().Unix(), norm(username))
	return err
}

func (s *Store) GetUser(username string) (*User, error) {
	return s.scanUser(s.db.QueryRow(
		`SELECT `+userCols+` FROM users WHERE username=?`,
		norm(username)))
}

// UserByPersonID returns a user with the given person id, preferring the most
// recently active one.
func (s *Store) UserByPersonID(personID int64) (*User, error) {
	return s.scanUser(s.db.QueryRow(`SELECT `+userCols+`
		FROM users WHERE person_id=? ORDER BY last_seen DESC, id DESC LIMIT 1`, personID))
}

// ClassForPerson returns the class a student person belongs to, so change
// events for that class can be fanned out to student-targeted topics.
func (s *Store) ClassForPerson(school string, personID int64) (int64, error) {
	var c int64
	err := s.db.QueryRow(`SELECT class_id FROM users
		WHERE person_id=? AND person_type=5 AND class_id>0 AND (school=? OR school='')
		ORDER BY last_seen DESC, id DESC LIMIT 1`, personID, school).Scan(&c)
	if err != nil {
		return 0, err
	}
	return c, nil
}

// Pool returns the pooled classes for a school: every class whose users belong
// to the given school (or whose school has not been recorded, a pre-multi-
// school account, treated as matching any school). Empty school matches all.
func (s *Store) Pool(school string) ([]Class, error) {
	rows, err := s.db.Query(`SELECT class_id, COALESCE(MAX(class_name),'') AS name
		FROM users WHERE class_id > 0 AND (school=? OR school='')
		GROUP BY class_id ORDER BY class_id`, school)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Class
	for rows.Next() {
		var c Class
		if err := rows.Scan(&c.ID, &c.Name); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) UserCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Store) PoolContains(school string, classID int64) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE class_id=? AND (school=? OR school='')`, classID, school).Scan(&n)
	return n > 0, err
}

// OwnerForClass returns the most recently active user in the given class,
// preferring accounts that can be replayed (password or key with secret).
func (s *Store) OwnerForClass(school string, classID int64) (*User, error) {
	return s.scanUser(s.db.QueryRow(`SELECT `+userCols+`
		FROM users WHERE class_id=? AND (school=? OR school='') AND password<>''
		ORDER BY last_seen DESC, id DESC LIMIT 1`, classID, school))
}

// BoostedSourceAccounts returns all users with replayable secrets (password <> ”)
// who are non-students (person_type != 5). These are the "teacher accounts
// lying around" that a boosted user draws raw data from.
func (s *Store) BoostedSourceAccounts(school string) ([]*User, error) {
	rows, err := s.db.Query(`SELECT `+userCols+`
		FROM users WHERE password<>'' AND person_type<>5 AND (school=? OR school='')
		ORDER BY last_seen DESC`, school)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := s.scanUserRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) AnyUser(school string) (*User, error) {
	return s.scanUser(s.db.QueryRow(`SELECT `+userCols+`
		FROM users WHERE school=? OR school='' ORDER BY last_seen DESC, id DESC LIMIT 1`, school))
}

// ListUsers returns every account, most recently seen first.
func (s *Store) ListUsers() ([]*User, error) {
	rows, err := s.db.Query(`SELECT ` + userCols + ` FROM users ORDER BY last_seen DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := s.scanUserRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ListStudents returns student accounts (person_type 5 = pupil, as stamped by
// upstream logins) with a person ID, ordered by class then display name.
// Teachers and unlogged-in accounts are excluded so the admin student picker
// never overlaps the teacher/room/subject pickers. Unrecorded-school accounts
// (school=”) count for any school.
func (s *Store) ListStudents(school string) ([]*User, error) {
	rows, err := s.db.Query(`SELECT `+userCols+`
		FROM users WHERE person_id > 0 AND person_type = 5 AND (school=? OR school='')
		ORDER BY class_name, display_name, username`, school)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := s.scanUserRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// DeleteUser removes an account plus its secret, per-user permission overrides
// and any personal calendar tokens bound to that person. Global switches are
// untouched.
func (s *Store) DeleteUser(username string) (int64, error) {
	username = norm(username)
	u, err := s.GetUser(username)
	if err != nil {
		return 0, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	n := int64(0)
	var del func(q string, args ...any) error
	del = func(q string, args ...any) error {
		res, err := tx.Exec(q, args...)
		if err != nil {
			return err
		}
		c, _ := res.RowsAffected()
		n += c
		return nil
	}
	if err := del(`DELETE FROM users WHERE username=?`, username); err != nil {
		tx.Rollback()
		return 0, err
	}
	if err := del(`DELETE FROM secrets WHERE username=?`, username); err != nil {
		tx.Rollback()
		return 0, err
	}
	if err := del(`DELETE FROM perms WHERE username=? AND username<>?`, username, globalPermUser); err != nil {
		tx.Rollback()
		return 0, err
	}
	if u != nil && u.PersonID > 0 {
		if err := del(`DELETE FROM class_tokens WHERE person_id=? AND class_id=0`, u.PersonID); err != nil {
			tx.Rollback()
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		tx.Rollback()
		return 0, err
	}
	return n, nil
}

func (s *Store) scanUser(row *sql.Row) (*User, error) {
	var u User
	var ca, ls int64
	var admin int
	err := row.Scan(&u.ID, &u.Username, &u.Password, &u.Method, &u.PersonID, &u.PersonType,
		&u.ClassID, &u.ClassName, &u.Email, &u.DisplayName, &ca, &ls, &u.School, &admin)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.Admin = admin != 0
	u.CreatedAt = time.Unix(ca, 0)
	u.LastSeen = time.Unix(ls, 0)
	return &u, nil
}

// scanUserRow is the *sql.Rows equivalent of scanUser.
func (s *Store) scanUserRow(row *sql.Rows) (*User, error) {
	var u User
	var ca, ls int64
	var admin int
	err := row.Scan(&u.ID, &u.Username, &u.Password, &u.Method, &u.PersonID, &u.PersonType,
		&u.ClassID, &u.ClassName, &u.Email, &u.DisplayName, &ca, &ls, &u.School, &admin)
	if err != nil {
		return nil, err
	}
	u.Admin = admin != 0
	u.CreatedAt = time.Unix(ca, 0)
	u.LastSeen = time.Unix(ls, 0)
	return &u, nil
}

func (s *Store) CreateClassToken(t *ClassToken) error {
	days := t.Days
	if days <= 0 {
		days = 30
	}
	_, err := s.db.Exec(`INSERT INTO class_tokens (token, school, class_id, person_id, element_type, element_id, timezone, days, created_at, last_access, created_by)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`, t.Token, t.School, t.ClassID, t.PersonID, t.ElementType, t.ElementID, t.Timezone, days, t.CreatedAt, t.LastAccess, t.CreatedBy)
	return err
}

// ClassTokenForClass returns the existing token for a school+class, if any.
func (s *Store) ClassTokenForClass(school string, classID int64) (*ClassToken, error) {
	return s.scanClassToken(s.db.QueryRow(`SELECT token, school, class_id, person_id, element_type, element_id, timezone, days, created_at, last_access, created_by
		FROM class_tokens WHERE school=? AND class_id=?`, school, classID))
}

// ClassTokenForPerson returns the existing personal token for a school+person.
func (s *Store) ClassTokenForPerson(school string, personID int64) (*ClassToken, error) {
	return s.scanClassToken(s.db.QueryRow(`SELECT token, school, class_id, person_id, element_type, element_id, timezone, days, created_at, last_access, created_by
		FROM class_tokens WHERE school=? AND person_id=?`, school, personID))
}

// ClassTokenForElement returns the existing token for a school+element type+id.
func (s *Store) ClassTokenForElement(school, elType string, elID int64) (*ClassToken, error) {
	return s.scanClassToken(s.db.QueryRow(`SELECT token, school, class_id, person_id, element_type, element_id, timezone, days, created_at, last_access, created_by
		FROM class_tokens WHERE school=? AND element_type=? AND element_id=?`, school, elType, elID))
}

func (s *Store) ClassTokenByToken(token string) (*ClassToken, error) {
	return s.scanClassToken(s.db.QueryRow(`SELECT token, school, class_id, person_id, element_type, element_id, timezone, days, created_at, last_access, created_by
		FROM class_tokens WHERE token=?`, token))
}

func (s *Store) scanClassToken(row *sql.Row) (*ClassToken, error) {
	var t ClassToken
	err := row.Scan(&t.Token, &t.School, &t.ClassID, &t.PersonID, &t.ElementType, &t.ElementID, &t.Timezone, &t.Days, &t.CreatedAt, &t.LastAccess, &t.CreatedBy)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if t.Days <= 0 {
		t.Days = 30
	}
	return &t, nil
}

// ListClassTokens returns every calendar subscription token, newest first.
func (s *Store) ListClassTokens() ([]*ClassToken, error) {
	rows, err := s.db.Query(`SELECT token, school, class_id, person_id, element_type, element_id, timezone, days, created_at, last_access, created_by
		FROM class_tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ClassToken
	for rows.Next() {
		var t ClassToken
		if err := rows.Scan(&t.Token, &t.School, &t.ClassID, &t.PersonID, &t.ElementType, &t.ElementID, &t.Timezone, &t.Days, &t.CreatedAt, &t.LastAccess, &t.CreatedBy); err != nil {
			return nil, err
		}
		if t.Days <= 0 {
			t.Days = 30
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

// DeleteClassToken removes a calendar subscription token by its value.
func (s *Store) DeleteClassToken(token string) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM class_tokens WHERE token=?`, token)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// TouchClassToken updates the last-access timestamp of an existing token.
func (s *Store) TouchClassToken(token string, at int64) error {
	_, err := s.db.Exec(`UPDATE class_tokens SET last_access=? WHERE token=?`, at, token)
	return err
}

// UpdateClassTokenDays changes the lookahead window (days) of a calendar token.
func (s *Store) UpdateClassTokenDays(token string, days int) (int64, error) {
	res, err := s.db.Exec(`UPDATE class_tokens SET days=? WHERE token=?`, days, token)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListElementsWithNames returns every persisted element ID and its name,
// grouped by element type (CLASS, TEACHER, ROOM, SUBJECT). Rows from
// unrecorded schools (school=”) count for every school. Used by the admin
// search picker so the dashboard never needs a live upstream call.
func (s *Store) ListElementsWithNames(school string) (map[string]map[int64]string, error) {
	rows, err := s.db.Query(
		`SELECT el_type, el_id, name FROM element_names WHERE (school=? OR school='') AND name!=''`,
		school)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[int64]string{}
	for rows.Next() {
		var t string
		var id int64
		var name string
		if err := rows.Scan(&t, &id, &name); err != nil {
			return nil, err
		}
		if out[t] == nil {
			out[t] = map[int64]string{}
		}
		out[t][id] = name
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Master element names (for CLI fuzzy lookup)
// ---------------------------------------------------------------------------

// SaveMasterNames persists element names from the upstream master data fetch.
// It updates the name column in element_names for matching IDs, inserting new
// rows only if they don't already exist.
func (s *Store) SaveMasterNames(school, elType string, names map[int64]string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for id, name := range names {
		// Upsert: insert if missing, then update name
		_, _ = tx.Exec(`INSERT OR IGNORE INTO element_names (school, el_type, el_id, name) VALUES (?,?,?,?)`, school, elType, id, name)
		_, _ = tx.Exec(`UPDATE element_names SET name=? WHERE school=? AND el_type=? AND el_id=?`, name, school, elType, id)
	}
	if err := tx.Commit(); err != nil {
		tx.Rollback()
		return err
	}
	return nil
}

// LookupElement resolves a user-provided name or numeric ID to an element ID.
// It supports:
//   - pure numeric input → returned as-is (validated to be > 0)
//   - exact case-insensitive name match → returned
//   - unique substring match → returned
//   - multiple matches → error with candidates listed
//   - no match → error with closest Levenshtein suggestions
//
// Rows from unrecorded-school entries (school=”) match any school.
func (s *Store) LookupElement(school, elType, input string) (int64, error) {
	// Pure numeric → use directly
	if id, ok := parseID(input); ok {
		return id, nil
	}

	// Fetch all elements of this type with names
	rows, err := s.db.Query(`SELECT el_id, name FROM element_names WHERE el_type=? AND (school=? OR school='') AND name!=''`, elType, school)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type elem struct {
		id   int64
		name string
	}
	var all []elem
	for rows.Next() {
		var e elem
		if err := rows.Scan(&e.id, &e.name); err != nil {
			return 0, err
		}
		all = append(all, e)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(all) == 0 {
		return 0, fmt.Errorf("no %s elements with known names in the database", elType)
	}

	query := strings.ToLower(input)

	// 1. Exact match
	for _, e := range all {
		if strings.ToLower(e.name) == query {
			return e.id, nil
		}
	}

	// 2. Substring match
	var subMatches []elem
	for _, e := range all {
		if strings.Contains(strings.ToLower(e.name), query) {
			subMatches = append(subMatches, e)
		}
	}
	if len(subMatches) == 1 {
		return subMatches[0].id, nil
	}
	if len(subMatches) > 1 {
		names := make([]string, len(subMatches))
		for i, e := range subMatches {
			names[i] = fmt.Sprintf("%s (id=%d)", e.name, e.id)
		}
		return 0, fmt.Errorf("ambiguous %q — did you mean:\n  %s", input, strings.Join(names, "\n  "))
	}

	// 3. Levenshtein suggestions
	type candidate struct {
		name string
		id   int64
		dist int
	}
	var cands []candidate
	for _, e := range all {
		d := levenshtein(strings.ToLower(e.name), query)
		if d <= 3 {
			cands = append(cands, candidate{e.name, e.id, d})
		}
	}
	if len(cands) > 0 {
		// Sort by distance, then alphabetically
		for i := 1; i < len(cands); i++ {
			for j := i; j > 0 && (cands[j].dist < cands[j-1].dist ||
				(cands[j].dist == cands[j-1].dist && cands[j].name < cands[j-1].name)); j-- {
				cands[j], cands[j-1] = cands[j-1], cands[j]
			}
		}
		names := make([]string, 0, len(cands))
		for _, c := range cands {
			names = append(names, fmt.Sprintf("%s (id=%d)", c.name, c.id))
		}
		return 0, fmt.Errorf("%q not found — did you mean:\n  %s", input, strings.Join(names, "\n  "))
	}

	return 0, fmt.Errorf("%q not found among %ss", input, elType)
}

// ElementName resolves a recon element's display name by id. It is used to
// match teacher/room/subject-targeted notifications against change rows.
func (s *Store) ElementName(school, elType string, id int64) string {
	if id <= 0 {
		return ""
	}
	var name string
	err := s.db.QueryRow(`SELECT name FROM element_names
		WHERE el_type=? AND el_id=? AND (school=? OR school='') AND name!=''`, elType, id, school).Scan(&name)
	if err != nil {
		// Fallback for rows written before names moved to element_names.
		err = s.db.QueryRow(`SELECT name FROM recon_elements
			WHERE el_type=? AND el_id=? AND (school=? OR school='') AND name!=''`, elType, id, school).Scan(&name)
		if err != nil {
			return ""
		}
	}
	return name
}

func parseID(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	var id int64
	for _, c := range s {
		id = id*10 + int64(c-'0')
	}
	return id, id > 0
}

// levenshtein computes the edit distance between two strings.
func levenshtein(a, b string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min3(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}

func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}

// PeriodRow is the persistent snapshot of one period used for change detection.
type PeriodRow struct {
	PeriodID    int64
	Kind        string // ADDED, CHANGED or REMOVED
	Start       string
	End         string
	Subject     string
	Room        string
	Teacher     string
	Description string
	ModVer      int64
}

func (s *Store) ClassVersion(school string, classID int64) int64 {
	var v int64
	_ = s.db.QueryRow(`SELECT version FROM timetable_versions WHERE school=? AND class_id=?`, school, classID).Scan(&v)
	return v
}

// SchoolVersion returns the highest class version in a school: a monotone
// counter that only advances when a polled class timetable actually changed
// (see ReplaceClassSnapshot). Used as a coarse change counter for feeds that
// are not tied to a single class, e.g. reconstructed teacher/room/subject ICS.
func (s *Store) SchoolVersion(school string) int64 {
	var v int64
	_ = s.db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM timetable_versions WHERE school=?`, school).Scan(&v)
	return v
}

// LoadClassSnapshot returns the latest known periods for a class.
func (s *Store) LoadClassSnapshot(school string, classID int64) ([]PeriodRow, error) {
	rows, err := s.db.Query(`SELECT period_id, kind, start, end, subject, room, teacher, description, mod_ver
		FROM timetable_changes WHERE school=? AND class_id=? ORDER BY period_id`, school, classID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PeriodRow
	for rows.Next() {
		var r PeriodRow
		if err := rows.Scan(&r.PeriodID, &r.Kind, &r.Start, &r.End, &r.Subject, &r.Room, &r.Teacher, &r.Description, &r.ModVer); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReplaceClassSnapshot transactionally rewrites the snapshot for a class and
// returns the number of rows whose mod version changed. See ApplyClassSnapshot
// for the change-detection rules; this is the form that queues no deliveries.
func (s *Store) ReplaceClassSnapshot(school string, classID int64, next []PeriodRow, newVer int64, dropRemovedBefore string) (int, error) {
	return s.ApplyClassSnapshot(school, classID, next, newVer, dropRemovedBefore, nil)
}

// Enqueuer queues one pending delivery while a snapshot transaction is still
// open. put writes the row into notification_outbox on the transaction's own
// connection, so the queue entry and the version bump commit together.
type Enqueuer func(changed []PeriodRow, put func(dest string, destID int64, payload []byte) error) error

// ApplyClassSnapshot transactionally rewrites the snapshot for a class. It
// returns the number of rows whose mod version changed. Removed periods whose
// start date is before dropRemovedBefore (format YYYY-MM-DD) are silently
// deleted rather than being reported, so periods that merely age out of the
// sliding fetch window do not trigger spurious REMOVED notifications.
//
// When the change is non-empty and enqueue is non-nil, enqueue is called with
// the changed rows while the transaction is still open, so queued deliveries
// become durable in the very same commit that stamps newVer. That ordering is
// the point: the version bump and the notification must not be able to disagree.
// Before this, the version was committed first and the send was fired from a
// goroutine afterwards, so a failed or dropped send lost the notification
// permanently — the next poll saw no change and re-sent nothing.
//
// enqueue must not query the Store. The pool runs a single connection, so a
// read inside this transaction would wait for the very connection the
// transaction holds and deadlock. Anything needed while enqueueing — such as
// the list of webhooks and ntfy topics — has to be read beforehand.
func (s *Store) ApplyClassSnapshot(school string, classID int64, next []PeriodRow, newVer int64, dropRemovedBefore string, enqueue Enqueuer) (int, error) {
	old, err := s.LoadClassSnapshot(school, classID)
	if err != nil {
		return 0, err
	}
	oldByID := map[int64]PeriodRow{}
	for _, r := range old {
		oldByID[r.PeriodID] = r
	}
	changed := 0
	var changedRows []PeriodRow
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM timetable_changes WHERE school=? AND class_id=?`, school, classID); err != nil {
		return 0, err
	}
	for _, r := range next {
		prev, existed := oldByID[r.PeriodID]
		cur := r
		// A period that is back after a cancellation is a reinstatement, and
		// kind is deliberately not part of the comparison below. Without this
		// a lesson that was cancelled and then restored with byte-identical
		// data matches UNCHANGED, keeps the mod version it was removed at, and
		// is never announced — the lesson silently reappears on the timetable
		// and nobody is told.
		reinstated := existed && prev.Kind == "REMOVED"
		if existed && !reinstated && prev.Start == r.Start && prev.End == r.End &&
			prev.Subject == r.Subject && prev.Room == r.Room && prev.Teacher == r.Teacher && prev.Description == r.Description {
			cur.Kind = "UNCHANGED"
			cur.ModVer = prev.ModVer
		} else {
			cur.ModVer = newVer
			switch {
			case reinstated:
				// ADDED rather than CHANGED: it is back on the timetable, and
				// the digest marks ADDED as "new". CHANGED carries no marker at
				// all, so subscribers would be told a lesson is back without
				// being told which lesson, or that it is back.
				cur.Kind = "ADDED"
			case existed:
				cur.Kind = "CHANGED"
			default:
				cur.Kind = "ADDED"
			}
			changed++
			changedRows = append(changedRows, cur)
		}
		oldByID[r.PeriodID] = cur
		if cur.Kind == "UNCHANGED" {
			// keep existing row; preserve old values
			cur.Start, cur.End, cur.Subject, cur.Room, cur.Teacher, cur.Description = prev.Start, prev.End, prev.Subject, prev.Room, prev.Teacher, prev.Description
			rows, err := tx.Exec(`INSERT INTO timetable_changes (school,class_id,period_id,kind,start,end,subject,room,teacher,description,mod_ver)
				VALUES (?,?,?,?,?,?,?,?,?,?,?)`, school, classID, r.PeriodID, cur.Kind, cur.Start, cur.End, cur.Subject, cur.Room, cur.Teacher, cur.Description, cur.ModVer)
			if err != nil {
				return 0, err
			}
			_ = rows
			continue
		}
		if _, err := tx.Exec(`INSERT INTO timetable_changes (school,class_id,period_id,kind,start,end,subject,room,teacher,description,mod_ver)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`, school, classID, cur.PeriodID, cur.Kind, cur.Start, cur.End, cur.Subject, cur.Room, cur.Teacher, cur.Description, cur.ModVer); err != nil {
			return 0, err
		}
	}
	// mark periods present in old but absent from new as REMOVED
	seen := map[int64]bool{}
	for _, r := range next {
		seen[r.PeriodID] = true
	}
	for pid, r := range oldByID {
		if seen[pid] {
			continue
		}
		// silently drop past periods that left the window
		if dropRemovedBefore != "" && len(r.Start) >= 10 && r.Start[:10] < dropRemovedBefore {
			continue
		}
		// A period already recorded as REMOVED is still missing upstream, so
		// there is nothing new to report. Re-insert it with its ORIGINAL mod
		// version so /api/timetable/changes can still see it, but do not stamp
		// the new version and do not count it as changed.
		//
		// Without this, every poll re-stamped the row with the current version
		// and returned changed>0, so one cancelled lesson that had not yet
		// happened re-notified forever: once a minute, all day, per matching
		// subscription. The dropRemovedBefore guard above does not save us,
		// because it only covers periods whose start is already past — and a
		// cancellation is exactly a period that is today or later.
		if r.Kind == "REMOVED" {
			if _, err := tx.Exec(`INSERT INTO timetable_changes (school,class_id,period_id,kind,start,end,subject,room,teacher,description,mod_ver)
				VALUES (?,?,?,?,?,?,?,?,?,?,?)`, school, classID, pid, r.Kind, r.Start, r.End, r.Subject, r.Room, r.Teacher, r.Description, r.ModVer); err != nil {
				return 0, err
			}
			continue
		}
		r.Kind = "REMOVED"
		r.ModVer = newVer
		if _, err := tx.Exec(`INSERT INTO timetable_changes (school,class_id,period_id,kind,start,end,subject,room,teacher,description,mod_ver)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`, school, classID, pid, r.Kind, r.Start, r.End, r.Subject, r.Room, r.Teacher, r.Description, r.ModVer); err != nil {
			return 0, err
		}
		changed++
		changedRows = append(changedRows, r)
	}
	if changed > 0 {
		if _, err := tx.Exec(`INSERT INTO timetable_versions (school,class_id,version) VALUES (?,?,?)
			ON CONFLICT(school,class_id) DO UPDATE SET version=excluded.version`, school, classID, newVer); err != nil {
			return 0, err
		}
		if enqueue != nil {
			put := func(dest string, destID int64, payload []byte) error {
				return insertOutbox(tx, school, classID, newVer, dest, destID, payload, time.Now())
			}
			if err := enqueue(changedRows, put); err != nil {
				// The delivery could not be queued, so the change must not be
				// recorded: committing here would leave the new version in place
				// with nothing to send it, and the next poll would see no change.
				return 0, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return changed, nil
}

// outboxInserter is satisfied by both *sql.Tx and *sql.DB, so the queue insert
// can run inside the snapshot transaction or standalone.
type outboxInserter interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func insertOutbox(q outboxInserter, school string, classID, version int64, dest string, destID int64, payload []byte, now time.Time) error {
	_, err := q.Exec(`INSERT INTO notification_outbox
		(school,class_id,version,dest,dest_id,payload,state,attempts,last_error,created_at,next_attempt_at)
		VALUES (?,?,?,?,?,?,'pending',0,'',?,0)`,
		school, classID, version, dest, destID, string(payload), now.Unix())
	return err
}

// EnqueueOutbox queues one delivery outside a snapshot transaction. Delivery
// inside ApplyClassSnapshot uses that transaction instead; this exists for
// replaying a failed or dead delivery, and as the seam tests enqueue through.
func (s *Store) EnqueueOutbox(school string, classID, version int64, dest string, destID int64, payload []byte) error {
	return insertOutbox(s.db, school, classID, version, dest, destID, payload, time.Now())
}

// OutboxRow is one queued delivery to one destination.
type OutboxRow struct {
	ID       int64
	School   string
	ClassID  int64
	Version  int64
	Dest     string // "webhook" or "ntfy"
	DestID   int64
	Payload  []byte
	Attempts int
	LastErr  string
	Created  time.Time
}

// OutboxStats counts queued deliveries by state, for the dashboard.
type OutboxStats struct {
	Pending int
	Sending int
	Dead    int
}

// DueOutbox returns up to limit deliveries that are ready to attempt: state
// pending, and either due now or left behind in 'sending' by a process that died
// mid-flight. Reclaiming 'sending' rows is what makes a crash mid-send
// recoverable rather than a silent loss.
func (s *Store) DueOutbox(now time.Time, limit int) ([]OutboxRow, error) {
	cutoff := now.Unix()
	rows, err := s.db.Query(`SELECT id, school, class_id, version, dest, dest_id, payload,
		attempts, last_error, created_at
		FROM notification_outbox
		WHERE (state='pending' AND next_attempt_at<=?) OR (state='sending')
		ORDER BY id LIMIT ?`, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxRow
	for rows.Next() {
		var r OutboxRow
		var created int64
		if err := rows.Scan(&r.ID, &r.School, &r.ClassID, &r.Version, &r.Dest, &r.DestID,
			&r.Payload, &r.Attempts, &r.LastErr, &created); err != nil {
			return nil, err
		}
		r.Created = time.Unix(created, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ClaimOutbox marks a row as in flight and returns false if someone else got
// there first.
func (s *Store) ClaimOutbox(id int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE notification_outbox SET state='sending' WHERE id=? AND state='pending'`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// FinishOutbox records a successful delivery and prunes the row, so the table
// stays bounded. Keeping sent rows would grow without limit and nothing reads
// them.
func (s *Store) FinishOutbox(id int64) error {
	_, err := s.db.Exec(`DELETE FROM notification_outbox WHERE id=? AND state='sending'`, id)
	return err
}

// FailOutbox records a failed attempt and schedules the retry. Once attempts
// reaches maxAttempts the row is marked dead instead, so an unreachable
// destination stops consuming a retry slot forever but stays visible for an
// operator to inspect.
func (s *Store) FailOutbox(id int64, attempts int, cause string, retryAt time.Time, maxAttempts int) error {
	state := "pending"
	if attempts >= maxAttempts {
		state = "dead"
	}
	if len(cause) > 500 {
		cause = cause[:500]
	}
	_, err := s.db.Exec(`UPDATE notification_outbox
		SET state=?, attempts=?, last_error=?, next_attempt_at=? WHERE id=?`,
		state, attempts, cause, retryAt.Unix(), id)
	return err
}

// ReleaseOutboxStale requeues every row abandoned in the 'sending' state.
//
// A worker that dies mid-request leaves its claimed rows behind, and those
// notifications would otherwise never be delivered. This is called once at
// startup, before the worker begins, so no delivery can be in flight and every
// 'sending' row is by definition abandoned. Requeued rows become due
// immediately: the alternative is to silently delay a notification by a stale
// window every restart.
//
// This assumes a single running process, which is how the proxy is deployed.
// With two workers sharing the database, the correct approach is a separate
// claimed_at column and a cutoff; that is not needed here.
func (s *Store) ReleaseOutboxStale() error {
	_, err := s.db.Exec(`UPDATE notification_outbox SET state='pending', next_attempt_at=?
		WHERE state='sending'`, time.Now().Unix())
	return err
}

// OutboxStats counts queued deliveries by state.
func (s *Store) OutboxStats() (OutboxStats, error) {
	var st OutboxStats
	rows, err := s.db.Query(`SELECT state, COUNT(*) FROM notification_outbox GROUP BY state`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return st, err
		}
		switch state {
		case "pending":
			st.Pending = n
		case "sending":
			st.Sending = n
		case "dead":
			st.Dead = n
		}
	}
	return st, rows.Err()
}

// RecentDeadOutbox returns the most recent exhausted deliveries, newest first,
// so a failed destination can be diagnosed without opening the database.
func (s *Store) RecentDeadOutbox(limit int) ([]OutboxRow, error) {
	rows, err := s.db.Query(`SELECT id, school, class_id, version, dest, dest_id, payload,
		attempts, last_error, created_at
		FROM notification_outbox WHERE state='dead' ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxRow
	for rows.Next() {
		var r OutboxRow
		var created int64
		if err := rows.Scan(&r.ID, &r.School, &r.ClassID, &r.Version, &r.Dest, &r.DestID,
			&r.Payload, &r.Attempts, &r.LastErr, &created); err != nil {
			return nil, err
		}
		r.Created = time.Unix(created, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetHomeworkDone records that a student marked a piece of homework done, and
// returns when it happened.
//
// homeworkID is the upstream `homeWorks[].id`, which is stable for the life of
// the assignment. The viewer is the only identity accepted: callers must pass
// the resolved session user, never one taken from a request body.
func (s *Store) SetHomeworkDone(school, username string, homeworkID int64) (time.Time, error) {
	// Truncated to the second, because that is all the column keeps. The value
	// that is written and the value that is returned are the same one, so a client
	// sees the same doneAt right after writing as it does on any later read.
	now := time.Now().Truncate(time.Second)
	_, err := s.db.Exec(`INSERT INTO homework_done (school, username, hw_id, done_at) VALUES (?,?,?,?)
		ON CONFLICT(school, username, hw_id) DO UPDATE SET done_at=excluded.done_at`,
		school, norm(username), homeworkID, now.Unix())
	return now, err
}

// ClearHomeworkDone removes a student's done flag. Clearing an absent flag is
// not an error: the endpoint is idempotent, so a client that toggles twice or
// retries after a dropped response converges on the same state.
func (s *Store) ClearHomeworkDone(school, username string, homeworkID int64) error {
	_, err := s.db.Exec(`DELETE FROM homework_done WHERE school=? AND username=? AND hw_id=?`,
		school, norm(username), homeworkID)
	return err
}

// HomeworkDone returns the student's done flags keyed by homework id. A missing
// flag is simply absent from the map, so enrichment can treat absence as false
// without inventing a row per assignment.
func (s *Store) HomeworkDone(school, username string) (map[int64]time.Time, error) {
	rows, err := s.db.Query(`SELECT hw_id, done_at FROM homework_done WHERE school=? AND username=?`,
		school, norm(username))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]time.Time{}
	for rows.Next() {
		var id, at int64
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		out[id] = time.Unix(at, 0)
	}
	return out, rows.Err()
}

// HomeworkDoneCount reports how many flags a student has set. It exists so a
// caller can assert on the number of stored rows without enumerating them — the
// idempotency of SetHomeworkDone is a claim about rows, not about a filtered
// view, so a test that only checked HomeworkDone would pass even if a duplicate
// row had been written alongside the one it read back.
func (s *Store) HomeworkDoneCount(school, username string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM homework_done WHERE school=? AND username=?`,
		school, norm(username)).Scan(&n)
	return n, err
}

// SetAbsenceNote records a student's private note on one absence and returns when
// it was written.
//
// absenceKey is the upstream absence `id`. The viewer is the only identity
// accepted: callers must pass the resolved session user, never one taken from a
// request body.
func (s *Store) SetAbsenceNote(school, username string, absenceKey int64, note string) (time.Time, error) {
	now := time.Now().Truncate(time.Second)
	_, err := s.db.Exec(`INSERT INTO absence_notes (school, username, absence_key, note, updated_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT(school, username, absence_key) DO UPDATE SET
			note=excluded.note, updated_at=excluded.updated_at`,
		school, norm(username), absenceKey, note, now.Unix())
	return now, err
}

// ClearAbsenceNote removes a student's note. Clearing an absent note is not an
// error, so a repeated clear — or a client retrying after a dropped response —
// converges on the same state.
func (s *Store) ClearAbsenceNote(school, username string, absenceKey int64) error {
	_, err := s.db.Exec(`DELETE FROM absence_notes WHERE school=? AND username=? AND absence_key=?`,
		school, norm(username), absenceKey)
	return err
}

// AbsenceNote is one stored note and when it was last written.
type AbsenceNote struct {
	Key       int64
	Note      string
	UpdatedAt time.Time
}

// AbsenceNotes returns every note the student has written, keyed by absence id.
// An absence with no note is simply absent from the map, so enrichment does not
// have to invent an empty row per absence.
func (s *Store) AbsenceNotes(school, username string) (map[int64]AbsenceNote, error) {
	rows, err := s.db.Query(`SELECT absence_key, note, updated_at FROM absence_notes
		WHERE school=? AND username=?`, school, norm(username))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]AbsenceNote{}
	for rows.Next() {
		var key, at int64
		var note string
		if err := rows.Scan(&key, &note, &at); err != nil {
			return nil, err
		}
		out[key] = AbsenceNote{Key: key, Note: note, UpdatedAt: time.Unix(at, 0)}
	}
	return out, rows.Err()
}

// StudentEvent is one hand-authored event on a student's own schedule.
type StudentEvent struct {
	ID          int64
	School      string
	Username    string
	Date        string // YYYY-MM-DD, the only date form stored
	StartTime   string // HH:MM
	EndTime     string // HH:MM
	Title       string
	Subject     string
	Room        string
	Teacher     string
	Description string
	CreatedBy   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Revision    int64
}

// NewStudentEvent is the writable subset of a StudentEvent. The caller supplies
// the student, and the store owns the identity, the timestamps and the revision.
type NewStudentEvent struct {
	Date        string
	StartTime   string
	EndTime     string
	Title       string
	Subject     string
	Room        string
	Teacher     string
	Description string
}

// CreateStudentEvent inserts an event for a student and returns it as stored,
// including the id and revision the caller did not know.
func (s *Store) CreateStudentEvent(school, username string, ev NewStudentEvent, createdBy string) (StudentEvent, error) {
	now := time.Now().Truncate(time.Second)
	res, err := s.db.Exec(`INSERT INTO student_events
		(school, username, date, start_time, end_time, title, subject, room, teacher,
		 description, created_by, created_at, updated_at, revision)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,1)`,
		school, norm(username), ev.Date, ev.StartTime, ev.EndTime, ev.Title,
		ev.Subject, ev.Room, ev.Teacher, ev.Description, norm(createdBy),
		now.Unix(), now.Unix())
	if err != nil {
		return StudentEvent{}, err
	}
	if err := s.BumpStudentEventVersion(school, username); err != nil {
		return StudentEvent{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return StudentEvent{}, err
	}
	return StudentEvent{
		ID: id, School: school, Username: norm(username),
		Date: ev.Date, StartTime: ev.StartTime, EndTime: ev.EndTime, Title: ev.Title,
		Subject: ev.Subject, Room: ev.Room, Teacher: ev.Teacher, Description: ev.Description,
		CreatedBy: norm(createdBy), CreatedAt: now, UpdatedAt: now, Revision: 1,
	}, nil
}

// StudentEventPatch is a partial edit. A nil field is left alone, which is what
// makes "change only the room" possible without the caller reading the row first
// and racing another admin.
type StudentEventPatch struct {
	Date        *string
	StartTime   *string
	EndTime     *string
	Title       *string
	Subject     *string
	Room        *string
	Teacher     *string
	Description *string
}

// UpdateStudentEvent applies a partial edit and bumps the revision, so a client
// holding the event can tell it changed even if both edits land in one second.
//
// It reports whether a row matched. A missing id is not an error: an admin
// deleting an event twice, or editing one another admin just deleted, converges
// on the same outcome as editing one that is there.
func (s *Store) UpdateStudentEvent(school string, id int64, patch StudentEventPatch) (StudentEvent, bool, error) {
	before, found, err := s.StudentEventByID(school, id)
	if err != nil || !found {
		return StudentEvent{}, found, err
	}
	// Applied by hand rather than built from a map so every column is named once
	// and a field absent from the patch cannot be overwritten with a zero value.
	date, start, end := before.Date, before.StartTime, before.EndTime
	title, subject, room := before.Title, before.Subject, before.Room
	teacher, description := before.Teacher, before.Description
	if patch.Date != nil {
		date = *patch.Date
	}
	if patch.StartTime != nil {
		start = *patch.StartTime
	}
	if patch.EndTime != nil {
		end = *patch.EndTime
	}
	if patch.Title != nil {
		title = *patch.Title
	}
	if patch.Subject != nil {
		subject = *patch.Subject
	}
	if patch.Room != nil {
		room = *patch.Room
	}
	if patch.Teacher != nil {
		teacher = *patch.Teacher
	}
	if patch.Description != nil {
		description = *patch.Description
	}
	now := time.Now().Truncate(time.Second)
	_, err = s.db.Exec(`UPDATE student_events SET
			date=?, start_time=?, end_time=?, title=?, subject=?, room=?, teacher=?,
			description=?, updated_at=?, revision=revision+1
		WHERE school=? AND event_id=?`,
		date, start, end, title, subject, room, teacher, description, now.Unix(), school, id)
	if err != nil {
		return StudentEvent{}, false, err
	}
	if err := s.BumpStudentEventVersion(school, before.Username); err != nil {
		return StudentEvent{}, false, err
	}
	before.Date, before.StartTime, before.EndTime = date, start, end
	before.Title, before.Subject, before.Room = title, subject, room
	before.Teacher, before.Description = teacher, description
	before.UpdatedAt = now
	before.Revision++
	return before, true, nil
}

// DeleteStudentEvent removes an event and reports whether a row was there.
// Deleting an absent event is not an error, so a retry after a lost response
// converges rather than failing.
func (s *Store) DeleteStudentEvent(school string, id int64) (bool, error) {
	// The username is read before the delete: afterwards the row that names the
	// student whose counter needs bumping is gone.
	var username string
	err := s.db.QueryRow(`SELECT username FROM student_events WHERE school=? AND event_id=?`, school, id).Scan(&username)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	res, err := s.db.Exec(`DELETE FROM student_events WHERE school=? AND event_id=?`, school, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return n > 0, err
	}
	if err := s.BumpStudentEventVersion(school, username); err != nil {
		return true, err
	}
	return true, nil
}

// BumpStudentEventVersion raises one student's event counter and returns the new
// value. Called on every admin edit so the student's connected clients can be told
// to refresh.
func (s *Store) BumpStudentEventVersion(school, username string) error {
	_, err := s.db.Exec(`INSERT INTO student_event_versions (school, username, version)
		VALUES (?,?,1)
		ON CONFLICT(school, username) DO UPDATE SET version=version+1`,
		school, norm(username))
	return err
}

// StudentEventVersion returns the student's current event counter. A student who
// has never had an event reports 0, which is the same value a client would have
// stored from an empty timeline, so no change looks like no change.
func (s *Store) StudentEventVersion(school, username string) (int64, error) {
	var v int64
	err := s.db.QueryRow(`SELECT version FROM student_event_versions WHERE school=? AND username=?`,
		school, norm(username)).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return v, nil
}

// StudentEventByID returns one event scoped to a school. The school is part of
// the lookup rather than a returned field to check afterwards, so a caller cannot
// accidentally act on an id belonging to another school.
func (s *Store) StudentEventByID(school string, id int64) (StudentEvent, bool, error) {
	row := s.db.QueryRow(`SELECT `+studentEventCols+` FROM student_events WHERE school=? AND event_id=?`, school, id)
	ev, err := scanStudentEvent(row)
	if err == sql.ErrNoRows {
		return StudentEvent{}, false, nil
	}
	return ev, err == nil, err
}

const studentEventCols = `event_id, school, username, date, start_time, end_time, title,
	subject, room, teacher, description, created_by, created_at, updated_at, revision`

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

func scanStudentEvent(row scanner) (StudentEvent, error) {
	var ev StudentEvent
	var created, updated int64
	if err := row.Scan(&ev.ID, &ev.School, &ev.Username, &ev.Date, &ev.StartTime, &ev.EndTime,
		&ev.Title, &ev.Subject, &ev.Room, &ev.Teacher, &ev.Description, &ev.CreatedBy,
		&created, &updated, &ev.Revision); err != nil {
		return StudentEvent{}, err
	}
	ev.CreatedAt = time.Unix(created, 0)
	ev.UpdatedAt = time.Unix(updated, 0)
	return ev, nil
}

// StudentEventsForRange returns a student's events whose date lies in [from, to)
// — half-open, so a caller walking consecutive days does not serve the same
// event twice at a boundary.
func (s *Store) StudentEventsForRange(school, username, from, to string) ([]StudentEvent, error) {
	rows, err := s.db.Query(`SELECT `+studentEventCols+` FROM student_events
		WHERE school=? AND username=? AND date>=? AND date<?
		ORDER BY date, start_time, event_id`, school, norm(username), from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStudentEvents(rows)
}

// StudentEvents returns every event for a student, for the admin listing. Unlike
// the range read this is not bounded, so it is only for administration.
func (s *Store) StudentEvents(school, username string) ([]StudentEvent, error) {
	rows, err := s.db.Query(`SELECT `+studentEventCols+` FROM student_events
		WHERE school=? AND username=? ORDER BY date, start_time, event_id`, school, norm(username))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStudentEvents(rows)
}

// ListStudentEvents returns one student's events across every school, for the
// admin search. A username is unique per proxy instance, so scoping by it alone
// is enough and the school stays a returned field for display.
func (s *Store) ListStudentEvents(username string) ([]StudentEvent, error) {
	rows, err := s.db.Query(`SELECT `+studentEventCols+` FROM student_events
		WHERE username=? ORDER BY date, start_time, event_id`, norm(username))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStudentEvents(rows)
}

// StudentEventCounts reports how many events each student has, for the admin
// dashboard. A map keyed by "school|username" keeps two students who share a
// name at different schools apart.
func (s *Store) StudentEventCounts() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT school, username, COUNT(*) FROM student_events
		GROUP BY school, username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var school, username string
		var n int
		if err := rows.Scan(&school, &username, &n); err != nil {
			return nil, err
		}
		out[school+"|"+username] = n
	}
	return out, rows.Err()
}

func scanStudentEvents(rows *sql.Rows) ([]StudentEvent, error) {
	out := []StudentEvent{}
	for rows.Next() {
		ev, err := scanStudentEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// StudentEventRange is the widest window StudentEventsForRange will serve in one
// call. An admin-authored event is a scheduled appointment, so it lives on a
// specific date rather than a term, but the ICS feed still asks for a rolling
// lookahead of months.
const StudentEventRange = 400

// ClassPeriodsOnDate returns the periods a class has on one date, for matching an
// absence to the lesson it displaced.
//
// Only the current snapshot is searched, and periods that fall out of the polling
// window are deleted, so a date further back than the horizon returns nothing.
// Callers must therefore treat an empty result as "unknown", never as "no lesson".
func (s *Store) ClassPeriodsOnDate(school string, classID int64, date string) ([]PeriodRow, error) {
	rows, err := s.db.Query(`SELECT period_id, kind, start, end, subject, room, teacher, description, mod_ver
		FROM timetable_changes WHERE school=? AND class_id=? AND substr(start,1,10)=?
		ORDER BY start`, school, classID, date)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PeriodRow
	for rows.Next() {
		var r PeriodRow
		if err := rows.Scan(&r.PeriodID, &r.Kind, &r.Start, &r.End, &r.Subject, &r.Room, &r.Teacher, &r.Description, &r.ModVer); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PendingChanges returns the periods modified after the given version, plus the
// current class version.
func (s *Store) PendingChanges(school string, classID int64, since int64) ([]PeriodRow, int64, error) {
	rows, err := s.db.Query(`SELECT period_id, kind, start, end, subject, room, teacher, description, mod_ver
		FROM timetable_changes WHERE school=? AND class_id=? AND mod_ver>?
		ORDER BY period_id`, school, classID, since)
	if err != nil {
		return nil, s.ClassVersion(school, classID), err
	}
	defer rows.Close()
	var out []PeriodRow
	for rows.Next() {
		var r PeriodRow
		if err := rows.Scan(&r.PeriodID, &r.Kind, &r.Start, &r.End, &r.Subject, &r.Room, &r.Teacher, &r.Description, &r.ModVer); err != nil {
			return nil, s.ClassVersion(school, classID), err
		}
		out = append(out, r)
	}
	return out, s.ClassVersion(school, classID), rows.Err()
}
