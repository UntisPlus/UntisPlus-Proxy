package proxy

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"untis-proxy/internal/access"
	"untis-proxy/internal/store"
)

var startedAt = time.Now()

// version is the reported build version; set at build time via
// -ldflags "-X untis-proxy/internal/proxy.version=<...>". Defaults to "dev".
var version = "dev"

// handleStatus reports the process status: uptime, mode (dev/beta/prod) and
// version. It is self-measured only; external uptime monitoring is out of scope.
func (p *Proxy) handleStatus(w http.ResponseWriter, r *http.Request) {
	mode := os.Getenv("UNTIS_ENV")
	if mode == "" {
		mode = "dev"
	}
	v := os.Getenv("UNTIS_VERSION")
	if v == "" {
		v = version
	}
	p.writeJSON(w, map[string]any{
		"status":     "ok",
		"mode":       mode,
		"version":    v,
		"uptime_sec": int64(time.Since(startedAt).Seconds()),
	})
}

// handleMe reports the signed-in requester's fine-grained effective permission
// state and supports user preference changes within the allowed/toggleable ceiling.
// v2: authenticated real accounts only; anonymous is denied.
func (p *Proxy) handleMe(w http.ResponseWriter, r *http.Request) {
	user := p.sessionUser(r)
	if user == nil || user.Username == "#anonymous#" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "not logged in"})
		return
	}
	school := user.School
	if school == "" {
		school = p.requestSchool(r)
	}

	switch r.Method {
	case http.MethodGet, "":
		p.writeMe(w, school, user)
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		var req struct {
			Preferences map[string]bool `json:"preferences"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "bad request"})
			return
		}
		if len(req.Preferences) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "preferences required"})
			return
		}
		res, err := p.store.ResolveEffective(school, user)
		if err != nil || res == nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "resolve failed"})
			return
		}
		for capStr, optIn := range req.Preferences {
			cap := access.Capability(strings.TrimSpace(capStr))
			d, ok := res.Decisions[cap]
			if !ok {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "unknown capability", "capability": capStr})
				return
			}
			if !d.Toggleable {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "capability not toggleable", "capability": capStr})
				return
			}
			if optIn && (!d.ServerEnabled || !d.AdminAllowed) {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "cannot opt in beyond allowed ceiling", "capability": capStr})
				return
			}
			if err := p.store.SetUserCapPref(school, user.Username, string(cap), optIn); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "store error"})
				return
			}
		}
		// reload user in case needed and respond with updated state
		if u2, err := p.store.GetUser(user.Username); err == nil && u2 != nil {
			user = u2
			if school == "" {
				school = user.School
			}
		}
		p.writeMe(w, school, user)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (p *Proxy) writeMe(w http.ResponseWriter, school string, user *store.User) {
	p.writeMeUser(w, school, user)
}

func (p *Proxy) writeMeUser(w http.ResponseWriter, school string, user *store.User) {
	res, err := p.store.ResolveEffective(school, user)
	if err != nil || res == nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "resolve failed"})
		return
	}
	caps := make([]string, 0, len(res.Decisions))
	for c := range res.Decisions {
		caps = append(caps, string(c))
	}
	sort.Strings(caps)
	capStates := make(map[string]any, len(caps))
	effective := make([]string, 0)
	for _, c := range caps {
		d := res.Decisions[access.Capability(c)]
		capStates[c] = map[string]any{
			"server_enabled": d.ServerEnabled,
			"admin_allowed":  d.AdminAllowed,
			"user_opted_in":  d.UserOptedIn,
			"effective":      d.Effective,
			"toggleable":     d.Toggleable,
			"source":         d.Source,
		}
		if d.Effective {
			effective = append(effective, c)
		}
	}
	groups, _ := p.store.ListUserCapGroups(school, user.Username)
	groupNames := make([]string, 0, len(groups))
	for _, g := range groups {
		groupNames = append(groupNames, g.Name)
	}
	sort.Strings(groupNames)

	// coarse compat view derived from effective caps (best-effort)
	boosted := p.store.EffectiveHas(school, user, access.CapTimetableClassBoostedRawView) || p.store.EffectiveHas(school, user, access.CapAbsenceWrite)
	editor := p.store.EffectiveHas(school, user, access.CapAbsenceWrite) || p.store.EffectiveHas(school, user, access.CapLessonWrite)
	recon, _ := p.store.ReconAccess(user.Username, "TEACHER")
	level := "basic"
	switch {
	case editor:
		level = "boosted+editor"
	case boosted:
		level = "boosted"
	case recon:
		level = "recon"
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"v":            2,
		"username":     user.Username,
		"school":       school,
		"role":         res.Role,
		"groups":       groupNames,
		"effective":    effective,
		"capabilities": capStates,
		// compat summary for easier migration/debug
		"level": level,
		"permissions": map[string]any{
			"recon":   recon,
			"boosted": boosted,
			"editor":  editor,
		},
	})
}
