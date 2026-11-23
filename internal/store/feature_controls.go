package store

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"untis-proxy/internal/access"
)

var FeatureDefaults = map[string]bool{"update_check": true, "analytics": false, "technik": true, "calendar": true, "ntfy": true, "webhooks": true, "changes": true, "reconstruction": true, "homework": true, "absence_notes": true}

func FeatureForCapability(cap string) string {
	for prefix, feature := range map[string]string{"analytics.": "analytics", "events.technik.": "technik", "calendar.": "calendar", "notify.ntfy.": "ntfy", "notify.webhook.": "webhooks", "changes.": "changes"} {
		if strings.HasPrefix(cap, prefix) {
			return feature
		}
	}
	if strings.Contains(cap, ".reconstructed.") {
		return "reconstruction"
	}
	return ""
}
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
		if json.Unmarshal([]byte(raw), &enabled) != nil {
			return false
		}
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
			if json.Unmarshal([]byte(raw), &enabled) != nil {
				return false
			}
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

func (s *Store) previewGroupEffects(id int64, p *GroupPreview) (map[access.Capability]access.Effect, error) {
	if p == nil || p.ID != id {
		return s.groupGrantEffects(id)
	}
	out := map[access.Capability]access.Effect{}
	for c, e := range p.Grants {
		if e > 0 {
			out[access.Capability(c)] = access.EffectAllow
		} else if e < 0 {
			out[access.Capability(c)] = access.EffectDeny
		}
	}
	return out, nil
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
	users, err := s.ListUsers()
	if err != nil {
		return nil, err
	}
	out := []UserPermissionPreview{}
	for _, u := range users {
		if u.School != p.School {
			continue
		}
		before, err := s.ResolveEffective(p.School, u)
		if err != nil {
			return nil, err
		}
		after, err := s.resolveEffective(p.School, u, &p)
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
	return out, nil
}

// FeatureStates reads the two applicable scopes in one query, so resolving many
// capabilities does not perform a settings query for every permission family.
func (s *Store) FeatureStates(school string) map[string]bool {
	out := map[string]bool{}
	for name, enabled := range FeatureDefaults {
		out[name] = enabled
	}
	rows, err := s.db.Query(`SELECT key,value FROM settings WHERE key LIKE 'feature/%'`)
	if err != nil {
		for name := range out {
			out[name] = false
		}
		return out
	}
	defer rows.Close()
	raw := map[string]string{}
	for rows.Next() {
		var k, v string
		if rows.Scan(&k, &v) != nil {
			for name := range out {
				out[name] = false
			}
			return out
		}
		raw[k] = v
	}
	if rows.Err() != nil {
		for name := range out {
			out[name] = false
		}
		return out
	}
	for name, enabled := range out {
		if value, ok := raw[controlKey("feature", "", name)]; ok {
			if json.Unmarshal([]byte(value), &enabled) != nil {
				enabled = false
			}
		}
		if enabled && school != "" {
			if value, ok := raw[controlKey("feature", school, name)]; ok {
				if json.Unmarshal([]byte(value), &enabled) != nil {
					enabled = false
				}
			}
		}
		out[name] = enabled
	}
	return out
}

func controlKey(prefix string, parts ...string) string {
	encoded := make([]string, len(parts))
	for i, p := range parts {
		encoded[i] = base64.RawURLEncoding.EncodeToString([]byte(p))
	}
	return prefix + "/" + strings.Join(encoded, "/")
}
