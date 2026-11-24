package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"untis-proxy/internal/access"
)

// Access/permission schema and migration bridge from v1 flags/settings.
// Additive and idempotent so v2 can run in parallel/migrated environments.

func migrateAccessSchema(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS account_person_links (school TEXT NOT NULL,username TEXT NOT NULL,active_person_id INTEGER NOT NULL,persons_json TEXT NOT NULL,PRIMARY KEY(school,username))`,
		`CREATE TABLE IF NOT EXISTS cap_groups (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			school TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL DEFAULT 0,
			UNIQUE(school, name)
		)`,
		`CREATE TABLE IF NOT EXISTS cap_group_grants (
			group_id INTEGER NOT NULL,
			capability TEXT NOT NULL,
			effect INTEGER NOT NULL,
			PRIMARY KEY(group_id, capability)
		)`,
		`CREATE TABLE IF NOT EXISTS user_cap_groups (
			school TEXT NOT NULL DEFAULT '',
			username TEXT NOT NULL,
			group_id INTEGER NOT NULL,
			created_at INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY(school, username, group_id)
		)`,
		`CREATE TABLE IF NOT EXISTS user_cap_grants (
			school TEXT NOT NULL DEFAULT '',
			username TEXT NOT NULL,
			capability TEXT NOT NULL,
			effect INTEGER NOT NULL,
			updated_at INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY(school, username, capability)
		)`,
		`CREATE TABLE IF NOT EXISTS role_default_groups (
			school TEXT NOT NULL DEFAULT '',
			role TEXT NOT NULL,
			group_id INTEGER NOT NULL,
			priority INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY(school, role, group_id)
		)`,
		`CREATE TABLE IF NOT EXISTS user_cap_prefs (
			school TEXT NOT NULL DEFAULT '',
			username TEXT NOT NULL,
			capability TEXT NOT NULL,
			opted_in INTEGER NOT NULL,
			updated_at INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY(school, username, capability)
		)`,
		`CREATE TABLE IF NOT EXISTS server_cap_gates (
			scope TEXT NOT NULL DEFAULT '',
			capability TEXT NOT NULL,
			enabled INTEGER NOT NULL,
			updated_at INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY(scope, capability)
		)`,
		`CREATE TABLE IF NOT EXISTS school_anon_profiles (
			school TEXT NOT NULL PRIMARY KEY,
			login_ok INTEGER NOT NULL DEFAULT 0,
			profile_json TEXT NOT NULL DEFAULT '',
			detected_at INTEGER NOT NULL DEFAULT 0,
			expires_at INTEGER NOT NULL DEFAULT 0,
			source TEXT NOT NULL DEFAULT 'detected'
		)`,
		`CREATE TABLE IF NOT EXISTS person_type_roles (
			scope TEXT NOT NULL DEFAULT '',
			person_type INTEGER NOT NULL,
			role TEXT NOT NULL,
			PRIMARY KEY(scope, person_type)
		)`,
		`CREATE INDEX IF NOT EXISTS user_cap_groups_user ON user_cap_groups(school, username)`,
		`CREATE INDEX IF NOT EXISTS user_cap_grants_user ON user_cap_grants(school, username)`,
		`CREATE INDEX IF NOT EXISTS cap_group_grants_group ON cap_group_grants(group_id)`,
		`CREATE INDEX IF NOT EXISTS role_default_groups_scope ON role_default_groups(school, role)`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

const accessMigrationDoneKey = "v2_access_migrated"

func (s *Store) migrateAccessData() error {
	// Always ensure builtin groups/grants are present and up to date.
	if err := s.ensureBuiltinCapGroups(); err != nil {
		return err
	}
	var v string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, accessMigrationDoneKey).Scan(&v); err == nil && v == "1" {
		return nil
	}
	log.Printf("[access] migrating v1 permissions/anonymous settings to access model")
	if err := s.migrateV1DefaultPermsToRoleDefaults(); err != nil {
		return err
	}
	if err := s.migrateV1UserPermsToAccessModel(); err != nil {
		return err
	}
	if err := s.migrateV1AnonymousToAccessModel(); err != nil {
		return err
	}
	_, err := s.db.Exec(`INSERT INTO settings (key,value) VALUES (?, '1')
		ON CONFLICT(key) DO UPDATE SET value='1'`, accessMigrationDoneKey)
	return err
}

// Built-in group names used as migration targets/compat anchors.
const (
	CapGroupStudentBase    = "v2.student_base"
	CapGroupTeacherBase    = "v2.teacher_base"
	CapGroupUpstreamBasic  = "v2.upstream_basic"
	CapGroupRecon          = "v2.recon"
	CapGroupBoosted        = "v2.boosted"
	CapGroupEditor         = "v2.editor"
	CapGroupTechniker      = "v2.techniker"
	CapGroupDefaultNewUser = "v2.default_new_user"
)

func (s *Store) ensureBuiltinCapGroups() error {
	groups := []struct {
		name   string
		desc   string
		grants []access.Grant
	}{
		{name: CapGroupUpstreamBasic, desc: "Basic: stock upstream reads only; add proxy features separately", grants: []access.Grant{
			{Capability: access.CapTimetableUpstreamView, Allow: true},
			{Capability: access.CapTimetableClassUpstreamView, Allow: true},
			{Capability: access.CapTimetableStudentPersonalUpstreamView, Allow: true},
			{Capability: access.CapAbsenceOwnRead, Allow: true},
		}},
		{name: CapGroupStudentBase, desc: "Baseline capabilities for authenticated real student accounts", grants: []access.Grant{
			{Capability: access.CapHomeworkOwnManage, Allow: true},
			{Capability: access.CapAbsenceNotesOwnManage, Allow: true},
			{Capability: access.CapEventsPersonalView, Allow: true},

			{Capability: access.CapTimetableClassPooledView, Allow: true},
			{Capability: access.CapTimetableClassUpstreamView, Allow: true},
			{Capability: access.CapPoolTimetableShare, Allow: true},
			{Capability: access.CapTimetableStudentPersonalUpstreamView, Allow: true},
			{Capability: access.CapPoolClassView, Allow: true},
			{Capability: access.CapAbsenceOwnRead, Allow: true},
			{Capability: access.CapChangesClassView, Allow: true},
			{Capability: access.CapChangesClassStream, Allow: true},
			{Capability: access.CapNotifyNtfyCreate, Allow: true},
			{Capability: access.CapNotifyNtfyManageOwn, Allow: true},
			{Capability: access.CapNotifyWebhookCreate, Allow: true},
			{Capability: access.CapNotifyWebhookManageOwn, Allow: true},
			{Capability: access.CapCalendarManageOwn, Allow: true},
			{Capability: access.CapCalendarClassMint, Allow: true},
			{Capability: access.CapCalendarStudentPersonalMint, Allow: true},
		}},
		{name: CapGroupTeacherBase, desc: "Baseline capabilities for authenticated teacher/staff accounts", grants: []access.Grant{
			{Capability: access.CapHomeworkOwnManage, Allow: true},
			{Capability: access.CapAbsenceNotesOwnManage, Allow: true},
			{Capability: access.CapEventsPersonalView, Allow: true},

			{Capability: access.CapTimetableClassUpstreamView, Allow: true},
			{Capability: access.CapPoolTimetableShare, Allow: true},
			{Capability: access.CapAbsenceOwnRead, Allow: true},
			{Capability: access.CapChangesClassView, Allow: true},
			{Capability: access.CapChangesClassStream, Allow: true},
			{Capability: access.CapNotifyNtfyCreate, Allow: true},
			{Capability: access.CapNotifyNtfyManageOwn, Allow: true},
			{Capability: access.CapNotifyWebhookCreate, Allow: true},
			{Capability: access.CapNotifyWebhookManageOwn, Allow: true},
			{Capability: access.CapCalendarManageOwn, Allow: true},
			{Capability: access.CapCalendarClassMint, Allow: true},
			{Capability: access.CapCalendarStudentPersonalMint, Allow: true},
		}},
		{name: CapGroupRecon, desc: "Reconstructed teacher/room/subject views and related discovery/calendar/changes", grants: []access.Grant{
			{Capability: access.CapTimetableTeacherReconstructedView, Allow: true},
			{Capability: access.CapTimetableRoomReconstructedView, Allow: true},
			{Capability: access.CapTimetableSubjectReconstructedView, Allow: true},
			{Capability: access.CapCalendarTeacherMint, Allow: true},
			{Capability: access.CapCalendarRoomMint, Allow: true},
			{Capability: access.CapCalendarSubjectMint, Allow: true},
			{Capability: access.CapChangesElementView, Allow: true},
			{Capability: access.CapChangesElementStream, Allow: true},
		}},
		{name: CapGroupBoosted, desc: "Raw forwarding via teacher/boosted account (teacher-grade visibility)", grants: []access.Grant{
			{Capability: access.CapTimetableClassBoostedRawView, Allow: true},
			{Capability: access.CapTimetableStudentPersonalBoostedRawView, Allow: true},
			{Capability: access.CapTimetableTeacherBoostedRawView, Allow: true},
			{Capability: access.CapTimetableRoomBoostedRawView, Allow: true},
			{Capability: access.CapTimetableSubjectBoostedRawView, Allow: true},
			{Capability: access.CapTimetableTeacherReconstructedView, Allow: true},
			{Capability: access.CapTimetableRoomReconstructedView, Allow: true},
			{Capability: access.CapTimetableSubjectReconstructedView, Allow: true},
			{Capability: access.CapCalendarManageOwn, Allow: true},
			{Capability: access.CapCalendarClassMint, Allow: true},
			{Capability: access.CapCalendarStudentPersonalMint, Allow: true},
			{Capability: access.CapCalendarTeacherMint, Allow: true},
			{Capability: access.CapCalendarRoomMint, Allow: true},
			{Capability: access.CapCalendarSubjectMint, Allow: true},
			{Capability: access.CapChangesElementView, Allow: true},
			{Capability: access.CapChangesElementStream, Allow: true},
			{Capability: access.CapChangesSchoolView, Allow: true},
			{Capability: access.CapChangesSchoolStream, Allow: true},
		}},
		{name: CapGroupEditor, desc: "Editing/writes (implies boosted-class data access patterns in v1)", grants: []access.Grant{
			{Capability: access.CapAbsenceWrite, Allow: true},
			{Capability: access.CapLessonWrite, Allow: true},
			{Capability: access.CapChangesSchoolView, Allow: true},
			{Capability: access.CapChangesSchoolStream, Allow: true},
		}},
		{name: CapGroupTechniker, desc: "Technik aggregate class and proxy event surfaces", grants: []access.Grant{
			{Capability: access.CapEventsTechnikView, Allow: true},
			{Capability: access.CapEventsTechnikManage, Allow: true},
			{Capability: access.CapCalendarManageOwn, Allow: true},
			{Capability: access.CapCalendarTechnikMint, Allow: true},
		}},
		{name: "v2.guardian_base", desc: "Own upstream linked-child timetable reads for guardian accounts", grants: []access.Grant{
			{Capability: access.CapHomeworkOwnManage, Allow: true},
			{Capability: access.CapAbsenceNotesOwnManage, Allow: true},
			{Capability: access.CapEventsPersonalView, Allow: true},

			{Capability: access.CapTimetableStudentPersonalUpstreamView, Allow: true},
			{Capability: access.CapAbsenceOwnRead, Allow: true},
			{Capability: access.CapNotifyNtfyCreate, Allow: true},
			{Capability: access.CapNotifyNtfyManageOwn, Allow: true},
		}},
		{name: CapGroupDefaultNewUser, desc: "Migrated v1 default permissions for newly seen users", grants: nil},
	}
	for _, g := range groups {
		id, err := s.ensureGroup("", g.name, g.desc)
		if err != nil {
			return err
		}
		for _, gr := range g.grants {
			if err := s.setGroupGrant(id, gr.Capability, gr.Allow); err != nil {
				return err
			}
		}
	}
	if id, ok, _ := s.groupIDByName("", "v2.guardian_base"); ok {
		_ = s.ensureRoleDefaultGroup("", "parent", id, 0)
	}
	// Ensure base role defaults exist (idempotent).
	if id, ok, _ := s.groupIDByName("", CapGroupStudentBase); ok {
		_ = s.ensureRoleDefaultGroup("", string(access.RoleStudent), id, 0)
		// Compat baseline for authenticated accounts whose person type is unknown/other.
		_ = s.ensureRoleDefaultGroup("", string(access.RoleUnknown), id, 0)
		_ = s.ensureRoleDefaultGroup("", string(access.RoleOther), id, 0)
	}
	if id, ok, _ := s.groupIDByName("", CapGroupTeacherBase); ok {
		_ = s.ensureRoleDefaultGroup("", string(access.RoleTeacher), id, 0)
		_ = s.ensureRoleDefaultGroup("", string(access.RoleStaff), id, 0)
	}
	return nil
}

func (s *Store) ensureGroup(school, name, desc string) (int64, error) {
	now := time.Now().Unix()
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO cap_groups (school,name,description,created_at,updated_at) VALUES (?,?,?,?,?)`,
		school, name, desc, now, now); err != nil {
		return 0, err
	}
	id, ok, err := s.groupIDByName(school, name)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("group %q not found after ensure", name)
	}
	return id, nil
}

func (s *Store) groupIDByName(school, name string) (int64, bool, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM cap_groups WHERE school=? AND name=?`, school, name).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	return id, err == nil, err
}

func (s *Store) setGroupGrant(groupID int64, cap access.Capability, allow bool) error {
	eff := -1
	if allow {
		eff = 1
	}
	_, err := s.db.Exec(`INSERT INTO cap_group_grants (group_id,capability,effect) VALUES (?,?,?)
		ON CONFLICT(group_id,capability) DO UPDATE SET effect=excluded.effect`, groupID, string(cap), eff)
	return err
}

func (s *Store) ensureRoleDefaultGroup(school, role string, groupID int64, priority int) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO role_default_groups (school,role,group_id,priority) VALUES (?,?,?,?)`,
		school, strings.ToLower(role), groupID, priority)
	return err
}

func (s *Store) migrateV1DefaultPermsToRoleDefaults() error {
	// Keep CapGroupDefaultNewUser as a template reflecting current default-perm
	// settings (for introspection), but do NOT assign it as a role default.
	// v1 default permissions are applied per-user on first insert via
	// applyDefaultPerms; making them role defaults would grant them to everyone.
	id, ok, err := s.groupIDByName("", CapGroupDefaultNewUser)
	if err != nil || !ok {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM cap_group_grants WHERE group_id=?`, id); err != nil {
		return err
	}
	// Remove any legacy role-default assignments of this template group.
	_, _ = s.db.Exec(`DELETE FROM role_default_groups WHERE group_id=?`, id)

	def, err := s.DefaultPerms()
	if err != nil {
		return err
	}
	if def[FeatureRecon] {
		if gid, ok, _ := s.groupIDByName("", CapGroupRecon); ok {
			_ = s.copyGroupGrants(gid, id)
		}
	}
	if def[FeatureBoosted] {
		if gid, ok, _ := s.groupIDByName("", CapGroupBoosted); ok {
			_ = s.copyGroupGrants(gid, id)
		}
	}
	if def[FeatureEditor] {
		if gid, ok, _ := s.groupIDByName("", CapGroupEditor); ok {
			_ = s.copyGroupGrants(gid, id)
		}
		if gid, ok, _ := s.groupIDByName("", CapGroupBoosted); ok {
			// v1 editor implied boosted access patterns.
			_ = s.copyGroupGrants(gid, id)
		}
	}
	if def[FeatureTechniker] {
		if gid, ok, _ := s.groupIDByName("", CapGroupTechniker); ok {
			_ = s.copyGroupGrants(gid, id)
		}
	}

	// Server gates: common capabilities enabled by default unless overridden later.
	_ = s.ensureServerGateDefault("", access.CapAbsenceOwnRead, true)
	_ = s.ensureServerGateDefault("", access.CapTimetableClassPooledView, true)
	_ = s.ensureServerGateDefault("", access.CapTimetableClassUpstreamView, true)
	_ = s.ensureServerGateDefault("", access.CapTimetableStudentPersonalUpstreamView, true)
	_ = s.ensureServerGateDefault("", access.CapPoolClassView, true)
	_ = s.ensureServerGateDefault("", access.CapCalendarManageOwn, true)
	return nil
}

func (s *Store) copyGroupGrants(src, dst int64) error {
	rows, err := s.db.Query(`SELECT capability, effect FROM cap_group_grants WHERE group_id=?`, src)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cap string
		var eff int
		if err := rows.Scan(&cap, &eff); err != nil {
			return err
		}
		_, err = s.db.Exec(`INSERT INTO cap_group_grants (group_id,capability,effect) VALUES (?,?,?)
			ON CONFLICT(group_id,capability) DO UPDATE SET effect=excluded.effect`, dst, cap, eff)
		if err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *Store) ensureServerGateDefault(scope string, cap access.Capability, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := s.db.Exec(`INSERT OR IGNORE INTO server_cap_gates (scope,capability,enabled,updated_at) VALUES (?,?,?,?)`,
		scope, string(cap), v, time.Now().Unix())
	return err
}

func (s *Store) migrateV1UserPermsToAccessModel() error {
	rows, err := s.db.Query(`SELECT username, feature, allowed FROM perms WHERE username<>?`, globalPermUser)
	if err != nil {
		return err
	}
	defer rows.Close()
	type row struct {
		user    string
		feature string
		allow   bool
	}
	var all []row
	for rows.Next() {
		var r row
		var a int
		if err := rows.Scan(&r.user, &r.feature, &a); err != nil {
			return err
		}
		r.allow = a != 0
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range all {
		switch r.feature {
		case FeatureRecon, FeatureBoosted, FeatureEditor, FeatureTechniker:
			_ = s.applyDirectFeatureFromV1(r.user, r.feature, r.allow)
		}
	}
	// Global recon switch from v1 -> make recon group a broad role default if enabled.
	if v, err := s.hasPermRow(globalPermUser, FeatureRecon); err == nil && v {
		_ = s.setGlobalReconRoleDefaults(true)
	}
	return nil
}

func (s *Store) hasPermRow(username, feature string) (bool, error) {
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

func (s *Store) userSchool(username string) string {
	var school string
	_ = s.db.QueryRow(`SELECT school FROM users WHERE username=?`, norm(username)).Scan(&school)
	return school
}

func (s *Store) ensureUserInGroup(school, username, groupName string) error {
	gid, ok, err := s.groupIDByName("", groupName)
	if err != nil || !ok {
		return err
	}
	_, err = s.db.Exec(`INSERT OR IGNORE INTO user_cap_groups (school,username,group_id,created_at) VALUES (?,?,?,?)`,
		school, norm(username), gid, time.Now().Unix())
	return err
}

func (s *Store) migrateV1AnonymousToAccessModel() error {
	// v1 had global anon feature toggles off by default. In the access model, anonymous access
	// is per-school upstream-detected and never gets proxy-pooled supplements; we
	// keep anonymous login per school (schools.anonymous) and record conservative
	// server gates/placeholders so detection can fill profiles later.
	// No automatic enabling of proxy supplements.
	return nil
}

// Capability group management

type CapGroup struct {
	ID          int64
	School      string
	Name        string
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type CapGroupGrant struct {
	Capability string
	Effect     int // 1 allow, -1 deny
}

func (s *Store) ListCapGroups(school string) ([]CapGroup, error) {
	rows, err := s.db.Query(`SELECT id,school,name,description,created_at,updated_at FROM cap_groups WHERE school=? OR school='' ORDER BY school,name`, school)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CapGroup
	for rows.Next() {
		var g CapGroup
		var ca, ua int64
		if err := rows.Scan(&g.ID, &g.School, &g.Name, &g.Description, &ca, &ua); err != nil {
			return nil, err
		}
		g.CreatedAt, g.UpdatedAt = time.Unix(ca, 0), time.Unix(ua, 0)
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) CreateCapGroup(school, name, desc string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, fmt.Errorf("group name required")
	}
	now := time.Now().Unix()
	res, err := s.db.Exec(`INSERT INTO cap_groups (school,name,description,created_at,updated_at) VALUES (?,?,?,?,?)`,
		school, name, desc, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) DeleteCapGroup(id int64) error {
	if _, err := s.db.Exec(`DELETE FROM cap_group_grants WHERE group_id=?`, id); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM user_cap_groups WHERE group_id=?`, id); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM role_default_groups WHERE group_id=?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM cap_groups WHERE id=?`, id)
	return err
}

func (s *Store) GetCapGroup(id int64) (*CapGroup, []CapGroupGrant, error) {
	var g CapGroup
	var ca, ua int64
	err := s.db.QueryRow(`SELECT id,school,name,description,created_at,updated_at FROM cap_groups WHERE id=?`, id).
		Scan(&g.ID, &g.School, &g.Name, &g.Description, &ca, &ua)
	if err == sql.ErrNoRows {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	g.CreatedAt, g.UpdatedAt = time.Unix(ca, 0), time.Unix(ua, 0)
	rows, err := s.db.Query(`SELECT capability,effect FROM cap_group_grants WHERE group_id=? ORDER BY capability`, id)
	if err != nil {
		return &g, nil, err
	}
	defer rows.Close()
	var grants []CapGroupGrant
	for rows.Next() {
		var gr CapGroupGrant
		if err := rows.Scan(&gr.Capability, &gr.Effect); err != nil {
			return &g, nil, err
		}
		grants = append(grants, gr)
	}
	return &g, grants, rows.Err()
}

func (s *Store) SetCapGroupGrant(groupID int64, cap string, effect int) error {
	cap = strings.TrimSpace(cap)
	if cap == "" {
		return fmt.Errorf("capability required")
	}
	if effect == 0 {
		_, err := s.db.Exec(`DELETE FROM cap_group_grants WHERE group_id=? AND capability=?`, groupID, cap)
		return err
	}
	if effect > 0 {
		effect = 1
	} else {
		effect = -1
	}
	_, err := s.db.Exec(`INSERT INTO cap_group_grants (group_id,capability,effect) VALUES (?,?,?)
		ON CONFLICT(group_id,capability) DO UPDATE SET effect=excluded.effect`, groupID, cap, effect)
	if err == nil {
		_, _ = s.db.Exec(`UPDATE cap_groups SET updated_at=? WHERE id=?`, time.Now().Unix(), groupID)
	}
	return err
}

// User/group assignment and grants

func (s *Store) AssignUserCapGroup(school, username string, groupID int64) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO user_cap_groups (school,username,group_id,created_at) VALUES (?,?,?,?)`,
		school, norm(username), groupID, time.Now().Unix())
	return err
}

func (s *Store) UnassignUserCapGroup(school, username string, groupID int64) error {
	_, err := s.db.Exec(`DELETE FROM user_cap_groups WHERE school=? AND username=? AND group_id=?`,
		school, norm(username), groupID)
	return err
}

func (s *Store) ListUserCapGroups(school, username string) ([]CapGroup, error) {
	q := `SELECT g.id,g.school,g.name,g.description,g.created_at,g.updated_at
		FROM cap_groups g JOIN user_cap_groups ug ON ug.group_id=g.id
		WHERE ug.username=? AND ug.school=? ORDER BY g.name`
	args := []any{norm(username), school}
	if school != "" {
		q = `SELECT g.id,g.school,g.name,g.description,g.created_at,g.updated_at
		FROM cap_groups g JOIN user_cap_groups ug ON ug.group_id=g.id
		WHERE ug.username=? AND (ug.school=? OR ug.school='') ORDER BY g.name, g.id`
		args = []any{norm(username), school}
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[int64]bool{}
	var out []CapGroup
	for rows.Next() {
		var g CapGroup
		var ca, ua int64
		if err := rows.Scan(&g.ID, &g.School, &g.Name, &g.Description, &ca, &ua); err != nil {
			return nil, err
		}
		if seen[g.ID] {
			continue
		}
		seen[g.ID] = true
		g.CreatedAt, g.UpdatedAt = time.Unix(ca, 0), time.Unix(ua, 0)
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) SetUserCapGrant(school, username, cap string, effect int) error {
	cap = strings.TrimSpace(cap)
	if cap == "" {
		return fmt.Errorf("capability required")
	}
	if effect == 0 {
		_, err := s.db.Exec(`DELETE FROM user_cap_grants WHERE school=? AND username=? AND capability=?`,
			school, norm(username), cap)
		return err
	}
	if effect > 0 {
		effect = 1
	} else {
		effect = -1
	}
	_, err := s.db.Exec(`INSERT INTO user_cap_grants (school,username,capability,effect,updated_at) VALUES (?,?,?,?,?)
		ON CONFLICT(school,username,capability) DO UPDATE SET effect=excluded.effect, updated_at=excluded.updated_at`,
		school, norm(username), cap, effect, time.Now().Unix())
	return err
}

func (s *Store) ListUserCapGrants(school, username string) ([]CapGroupGrant, error) {
	q := `SELECT capability,effect FROM user_cap_grants WHERE school=? AND username=? ORDER BY capability`
	args := []any{school, norm(username)}
	if school != "" {
		q = `SELECT capability,effect,school FROM user_cap_grants WHERE username=? AND (school=? OR school='') ORDER BY capability, school`
		args = []any{norm(username), school}
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type item struct {
		eff   int
		scope string
	}
	best := map[string]item{}
	for rows.Next() {
		var cap string
		var eff int
		scope := school
		if school != "" {
			if err := rows.Scan(&cap, &eff, &scope); err != nil {
				return nil, err
			}
		} else {
			if err := rows.Scan(&cap, &eff); err != nil {
				return nil, err
			}
		}
		if prev, ok := best[cap]; ok {
			// prefer school-specific over global
			if prev.scope == school && scope == "" {
				continue
			}
		}
		best[cap] = item{eff: eff, scope: scope}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(best))
	for k := range best {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]CapGroupGrant, 0, len(keys))
	for _, k := range keys {
		out = append(out, CapGroupGrant{Capability: k, Effect: best[k].eff})
	}
	return out, nil
}

// Role defaults

func (s *Store) SetRoleDefaultGroup(school, role string, groupID int64, priority int) error {
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "" {
		return fmt.Errorf("role required")
	}
	if groupID <= 0 {
		_, err := s.db.Exec(`DELETE FROM role_default_groups WHERE school=? AND role=?`, school, role)
		return err
	}
	_, err := s.db.Exec(`INSERT INTO role_default_groups (school,role,group_id,priority) VALUES (?,?,?,?)
		ON CONFLICT(school,role,group_id) DO UPDATE SET priority=excluded.priority`, school, role, groupID, priority)
	return err
}

func (s *Store) ListRoleDefaultGroups(school, role string) ([]int64, error) {
	rows, err := s.db.Query(`SELECT group_id FROM role_default_groups WHERE (school=? OR school='') AND role=? ORDER BY priority,group_id`, school, strings.ToLower(role))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[int64]bool{}
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, rows.Err()
}

// DeleteRoleDefaultGroup removes one default group assignment for a role.
func (s *Store) DeleteRoleDefaultGroup(school, role string, groupID int64) error {
	role = strings.ToLower(strings.TrimSpace(role))
	_, err := s.db.Exec(`DELETE FROM role_default_groups WHERE school=? AND role=? AND group_id=?`, school, role, groupID)
	return err
}

// Person type -> role mappings (admin-configurable). Scope "" is global.

type PersonTypeRole struct {
	Scope      string `json:"scope"`
	PersonType int64  `json:"personType"`
	Role       string `json:"role"`
}

func (s *Store) ListPersonTypeRoles(scope string) ([]PersonTypeRole, error) {
	rows, err := s.db.Query(`SELECT scope, person_type, role FROM person_type_roles WHERE (?='' OR scope=?) ORDER BY scope, person_type`, scope, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PersonTypeRole
	for rows.Next() {
		var r PersonTypeRole
		if err := rows.Scan(&r.Scope, &r.PersonType, &r.Role); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) SetPersonTypeRole(scope string, personType int64, role string) error {
	role = strings.ToLower(strings.TrimSpace(string(access.NormalizeRole(role))))
	if role == "" {
		return fmt.Errorf("role required")
	}
	_, err := s.db.Exec(`INSERT INTO person_type_roles (scope, person_type, role) VALUES (?,?,?)
		ON CONFLICT(scope, person_type) DO UPDATE SET role=excluded.role`, scope, personType, role)
	return err
}

func (s *Store) DeletePersonTypeRole(scope string, personType int64) error {
	_, err := s.db.Exec(`DELETE FROM person_type_roles WHERE scope=? AND person_type=?`, scope, personType)
	return err
}

// User preferences

func (s *Store) SetUserCapPref(school, username, cap string, optedIn bool) error {
	v := 0
	if optedIn {
		v = 1
	}
	_, err := s.db.Exec(`INSERT INTO user_cap_prefs (school,username,capability,opted_in,updated_at) VALUES (?,?,?,?,?)
		ON CONFLICT(school,username,capability) DO UPDATE SET opted_in=excluded.opted_in, updated_at=excluded.updated_at`,
		school, norm(username), cap, v, time.Now().Unix())
	return err
}

func (s *Store) UserCapPrefs(school, username string) (map[string]bool, error) {
	out := map[string]bool{}
	if school != "" {
		// global defaults first, then school-specific overrides
		rows, err := s.db.Query(`SELECT capability,opted_in,school FROM user_cap_prefs WHERE username=? AND (school=? OR school='') ORDER BY school='' DESC`, norm(username), school)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var cap string
			var v int
			var sc string
			if err := rows.Scan(&cap, &v, &sc); err != nil {
				return nil, err
			}
			// rows ordered so global ('') come last? "school='' DESC" gives true(1) for '' in SQLite? in SQLite boolean expr returns 0/1; DESC means 1 first (global) then 0 (specific) - so specific would overwrite. better to process in deterministic way: read all then apply with precedence.
			out[cap] = v != 0
			_ = sc
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		// re-apply with explicit precedence: global then specific
		out = map[string]bool{}
		rows2, err := s.db.Query(`SELECT capability,opted_in,school FROM user_cap_prefs WHERE username=? AND (school=? OR school='')`, norm(username), school)
		if err != nil {
			return nil, err
		}
		defer rows2.Close()
		type pv struct {
			v        bool
			specific bool
		}
		tmp := map[string]pv{}
		for rows2.Next() {
			var cap string
			var v int
			var sc string
			if err := rows2.Scan(&cap, &v, &sc); err != nil {
				return nil, err
			}
			cur, ok := tmp[cap]
			isSpecific := sc == school
			if !ok || (!cur.specific && isSpecific) {
				tmp[cap] = pv{v: v != 0, specific: isSpecific}
			}
		}
		for k, v := range tmp {
			out[k] = v.v
		}
		return out, rows2.Err()
	}
	rows, err := s.db.Query(`SELECT capability,opted_in FROM user_cap_prefs WHERE school=? AND username=?`, school, norm(username))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var cap string
		var v int
		if err := rows.Scan(&cap, &v); err != nil {
			return nil, err
		}
		out[cap] = v != 0
	}
	return out, rows.Err()
}

// Server gates and anon profiles

func (s *Store) SetServerCapGate(scope, cap string, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := s.db.Exec(`INSERT INTO server_cap_gates (scope,capability,enabled,updated_at) VALUES (?,?,?,?)
		ON CONFLICT(scope,capability) DO UPDATE SET enabled=excluded.enabled, updated_at=excluded.updated_at`,
		scope, cap, v, time.Now().Unix())
	return err
}

// A global deny is a hard ceiling. Schools can disable a feature but cannot
// re-enable a capability disabled by the server operator.
func (s *Store) ServerCapGates(scope string) (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT capability,enabled FROM server_cap_gates WHERE scope='' OR scope=?`, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var cap string
		var enabled int
		if err := rows.Scan(&cap, &enabled); err != nil {
			return nil, err
		}
		previous, exists := out[cap]
		out[cap] = enabled != 0 && (!exists || previous)
	}
	return out, rows.Err()
}

type SchoolAnonProfile struct {
	School      string
	LoginOK     bool
	ProfileJSON string
	DetectedAt  time.Time
	ExpiresAt   time.Time
	Source      string
}

func (s *Store) GetSchoolAnonProfile(school string) (*SchoolAnonProfile, error) {
	var p SchoolAnonProfile
	var login int
	var da, ea int64
	err := s.db.QueryRow(`SELECT school,login_ok,profile_json,detected_at,expires_at,source FROM school_anon_profiles WHERE school=?`, school).
		Scan(&p.School, &login, &p.ProfileJSON, &da, &ea, &p.Source)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.LoginOK = login != 0
	p.DetectedAt, p.ExpiresAt = time.Unix(da, 0), time.Unix(ea, 0)
	return &p, nil
}

func (s *Store) UpsertSchoolAnonProfile(p *SchoolAnonProfile) error {
	login := 0
	if p.LoginOK {
		login = 1
	}
	_, err := s.db.Exec(`INSERT INTO school_anon_profiles (school,login_ok,profile_json,detected_at,expires_at,source) VALUES (?,?,?,?,?,?)
		ON CONFLICT(school) DO UPDATE SET login_ok=excluded.login_ok, profile_json=excluded.profile_json, detected_at=excluded.detected_at, expires_at=excluded.expires_at, source=excluded.source`,
		p.School, login, p.ProfileJSON, p.DetectedAt.Unix(), p.ExpiresAt.Unix(), p.Source)
	return err
}

func (s *Store) DeleteSchoolAnonProfile(school string) error {
	_, err := s.db.Exec(`DELETE FROM school_anon_profiles WHERE school=?`, school)
	return err
}

func (s *Store) ListSchoolAnonProfiles() ([]*SchoolAnonProfile, error) {
	rows, err := s.db.Query(`SELECT school,login_ok,profile_json,detected_at,expires_at,source FROM school_anon_profiles ORDER BY school`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*SchoolAnonProfile
	for rows.Next() {
		var p SchoolAnonProfile
		var login int
		var da, ea int64
		if err := rows.Scan(&p.School, &login, &p.ProfileJSON, &da, &ea, &p.Source); err != nil {
			return nil, err
		}
		p.LoginOK = login != 0
		p.DetectedAt, p.ExpiresAt = time.Unix(da, 0), time.Unix(ea, 0)
		out = append(out, &p)
	}
	return out, rows.Err()
}

func (s *Store) ListServerCapGates() ([]struct {
	Scope   string
	Cap     string
	Enabled bool
}, error) {
	rows, err := s.db.Query(`SELECT scope,capability,enabled FROM server_cap_gates ORDER BY scope,capability`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct {
		Scope   string
		Cap     string
		Enabled bool
	}
	for rows.Next() {
		var r struct {
			Scope   string
			Cap     string
			Enabled bool
		}
		var en int
		if err := rows.Scan(&r.Scope, &r.Cap, &en); err != nil {
			return nil, err
		}
		r.Enabled = en != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// Effective resolution (v2)

type EffectiveResult struct {
	Role                    access.Role
	Decisions               map[access.Capability]access.Decision
	GroupIDs                []int64
	DirectGrants            []CapGroupGrant
	InheritedGroupIDs       []int64
	ExcludedDefaultGroupIDs []int64
	InheritRoleDefaults     bool
}

func (s *Store) ResolveEffective(school string, u *User) (*EffectiveResult, error) {
	return s.resolveEffective(school, u, nil)
}
func (s *Store) resolveEffective(school string, u *User, preview *GroupPreview) (*EffectiveResult, error) {
	if u == nil {
		return &EffectiveResult{Role: access.RoleUnknown, Decisions: access.Resolve(access.Inputs{})}, nil
	}
	role := s.resolveUserRole(school, u.PersonType)
	if links, err := s.AccountLinks(school, u.Username); err == nil && links.Guardian {
		role = access.Role("parent")
	}
	gates, gateErr := s.ServerCapGates(school)
	if gateErr != nil {
		return nil, gateErr
	}
	serverEnabled := map[access.Capability]bool{}
	for _, c := range access.AllCapabilities {
		// default enabled unless explicitly disabled
		if v, ok := gates[string(c)]; ok {
			serverEnabled[c] = v
		} else {
			serverEnabled[c] = defaultServerEnabled(c)
		}
	}

	featureStates := s.FeatureStates(school)
	for _, c := range access.AllCapabilities {
		if feature := FeatureForCapability(string(c)); feature != "" && !featureStates[feature] {
			serverEnabled[c] = false
		}
	}

	direct := map[access.Capability]access.Effect{}
	directRows, directErr := s.ListUserCapGrants(school, u.Username)
	if directErr != nil {
		return nil, directErr
	}
	for _, g := range directRows {
		if g.Effect > 0 {
			direct[access.Capability(g.Capability)] = access.EffectAllow
		} else if g.Effect < 0 {
			direct[access.Capability(g.Capability)] = access.EffectDeny
		}
	}

	// User groups (school-specific and global? assignments stored with school; also include global groups by group scope handling)
	groups, groupErr := s.ListUserCapGroups(school, u.Username)
	if groupErr != nil {
		return nil, groupErr
	}
	var groupGrants []map[access.Capability]access.Effect
	var groupIDs []int64
	for _, g := range groups {
		groupIDs = append(groupIDs, g.ID)
		if m, err := s.previewGroupEffects(g.ID, preview); err == nil {
			groupGrants = append(groupGrants, m)
		} else {
			return nil, err
		}
	}

	// Role defaults
	defIDs, defaultsErr := s.ListRoleDefaultGroups(school, string(role))
	if defaultsErr != nil {
		return nil, defaultsErr
	}
	if len(defIDs) == 0 {
		defIDs, defaultsErr = s.ListRoleDefaultGroups("", string(role))
		if defaultsErr != nil {
			return nil, defaultsErr
		}
	}
	// The everyone audience applies to current and future authenticated accounts.
	everyoneIDs, everyoneErr := s.ListRoleDefaultGroups(school, "*")
	if everyoneErr != nil {
		return nil, everyoneErr
	}
	defIDs = append(defIDs, everyoneIDs...)
	if preview != nil && preview.AssignTo != "" && (preview.AssignTo == "*" || preview.AssignTo == string(role)) {
		defIDs = append(defIDs, preview.ID)
	}

	inheritance, inheritErr := s.DefaultGroupPolicy(school, u.Username)
	if inheritErr != nil {
		return nil, inheritErr
	}
	inheritedIDs := append([]int64{}, defIDs...)
	filteredIDs := []int64{}
	for _, id := range defIDs {
		if inheritance.Inherit && !containsGroupID(inheritance.ExcludedGroupIDs, id) {
			filteredIDs = append(filteredIDs, id)
		}
	}
	defIDs = filteredIDs

	var roleGrants []map[access.Capability]access.Effect
	for _, id := range defIDs {
		if m, err := s.previewGroupEffects(id, preview); err == nil {
			roleGrants = append(roleGrants, m)
		} else {
			return nil, err
		}
	}

	prefsRaw, prefsErr := s.UserCapPrefs(school, u.Username)
	if prefsErr != nil {
		return nil, prefsErr
	}
	prefs := map[access.Capability]bool{}
	for k, v := range prefsRaw {
		prefs[access.Capability(k)] = v
	}

	in := access.Inputs{
		ServerEnabled:     serverEnabled,
		Direct:            direct,
		GroupGrants:       groupGrants,
		RoleDefaultGrants: roleGrants,
		UserPrefs:         prefs,
		Toggleable:        defaultToggleableCaps(),
	}
	decisions := access.Resolve(in)
	// Expand legacy manage only when no operation-specific rule exists. Preview
	// and enforcement consume exactly these same resolved decisions.
	legacy := decisions[access.CapEventsTechnikManage]
	for _, cap := range []access.Capability{access.CapEventsTechnikCreate, access.CapEventsTechnikEdit, access.CapEventsTechnikDelete, access.CapEventsTechnikImport, access.CapEventsTechnikAttendanceRead, access.CapEventsTechnikAttendanceWrite} {
		d := decisions[cap]
		if d.Source == access.SourceImplicit && legacy.AdminAllowed {
			d.AdminAllowed = true
			d.Effective = d.ServerEnabled && d.UserOptedIn && legacy.Effective
			d.Source = legacy.Source
			decisions[cap] = d
		}
	}

	return &EffectiveResult{
		Role:              role,
		Decisions:         decisions,
		GroupIDs:          groupIDs,
		DirectGrants:      directRows,
		InheritedGroupIDs: inheritedIDs, ExcludedDefaultGroupIDs: inheritance.ExcludedGroupIDs, InheritRoleDefaults: inheritance.Inherit,
	}, nil
}

func (s *Store) groupGrantEffects(groupID int64) (map[access.Capability]access.Effect, error) {
	rows, err := s.db.Query(`SELECT capability,effect FROM cap_group_grants WHERE group_id=?`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[access.Capability]access.Effect{}
	for rows.Next() {
		var cap string
		var eff int
		if err := rows.Scan(&cap, &eff); err != nil {
			return nil, err
		}
		if eff > 0 {
			out[access.Capability(cap)] = access.EffectAllow
		} else if eff < 0 {
			out[access.Capability(cap)] = access.EffectDeny
		}
	}
	return out, rows.Err()
}

func (s *Store) resolveUserRole(school string, personType int64) access.Role {
	// scope-specific mapping wins over global
	for _, scope := range []string{school, ""} {
		var role string
		err := s.db.QueryRow(`SELECT role FROM person_type_roles WHERE scope=? AND person_type=?`, scope, personType).Scan(&role)
		if err == nil && strings.TrimSpace(role) != "" {
			return access.NormalizeRole(role)
		}
	}
	return access.DefaultRoleForPersonType(personType)
}

func defaultServerEnabled(cap access.Capability) bool {
	// first-level gate: server enablement. Default to enabled so migrated v1
	// grants/defaults remain effective; operators can restrict via server gates.
	// Anonymous calendar minting is still forbidden by construction in handlers.
	_ = cap
	return true
}

func defaultToggleableCaps() map[access.Capability]bool {
	// Conservative set: user may opt out of many granted capabilities.
	out := map[access.Capability]bool{}
	for _, c := range access.AllCapabilities {
		out[c] = true
	}
	// Keep own absence read non-toggleable as baseline safety/usability.
	out[access.CapAbsenceOwnRead] = false
	return out
}

// EffectiveHas is a convenience helper.
func (s *Store) EffectiveHas(school string, u *User, cap access.Capability) bool {
	res, err := s.ResolveEffective(school, u)
	if err != nil || res == nil {
		return false
	}
	return access.Effective(res.Decisions, cap)
}

// v1 compat helpers

func (s *Store) userSchoolByUsername(username string) string {
	var school string
	_ = s.db.QueryRow(`SELECT school FROM users WHERE username=?`, norm(username)).Scan(&school)
	return school
}

func (s *Store) ensureUserInCapGroup(school, username, groupName string, on bool) error {
	gid, ok, err := s.groupIDByName("", groupName)
	if err != nil || !ok {
		return err
	}
	if on {
		_, err = s.db.Exec(`INSERT OR IGNORE INTO user_cap_groups (school,username,group_id,created_at) VALUES (?,?,?,?)`,
			school, norm(username), gid, time.Now().Unix())
		return err
	}
	_, err = s.db.Exec(`DELETE FROM user_cap_groups WHERE school=? AND username=? AND group_id=?`,
		school, norm(username), gid)
	return err
}

func (s *Store) setDirectCapGrant(school, username, cap string, effect int) error {
	return s.SetUserCapGrant(school, username, cap, effect)
}

func (s *Store) clearUserAccessAssignments(school, username string) error {
	u := norm(username)
	if _, err := s.db.Exec(`DELETE FROM user_cap_groups WHERE school=? AND username=?`, school, u); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM user_cap_grants WHERE school=? AND username=?`, school, u); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM user_cap_prefs WHERE school=? AND username=?`, school, u); err != nil {
		return err
	}
	return nil
}

func (s *Store) setGlobalReconRoleDefaults(on bool) error {
	gid, ok, err := s.groupIDByName("", CapGroupRecon)
	if err != nil || !ok {
		return err
	}
	roles := []string{string(access.RoleStudent), string(access.RoleTeacher), string(access.RoleStaff), string(access.RoleUnknown), string(access.RoleOther)}
	for _, role := range roles {
		if on {
			_, err = s.db.Exec(`INSERT INTO role_default_groups (school,role,group_id,priority) VALUES ('',?,?,5)
				ON CONFLICT(school,role,group_id) DO UPDATE SET priority=excluded.priority`, role, gid)
		} else {
			_, err = s.db.Exec(`DELETE FROM role_default_groups WHERE school='' AND role=? AND group_id=?`, role, gid)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) setDefaultNewUserCapGroup(feature string, on bool) error {
	id, ok, err := s.groupIDByName("", CapGroupDefaultNewUser)
	if err != nil || !ok {
		return err
	}
	// Keep template grants in sync with default settings, but never make this a
	// role default (defaults apply only on first user insert in v1 compat).
	_, _ = s.db.Exec(`DELETE FROM role_default_groups WHERE group_id=?`, id)
	if _, err := s.db.Exec(`DELETE FROM cap_group_grants WHERE group_id=?`, id); err != nil {
		return err
	}
	def, err := s.DefaultPerms()
	if err != nil {
		return err
	}
	// apply requested toggle on top of current settings view (settings may already be updated)
	def[feature] = on
	// enforce mutual exclusion in defaults (v1 compat)
	if on {
		switch feature {
		case FeatureRecon:
			def[FeatureBoosted] = false
		case FeatureBoosted:
			def[FeatureRecon] = false
		}
	}
	if def[FeatureRecon] {
		if gid, ok, _ := s.groupIDByName("", CapGroupRecon); ok {
			_ = s.copyGroupGrants(gid, id)
		}
	}
	if def[FeatureBoosted] {
		if gid, ok, _ := s.groupIDByName("", CapGroupBoosted); ok {
			_ = s.copyGroupGrants(gid, id)
		}
	}
	if def[FeatureEditor] {
		if gid, ok, _ := s.groupIDByName("", CapGroupEditor); ok {
			_ = s.copyGroupGrants(gid, id)
		}
		if gid, ok, _ := s.groupIDByName("", CapGroupBoosted); ok {
			_ = s.copyGroupGrants(gid, id)
		}
	}
	if def[FeatureTechniker] {
		if gid, ok, _ := s.groupIDByName("", CapGroupTechniker); ok {
			_ = s.copyGroupGrants(gid, id)
		}
	}
	return nil
}

func v1ReconCapsFor(elType string) []access.Capability {
	switch strings.ToUpper(strings.TrimSpace(elType)) {
	case "TEACHER":
		return []access.Capability{access.CapTimetableTeacherReconstructedView}
	case "ROOM":
		return []access.Capability{access.CapTimetableRoomReconstructedView}
	case "SUBJECT":
		return []access.Capability{access.CapTimetableSubjectReconstructedView}
	case "", "ALL":
		return []access.Capability{
			access.CapTimetableTeacherReconstructedView,
			access.CapTimetableRoomReconstructedView,
			access.CapTimetableSubjectReconstructedView,
		}
	default:
		return []access.Capability{
			access.CapTimetableTeacherReconstructedView,
			access.CapTimetableRoomReconstructedView,
			access.CapTimetableSubjectReconstructedView,
		}
	}
}

func (s *Store) reconAccessEffective(username, elType string) bool {
	username = norm(username)
	school := s.userSchoolByUsername(username)
	u, err := s.GetUser(username)
	if err != nil || u == nil {
		// fall back to a synthetic user shell for resolution by username-only cases
		u = &User{Username: username, School: school}
	} else if school == "" {
		school = u.School
	}
	res, err := s.ResolveEffective(school, u)
	if err != nil || res == nil {
		return false
	}
	for _, cap := range v1ReconCapsFor(elType) {
		if access.Effective(res.Decisions, cap) {
			return true
		}
	}
	return false
}

func (s *Store) boostedAccessEffective(username string) bool {
	username = norm(username)
	school := s.userSchoolByUsername(username)
	u, err := s.GetUser(username)
	if err != nil || u == nil {
		u = &User{Username: username, School: school}
	} else if school == "" {
		school = u.School
	}
	res, err := s.ResolveEffective(school, u)
	if err != nil || res == nil {
		return false
	}
	// v1: boosted OR editor implies boosted access.
	for _, cap := range []access.Capability{
		access.CapTimetableClassBoostedRawView,
		access.CapTimetableTeacherBoostedRawView,
		access.CapTimetableRoomBoostedRawView,
		access.CapTimetableSubjectBoostedRawView,
		access.CapTimetableStudentPersonalBoostedRawView,
	} {
		if access.Effective(res.Decisions, cap) {
			return true
		}
	}
	// editor group in v1 implied boosted behavior; treat effective editor write caps as boosted-capable for compat
	if access.Effective(res.Decisions, access.CapAbsenceWrite) || access.Effective(res.Decisions, access.CapLessonWrite) {
		return true
	}
	return false
}

func (s *Store) editorAccessEffective(username string) bool {
	username = norm(username)
	school := s.userSchoolByUsername(username)
	u, err := s.GetUser(username)
	if err != nil || u == nil {
		u = &User{Username: username, School: school}
	} else if school == "" {
		school = u.School
	}
	res, err := s.ResolveEffective(school, u)
	if err != nil || res == nil {
		return false
	}
	return access.Effective(res.Decisions, access.CapAbsenceWrite) || access.Effective(res.Decisions, access.CapLessonWrite)
}

func (s *Store) technikerAccessEffective(username string) bool {
	username = norm(username)
	school := s.userSchoolByUsername(username)
	u, err := s.GetUser(username)
	if err != nil || u == nil {
		u = &User{Username: username, School: school}
	} else if school == "" {
		school = u.School
	}
	res, err := s.ResolveEffective(school, u)
	if err != nil || res == nil {
		return false
	}
	return access.Effective(res.Decisions, access.CapEventsTechnikView) || access.Effective(res.Decisions, access.CapEventsTechnikManage)
}

func (s *Store) applyDirectFeatureFromV1(username, feature string, allowed bool) error {
	username = norm(username)
	school := s.userSchoolByUsername(username)
	switch feature {
	case FeatureRecon:
		if allowed {
			_ = s.ensureUserInCapGroup(school, username, CapGroupRecon, true)
			// clear explicit recon denies
			for _, cap := range v1ReconCapsFor("") {
				_ = s.setDirectCapGrant(school, username, string(cap), 0)
			}
		} else {
			_ = s.ensureUserInCapGroup(school, username, CapGroupRecon, false)
			// explicit deny overrides role/global defaults
			for _, cap := range v1ReconCapsFor("") {
				_ = s.setDirectCapGrant(school, username, string(cap), -1)
			}
		}
		return nil
	case FeatureBoosted:
		if allowed {
			_ = s.ensureUserInCapGroup(school, username, CapGroupBoosted, true)
			// boosted implies not having recon denies forced except mutual exclusion handled separately
			for _, cap := range []access.Capability{
				access.CapTimetableClassBoostedRawView,
				access.CapTimetableTeacherBoostedRawView,
				access.CapTimetableRoomBoostedRawView,
				access.CapTimetableSubjectBoostedRawView,
				access.CapTimetableStudentPersonalBoostedRawView,
			} {
				_ = s.setDirectCapGrant(school, username, string(cap), 0)
			}
		} else {
			_ = s.ensureUserInCapGroup(school, username, CapGroupBoosted, false)
		}
		return nil
	case FeatureEditor:
		if allowed {
			_ = s.ensureUserInCapGroup(school, username, CapGroupEditor, true)
			_ = s.ensureUserInCapGroup(school, username, CapGroupBoosted, true)
			_ = s.setDirectCapGrant(school, username, string(access.CapAbsenceWrite), 0)
			_ = s.setDirectCapGrant(school, username, string(access.CapLessonWrite), 0)
		} else {
			_ = s.ensureUserInCapGroup(school, username, CapGroupEditor, false)
			// editor implies boosted in v1; revoking editor alone should not revoke explicit boosted flag,
			// but compat callers handle combinations. leave boosted group state as-is unless caller also revokes.
		}
		return nil
	case FeatureTechniker:
		if allowed {
			_ = s.ensureUserInCapGroup(school, username, CapGroupTechniker, true)
			for _, cap := range []access.Capability{access.CapEventsTechnikView, access.CapEventsTechnikManage, access.CapCalendarTechnikMint} {
				_ = s.setDirectCapGrant(school, username, string(cap), 0)
			}
		} else {
			_ = s.ensureUserInCapGroup(school, username, CapGroupTechniker, false)
			for _, cap := range []access.Capability{access.CapEventsTechnikView, access.CapEventsTechnikManage, access.CapCalendarTechnikMint} {
				_ = s.setDirectCapGrant(school, username, string(cap), -1)
			}
		}
		return nil
	default:
		// generic: store as a direct grant marker capability not used in resolution, no-op for access model
		return nil
	}
}

// syncUserAccessFromPermRows rebuilds v2 direct/group state for known v1 features
// from explicit perm rows for the canonical username. Rows absent mean "no
// explicit override" (fall back to defaults/role defaults).
func (s *Store) syncUserAccessFromPermRows(username string) error {
	username = norm(username)
	school := s.userSchoolByUsername(username)
	// Clear access-model state for known feature groups/grants for this user in this school.
	for _, g := range []string{CapGroupRecon, CapGroupBoosted, CapGroupEditor, CapGroupTechniker} {
		_ = s.ensureUserInCapGroup(school, username, g, false)
	}
	for _, cap := range v1ReconCapsFor("") {
		_ = s.setDirectCapGrant(school, username, string(cap), 0)
	}
	for _, cap := range []access.Capability{access.CapEventsTechnikView, access.CapEventsTechnikManage, access.CapCalendarTechnikMint} {
		_ = s.setDirectCapGrant(school, username, string(cap), 0)
	}
	// Read explicit perm rows.
	rows, err := s.db.Query(`SELECT feature, allowed FROM perms WHERE username=? AND username<>?`, username, globalPermUser)
	if err != nil {
		return err
	}
	defer rows.Close()
	type pr struct {
		f string
		a bool
	}
	var prs []pr
	for rows.Next() {
		var f string
		var a int
		if err := rows.Scan(&f, &a); err != nil {
			return err
		}
		prs = append(prs, pr{f: f, a: a != 0})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// Apply in deterministic order; handle v1 mutual exclusion semantics.
	// Boosted/editor/recon interactions: if boosted or editor allowed, ensure recon not allowed.
	by := map[string]bool{}
	for _, p := range prs {
		by[p.f] = p.a
	}
	// If both present and true, boosted wins over recon (matches merge behavior).
	if by[FeatureBoosted] || by[FeatureEditor] {
		if by[FeatureRecon] {
			by[FeatureRecon] = false
		}
	}
	// Apply explicit states for features that have rows.
	for _, f := range []string{FeatureRecon, FeatureBoosted, FeatureEditor, FeatureTechniker} {
		if v, ok := by[f]; ok {
			_ = s.applyDirectFeatureFromV1(username, f, v)
		}
	}
	return nil
}

// Anonymous profile helper: JSON payload structure (extensible).
type AnonProfilePayload struct {
	// Upstream-detected anonymous visibility/capabilities for this school.
	ClassesDisplayable []int64         `json:"classesDisplayable,omitempty"`
	Capabilities       map[string]bool `json:"capabilities,omitempty"`
	Notes              string          `json:"notes,omitempty"`
}

func EncodeAnonProfilePayload(p AnonProfilePayload) string {
	b, _ := json.Marshal(p)
	return string(b)
}

func DecodeAnonProfilePayload(s string) AnonProfilePayload {
	var p AnonProfilePayload
	if strings.TrimSpace(s) == "" {
		p.Capabilities = map[string]bool{}
		return p
	}
	_ = json.Unmarshal([]byte(s), &p)
	if p.Capabilities == nil {
		p.Capabilities = map[string]bool{}
	}
	return p
}

func (s *Store) GetSchoolAnonProfilePayload(school string) (AnonProfilePayload, *SchoolAnonProfile, error) {
	prof, err := s.GetSchoolAnonProfile(school)
	if err != nil || prof == nil {
		return AnonProfilePayload{Capabilities: map[string]bool{}}, prof, err
	}
	return DecodeAnonProfilePayload(prof.ProfileJSON), prof, nil
}

// SortedCapabilities helper for deterministic outputs.
func SortedCapabilities(m map[access.Capability]access.Decision) []access.Capability {
	out := make([]access.Capability, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
