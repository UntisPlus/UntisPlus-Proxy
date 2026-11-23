package proxy

import (
	"net/http"
	"strings"
	"untis-proxy/internal/access"
	"untis-proxy/internal/store"
	"untis-proxy/internal/untis"
)

func (p *Proxy) admitLogin(school, user string, info *untis.PersonInfo) bool {
	guardian := info.PersonID > 0 && len(info.LinkedPersons) > 0
	for _, person := range info.LinkedPersons {
		if person.ID == info.PersonID || person.Type != 5 {
			guardian = false
		}
	}
	identity := store.AdmissionIdentity{Role: p.store.LoginRoleForSchool(school, info.PersonType, guardian), UpstreamAdmin: info.UpstreamAdmin}
	if !p.store.AdmissionAllowed(school, identity) {
		return false
	}
	return p.store.SaveAdmissionIdentity(school, user, identity) == nil
}
func (p *Proxy) featureRouteAllowed(r *http.Request) bool {
	path := r.URL.Path
	feature := ""
	switch {
	case strings.HasPrefix(path, "/api/analytics/"):
		feature = "analytics"
	case strings.HasPrefix(path, "/api/calendar/"), strings.HasPrefix(path, "/week/"):
		feature = "calendar"
	case strings.HasPrefix(path, "/api/ntfy"):
		feature = "ntfy"
	case strings.HasPrefix(path, "/api/webhooks"):
		feature = "webhooks"
	case strings.HasPrefix(path, "/api/timetable/"):
		feature = "changes"
	case strings.HasPrefix(path, "/api/homework/"):
		feature = "homework"
	case strings.HasPrefix(path, "/api/absence/notes"):
		feature = "absence_notes"
	case strings.HasPrefix(path, "/api/admin/events"), strings.HasPrefix(path, "/api/technik/"):
		feature = "technik"
	}
	school := r.URL.Query().Get("school")
	if school == "" {
		school = p.opts.School
	}
	return feature == "" || p.store.FeatureEnabled(school, feature)
}

// Operation grants do not imply any other write. Explicit per-operation blocks
// also override the compatibility manage grant.
func (p *Proxy) technikAllowed(u *store.User, school string, cap access.Capability) bool {
	if u == nil || !p.store.FeatureEnabled(school, "technik") {
		return false
	}
	if !p.isAdmin(u.Username) && p.userSchool(u, p.opts.School) != school {
		return false
	}
	gates, err := p.store.ServerCapGates(school)
	if err != nil {
		return false
	}
	if enabled, found := gates[string(cap)]; found && !enabled {
		return false
	}
	if p.isAdmin(u.Username) {
		return true
	}
	result, err := p.store.ResolveEffective(school, u)
	if err != nil {
		return false
	}
	d := result.Decisions[cap]
	if !d.UserOptedIn {
		return false
	}
	if d.Effective {
		return true
	}
	// Only absent rules inherit the former all-in-one management grant.
	if d.Source != access.SourceImplicit {
		return false
	}
	return result.Decisions[access.CapEventsTechnikManage].Effective
}
