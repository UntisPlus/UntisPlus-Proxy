package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"

	"untis-proxy/internal/access"
)

type queryReader interface {
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

// Shared by public lookups and the same lookups inside a WAL read snapshot.
type accessReader struct{ queryReader }

func (s *Store) beginRead() (*sql.Tx, error) {
	return s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
}

// All memoized data belongs to one school in one transaction. A new request
// sees every committed role change, group edit, preference and revocation.
type permissionScope struct {
	accessReader
	school   string
	gates    map[access.Capability]bool
	roles    map[int64]access.Role
	defaults map[string][]int64
	groups   map[int64]map[access.Capability]access.Effect
}

func newPermissionScope(tx *sql.Tx, school string) (*permissionScope, error) {
	r := accessReader{tx}
	gates, err := r.ServerCapGates(school)
	if err != nil {
		return nil, err
	}
	states, err := r.featureStates(school)
	if err != nil {
		return nil, err
	}
	enabled := make(map[access.Capability]bool, len(gates))
	for cap, on := range gates {
		enabled[access.Capability(cap)] = on
	}
	for cap, feature := range capabilityFeatures {
		if !states[feature] {
			enabled[cap] = false
		}
	}
	return &permissionScope{accessReader: r, school: school, gates: enabled, roles: map[int64]access.Role{}, defaults: map[string][]int64{}, groups: map[int64]map[access.Capability]access.Effect{}}, nil
}

func (r accessReader) resolveUserRole(school string, personType int64) (access.Role, error) {
	var role string
	err := r.QueryRow(`SELECT role FROM person_type_roles WHERE person_type=? AND scope IN ('',?) AND trim(role)<>'' ORDER BY scope DESC LIMIT 1`, personType, school).Scan(&role)
	if err == sql.ErrNoRows {
		return access.DefaultRoleForPersonType(personType), nil
	}
	if err != nil {
		return access.RoleUnknown, err
	}
	return access.NormalizeRole(role), nil
}

func (p *permissionScope) userRole(u *User) (access.Role, error) {
	links, err := p.AccountLinks(p.school, u.Username)
	if err != nil && err != sql.ErrNoRows {
		return access.RoleUnknown, err
	}
	if links.Guardian {
		return access.Role("parent"), nil
	}
	if role, ok := p.roles[u.PersonType]; ok {
		return role, nil
	}
	role, err := p.resolveUserRole(p.school, u.PersonType)
	if err == nil {
		p.roles[u.PersonType] = role
	}
	return role, err
}

func (p *permissionScope) defaultGroups(role string) ([]int64, error) {
	if ids, ok := p.defaults[role]; ok {
		return ids, nil
	}
	ids, err := p.ListRoleDefaultGroups(p.school, role)
	if err == nil {
		p.defaults[role] = ids
	}
	return ids, err
}

// Batch-load newly encountered group rules, including empty groups.
func (p *permissionScope) loadGroups(assigned, inherited []int64) error {
	var missing []int64
	for _, ids := range [][]int64{assigned, inherited} {
		for _, id := range ids {
			if _, ok := p.groups[id]; !ok {
				p.groups[id] = make(map[access.Capability]access.Effect)
				missing = append(missing, id)
			}
		}
	}
	if len(missing) == 0 {
		return nil
	}
	encoded, err := json.Marshal(missing)
	if err != nil {
		return err
	}
	rows, err := p.Query(`SELECT group_id,capability,effect FROM cap_group_grants WHERE group_id IN (SELECT value FROM json_each(?))`, string(encoded))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var cap access.Capability
		var effect int
		if err := rows.Scan(&id, &cap, &effect); err != nil {
			return err
		}
		p.groups[id][cap] = storedEffect(effect)
	}
	return rows.Err()
}

func storedEffect(value int) access.Effect {
	switch {
	case value > 0:
		return access.EffectAllow
	case value < 0:
		return access.EffectDeny
	default:
		return access.EffectUnset
	}
}

func (p *permissionScope) effects(ids []int64, preview *GroupPreview) []map[access.Capability]access.Effect {
	effects := make([]map[access.Capability]access.Effect, 0, len(ids))
	for _, id := range ids {
		rules := p.groups[id]
		if preview != nil && preview.ID == id {
			rules = make(map[access.Capability]access.Effect, len(preview.Grants))
			for capability, v := range preview.Grants {
				rules[access.Capability(capability)] = storedEffect(v)
			}
		}
		effects = append(effects, rules)
	}
	return effects
}

var toggleableCapabilities = func() map[access.Capability]bool {
	m := make(map[access.Capability]bool, len(access.AllCapabilities))
	for _, c := range access.AllCapabilities {
		m[c] = c != access.CapAbsenceOwnRead
	}
	return m
}()

func (s *Store) ResolveEffective(school string, u *User) (*EffectiveResult, error) {
	if u == nil {
		return &EffectiveResult{Role: access.RoleUnknown, Decisions: access.Resolve(access.Inputs{})}, nil
	}
	return s.resolveCapabilities(school, u, access.AllCapabilities)
}

func (s *Store) resolveCapabilities(school string, u *User, caps []access.Capability) (*EffectiveResult, error) {
	tx, err := s.beginRead()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	p, err := newPermissionScope(tx, school)
	if err != nil {
		return nil, err
	}
	return p.resolve(u, nil, caps)
}

func (p *permissionScope) resolve(u *User, preview *GroupPreview, caps []access.Capability) (*EffectiveResult, error) {
	if u.School != "" && p.school != u.School {
		return nil, fmt.Errorf("account belongs to a different school")
	}
	role, err := p.userRole(u)
	if err != nil {
		return nil, err
	}
	directRows, err := p.ListUserCapGrants(p.school, u.Username)
	if err != nil {
		return nil, err
	}
	direct := make(map[access.Capability]access.Effect, len(directRows))
	for _, g := range directRows {
		direct[access.Capability(g.Capability)] = storedEffect(g.Effect)
	}
	groups, err := p.ListUserCapGroups(p.school, u.Username)
	if err != nil {
		return nil, err
	}
	groupIDs := make([]int64, 0, len(groups))
	for _, g := range groups {
		groupIDs = append(groupIDs, g.ID)
	}
	policy, err := p.DefaultGroupPolicy(p.school, u.Username)
	if err != nil {
		return nil, err
	}
	roleIDs, err := p.defaultGroups(string(role))
	if err != nil {
		return nil, err
	}
	allIDs, err := p.defaultGroups("*")
	if err != nil {
		return nil, err
	}
	inherited := make([]int64, 0, len(roleIDs)+len(allIDs))
	for _, ids := range [][]int64{roleIDs, allIDs} {
		for _, id := range ids {
			if !slices.Contains(inherited, id) {
				inherited = append(inherited, id)
			}
		}
	}
	if preview != nil && (preview.AssignTo == "*" || preview.AssignTo == string(role)) && !slices.Contains(inherited, preview.ID) {
		inherited = append(inherited, preview.ID)
	}
	var active []int64
	if policy.Inherit {
		for _, id := range inherited {
			if !slices.Contains(policy.ExcludedGroupIDs, id) {
				active = append(active, id)
			}
		}
	}
	if err := p.loadGroups(groupIDs, active); err != nil {
		return nil, err
	}
	prefsRaw, err := p.UserCapPrefs(p.school, u.Username)
	if err != nil {
		return nil, err
	}
	prefs := make(map[access.Capability]bool, len(prefsRaw))
	for k, v := range prefsRaw {
		prefs[access.Capability(k)] = v
	}
	in := access.Inputs{ServerEnabled: p.gates, Direct: direct, GroupGrants: p.effects(groupIDs, preview), RoleDefaultGrants: p.effects(active, preview), UserPrefs: prefs, Toggleable: toggleableCapabilities}
	decisions := access.ResolveSet(caps, in)
	// Expand legacy manage only where no operation-specific rule exists.
	legacy := decisions[access.CapEventsTechnikManage]
	if legacy.AdminAllowed {
		for _, cap := range []access.Capability{access.CapEventsTechnikCreate, access.CapEventsTechnikEdit, access.CapEventsTechnikDelete, access.CapEventsTechnikImport, access.CapEventsTechnikAttendanceRead, access.CapEventsTechnikAttendanceWrite} {
			if d, ok := decisions[cap]; ok && d.Source == access.SourceImplicit {
				d.AdminAllowed = true
				d.Effective = d.ServerEnabled && d.UserOptedIn && legacy.Effective
				d.Source = legacy.Source
				decisions[cap] = d
			}
		}
	}
	return &EffectiveResult{Role: role, Decisions: decisions, GroupIDs: groupIDs, DirectGrants: directRows, InheritedGroupIDs: inherited, ExcludedDefaultGroupIDs: policy.ExcludedGroupIDs, InheritRoleDefaults: policy.Inherit}, nil
}
