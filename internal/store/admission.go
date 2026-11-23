package store

import (
	"encoding/json"
	"fmt"
	"untis-proxy/internal/access"
)

type AdmissionPolicy struct {
	BlockedRoles []string `json:"blockedRoles"`
}
type AdmissionIdentity struct {
	Role          string `json:"role"`
	UpstreamAdmin *bool  `json:"upstreamAdmin"`
}

func validLoginRole(role string) bool {
	switch role {
	case "student", "teacher", "parent", "staff", "other", "unknown", "upstream_admin":
		return true
	}
	return false
}
func (s *Store) AdmissionPolicy(school string) (AdmissionPolicy, error) {
	var p AdmissionPolicy
	raw, ok, err := s.GetSetting(controlKey("admission", school))
	if err != nil {
		return p, err
	}
	if ok {
		err = json.Unmarshal([]byte(raw), &p)
	}
	return p, err
}
func (s *Store) SetAdmissionPolicy(school string, p AdmissionPolicy) error {
	for _, r := range p.BlockedRoles {
		if !validLoginRole(r) {
			return fmt.Errorf("unknown account type")
		}
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return s.SetSetting(controlKey("admission", school), string(b))
}
func (s *Store) AdmissionAllowed(school string, identity AdmissionIdentity) bool {
	if !validLoginRole(identity.Role) {
		identity.Role = "unknown"
	}
	for _, scope := range []string{"", school} {
		p, err := s.AdmissionPolicy(scope)
		if err != nil {
			return false
		}
		for _, role := range p.BlockedRoles {
			if role == identity.Role {
				return false
			}
			if role == "upstream_admin" && (identity.UpstreamAdmin == nil || *identity.UpstreamAdmin) {
				return false
			}
		}
	}
	return true
}
func (s *Store) SaveAdmissionIdentity(school, user string, i AdmissionIdentity) error {
	b, err := json.Marshal(i)
	if err != nil {
		return err
	}
	return s.SetSetting(controlKey("admission-identity", school, norm(user)), string(b))
}
func (s *Store) UserAdmissionAllowed(school string, u *User) bool {
	if u == nil {
		return false
	}
	i := AdmissionIdentity{Role: string(s.resolveUserRole(school, u.PersonType))}
	if links, err := s.AccountLinks(school, u.Username); err == nil && links.Guardian {
		i.Role = "parent"
	}
	raw, ok, err := s.GetSetting(controlKey("admission-identity", school, norm(u.Username)))
	if err != nil {
		return false
	}
	if ok {
		var saved AdmissionIdentity
		if json.Unmarshal([]byte(raw), &saved) != nil {
			return false
		}
		i.UpstreamAdmin = saved.UpstreamAdmin
	}
	if i.Role == "" {
		i.Role = string(access.RoleUnknown)
	}
	return s.AdmissionAllowed(school, i)
}
func (s *Store) LoginRole(personType int64, guardian bool) string {
	if guardian {
		return "parent"
	}
	return string(s.resolveUserRole("", personType))
}
func (s *Store) LoginRoleForSchool(school string, personType int64, guardian bool) string {
	if guardian {
		return "parent"
	}
	return string(s.resolveUserRole(school, personType))
}

func (s *Store) HasAdmissionRestrictions(school string) bool {
	for _, scope := range []string{"", school} {
		p, err := s.AdmissionPolicy(scope)
		if err != nil || len(p.BlockedRoles) > 0 {
			return true
		}
	}
	return false
}
