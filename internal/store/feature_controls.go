package store

import (
	"encoding/base64"
	"fmt"
	"strings"
	"untis-proxy/internal/access"
)

var FeatureDefaults = map[string]bool{"update_check": true, "analytics": false, "technik": true, "calendar": true, "ntfy": true, "webhooks": true, "changes": true, "reconstruction": true, "homework": true, "absence_notes": true}

var featurePrefixes = []struct{ prefix, feature string }{
	{"analytics.", "analytics"}, {"homework.", "homework"}, {"absence.notes.", "absence_notes"},
	{"events.personal.", "technik"}, {"events.technik.", "technik"}, {"calendar.", "calendar"},
	{"notify.ntfy.", "ntfy"}, {"notify.webhook.", "webhooks"}, {"changes.", "changes"},
}

func FeatureForCapability(cap string) string {
	for _, p := range featurePrefixes {
		if strings.HasPrefix(cap, p.prefix) {
			return p.feature
		}
	}
	if strings.Contains(cap, ".reconstructed.") {
		return "reconstruction"
	}
	return ""
}

var capabilityFeatures = func() map[access.Capability]string {
	m := map[access.Capability]string{}
	for _, c := range access.AllCapabilities {
		if feature := FeatureForCapability(string(c)); feature != "" {
			m[c] = feature
		}
	}
	return m
}()

func (s *Store) FeatureEnabled(school, feature string) bool {
	enabled, known := FeatureDefaults[feature]
	if s == nil {
		return enabled && known
	}
	if !known {
		return false
	}
	raw, found, err := s.GetSetting(controlKey("feature", "", feature))
	if err != nil {
		return false
	}
	if found {
		enabled = strings.TrimSpace(raw) == "true"
	}
	if !enabled {
		return false
	}
	if school != "" {
		raw, found, err = s.GetSetting(controlKey("feature", school, feature))
		if err != nil {
			return false
		}
		if found {
			enabled = strings.TrimSpace(raw) == "true"
		}
	}
	return enabled
}
func (s *Store) SetFeatureEnabled(school, feature string, enabled bool) error {
	if feature == "update_check" && school != "" {
		return fmt.Errorf("release checks are server-wide; select server controls")
	}
	if _, ok := FeatureDefaults[feature]; !ok {
		return fmt.Errorf("unknown feature")
	}
	return s.SetSetting(controlKey("feature", school, feature), fmt.Sprint(enabled))
}

type GroupPreview struct {
	ID       int64          `json:"id"`
	School   string         `json:"school"`
	Grants   map[string]int `json:"grants"`
	AssignTo string         `json:"assignTo"`
}
type PermissionChange struct {
	Capability string          `json:"capability"`
	Before     access.Decision `json:"before"`
	After      access.Decision `json:"after"`
}
type UserPermissionPreview struct {
	Username string             `json:"username"`
	Changes  []PermissionChange `json:"changes"`
}

func (s *Store) PreviewPermissionGroup(p GroupPreview) ([]UserPermissionPreview, error) {
	if p.School == "" {
		return nil, fmt.Errorf("school required")
	}
	if p.ID == 0 {
		p.ID = -1
	} else {
		g, _, err := s.GetCapGroup(p.ID)
		if err != nil || g == nil || g.School != p.School || strings.HasPrefix(g.Name, "v2.") {
			return nil, fmt.Errorf("editable school group required")
		}
	}
	known := map[string]bool{}
	for _, c := range access.AllCapabilities {
		known[string(c)] = true
	}
	for c, e := range p.Grants {
		if !known[c] || e < -1 || e > 1 {
			return nil, fmt.Errorf("invalid permission rule")
		}
	}
	switch p.AssignTo {
	case "", "*", "student", "teacher", "parent", "staff", "other", "unknown":
	default:
		return nil, fmt.Errorf("invalid audience")
	}
	tx, err := s.beginRead()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	scope, err := newPermissionScope(tx, p.School)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(`SELECT username,person_type FROM users WHERE school=? ORDER BY last_seen DESC,id DESC`, p.School)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UserPermissionPreview{}
	for rows.Next() {
		u := User{School: p.School}
		if err := rows.Scan(&u.Username, &u.PersonType); err != nil {
			return nil, err
		}
		before, err := scope.resolve(&u, nil, access.AllCapabilities)
		if err != nil {
			return nil, err
		}
		after, err := scope.resolve(&u, &p, access.AllCapabilities)
		if err != nil {
			return nil, err
		}
		changes := []PermissionChange{}
		for _, c := range access.AllCapabilities {
			a, b := before.Decisions[c], after.Decisions[c]
			if a != b {
				changes = append(changes, PermissionChange{string(c), a, b})
			}
		}
		if len(changes) > 0 {
			out = append(out, UserPermissionPreview{u.Username, changes})
		}
	}
	return out, rows.Err()
}

// FeatureStates reads the two applicable scopes in one query, so resolving many
// capabilities does not perform a settings query for every permission family.
func (s *Store) FeatureStates(school string) map[string]bool {
	states, err := (accessReader{s.db}).featureStates(school)
	if err != nil {
		states = make(map[string]bool, len(FeatureDefaults))
		for name := range FeatureDefaults {
			states[name] = false
		}
	}
	return states
}

func (r accessReader) featureStates(school string) (map[string]bool, error) {
	// '/' and '0' are consecutive bytes. These exact, case-sensitive ranges
	// use the settings primary key and do not scan other schools or cache blobs.
	serverPrefix := controlKey("feature", "")
	schoolPrefix := controlKey("feature", school)
	rows, err := r.Query(`SELECT key,value FROM settings WHERE
		(key>=? AND key<?) OR (key>=? AND key<?)`, serverPrefix+"/", serverPrefix+"0", schoolPrefix+"/", schoolPrefix+"0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := map[string]bool{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		values[key] = strings.TrimSpace(value) == "true"
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(FeatureDefaults))
	for name, on := range FeatureDefaults {
		suffix := "/" + base64.RawURLEncoding.EncodeToString([]byte(name))
		if v, ok := values[serverPrefix+suffix]; ok {
			on = v
		}
		if v, ok := values[schoolPrefix+suffix]; ok {
			on = on && v
		}
		out[name] = on
	}
	return out, nil
}

func controlKey(prefix string, parts ...string) string {
	encoded := make([]string, len(parts))
	for i, p := range parts {
		encoded[i] = base64.RawURLEncoding.EncodeToString([]byte(p))
	}
	return prefix + "/" + strings.Join(encoded, "/")
}
