package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
	"untis-proxy/internal/store"
	"untis-proxy/internal/untis"
)

type compatibilityProbe struct {
	From          string   `json:"from"`
	To            string   `json:"to"`
	PersonID      int64    `json:"personId,omitempty"`
	CheckedAt     string   `json:"checkedAt"`
	Status        string   `json:"status"`
	PeriodCount   int      `json:"periodCount"`
	Role          string   `json:"role"`
	UpstreamAdmin *bool    `json:"upstreamAdmin"`
	Notes         []string `json:"notes"`
}

func (p *Proxy) adminBaselineControls(w http.ResponseWriter, r *http.Request, part string) {
	school, user := r.URL.Query().Get("school"), r.URL.Query().Get("user")
	u, err := p.store.GetUserInSchool(school, user)
	if err != nil || u == nil || school == "" || u.School != school {
		badRequest(w, "choose an account in this school")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	switch part {
	case "default-group-policy":
		if r.Method == http.MethodGet {
			policy, err := p.store.DefaultGroupPolicy(school, user)
			if err != nil {
				writeJSONError(w, 500, "policy unavailable")
				return
			}
			p.writeJSON(w, policy)
			return
		}
		if r.Method != http.MethodPut {
			w.WriteHeader(405)
			return
		}
		var req struct {
			Inherit          *bool   `json:"inherit"`
			ExcludedGroupIDs []int64 `json:"excludedGroupIds"`
		}
		if decodeJSONBody(r, &req) != nil || req.Inherit == nil {
			badRequest(w, "invalid inheritance policy")
			return
		}
		policy := store.DefaultGroupPolicy{Inherit: *req.Inherit, ExcludedGroupIDs: req.ExcludedGroupIDs}
		if err := p.store.SetDefaultGroupPolicy(school, user, policy); err != nil {
			badRequest(w, err.Error())
			return
		}
		p.audit(r, school, "access.defaults.person", user, "")
		p.writeJSON(w, map[string]any{"ok": true})
	case "apply-basic":
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		if err := p.store.ApplyUpstreamBasic(school, user); err != nil {
			badRequest(w, err.Error())
			return
		}
		p.audit(r, school, "access.basic.reset", user, "Explicit and global person grants/groups/preferences cleared; role inheritance disabled; admin flag unchanged")
		p.writeJSON(w, map[string]any{"ok": true, "adminUnchanged": true})
	case "compatibility":
		if r.Method != http.MethodGet {
			w.WriteHeader(405)
			return
		}
		links, _ := p.store.AccountLinks(school, user)
		secret, password, credErr := p.store.GetCredential(user)
		role := p.store.LoginRoleForSchool(school, u.PersonType, links.Guardian)
		identity := store.AdmissionIdentity{Role: role}
		raw, found, metaErr := p.store.AdmissionIdentity(school, user)
		if found {
			identity.UpstreamAdmin = raw.UpstreamAdmin
		}
		var last *compatibilityProbe
		cached, exists, cacheErr := p.store.CompatibilityResult(school, user)
		if cacheErr == nil && exists {
			var probe compatibilityProbe
			if json.Unmarshal(cached, &probe) == nil {
				last = &probe
			}
		}
		years, yearErr := p.store.AnalyticsYears(school)
		p.writeJSON(w, map[string]any{"school": school, "user": user, "storedRole": role, "personType": u.PersonType, "guardian": links.Guardian, "linkedPeople": len(links.Persons), "upstreamAdmin": identity.UpstreamAdmin, "classificationSource": "stored authenticated upstream identity; not live-tested by this GET", "admissionAllowed": metaErr == nil && p.store.UserAdmissionAllowed(school, u), "credentialsAvailable": credErr == nil && (secret != "" || password != "" || u.Password != ""), "hasToken": secret != "" || (u.Method == "key" && u.Password != ""), "hasPassword": password != "" || (u.Method == "password" && u.Password != ""), "historySupport": "not inferred from configured years or empty responses", "configuredYears": years, "yearConfigurationAvailable": yearErr == nil, "lastProbe": last, "note": "An explicit probe reads only this account's own upstream identity/timetable. A successful date window does not prove full historical coverage."})
	case "compatibility-probe":
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		var req struct {
			From     string `json:"from"`
			To       string `json:"to"`
			PersonID int64  `json:"personId"`
		}
		if decodeJSONBody(r, &req) != nil {
			badRequest(w, "invalid probe")
			return
		}
		from, e := time.Parse("2006-01-02", req.From)
		to, f := time.Parse("2006-01-02", req.To)
		if e != nil || f != nil || to.Before(from) || to.Sub(from) > 30*24*time.Hour || req.To > time.Now().UTC().Format("2006-01-02") {
			badRequest(w, "use a past/current window of at most 31 days")
			return
		}
		// Bound credential replay and upstream work; no target supplied by the caller
		// can escape this account's verified active or linked identities.
		key := "probe/" + loginLimitKey(school, user)
		if locked, _ := p.analyticsLimiter.locked(key, time.Now()); locked {
			writeJSONError(w, 429, "probe limit reached; retry later")
			return
		}
		p.analyticsLimiter.fail(key, time.Now())
		select {
		case p.analyticsSlots <- struct{}{}:
			defer func() { <-p.analyticsSlots }()
		default:
			writeJSONError(w, 429, "diagnostic workers busy")
			return
		}
		report := compatibilityProbe{From: req.From, To: req.To, CheckedAt: time.Now().UTC().Format(time.RFC3339), Status: "unavailable", Notes: []string{}}
		secret, password, _ := p.store.GetCredential(user)
		if u.Method == "key" && secret == "" {
			secret = u.Password
		}
		if u.Method == "password" && password == "" {
			password = u.Password
		}
		cookie := ""
		if u.Method == "key" && secret != "" {
			cookie, _, err = p.untis.KeyLogin(school, u.Username, untis.TOTP(secret), time.Now().UnixMilli(), nil)
		} else if password != "" {
			cookie, _, err = p.untis.PasswordLogin(school, u.Username, password, "UntisPlus compatibility probe")
		} else {
			err = http.ErrNoCookie
		}
		if err == nil && cookie != "" {
			defer p.untis.Logout(school, cookie)
			info, infoErr := p.untis.PersonInfo(school, cookie)
			if infoErr == nil && info != nil {
				guardian := info.PersonID > 0 && len(info.LinkedPersons) > 0
				for _, person := range info.LinkedPersons {
					if person.ID == info.PersonID || person.Type != 5 {
						guardian = false
					}
				}
				report.Role = p.store.LoginRoleForSchool(school, info.PersonType, guardian)
				report.UpstreamAdmin = info.UpstreamAdmin
				id, kind := info.PersonID, "STUDENT"
				if info.PersonType == 1 || info.PersonType == 2 {
					kind = "TEACHER"
				}
				allowed := id > 0 && !guardian
				if req.PersonID > 0 {
					id = req.PersonID
					allowed = false
					if !guardian && id == info.PersonID {
						allowed = true
					}
					for _, person := range info.LinkedPersons {
						if person.ID == id && person.Type == 5 {
							allowed = true
							kind = "STUDENT"
						}
					}
				}
				if allowed {
					report.PersonID = id
					body, _ := json.Marshal(map[string]any{"id": "compatibility", "method": "getTimetable2017", "jsonrpc": "2.0", "params": []any{map[string]any{"id": id, "type": kind, "startDate": req.From, "endDate": req.To, "auth": map[string]any{}}}})
					owned := *u
					if secret != "" {
						owned.Method = "key"
						owned.Password = secret
					}
					if rewritten, rewriteErr := p.rewriteAuthForOwner(school, body, &owned); rewriteErr == nil {
						body = rewritten
					}
					data, status, _, fetchErr := p.untis.RawIntern(school, cookie, "getTimetable2017", body)
					if fetchErr == nil && status == 200 {
						lessons, parseErr := p.parseAnalyticsLessons(school, data, req.From, req.To)
						switch {
						case errors.Is(parseErr, errAnalyticsUpstreamRejected):
							report.Status = "upstream_rejected"
						case parseErr == nil && len(lessons) > 0:
							report.PeriodCount = len(lessons)
							report.Status = "observed_nonempty"
						case parseErr == nil:
							report.Status = "empty_or_unavailable"
						}
					}
				} else {
					report.Status = "identity_selection_required"
					report.Notes = append(report.Notes, "Select a verified linked student for guardian accounts; arbitrary person IDs are never probed.")
				}
			}
		}
		if report.UpstreamAdmin == nil {
			report.Notes = append(report.Notes, "Upstream administrator status was not reported; do not assume this is a non-administrator.")
		}
		report.Notes = append(report.Notes, "Empty timetables do not prove historical support or its absence. No raw lessons or credentials are returned.")
		encoded, _ := json.Marshal(report)
		if err := p.store.SaveCompatibilityResult(school, user, encoded); err != nil {
			writeJSONError(w, 500, "could not save diagnostic result")
			return
		}
		p.audit(r, school, "compatibility.read_probe", user, req.From+".."+req.To+" "+report.Status+" "+strconv.Itoa(report.PeriodCount))
		p.writeJSON(w, report)
	}
}
