package store

import (
	"fmt"
	"strings"
	"time"
	"untis-proxy/internal/access"
)

// SavePermissionGroup commits the group's complete rules and optional default
// assignment together. A failure never leaves an empty or partially saved group.
func (s *Store) SavePermissionGroup(id int64, school, name, description string, grants map[string]int, assignTo string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 120 {
		return 0, fmt.Errorf("enter a group name of 1–120 characters")
	}
	if strings.HasPrefix(strings.ToLower(name), "v2.") {
		return 0, fmt.Errorf("this name is reserved for built-in groups")
	}
	known := map[string]bool{}
	for _, c := range access.AllCapabilities {
		known[string(c)] = true
	}
	for c, e := range grants {
		if !known[c] || e < -1 || e > 1 {
			return 0, fmt.Errorf("invalid permission rule: %s", c)
		}
	}
	switch assignTo {
	case "", "*", "student", "teacher", "staff", "parent", "other", "unknown":
	default:
		return 0, fmt.Errorf("unsupported account type")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	if id > 0 {
		var existingName, existingSchool string
		if err = tx.QueryRow(`SELECT name,school FROM cap_groups WHERE id=?`, id).Scan(&existingName, &existingSchool); err != nil {
			return 0, fmt.Errorf("group not found")
		}
		if existingSchool != school {
			return 0, fmt.Errorf("select the group's school before editing")
		}
		if strings.HasPrefix(existingName, "v2.") {
			return 0, fmt.Errorf("built-in groups are read-only; create your own copy")
		}
		_, err = tx.Exec(`UPDATE cap_groups SET name=?,description=?,updated_at=? WHERE id=?`, name, description, now, id)
	} else {
		result, e := tx.Exec(`INSERT INTO cap_groups(school,name,description,created_at,updated_at) VALUES(?,?,?,?,?)`, school, name, description, now, now)
		err = e
		if err == nil {
			id, err = result.LastInsertId()
		}
	}
	if err != nil {
		return 0, err
	}
	if _, err = tx.Exec(`DELETE FROM cap_group_grants WHERE group_id=?`, id); err != nil {
		return 0, err
	}
	for c, e := range grants {
		if e != 0 {
			if _, err = tx.Exec(`INSERT INTO cap_group_grants(group_id,capability,effect) VALUES(?,?,?)`, id, c, e); err != nil {
				return 0, err
			}
		}
	}
	if assignTo != "" {
		if _, err = tx.Exec(`INSERT OR IGNORE INTO role_default_groups(school,role,group_id,priority) VALUES(?,?,?,0)`, school, assignTo, id); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}
