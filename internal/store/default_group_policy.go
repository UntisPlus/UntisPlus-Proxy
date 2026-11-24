package store

import (
	"encoding/json"
	"fmt"
)

type DefaultGroupPolicy struct {
	Inherit          bool    `json:"inherit"`
	ExcludedGroupIDs []int64 `json:"excludedGroupIds"`
}

func containsGroupID(ids []int64, id int64) bool {
	for _, value := range ids {
		if value == id {
			return true
		}
	}
	return false
}
func (s *Store) DefaultGroupPolicy(school, user string) (DefaultGroupPolicy, error) {
	p := DefaultGroupPolicy{Inherit: true, ExcludedGroupIDs: []int64{}}
	raw, found, err := s.GetSetting(controlKey("default-groups", school, norm(user)))
	if err == nil && found {
		err = json.Unmarshal([]byte(raw), &p)
	}
	return p, err
}
func (s *Store) SetDefaultGroupPolicy(school, user string, p DefaultGroupPolicy) error {
	u, err := s.GetUserInSchool(school, user)
	if err != nil || u == nil || u.School != school {
		return fmt.Errorf("choose an account in this school")
	}
	if len(p.ExcludedGroupIDs) > 500 {
		return fmt.Errorf("too many excluded groups")
	}
	ids := []int64{}
	for _, id := range p.ExcludedGroupIDs {
		g, _, err := s.GetCapGroup(id)
		if err != nil || g == nil || (g.School != "" && g.School != school) {
			return fmt.Errorf("group unavailable in this school")
		}
		if !containsGroupID(ids, id) {
			ids = append(ids, id)
		}
	}
	p.ExcludedGroupIDs = ids
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return s.SetSetting(controlKey("default-groups", school, norm(user)), string(raw))
}

// ApplyUpstreamBasic is an explicit, atomic reset. Merely assigning the Basic
// group is additive and does not remove other groups or inherited defaults.
func (s *Store) ApplyUpstreamBasic(school, user string) error {
	u, err := s.GetUserInSchool(school, user)
	if err != nil || u == nil || u.School != school {
		return fmt.Errorf("choose an account in this school")
	}
	id, found, err := s.groupIDByName("", CapGroupUpstreamBasic)
	if err != nil || !found {
		return fmt.Errorf("Basic template unavailable")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []string{"user_cap_groups", "user_cap_grants", "user_cap_prefs"} {
		if _, err := tx.Exec("DELETE FROM "+table+" WHERE username=? AND (school=? OR school='')", norm(user), school); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO user_cap_groups(school,username,group_id,created_at) VALUES(?,?,?,0)`, school, norm(user), id); err != nil {
		return err
	}
	raw, _ := json.Marshal(DefaultGroupPolicy{Inherit: false, ExcludedGroupIDs: []int64{}})
	if _, err := tx.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, controlKey("default-groups", school, norm(user)), string(raw)); err != nil {
		return err
	}
	return tx.Commit()
}
