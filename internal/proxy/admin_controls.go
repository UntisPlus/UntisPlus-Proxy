package proxy

import (
	"net/http"
	"sort"
	"time"
	"untis-proxy/internal/store"
)

func (p *Proxy) adminControls(w http.ResponseWriter, r *http.Request, part string) {
	school := r.URL.Query().Get("school")
	switch part {
	case "default-group-policy", "apply-basic", "compatibility", "compatibility-probe":
		p.adminBaselineControls(w, r, part)
		return

	case "preview":
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		var proposal store.GroupPreview
		if decodeJSONBody(r, &proposal) != nil {
			badRequest(w, "invalid preview")
			return
		}
		changes, err := p.store.PreviewPermissionGroup(proposal)
		if err != nil {
			badRequest(w, err.Error())
			return
		}
		p.writeJSON(w, map[string]any{"users": changes, "saved": false, "existingAccountsOnly": true, "note": "Dashboard administrator powers are separate; preview does not change administrator status."})
	case "features":
		if r.Method == http.MethodGet {
			names := []string{}
			for name := range store.FeatureDefaults {
				names = append(names, name)
			}
			sort.Strings(names)
			serverStates := p.store.FeatureStates("")
			schoolStates := serverStates
			if school != "" {
				schoolStates = p.store.FeatureStates(school)
			}
			features := make([]map[string]any, 0, len(names))
			for _, name := range names {
				features = append(features, map[string]any{"name": name, "serverEnabled": serverStates[name], "effective": schoolStates[name]})
			}
			p.writeJSON(w, map[string]any{"features": features})
			return
		}
		if r.Method != http.MethodPut {
			w.WriteHeader(405)
			return
		}
		var req struct {
			Feature string `json:"feature"`
			Enabled *bool  `json:"enabled"`
		}
		if decodeJSONBody(r, &req) != nil || req.Enabled == nil {
			badRequest(w, "feature and enabled required")
			return
		}
		if err := p.store.SetFeatureEnabled(school, req.Feature, *req.Enabled); err != nil {
			badRequest(w, err.Error())
			return
		}
		p.audit(r, school, "feature.set", req.Feature, "")
		p.writeJSON(w, map[string]any{"ok": true, "effective": p.store.FeatureEnabled(school, req.Feature)})
	case "admission":
		if r.Method == http.MethodGet {
			policy, err := p.store.AdmissionPolicy(school)
			if err != nil {
				writeJSONError(w, 500, "policy unavailable")
				return
			}
			p.writeJSON(w, map[string]any{"policy": policy, "upstreamAdminRequiresVerifiedFlag": true, "warning": "Blocking upstream_admin also denies accounts whose upstream admin status is unknown. Local dashboard administrator status is unrelated."})
			return
		}
		if r.Method != http.MethodPut {
			w.WriteHeader(405)
			return
		}
		var policy store.AdmissionPolicy
		if decodeJSONBody(r, &policy) != nil {
			badRequest(w, "invalid policy")
			return
		}
		if err := p.store.SetAdmissionPolicy(school, policy); err != nil {
			badRequest(w, err.Error())
			return
		}
		p.audit(r, school, "admission.set", "", "")
		p.writeJSON(w, map[string]any{"ok": true})
	case "analytics-access":
		username := r.URL.Query().Get("user")
		u, err := p.store.GetUserInSchool(school, username)
		if err != nil || u == nil || u.School != school {
			badRequest(w, "choose an account in this school")
			return
		}
		if r.Method == http.MethodGet {
			a, err := p.store.AnalyticsAccess(school, username)
			if err != nil {
				writeJSONError(w, 500, "scope unavailable")
				return
			}
			p.writeJSON(w, a)
			return
		}
		if r.Method != http.MethodPut {
			w.WriteHeader(405)
			return
		}
		var a store.AnalyticsAccess
		if decodeJSONBody(r, &a) != nil {
			badRequest(w, "invalid scope")
			return
		}
		if err := p.store.SetAnalyticsAccess(school, username, a); err != nil {
			badRequest(w, err.Error())
			return
		}
		p.audit(r, school, "analytics.scope.set", username, "")
		p.writeJSON(w, map[string]any{"ok": true})
	case "analytics-years":
		if r.Method == http.MethodPost {
			u := p.sessionUser(r)
			if u == nil || school == "" {
				badRequest(w, "choose a school to load upstream dates")
				return
			}
			now := time.Now()
			key := "school-years:" + loginLimitKey(school, u.Username)
			if locked, _ := p.analyticsLimiter.locked(key, now); locked {
				w.Header().Set("Retry-After", "60")
				writeJSONError(w, http.StatusTooManyRequests, "analytics request limit reached")
				return
			}
			p.analyticsLimiter.fail(key, now)
			years, err := p.upstreamAnalyticsYears(school)
			if err != nil {
				writeJSONError(w, http.StatusBadGateway, err.Error())
				return
			}
			p.writeJSON(w, map[string]any{"years": years})
			return
		}
		if r.Method == http.MethodGet {
			years, err := p.store.AnalyticsYears(school)
			if err != nil {
				writeJSONError(w, 500, "school years unavailable")
				return
			}
			p.writeJSON(w, map[string]any{"years": years})
			return
		}
		if r.Method != http.MethodPut {
			w.WriteHeader(405)
			return
		}
		var req struct {
			Years []store.AnalyticsYear `json:"years"`
		}
		if decodeJSONBody(r, &req) != nil {
			badRequest(w, "invalid years")
			return
		}
		if err := p.store.SetAnalyticsYears(school, req.Years); err != nil {
			badRequest(w, err.Error())
			return
		}
		p.audit(r, school, "analytics.years.set", "", "")
		p.writeJSON(w, map[string]any{"ok": true})
	default:
		writeJSONError(w, 404, "not found")
	}
}
