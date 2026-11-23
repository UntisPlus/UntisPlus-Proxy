package proxy

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"untis-proxy/internal/access"
	"untis-proxy/internal/store"
)

// adminDashboardHTML is the embedded single-page admin UI served at /admin.
//
//go:embed static/admin.html
var adminDashboardHTML string

// adminLoginHTML is the browser login page served anonymously at /admin/login.
// It authenticates through the same getUserData2017 call the app uses, then
// redirects to /admin.
//
//go:embed static/login.html
var adminLoginHTML string

// handleAdminDashboard serves the single-page admin UI. It requires an admin
// session; the page itself talks to the JSON /admin/* endpoints in the browser
// so the server never mixes HTML into the API.
func (p *Proxy) handleAdminDashboard(w http.ResponseWriter, r *http.Request) {
	u := p.sessionUser(r)
	if u == nil {
		// Browsers land here without a session cookie (the app mints sessions,
		// not the browser). Send them to the login page; API clients keep the
		// plain 401.
		if strings.Contains(r.Header.Get("Accept"), "text/html") {
			http.Redirect(w, r, "/admin/login", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if !p.isAdmin(u.Username) {
		p.forbidden(w)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(adminDashboardHTML))
}

// handleAdminLogin serves the browser login page anonymously. It reuses the
// existing getUserData2017 JSON-RPC path: the page posts the user's OTP there
// same-origin, the proxy mints the real session cookies, and the page then
// redirects to /admin if the account holds the admin flag.
func (p *Proxy) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if u := p.sessionUser(r); u != nil && p.isAdmin(u.Username) {
		http.Redirect(w, r, "/admin", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(adminLoginHTML))
}

// handleAdminLogout revokes the current browser session and expires its cookies.
// A POST plus same-origin checks prevents a third-party page from logging out a
// signed-in administrator as a side effect.
func (p *Proxy) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !sameOriginBrowserRequest(p, r) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if ck, err := r.Cookie("JSESSIONID"); err == nil && ck.Value != "" {
		p.sessions.Delete(ck.Value)
	}
	secure := p.requestIsHTTPS(r)
	http.SetCookie(w, &http.Cookie{Name: "JSESSIONID", Value: "", Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	http.SetCookie(w, &http.Cookie{Name: "schoolname", Value: "", Path: "/", Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	p.writeJSON(w, map[string]any{"ok": true})
}

func sameOriginBrowserRequest(p *Proxy, r *http.Request) bool {
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" { // Native API clients do not send Origin.
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || !strings.EqualFold(u.Host, r.Host) {
		return false
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return false
	}
	return (u.Scheme == "https") == p.requestIsHTTPS(r)
}

// handleAdmin is the HTTP root for the admin API. Every route requires an
// admin session (users.admin flag). It exposes everything untisctl can do —
// users, pool, permissions, tokens, schools, webhooks and ntfy topics — as
// JSON for the /admin dashboard (and for scripting). Requests that hit this
// handler arrive via /admin/ (the trailing slash), which never serves HTML.
func (p *Proxy) handleAdmin(w http.ResponseWriter, r *http.Request) {
	u := p.sessionUser(r)
	if u == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if !p.isAdmin(u.Username) {
		p.forbidden(w)
		return
	}
	if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch || r.Method == http.MethodDelete {
		if !sameOriginBrowserRequest(p, r) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
	}

	path := strings.TrimPrefix(r.URL.Path, "/admin")
	path = strings.Trim(path, "/")
	parts := strings.Split(path, "/")

	switch {
	case len(parts) == 1 && parts[0] == "status" && r.Method == http.MethodGet:
		p.adminStatus(w)
	case parts[0] == "users":
		p.adminUsers(w, r, parts[1:])
	case parts[0] == "perms":
		p.adminPerms(w, r, parts[1:])
	case parts[0] == "pool":
		p.adminPool(w, r, parts[1:])
	case parts[0] == "tokens":
		p.adminTokens(w, r, parts[1:])
	case parts[0] == "schools":
		p.adminSchools(w, r, parts[1:])
	case parts[0] == "anonymous":
		p.adminAnonymous(w, r, parts[1:])
	case parts[0] == "search" && r.Method == http.MethodGet:
		p.adminSearch(w, r)
	case parts[0] == "webhooks":
		p.adminWebhooks(w, r, parts[1:])
	case parts[0] == "ntfy":
		p.adminNtfy(w, r, parts[1:])
	case parts[0] == "outage-ntfy":
		p.adminOutageNtfy(w, r, parts[1:])
	case parts[0] == "outbox" && r.Method == http.MethodGet:
		p.adminOutbox(w, r)
	case parts[0] == "audit" && r.Method == http.MethodGet:
		p.adminAudit(w, r)
	case parts[0] == "recon":
		p.adminRecon(w, r, parts[1:])
	case parts[0] == "rooms":
		p.adminRooms(w, r, parts[1:])
	case parts[0] == "updates":
		p.handleUpdates(w, r)
	case parts[0] == "setup":
		p.handleAdminSetup(w, r, parts)
	case parts[0] == "access" || parts[0] == "v2":
		p.adminAccessAPI(w, r, parts[1:])
	default:
		p.writeJSON(w, map[string]any{"error": "not found"})
	}
}

func (p *Proxy) adminAccessAPI(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) == 0 {
		p.writeJSON(w, map[string]any{"error": "not found"})
		return
	}
	switch parts[0] {
	case "preview", "features", "admission", "analytics-access", "analytics-years":
		p.adminControls(w, r, parts[0])
	case "group-editor":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ID          int64          `json:"id"`
			School      string         `json:"school"`
			Name        string         `json:"name"`
			Description string         `json:"description"`
			Grants      map[string]int `json:"grants"`
			AssignTo    string         `json:"assignTo"`
		}
		if err := decodeJSONBody(r, &req); err != nil {
			badRequest(w, "invalid group")
			return
		}
		id, err := p.store.SavePermissionGroup(req.ID, req.School, req.Name, req.Description, req.Grants, req.AssignTo)
		if err != nil {
			badRequest(w, err.Error())
			return
		}
		p.audit(r, req.School, "access.group.save", strconv.FormatInt(id, 10), req.Name)
		p.writeJSON(w, map[string]any{"ok": true, "id": id})
	case "effective":
		if r.Method != http.MethodGet {
			p.writeJSON(w, map[string]any{"error": "not found"})
			return
		}
		school := strings.TrimSpace(r.URL.Query().Get("school"))
		username := strings.TrimSpace(r.URL.Query().Get("user"))
		if username == "" {
			username = strings.TrimSpace(r.URL.Query().Get("username"))
		}
		if school == "" || username == "" {
			p.writeJSON(w, map[string]any{"error": "school and user required"})
			return
		}
		u, err := p.store.GetUser(username)
		if err != nil || u == nil {
			p.writeJSON(w, map[string]any{"error": "user not found"})
			return
		}
		res, err := p.store.ResolveEffective(school, u)
		if err != nil || res == nil {
			p.writeJSON(w, map[string]any{"error": "resolve failed"})
			return
		}
		caps := make([]string, 0, len(res.Decisions))
		for c := range res.Decisions {
			caps = append(caps, string(c))
		}
		sort.Strings(caps)
		decisions := make(map[string]any, len(caps))
		effective := make([]string, 0)
		for _, c := range caps {
			d := res.Decisions[access.Capability(c)]
			decisions[c] = map[string]any{
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
		groups, _ := p.store.ListUserCapGroups(school, username)
		roleDefIDs, _ := p.store.ListRoleDefaultGroups(school, string(res.Role))
		if len(roleDefIDs) == 0 {
			roleDefIDs, _ = p.store.ListRoleDefaultGroups("", string(res.Role))
		}
		everyoneIDs, _ := p.store.ListRoleDefaultGroups(school, "*")
		roleDefIDs = append(roleDefIDs, everyoneIDs...)
		p.writeJSON(w, map[string]any{
			"school": school, "user": username, "role": res.Role,
			"groups": groups, "groupIds": res.GroupIDs,
			"directGrants": res.DirectGrants, "roleDefaultGroupIds": roleDefIDs,
			"effective": effective, "decisions": decisions,
		})
	case "groups":
		// GET /admin/v2/groups?school=, GET /admin/v2/groups/{id}
		// POST /admin/v2/groups {school,name,description}
		// DELETE /admin/v2/groups/{id}
		// POST|PUT /admin/v2/groups/{id}/grants {capability,effect}
		switch {
		case r.Method == http.MethodGet && len(parts) == 1:
			school := strings.TrimSpace(r.URL.Query().Get("school"))
			groups, err := p.store.ListCapGroups(school)
			if err != nil {
				p.writeJSON(w, map[string]any{"error": "store error"})
				return
			}
			p.writeJSON(w, map[string]any{"groups": groups})
			return
		case r.Method == http.MethodGet && len(parts) == 2:
			id, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				p.writeJSON(w, map[string]any{"error": "bad group id"})
				return
			}
			g, grants, err := p.store.GetCapGroup(id)
			if err != nil || g == nil {
				p.writeJSON(w, map[string]any{"error": "not found"})
				return
			}
			p.writeJSON(w, map[string]any{"group": g, "grants": grants})
			return
		case r.Method == http.MethodPost && len(parts) == 1:
			var req struct {
				School      string `json:"school"`
				Name        string `json:"name"`
				Description string `json:"description"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				p.writeJSON(w, map[string]any{"error": "bad request"})
				return
			}
			id, err := p.store.CreateCapGroup(req.School, req.Name, req.Description)
			if err != nil {
				p.writeJSON(w, map[string]any{"error": err.Error()})
				return
			}
			p.audit(r, req.School, "v2.group.create", req.Name, strconv.FormatInt(id, 10))
			p.writeJSON(w, map[string]any{"ok": true, "id": id, "school": req.School, "name": req.Name})
			return
		case r.Method == http.MethodDelete && len(parts) == 2:
			id, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				p.writeJSON(w, map[string]any{"error": "bad group id"})
				return
			}
			if err := p.store.DeleteCapGroup(id); err != nil {
				p.writeJSON(w, map[string]any{"error": err.Error()})
				return
			}
			p.audit(r, "", "v2.group.delete", strconv.FormatInt(id, 10), "")
			p.writeJSON(w, map[string]any{"ok": true, "deleted": id})
			return
		case (r.Method == http.MethodPost || r.Method == http.MethodPut) && len(parts) == 3 && parts[2] == "grants":
			id, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				p.writeJSON(w, map[string]any{"error": "bad group id"})
				return
			}
			var req struct {
				Capability string `json:"capability"`
				Cap        string `json:"cap"`
				Effect     any    `json:"effect"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				p.writeJSON(w, map[string]any{"error": "bad request"})
				return
			}
			cap := req.Capability
			if cap == "" {
				cap = req.Cap
			}
			if cap == "" {
				p.writeJSON(w, map[string]any{"error": "capability required"})
				return
			}
			effect := 0
			switch v := req.Effect.(type) {
			case float64:
				if v > 0 {
					effect = 1
				} else if v < 0 {
					effect = -1
				}
			case string:
				switch strings.ToLower(strings.TrimSpace(v)) {
				case "allow", "grant", "1", "true":
					effect = 1
				case "deny", "revoke", "-1", "false":
					effect = -1
				case "inherit", "clear", "0", "":
					effect = 0
				default:
					p.writeJSON(w, map[string]any{"error": "invalid effect"})
					return
				}
			case bool:
				if v {
					effect = 1
				} else {
					effect = -1
				}
			case nil:
				effect = 0
			default:
				p.writeJSON(w, map[string]any{"error": "invalid effect"})
				return
			}
			if err := p.store.SetCapGroupGrant(id, cap, effect); err != nil {
				p.writeJSON(w, map[string]any{"error": err.Error()})
				return
			}
			p.audit(r, "", "v2.group.grant.set", strconv.FormatInt(id, 10), cap+"="+strconv.Itoa(effect))
			p.writeJSON(w, map[string]any{"ok": true, "groupId": id, "capability": cap, "effect": effect})
			return
		}
		p.writeJSON(w, map[string]any{"error": "not found"})
		return
	case "grants":
		if r.Method != http.MethodGet {
			p.writeJSON(w, map[string]any{"error": "not found"})
			return
		}
		school := strings.TrimSpace(r.URL.Query().Get("school"))
		username := strings.TrimSpace(r.URL.Query().Get("user"))
		if username == "" {
			username = strings.TrimSpace(r.URL.Query().Get("username"))
		}
		if school == "" || username == "" {
			p.writeJSON(w, map[string]any{"error": "school and user required"})
			return
		}
		grants, err := p.store.ListUserCapGrants(school, username)
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		p.writeJSON(w, map[string]any{"school": school, "user": username, "grants": grants})
	case "grant":
		if r.Method != http.MethodPost && r.Method != http.MethodPut {
			p.writeJSON(w, map[string]any{"error": "not found"})
			return
		}
		var req struct {
			School     string `json:"school"`
			User       string `json:"user"`
			Username   string `json:"username"`
			Capability string `json:"capability"`
			Effect     any    `json:"effect"` // -1,0,1 or "allow"/"deny"/"inherit"
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			p.writeJSON(w, map[string]any{"error": "bad request"})
			return
		}
		user := req.User
		if user == "" {
			user = req.Username
		}
		if req.School == "" || user == "" || req.Capability == "" {
			p.writeJSON(w, map[string]any{"error": "school, user, capability required"})
			return
		}
		effect := 0
		switch v := req.Effect.(type) {
		case float64:
			if v > 0 {
				effect = 1
			} else if v < 0 {
				effect = -1
			}
		case string:
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "allow", "grant", "1", "true":
				effect = 1
			case "deny", "revoke", "-1", "false":
				effect = -1
			case "inherit", "clear", "0", "":
				effect = 0
			default:
				p.writeJSON(w, map[string]any{"error": "invalid effect"})
				return
			}
		case bool:
			if v {
				effect = 1
			} else {
				effect = -1
			}
		case nil:
			effect = 0
		default:
			p.writeJSON(w, map[string]any{"error": "invalid effect"})
			return
		}
		if err := p.store.SetUserCapGrant(req.School, user, req.Capability, effect); err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		p.audit(r, req.School, "v2.grant.set", user, req.Capability+"="+strconv.Itoa(effect))
		p.writeJSON(w, map[string]any{"ok": true, "school": req.School, "user": user, "capability": req.Capability, "effect": effect})
	case "user-groups":
		if r.Method == http.MethodGet {
			school := strings.TrimSpace(r.URL.Query().Get("school"))
			username := strings.TrimSpace(r.URL.Query().Get("user"))
			if username == "" {
				username = strings.TrimSpace(r.URL.Query().Get("username"))
			}
			if school == "" || username == "" {
				p.writeJSON(w, map[string]any{"error": "school and user required"})
				return
			}
			groups, err := p.store.ListUserCapGroups(school, username)
			if err != nil {
				p.writeJSON(w, map[string]any{"error": "store error"})
				return
			}
			p.writeJSON(w, map[string]any{"school": school, "user": username, "groups": groups})
			return
		}
		if r.Method != http.MethodPost {
			p.writeJSON(w, map[string]any{"error": "not found"})
			return
		}
		action := ""
		if len(parts) > 1 {
			action = parts[1]
		}
		var req struct {
			School   string `json:"school"`
			User     string `json:"user"`
			Username string `json:"username"`
			GroupID  int64  `json:"groupId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			p.writeJSON(w, map[string]any{"error": "bad request"})
			return
		}
		user := req.User
		if user == "" {
			user = req.Username
		}
		if req.School == "" || user == "" || req.GroupID <= 0 {
			p.writeJSON(w, map[string]any{"error": "school, user, groupId required"})
			return
		}
		switch action {
		case "assign":
			if err := p.store.AssignUserCapGroup(req.School, user, req.GroupID); err != nil {
				p.writeJSON(w, map[string]any{"error": "store error"})
				return
			}
			p.audit(r, req.School, "v2.user_group.assign", user, strconv.FormatInt(req.GroupID, 10))
		case "unassign":
			if err := p.store.UnassignUserCapGroup(req.School, user, req.GroupID); err != nil {
				p.writeJSON(w, map[string]any{"error": "store error"})
				return
			}
			p.audit(r, req.School, "v2.user_group.unassign", user, strconv.FormatInt(req.GroupID, 10))
		default:
			p.writeJSON(w, map[string]any{"error": "action must be assign or unassign"})
			return
		}
		p.writeJSON(w, map[string]any{"ok": true})
	case "role-defaults":
		if r.Method == http.MethodGet {
			school := strings.TrimSpace(r.URL.Query().Get("school"))
			role := strings.TrimSpace(r.URL.Query().Get("role"))
			roles := []string{role}
			if role == "" {
				roles = []string{"*", "student", "teacher", "staff", "parent", "unknown", "other"}
			}
			out := map[string]any{}
			for _, rname := range roles {
				ids, err := p.store.ListRoleDefaultGroups(school, rname)
				if err != nil {
					p.writeJSON(w, map[string]any{"error": "store error"})
					return
				}
				if len(ids) > 0 || role != "" {
					out[rname] = ids
				}
			}
			p.writeJSON(w, map[string]any{"school": school, "roleDefaults": out})
			return
		}
		if r.Method == http.MethodDelete {
			school := strings.TrimSpace(r.URL.Query().Get("school"))
			role := strings.TrimSpace(r.URL.Query().Get("role"))
			gid, _ := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("groupId")), 10, 64)
			if role == "" || gid <= 0 {
				p.writeJSON(w, map[string]any{"error": "role and groupId required"})
				return
			}
			if err := p.store.DeleteRoleDefaultGroup(school, role, gid); err != nil {
				p.writeJSON(w, map[string]any{"error": "store error"})
				return
			}
			p.audit(r, school, "v2.role_default.delete", role, strconv.FormatInt(gid, 10))
			p.writeJSON(w, map[string]any{"ok": true})
			return
		}
		if r.Method != http.MethodPost && r.Method != http.MethodPut {
			p.writeJSON(w, map[string]any{"error": "not found"})
			return
		}
		action := ""
		if len(parts) > 1 {
			action = parts[1]
		}
		if action != "" && action != "set" && action != "delete" {
			p.writeJSON(w, map[string]any{"error": "not found"})
			return
		}
		var req struct {
			School   string `json:"school"`
			Role     string `json:"role"`
			GroupID  int64  `json:"groupId"`
			Priority int    `json:"priority"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			p.writeJSON(w, map[string]any{"error": "bad request"})
			return
		}
		if req.Role == "" || req.GroupID <= 0 {
			p.writeJSON(w, map[string]any{"error": "role and groupId required"})
			return
		}
		if action == "delete" {
			if err := p.store.DeleteRoleDefaultGroup(req.School, req.Role, req.GroupID); err != nil {
				p.writeJSON(w, map[string]any{"error": "store error"})
				return
			}
			p.audit(r, req.School, "v2.role_default.delete", req.Role, strconv.FormatInt(req.GroupID, 10))
			p.writeJSON(w, map[string]any{"ok": true})
			return
		}
		if err := p.store.SetRoleDefaultGroup(req.School, req.Role, req.GroupID, req.Priority); err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		p.audit(r, req.School, "v2.role_default.set", req.Role, strconv.FormatInt(req.GroupID, 10))
		p.writeJSON(w, map[string]any{"ok": true})
	case "gates":
		if r.Method == http.MethodGet {
			gates, err := p.store.ListServerCapGates()
			if err != nil {
				p.writeJSON(w, map[string]any{"error": "store error"})
				return
			}
			p.writeJSON(w, map[string]any{"gates": gates})
			return
		}
		if r.Method != http.MethodPost && r.Method != http.MethodPut {
			p.writeJSON(w, map[string]any{"error": "not found"})
			return
		}
		if len(parts) > 1 && parts[1] != "set" {
			p.writeJSON(w, map[string]any{"error": "not found"})
			return
		}
		var req struct {
			Scope      string `json:"scope"`
			Capability string `json:"capability"`
			Cap        string `json:"cap"`
			Enabled    *bool  `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Enabled == nil {
			p.writeJSON(w, map[string]any{"error": "scope, capability/cap and enabled required"})
			return
		}
		cap := req.Capability
		if cap == "" {
			cap = req.Cap
		}
		if req.Scope == "" || cap == "" {
			p.writeJSON(w, map[string]any{"error": "scope and capability required"})
			return
		}
		if err := p.store.SetServerCapGate(req.Scope, cap, *req.Enabled); err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		p.audit(r, req.Scope, "v2.gate.set", cap, strconv.FormatBool(*req.Enabled))
		p.writeJSON(w, map[string]any{"ok": true, "scope": req.Scope, "capability": cap, "enabled": *req.Enabled})
	case "person-type-roles", "role-mappings":
		switch r.Method {
		case http.MethodGet:
			scope := strings.TrimSpace(r.URL.Query().Get("scope"))
			mappings, err := p.store.ListPersonTypeRoles(scope)
			if err != nil {
				p.writeJSON(w, map[string]any{"error": "store error"})
				return
			}
			p.writeJSON(w, map[string]any{"mappings": mappings})
			return
		case http.MethodPost, http.MethodPut:
			var req struct {
				Scope      string `json:"scope"`
				PersonType int64  `json:"personType"`
				Role       string `json:"role"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				p.writeJSON(w, map[string]any{"error": "bad request"})
				return
			}
			if strings.TrimSpace(req.Role) == "" {
				p.writeJSON(w, map[string]any{"error": "role required"})
				return
			}
			if err := p.store.SetPersonTypeRole(req.Scope, req.PersonType, req.Role); err != nil {
				p.writeJSON(w, map[string]any{"error": err.Error()})
				return
			}
			p.audit(r, req.Scope, "v2.person_type_role.set", strconv.FormatInt(req.PersonType, 10), req.Role)
			p.writeJSON(w, map[string]any{"ok": true, "scope": req.Scope, "personType": req.PersonType, "role": access.NormalizeRole(req.Role)})
			return
		case http.MethodDelete:
			scope := strings.TrimSpace(r.URL.Query().Get("scope"))
			ptStr := strings.TrimSpace(r.URL.Query().Get("personType"))
			if ptStr == "" {
				ptStr = strings.TrimSpace(r.URL.Query().Get("person_type"))
			}
			pt, err := strconv.ParseInt(ptStr, 10, 64)
			if err != nil {
				p.writeJSON(w, map[string]any{"error": "personType required"})
				return
			}
			if err := p.store.DeletePersonTypeRole(scope, pt); err != nil {
				p.writeJSON(w, map[string]any{"error": "store error"})
				return
			}
			p.audit(r, scope, "v2.person_type_role.delete", strconv.FormatInt(pt, 10), "")
			p.writeJSON(w, map[string]any{"ok": true})
			return
		}
		p.writeJSON(w, map[string]any{"error": "not found"})
		return
	case "anon-profiles":
		if r.Method != http.MethodGet {
			p.writeJSON(w, map[string]any{"error": "not found"})
			return
		}
		profiles, err := p.store.ListSchoolAnonProfiles()
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		out := make([]map[string]any, 0, len(profiles))
		for _, prof := range profiles {
			payload, _, _ := p.store.GetSchoolAnonProfilePayload(prof.School)
			out = append(out, map[string]any{
				"school": prof.School, "loginOk": prof.LoginOK, "source": prof.Source,
				"detectedAt": prof.DetectedAt.Unix(), "expiresAt": prof.ExpiresAt.Unix(),
				"classesDisplayable": payload.ClassesDisplayable, "capabilities": payload.Capabilities, "notes": payload.Notes,
			})
		}
		p.writeJSON(w, map[string]any{"profiles": out})
	default:
		p.writeJSON(w, map[string]any{"error": "not found"})
	}
}

func (p *Proxy) adminStatus(w http.ResponseWriter) {
	users, _ := p.store.ListUsers()
	userCount := len(users)
	pool, _ := p.store.Pool("")
	schools, _ := p.store.ListSchools()
	tokens, _ := p.store.ListClassTokens()
	perms, _ := p.store.AllPerms()
	webhooks, _ := p.store.ListWebhooks("")
	ntfy, _ := p.store.ListNtfyTopics("")
	outageNtfy, _ := p.store.ListOutageNtfy()
	// Delivery backlog: pending means retrying, dead means a destination gave up
	// and someone has to look at it.
	outbox, _ := p.store.OutboxStats()

	var adminCount, boosted, editor, recon int
	for _, us := range users {
		if us.Admin {
			adminCount++
		}
		if p.isBoosted(us.Username) {
			boosted++
		}
		if p.isEditor(us.Username) {
			editor++
		}
		if p.hasPerm(us.Username, store.FeatureRecon) {
			recon++
		}
	}

	global := map[string]bool{}
	for _, pr := range perms {
		if pr.Username == store.GlobalPermUser() {
			global[pr.Feature] = pr.Allowed
		}
	}

	p.writeJSON(w, map[string]any{
		"version":    version,
		"school":     p.opts.School,
		"users":      userCount,
		"admins":     adminCount,
		"boosted":    boosted,
		"editors":    editor,
		"recon":      recon,
		"pool":       len(pool),
		"schools":    len(schools),
		"tokens":     len(tokens),
		"webhooks":   len(webhooks),
		"ntfyTopics": len(ntfy),
		"outageNtfy": len(outageNtfy),
		"outbox": map[string]int{
			"pending": outbox.Pending,
			"sending": outbox.Sending,
			"dead":    outbox.Dead,
			"sent":    outbox.Sent,
		},
		"global": global,
	})
}

func (p *Proxy) adminPerms(w http.ResponseWriter, r *http.Request, parts []string) {
	switch {
	case r.Method == http.MethodGet && len(parts) == 0:
		perms, err := p.store.AllPerms()
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		defaults, err := p.store.DefaultPerms()
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		p.writeJSON(w, map[string]any{"perms": perms, "defaults": defaults})
		return
	case r.Method == http.MethodPost && len(parts) == 1 && parts[0] == "default":
		var req struct {
			Feature string `json:"feature"`
			Allowed *bool  `json:"allowed"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Allowed == nil || req.Feature == "" {
			p.writeJSON(w, map[string]any{"error": "feature and allowed required"})
			return
		}
		if err := p.store.SetDefaultPerm(req.Feature, *req.Allowed); err != nil {
			if errors.Is(err, store.ErrUnknownPerm) {
				p.writeJSON(w, map[string]any{"error": "unknown permission"})
				return
			}
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		p.audit(r, "", "perms.default", req.Feature, "allowed="+strconv.FormatBool(*req.Allowed))
		defaults, err := p.store.DefaultPerms()
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		p.writeJSON(w, map[string]any{"feature": req.Feature, "allowed": *req.Allowed, "defaults": defaults})
		return
	}
	p.writeJSON(w, map[string]any{"error": "not found"})
}

func (p *Proxy) adminPool(w http.ResponseWriter, r *http.Request, parts []string) {
	var school string
	if len(parts) > 0 && parts[0] != "" {
		school = parts[0]
	}
	classes, err := p.store.Pool(school)
	if err != nil {
		p.writeJSON(w, map[string]any{"error": "store error"})
		return
	}
	type poolEntry struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Owner string `json:"owner"`
	}
	out := make([]poolEntry, 0, len(classes))
	for _, c := range classes {
		owner, _ := p.store.OwnerForClass(school, c.ID)
		ownerName := ""
		if owner != nil {
			ownerName = owner.Username
		}
		out = append(out, poolEntry{c.ID, c.Name, ownerName})
	}
	p.writeJSON(w, map[string]any{"school": school, "pool": out})
}

// adminSearch powers the dashboard element pickers. It searches persisted
// master names (recon_elements) and pooled classes, so the admin never types a
// raw id. type filters to a single element kind; q is a case-insensitive
// substring match on name or id.
func (p *Proxy) adminSearch(w http.ResponseWriter, r *http.Request) {
	school := r.URL.Query().Get("school")
	if school == "" {
		school = p.opts.School
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	typ := strings.ToUpper(r.URL.Query().Get("type"))
	// limit bounds the response so a picker that fetches a whole element list
	// (rooms, say) is not silently cut off before the user can filter it. It
	// defaults to the historical 60 and is clamped to a sane maximum.
	limit := 60
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = n
	}
	if limit > 2000 {
		limit = 2000
	}

	type res struct {
		Type string `json:"type"`
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	var out []res
	seen := map[string]bool{}
	add := func(t string, id int64, name string) {
		if id <= 0 {
			return
		}
		key := fmt.Sprintf("%s:%d", t, id)
		if seen[key] {
			return
		}
		seen[key] = true
		if typ != "" && t != typ {
			return
		}
		if q != "" && !strings.Contains(strings.ToLower(name)+" "+strconv.FormatInt(id, 10), q) {
			return
		}
		out = append(out, res{t, id, name})
	}

	if elems, err := p.store.ListElementsWithNames(school); err == nil {
		for _, id := range sortedKeys(elems["CLASS"]) {
			add("CLASS", id, elems["CLASS"][id])
		}
		for _, id := range sortedKeys(elems["TEACHER"]) {
			add("TEACHER", id, elems["TEACHER"][id])
		}
		for _, id := range sortedKeys(elems["ROOM"]) {
			add("ROOM", id, elems["ROOM"][id])
		}
		for _, id := range sortedKeys(elems["SUBJECT"]) {
			add("SUBJECT", id, elems["SUBJECT"][id])
		}
	}
	if pool, err := p.store.Pool(school); err == nil {
		for _, c := range pool {
			add("CLASS", c.ID, c.Name)
		}
	}
	if students, err := p.store.ListStudents(school); err == nil {
		for _, s := range students {
			name := s.DisplayName
			if name == "" {
				name = s.Username
			}
			add("STUDENT", s.PersonID, name)
		}
	}

	typeOrder := map[string]int{"CLASS": 0, "STUDENT": 1, "TEACHER": 2, "ROOM": 3, "SUBJECT": 4}
	sort.Slice(out, func(i, j int) bool {
		if typeOrder[out[i].Type] != typeOrder[out[j].Type] {
			return typeOrder[out[i].Type] < typeOrder[out[j].Type]
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	p.writeJSON(w, map[string]any{"school": school, "results": out})
}

func sortedKeys(m map[int64]string) []int64 {
	keys := make([]int64, 0, len(m))
	for id := range m {
		keys = append(keys, id)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

func (p *Proxy) adminTokens(w http.ResponseWriter, r *http.Request, parts []string) {
	if r.Method == http.MethodPost && len(parts) == 0 {
		// POST /admin/tokens — create a calendar token for any element.
		var req struct {
			School      string `json:"school"`
			ElementType string `json:"elementType"`
			ElementID   int64  `json:"elementId"`
			Timezone    string `json:"timezone"`
			Days        int    `json:"days"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			p.writeJSON(w, map[string]any{"error": "bad request"})
			return
		}
		elType := strings.ToUpper(req.ElementType)
		switch elType {
		case "CLASS", "TEACHER", "ROOM", "SUBJECT", "STUDENT":
		default:
			p.writeJSON(w, map[string]any{"error": "elementType must be one of CLASS, TEACHER, ROOM, SUBJECT, STUDENT"})
			return
		}
		if req.ElementID <= 0 {
			p.writeJSON(w, map[string]any{"error": "elementId required"})
			return
		}
		school := req.School
		if school == "" {
			school = p.opts.School
		}
		tz := req.Timezone
		if tz == "" {
			tz = "Europe/Berlin"
		}
		days := req.Days
		if days <= 0 {
			days = 30
		}
		if days > 365 {
			days = 365
		}
		tok, err := p.store.ClassTokenForElement(school, elType, req.ElementID, "")
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		if tok == nil {
			now := time.Now().Unix()
			tok = &store.ClassToken{
				Token: newCalendarToken(), School: school,
				ElementType: elType, ElementID: req.ElementID,
				Timezone: tz, Days: days, CreatedAt: now, LastAccess: now,
				CreatedBy: p.sessionUser(r).Username,
			}
			if elType == "CLASS" {
				tok.ClassID = req.ElementID
			}
			if elType == "STUDENT" {
				tok.PersonID = req.ElementID
			}
			if err := p.store.CreateClassToken(tok); err != nil {
				p.writeJSON(w, map[string]any{"error": "store error"})
				return
			}
			p.audit(r, school, "token.create", tokenAuditTarget(elType, req.ElementID), "")
		}
		p.writeCalendarResponse(w, r, tok)
		return
	}
	if r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "rotate" {
		tok, err := p.store.ClassTokenByToken(parts[0])
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		if tok == nil {
			p.writeJSON(w, map[string]any{"error": "not found"})
			return
		}
		rotated, err := p.rotateCalendarToken(tok)
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		p.audit(r, tok.School, "token.rotate", tokenAuditTarget(tok.ElementType, tok.ElementID), "rotated")
		p.writeCalendarResponse(w, r, rotated)
		return
	}
	if r.Method == http.MethodPost && len(parts) == 1 {
		// POST /admin/tokens/{token} — edit days.
		var req struct {
			Days int `json:"days"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			p.writeJSON(w, map[string]any{"error": "bad request"})
			return
		}
		if req.Days <= 0 || req.Days > 365 {
			p.writeJSON(w, map[string]any{"error": "days must be between 1 and 365"})
			return
		}
		n, err := p.store.UpdateClassTokenDays(parts[0], req.Days)
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		if n == 0 {
			p.writeJSON(w, map[string]any{"error": "not found"})
			return
		}
		p.audit(r, "", "token.edit", p.tokenAuditKey(parts[0]), "days="+strconv.Itoa(req.Days))
		p.writeJSON(w, map[string]any{"updated": true, "days": req.Days})
		return
	}
	if r.Method == http.MethodDelete && len(parts) == 1 {
		key := p.tokenAuditKey(parts[0])
		n, err := p.store.DeleteClassToken(parts[0])
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		if n > 0 {
			p.audit(r, "", "token.delete", key, "")
		}
		p.writeJSON(w, map[string]any{"revoked": n > 0})
		return
	}
	// GET /admin/tokens and GET /admin/tokens/{school}
	school := ""
	if len(parts) == 1 && parts[0] != "" {
		school = parts[0]
	}
	tokens, err := p.store.ListClassTokens()
	if err != nil {
		p.writeJSON(w, map[string]any{"error": "store error"})
		return
	}
	if school != "" {
		var filtered []*store.ClassToken
		for _, t := range tokens {
			if t.School == school {
				filtered = append(filtered, t)
			}
		}
		tokens = filtered
	}
	type tokenOut struct {
		Token       string `json:"token"`
		School      string `json:"school"`
		ClassID     int64  `json:"classId"`
		ElementType string `json:"elementType"`
		ElementID   int64  `json:"elementId"`
		ElementName string `json:"elementName"`
		Days        int    `json:"days"`
		CreatedAt   int64  `json:"createdAt"`
		LastAccess  int64  `json:"lastAccess"`
		CreatedBy   string `json:"createdBy"`
	}
	out := make([]tokenOut, 0, len(tokens))
	for _, t := range tokens {
		name := ""
		switch t.ElementType {
		case "CLASS", "ROOM", "TEACHER", "SUBJECT":
			name = elementName(p.masterData(t.School), t.ElementType, t.ElementID)
		case "STUDENT":
			if u, _ := p.store.UserByPersonID(t.ElementID); u != nil && u.DisplayName != "" {
				name = u.DisplayName
			}
		}
		out = append(out, tokenOut{t.Token, t.School, t.ClassID, t.ElementType, t.ElementID, name, t.Days, t.CreatedAt, t.LastAccess, t.CreatedBy})
	}
	p.writeJSON(w, map[string]any{"tokens": out})
}

func (p *Proxy) adminSchools(w http.ResponseWriter, r *http.Request, parts []string) {
	switch {
	case r.Method == http.MethodPost && len(parts) == 1 && parts[0] == "settings":
		var req struct {
			AllowNew *bool `json:"allowNew"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AllowNew == nil {
			p.writeJSON(w, map[string]any{"error": "allowNew required"})
			return
		}
		if err := p.store.SetAllowNewSchools(*req.AllowNew); err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		p.audit(r, "", "school.allowNew", "settings", "allowNew="+strconv.FormatBool(*req.AllowNew))
		p.writeJSON(w, map[string]any{"allowNew": *req.AllowNew})
		return
	case r.Method == http.MethodPost && len(parts) == 2 && (parts[1] == "block" || parts[1] == "unblock"):
		name := strings.TrimSpace(parts[0])
		if name == "" {
			p.writeJSON(w, map[string]any{"error": "school required"})
			return
		}
		blocked := parts[1] == "block"
		if blocked && name == p.opts.School {
			p.writeJSON(w, map[string]any{"error": "cannot block the configured default school"})
			return
		}
		if err := p.store.SetSchoolBlocked(name, blocked); err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		p.audit(r, name, "school.block", name, "blocked="+strconv.FormatBool(blocked))
		p.writeJSON(w, map[string]any{"school": name, "blocked": blocked})
		return
	case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "anonymous":
		name := strings.TrimSpace(parts[0])
		if name == "" {
			p.writeJSON(w, map[string]any{"error": "school required"})
			return
		}
		var req struct {
			Enabled *bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Enabled == nil {
			p.writeJSON(w, map[string]any{"error": "enabled required"})
			return
		}
		if err := p.store.SetSchoolAnonymous(name, *req.Enabled); err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		p.audit(r, name, "school.anonymous", name, "enabled="+strconv.FormatBool(*req.Enabled))
		p.writeJSON(w, map[string]any{"school": name, "anonymous": *req.Enabled})
		return
	case r.Method == http.MethodGet && len(parts) == 2 && parts[1] == "anon-profile":
		name := strings.TrimSpace(parts[0])
		if name == "" {
			p.writeJSON(w, map[string]any{"error": "school required"})
			return
		}
		payload, prof, err := p.store.GetSchoolAnonProfilePayload(name)
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		if prof == nil {
			p.writeJSON(w, map[string]any{"school": name, "profile": nil})
			return
		}
		p.writeJSON(w, map[string]any{
			"school": name,
			"profile": map[string]any{
				"loginOk":            prof.LoginOK,
				"detectedAt":         prof.DetectedAt.Unix(),
				"expiresAt":          prof.ExpiresAt.Unix(),
				"source":             prof.Source,
				"classesDisplayable": payload.ClassesDisplayable,
				"capabilities":       payload.Capabilities,
				"notes":              payload.Notes,
				"profileJson":        prof.ProfileJSON,
			},
		})
		return
	case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "anon-profile" && (r.URL.Query().Get("refresh") == "1" || strings.Contains(r.URL.RawQuery, "refresh")):
		name := strings.TrimSpace(parts[0])
		if name == "" {
			p.writeJSON(w, map[string]any{"error": "school required"})
			return
		}
		prof, err := p.RefreshSchoolAnonProfile(name)
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		payload, _, _ := p.store.GetSchoolAnonProfilePayload(name)
		p.audit(r, name, "school.anon_profile.refresh", name, "")
		p.writeJSON(w, map[string]any{
			"school": name,
			"profile": map[string]any{
				"loginOk":            prof.LoginOK,
				"detectedAt":         prof.DetectedAt.Unix(),
				"expiresAt":          prof.ExpiresAt.Unix(),
				"source":             prof.Source,
				"classesDisplayable": payload.ClassesDisplayable,
				"capabilities":       payload.Capabilities,
				"notes":              payload.Notes,
			},
		})
		return
	case r.Method == http.MethodPost && len(parts) == 3 && parts[1] == "anon-profile" && parts[2] == "refresh":
		name := strings.TrimSpace(parts[0])
		if name == "" {
			p.writeJSON(w, map[string]any{"error": "school required"})
			return
		}
		prof, err := p.RefreshSchoolAnonProfile(name)
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		payload, _, _ := p.store.GetSchoolAnonProfilePayload(name)
		p.audit(r, name, "school.anon_profile.refresh", name, "")
		p.writeJSON(w, map[string]any{
			"school": name,
			"profile": map[string]any{
				"loginOk":            prof.LoginOK,
				"detectedAt":         prof.DetectedAt.Unix(),
				"expiresAt":          prof.ExpiresAt.Unix(),
				"source":             prof.Source,
				"classesDisplayable": payload.ClassesDisplayable,
				"capabilities":       payload.Capabilities,
				"notes":              payload.Notes,
			},
		})
		return
	case (r.Method == http.MethodPost || r.Method == http.MethodPut) && len(parts) == 3 && parts[1] == "anon-profile" && parts[2] == "override":
		name := strings.TrimSpace(parts[0])
		if name == "" {
			p.writeJSON(w, map[string]any{"error": "school required"})
			return
		}
		var req struct {
			LoginOK            *bool           `json:"loginOk"`
			ClassesDisplayable []int64         `json:"classesDisplayable"`
			Capabilities       map[string]bool `json:"capabilities"`
			Notes              *string         `json:"notes"`
			ProfileJSON        *string         `json:"profileJson"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			p.writeJSON(w, map[string]any{"error": "bad request"})
			return
		}
		existing, _ := p.store.GetSchoolAnonProfile(name)
		prof := &store.SchoolAnonProfile{School: name, Source: "override", DetectedAt: time.Now(), ExpiresAt: time.Now().Add(365 * 24 * time.Hour)}
		if existing != nil {
			prof.LoginOK = existing.LoginOK
		}
		if req.LoginOK != nil {
			prof.LoginOK = *req.LoginOK
		}
		payload := store.AnonProfilePayload{Capabilities: map[string]bool{}}
		if existing != nil && existing.ProfileJSON != "" {
			payload = store.DecodeAnonProfilePayload(existing.ProfileJSON)
		}
		if req.ProfileJSON != nil {
			payload = store.DecodeAnonProfilePayload(*req.ProfileJSON)
			prof.ProfileJSON = *req.ProfileJSON
		}
		if req.ClassesDisplayable != nil {
			payload.ClassesDisplayable = req.ClassesDisplayable
		}
		if req.Capabilities != nil {
			if payload.Capabilities == nil {
				payload.Capabilities = map[string]bool{}
			}
			for k, v := range req.Capabilities {
				payload.Capabilities[k] = v
			}
		}
		if req.Notes != nil {
			payload.Notes = *req.Notes
		}
		if req.ProfileJSON == nil {
			prof.ProfileJSON = store.EncodeAnonProfilePayload(payload)
		}
		if err := p.store.UpsertSchoolAnonProfile(prof); err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		p.audit(r, name, "school.anon_profile.override", name, "")
		p.writeJSON(w, map[string]any{"school": name, "ok": true, "profile": map[string]any{
			"loginOk": prof.LoginOK, "source": prof.Source, "capabilities": payload.Capabilities, "classesDisplayable": payload.ClassesDisplayable, "notes": payload.Notes,
		}})
		return
	case r.Method == http.MethodDelete && len(parts) == 1:
		name := strings.TrimSpace(parts[0])
		if name == "" {
			p.writeJSON(w, map[string]any{"error": "school required"})
			return
		}
		if name == p.opts.School {
			p.writeJSON(w, map[string]any{"error": "cannot delete the configured default school"})
			return
		}
		if err := p.store.DeleteSchool(name); err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		p.forgetSchool(name)
		p.audit(r, name, "school.delete", name, "")
		p.writeJSON(w, map[string]any{"school": name, "deleted": true})
		return
	case r.Method == http.MethodPost && len(parts) == 1:
		_ = p.store.UpsertSchool(parts[0])
		p.stateFor(parts[0])
		p.audit(r, parts[0], "school.register", parts[0], "")
		p.writeJSON(w, map[string]any{"school": parts[0], "registered": true})
		return
	}
	schools, err := p.store.ListSchools()
	if err != nil {
		p.writeJSON(w, map[string]any{"error": "store error"})
		return
	}
	allowNew, err := p.store.AllowNewSchools()
	if err != nil {
		p.writeJSON(w, map[string]any{"error": "store error"})
		return
	}
	type schoolOut struct {
		Name      string `json:"name"`
		AddedAt   int64  `json:"addedAt"`
		LastSeen  int64  `json:"lastSeen"`
		Blocked   bool   `json:"blocked"`
		Anonymous bool   `json:"anonymous"`
	}
	out := make([]schoolOut, 0, len(schools))
	for _, sc := range schools {
		out = append(out, schoolOut{sc.Name, sc.AddedAt.Unix(), sc.LastSeen.Unix(), sc.Blocked, sc.Anonymous})
	}
	p.writeJSON(w, map[string]any{"schools": out, "allowNew": allowNew, "defaultSchool": p.opts.School})
}

// adminAnonymous serves the deployment-wide anonymous capability matrix.
// GET reports it; POST /admin/anonymous/{feature} flips one entry. The login
// switch stays per school and lives under /admin/schools/{school}/anonymous,
// because it is the one capability that can sensibly differ per school.
func (p *Proxy) adminAnonymous(w http.ResponseWriter, r *http.Request, parts []string) {
	switch {
	case r.Method == http.MethodGet && len(parts) == 0:
		features, err := p.store.AnonymousFeatures()
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		p.writeJSON(w, map[string]any{"features": features})
		return
	case r.Method == http.MethodPost && len(parts) == 1:
		feature := strings.TrimSpace(parts[0])
		if feature == "" {
			p.writeJSON(w, map[string]any{"error": "feature required"})
			return
		}
		var req struct {
			Enabled *bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Enabled == nil {
			p.writeJSON(w, map[string]any{"error": "enabled required"})
			return
		}
		if err := p.store.SetAnonymousFeature(feature, *req.Enabled); err != nil {
			if errors.Is(err, store.ErrUnknownAnonymousFeature) {
				p.writeJSON(w, map[string]any{"error": "unknown anonymous feature"})
				return
			}
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		p.audit(r, "", "anonymous.feature", feature, "enabled="+strconv.FormatBool(*req.Enabled))
		p.writeJSON(w, map[string]any{"feature": feature, "enabled": *req.Enabled})
		return
	}
	p.writeJSON(w, map[string]any{"error": "not found"})
}

func (p *Proxy) adminWebhooks(w http.ResponseWriter, r *http.Request, parts []string) {
	if r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "test" {
		// POST /admin/webhooks/{id}/test
		id, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil || id <= 0 {
			p.writeJSON(w, map[string]any{"error": "bad id"})
			return
		}
		hooks, err := p.store.ListWebhooks("")
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		for _, h := range hooks {
			if h.ID != id {
				continue
			}
			status, err := p.testWebhook(h)
			if err != nil {
				p.writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
			p.audit(r, h.School, "webhook.test", strconv.FormatInt(id, 10), "")
			p.writeJSON(w, map[string]any{"ok": true, "status": status})
			return
		}
		p.writeJSON(w, map[string]any{"error": "not found"})
		return
	}
	if r.Method == http.MethodPost {
		var req struct {
			School      string `json:"school"`
			ElementType string `json:"elementType"`
			ElementID   int64  `json:"elementId"`
			ClassID     int64  `json:"classId"` // legacy class target
			URL         string `json:"url"`
			Secret      string `json:"secret"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			p.writeJSON(w, map[string]any{"error": "bad request"})
			return
		}
		if req.School == "" {
			req.School = p.opts.School
		}
		if req.URL == "" {
			p.writeJSON(w, map[string]any{"error": "url required"})
			return
		}
		if err := validateTargetURL(req.URL, allowPrivateTargets); err != nil {
			p.writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		wh := &store.Webhook{
			School: req.School, URL: req.URL, Secret: req.Secret,
			Enabled: true, CreatedBy: "admin",
		}
		if req.ElementType != "" {
			wh.ElementType, wh.ElementID = req.ElementType, req.ElementID
		} else {
			wh.ClassID = req.ClassID
		}
		id, err := p.store.AddWebhook(wh)
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		p.audit(r, wh.School, "webhook.create", strconv.FormatInt(id, 10), auditURLHost(wh.URL))
		p.writeJSON(w, map[string]any{"id": id})
		return
	}
	if r.Method == http.MethodDelete && len(parts) == 1 {
		id, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "bad id"})
			return
		}
		n, err := p.store.DeleteWebhook(id)
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		if n > 0 {
			p.audit(r, "", "webhook.delete", strconv.FormatInt(id, 10), "")
		}
		p.writeJSON(w, map[string]any{"deleted": n > 0})
		return
	}
	// GET
	school := ""
	if len(parts) == 1 && parts[0] != "" {
		school = parts[0]
	}
	hooks, err := p.store.ListWebhooks(school)
	if err != nil {
		p.writeJSON(w, map[string]any{"error": "store error"})
		return
	}
	type hookOut struct {
		ID          int64  `json:"id"`
		School      string `json:"school"`
		ElementType string `json:"elementType"`
		ElementID   int64  `json:"elementId"`
		Name        string `json:"name"`
		ClassID     int64  `json:"classId"`
		URL         string `json:"url"`
		CreatedBy   string `json:"createdBy"`
	}
	out := make([]hookOut, 0, len(hooks))
	for _, h := range hooks {
		et, eid := h.Target()
		out = append(out, hookOut{
			ID: h.ID, School: h.School, ElementType: et, ElementID: eid,
			Name: p.elementTargetName(h.School, et, eid), ClassID: h.ClassID,
			URL: h.URL, CreatedBy: h.CreatedBy,
		})
	}
	p.writeJSON(w, map[string]any{"webhooks": out})
}

// adminOutbox reports the delivery backlog and the destinations that gave up.
//
// GET /admin/outbox            counts by state plus the most recent dead rows
// GET /admin/outbox?state=dead the exhausted deliveries only
//
// Dead rows are kept precisely so this is answerable without opening the
// database: they name the school, class, destination and the error, which is
// what tells an operator whether the fix is the URL or the receiver being down.
func (p *Proxy) adminOutbox(w http.ResponseWriter, r *http.Request) {
	stats, err := p.store.OutboxStats()
	if err != nil {
		p.writeJSON(w, map[string]any{"error": "store error"})
		return
	}
	state := r.URL.Query().Get("state")
	limit := 25

	switch state {
	case "dead":
		dead, err := p.store.RecentDeadOutbox(limit)
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		p.writeJSON(w, map[string]any{"dead": stats.Dead, "failed": deadSummaries(dead)})
	case "sent":
		sent, err := p.store.RecentSentOutbox(limit)
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		p.writeJSON(w, map[string]any{"sent": stats.Sent, "delivered": deadSummaries(sent)})
	default:
		dead, err := p.store.RecentDeadOutbox(limit)
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		sent, err := p.store.RecentSentOutbox(limit)
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		p.writeJSON(w, map[string]any{
			"pending":   stats.Pending,
			"sending":   stats.Sending,
			"dead":      stats.Dead,
			"sent":      stats.Sent,
			"failed":    deadSummaries(dead),
			"delivered": deadSummaries(sent),
		})
	}
}

// deadSummary describes one exhausted delivery without its payload, which would
// be a large blob of timetable data nobody needs to read to diagnose a failure.
type deadSummary struct {
	School   string `json:"school"`
	ClassID  int64  `json:"classId"`
	Version  int64  `json:"version"`
	Dest     string `json:"dest"`
	DestID   int64  `json:"destId"`
	Attempts int    `json:"attempts"`
	LastErr  string `json:"lastError"`
	Created  int64  `json:"createdAt"`
}

func deadSummaries(rows []store.OutboxRow) []deadSummary {
	out := make([]deadSummary, 0, len(rows))
	for _, row := range rows {
		out = append(out, deadSummary{
			School: row.School, ClassID: row.ClassID, Version: row.Version,
			Dest: row.Dest, DestID: row.DestID, Attempts: row.Attempts,
			LastErr: row.LastErr, Created: row.Created.Unix(),
		})
	}
	return out
}

func (p *Proxy) adminNtfy(w http.ResponseWriter, r *http.Request, parts []string) {
	if r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "test" {
		// POST /admin/ntfy/{id}/test
		id, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil || id <= 0 {
			p.writeJSON(w, map[string]any{"error": "bad id"})
			return
		}
		topics, err := p.store.ListNtfyTopics("")
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		for _, t := range topics {
			if t.ID != id {
				continue
			}
			status, err := p.testNtfy(t)
			if err != nil {
				p.writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
			p.audit(r, t.School, "ntfy.test", strconv.FormatInt(id, 10), "")
			p.writeJSON(w, map[string]any{
				"ok": true, "status": status,
				"topic": t.Topic, "server": ntfyBaseURL(t),
			})
			return
		}
		p.writeJSON(w, map[string]any{"error": "not found"})
		return
	}
	if r.Method == http.MethodPost {
		var req struct {
			School      string `json:"school"`
			ElementType string `json:"elementType"`
			ElementID   int64  `json:"elementId"`
			ClassID     int64  `json:"classId"` // legacy class target
			Topic       string `json:"topic"`
			BaseURL     string `json:"baseUrl"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			p.writeJSON(w, map[string]any{"error": "bad request"})
			return
		}
		if req.School == "" {
			req.School = p.opts.School
		}
		if req.Topic == "" {
			p.writeJSON(w, map[string]any{"error": "topic required"})
			return
		}
		baseURL := cleanNtfyBase(req.BaseURL)
		if baseURL != "" {
			if err := validateTargetURL(baseURL, allowPrivateTargets); err != nil {
				p.writeJSON(w, map[string]any{"error": err.Error()})
				return
			}
		}
		n := &store.NtfyTopic{
			School: req.School, Topic: req.Topic, BaseURL: baseURL,
			Enabled: true, CreatedBy: "admin",
		}
		if req.ElementType != "" {
			n.ElementType, n.ElementID = req.ElementType, req.ElementID
		} else {
			n.ClassID = req.ClassID
		}
		id, err := p.store.AddNtfyTopic(n)
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		p.audit(r, n.School, "ntfy.create", strconv.FormatInt(id, 10), ntfyBaseURL(n))
		p.writeJSON(w, map[string]any{"id": id})
		return
	}
	if r.Method == http.MethodDelete && len(parts) == 1 {
		id, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "bad id"})
			return
		}
		n, err := p.store.DeleteNtfyTopic(id)
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		if n > 0 {
			p.audit(r, "", "ntfy.delete", strconv.FormatInt(id, 10), "")
		}
		p.writeJSON(w, map[string]any{"deleted": n > 0})
		return
	}
	school := ""
	if len(parts) == 1 && parts[0] != "" {
		school = parts[0]
	}
	topics, err := p.store.ListNtfyTopics(school)
	if err != nil {
		p.writeJSON(w, map[string]any{"error": "store error"})
		return
	}
	type topicOut struct {
		ID          int64  `json:"id"`
		School      string `json:"school"`
		ElementType string `json:"elementType"`
		ElementID   int64  `json:"elementId"`
		Name        string `json:"name"`
		ClassID     int64  `json:"classId"`
		Topic       string `json:"topic"`
		BaseURL     string `json:"baseUrl"`
		CreatedBy   string `json:"createdBy"`
	}
	out := make([]topicOut, 0, len(topics))
	for _, n := range topics {
		et, eid := n.Target()
		out = append(out, topicOut{
			ID: n.ID, School: n.School, ElementType: et, ElementID: eid,
			Name: p.elementTargetName(n.School, et, eid), ClassID: n.ClassID,
			Topic: n.Topic, BaseURL: n.BaseURL, CreatedBy: n.CreatedBy,
		})
	}
	p.writeJSON(w, map[string]any{"topics": out})
}

// adminOutageNtfy manages ntfy subscriptions that report poll outages. It is a
// separate API from /admin/ntfy on purpose: these rows never take part in
// timetable change matching.
func (p *Proxy) adminOutageNtfy(w http.ResponseWriter, r *http.Request, parts []string) {
	if r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "test" {
		id, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil || id <= 0 {
			p.writeJSON(w, map[string]any{"error": "bad id"})
			return
		}
		o, err := p.store.OutageNtfyByID(id)
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		if o == nil {
			p.writeJSON(w, map[string]any{"error": "not found"})
			return
		}
		status, err := p.testOutageNtfy(o)
		if err != nil {
			p.writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		p.audit(r, o.School, "outageNtfy.test", strconv.FormatInt(id, 10), "")
		p.writeJSON(w, map[string]any{
			"ok": true, "status": status,
			"topic": o.Topic, "server": ntfyBaseURL(&store.NtfyTopic{BaseURL: o.BaseURL}),
			"scope": o.Scope(),
		})
		return
	}
	if r.Method == http.MethodPost && len(parts) == 0 {
		var req struct {
			School  string `json:"school"`
			Topic   string `json:"topic"`
			BaseURL string `json:"baseUrl"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			p.writeJSON(w, map[string]any{"error": "bad request"})
			return
		}
		req.School = strings.TrimSpace(req.School)
		req.Topic = strings.TrimSpace(req.Topic)
		if req.Topic == "" {
			p.writeJSON(w, map[string]any{"error": "topic required"})
			return
		}
		baseURL := cleanNtfyBase(req.BaseURL)
		if baseURL != "" {
			if err := validateTargetURL(baseURL, allowPrivateTargets); err != nil {
				p.writeJSON(w, map[string]any{"error": err.Error()})
				return
			}
		}
		id, err := p.store.AddOutageNtfy(&store.OutageNtfy{
			School: req.School, Topic: req.Topic, BaseURL: baseURL,
			Enabled: true, CreatedBy: "admin",
		})
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		p.audit(r, req.School, "outageNtfy.create", strconv.FormatInt(id, 10), scopeLabel(req.School))
		p.writeJSON(w, map[string]any{"id": id, "scope": scopeLabel(req.School)})
		return
	}
	if r.Method == http.MethodDelete && len(parts) == 1 {
		id, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil || id <= 0 {
			p.writeJSON(w, map[string]any{"error": "bad id"})
			return
		}
		n, err := p.store.DeleteOutageNtfy(id)
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		if n > 0 {
			p.audit(r, "", "outageNtfy.delete", strconv.FormatInt(id, 10), "")
		}
		p.writeJSON(w, map[string]any{"deleted": n > 0})
		return
	}
	if r.Method != http.MethodGet || len(parts) != 0 {
		p.writeJSON(w, map[string]any{"error": "not found"})
		return
	}
	subs, err := p.store.ListOutageNtfy()
	if err != nil {
		p.writeJSON(w, map[string]any{"error": "store error"})
		return
	}
	type outageOut struct {
		ID        int64  `json:"id"`
		School    string `json:"school"`
		Scope     string `json:"scope"`
		Topic     string `json:"topic"`
		BaseURL   string `json:"baseUrl"`
		Enabled   bool   `json:"enabled"`
		CreatedBy string `json:"createdBy"`
	}
	out := make([]outageOut, 0, len(subs))
	for _, o := range subs {
		out = append(out, outageOut{
			ID: o.ID, School: o.School, Scope: o.Scope(), Topic: o.Topic,
			BaseURL: o.BaseURL, Enabled: o.Enabled, CreatedBy: o.CreatedBy,
		})
	}
	p.writeJSON(w, map[string]any{"subscriptions": out})
}

func scopeLabel(school string) string {
	if school == "" {
		return "all"
	}
	return school
}

// reconProgress summarises, for one school, how much of the pooled class list
// the enumeration has covered and how stale the coverage is.
func (p *Proxy) reconProgress(school string) map[string]any {
	target := reconHorizon()
	refresh := p.reconRefreshDays()
	classes, _ := p.store.Pool(school)
	// The live registry is empty before this process's first sweep, so fill in
	// what the database recorded rather than reporting "never scanned".
	horizons := p.reconHorizons(school)
	rows := make([]map[string]any, 0, len(classes))
	scanned, stale := 0, 0
	oldest := ""
	for _, c := range classes {
		until := horizons[c.ID]
		if until == "" {
			until = "never"
		} else if !reconStale(until, target, refresh) {
			scanned++
		} else {
			stale++
		}
		if until != "never" && (oldest == "" || until < oldest) {
			oldest = until
		}
		rows = append(rows, map[string]any{"classId": c.ID, "scanUntil": until})
	}
	return map[string]any{
		"target":      target,
		"refreshDays": refresh,
		"classes":     len(classes),
		"scanned":     scanned,
		"stale":       stale,
		"oldestUntil": oldest,
		"perClass":    rows,
	}
}

func (p *Proxy) adminRecon(w http.ResponseWriter, r *http.Request, parts []string) {
	// POST /admin/recon/rescan [school] — forget the recorded scan horizons and
	// re-enumerate every pooled class from the year start. Matched before the
	// generic "parts[0] is a school" default below.
	if r.Method == http.MethodPost && len(parts) >= 1 && parts[0] == "rescan" {
		school := p.opts.School
		if len(parts) > 1 && parts[1] != "" {
			school = parts[1]
		}
		if err := p.store.ClearReconScan(school); err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		p.RequestRescan()
		ys, ye := schoolYearRange(time.Now())
		go p.StartRecon(school, ys, ye)
		p.audit(r, school, "recon.rescan", school, ys+".."+ye)
		p.writeJSON(w, map[string]any{"rescan": "started", "school": school})
		return
	}
	school := p.opts.School
	if len(parts) == 1 && parts[0] != "" {
		school = parts[0]
	}
	st := p.stateFor(school)
	st.recon.snapshot()
	p.writeJSON(w, map[string]any{
		"school":   school,
		"teachers": len(st.recon.teachers),
		"rooms":    len(st.recon.rooms),
		"subjects": len(st.recon.subjects),
		"counts":   st.recon.counts(),
		"scan":     p.reconProgress(school),
	})
}

// adminUsers handles GET /admin/users and the POST mutations:
//
//	POST /admin/users            {username, password?, method?, admin?}
//	POST /admin/users/{u}/perm   {feature, allowed}
//	POST /admin/users/{u}/admin  {admin: true|false}
//	DELETE /admin/users/{u}
func (p *Proxy) adminUsers(w http.ResponseWriter, r *http.Request, parts []string) {
	if r.Method == http.MethodGet && len(parts) == 0 {
		users, err := p.store.ListUsers()
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		permRows, _ := p.store.AllPerms()
		perms := map[string]map[string]bool{}
		for _, pr := range permRows {
			if perms[pr.Username] == nil {
				perms[pr.Username] = map[string]bool{}
			}
			perms[pr.Username][pr.Feature] = pr.Allowed
		}
		out := make([]map[string]any, 0, len(users))
		for _, u := range users {
			secret, password, credentialErr := p.store.GetCredential(u.Username)
			links, _ := p.store.AccountLinks(u.School, u.Username)
			hasSharedSecret := secret != "" || (u.Method == "key" && u.Password != "")
			hasPassword := password != "" || (u.Method == "password" && u.Password != "")
			out = append(out, map[string]any{
				"username":              u.Username,
				"method":                u.Method,
				"personId":              u.PersonID,
				"personType":            u.PersonType,
				"classId":               u.ClassID,
				"className":             u.ClassName,
				"school":                u.School,
				"admin":                 u.Admin,
				"displayName":           u.DisplayName,
				"email":                 u.Email,
				"lastSeen":              u.LastSeen.Unix(),
				"replayable":            u.Password != "",
				"credentialStatusKnown": credentialErr == nil,
				"hasSharedSecret":       hasSharedSecret,
				"hasPassword":           hasPassword,
				"accountLinks":          links,
				"permissions":           perms[u.Username],
				"boosted":               p.isBoosted(u.Username),
				"editor":                p.isEditor(u.Username),
				"reconAllowed":          p.hasPerm(u.Username, store.FeatureRecon),
				"techniker":             p.hasPerm(u.Username, store.FeatureTechniker),
			})
		}
		p.writeJSON(w, map[string]any{"users": out})
		return
	}

	if r.Method == http.MethodPost && len(parts) == 0 {
		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Method   string `json:"method"`
			Admin    bool   `json:"admin"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" {
			p.writeJSON(w, map[string]any{"error": "bad request"})
			return
		}
		if req.Method == "" {
			req.Method = "password"
		}
		existing, _ := p.store.GetUser(req.Username)
		if existing == nil {
			existing = &store.User{Username: req.Username}
		}
		existing.Admin = req.Admin
		if req.Password != "" {
			existing.Password = req.Password
			existing.Method = req.Method
			// Keep the credential in the secrets table too: a pasted base32
			// value for a key account is the replay key, anything else is the
			// account password.
			if req.Method == "key" && isBase32Secret(req.Password) {
				_ = p.store.UpsertSecret(req.Username, req.Password)
			} else {
				_ = p.store.SetAccountPassword(req.Username, req.Password)
			}
		}
		if err := p.store.UpsertUser(existing); err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		// UpsertUser never removes the admin flag (a login that cannot know it
		// must not demote), so an explicit demotion has to go through SetAdmin.
		if err := p.store.SetAdmin(existing.Username, req.Admin); err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		if req.Admin {
			_ = p.store.SetPerm(req.Username, store.FeatureBoosted, true)
		}
		school := existing.School
		if school == "" {
			school = p.opts.School
		}
		p.audit(r, school, "user.upsert", existing.Username, "admin="+strconv.FormatBool(req.Admin))
		p.writeJSON(w, map[string]any{"ok": true})
		return
	}

	if len(parts) == 1 {
		username := parts[0]
		switch {
		case r.Method == http.MethodDelete:
			n, err := p.store.DeleteUser(username)
			if err != nil {
				p.writeJSON(w, map[string]any{"error": "store error"})
				return
			}
			p.audit(r, p.opts.School, "user.delete", username, "")
			p.writeJSON(w, map[string]any{"deleted": n > 0})
			return
		case r.Method == http.MethodPost:
			var req map[string]any
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				p.writeJSON(w, map[string]any{"error": "bad request"})
				return
			}
			if _, ok := req["feature"]; ok {
				feature, _ := req["feature"].(string)
				allowed, _ := req["allowed"].(bool)
				var err error
				switch feature {
				case store.FeatureBoosted:
					err = p.store.SetBoostedFlag(username, allowed)
				case store.FeatureEditor:
					err = p.store.SetPerm(username, store.FeatureEditor, allowed)
				case store.FeatureRecon:
					err = p.store.SetPerm(username, store.FeatureRecon, allowed)
				case store.FeatureTechniker:
					err = p.store.SetPerm(username, store.FeatureTechniker, allowed)
				default:
					err = p.store.SetPerm(username, feature, allowed)
				}
				if err != nil {
					p.writeJSON(w, map[string]any{"error": "store error"})
					return
				}
				p.audit(r, p.opts.School, "user.perm", username, feature+"="+strconv.FormatBool(allowed))
				p.writeJSON(w, map[string]any{"ok": true})
				return
			}
			if v, ok := req["admin"].(bool); ok {
				if err := p.store.SetAdmin(username, v); err != nil {
					p.writeJSON(w, map[string]any{"error": "store error"})
					return
				}
				p.audit(r, p.opts.School, "user.admin", username, "admin="+strconv.FormatBool(v))
				p.writeJSON(w, map[string]any{"ok": true})
				return
			}
			p.writeJSON(w, map[string]any{"error": "unknown mutation"})
			return
		}
	}
	p.writeJSON(w, map[string]any{"error": "not found"})
}

// adminRooms reads and writes the per-school room-name overrides used when
// synthesizing event elements. A GET returns the current map; a POST replaces it
// with the posted one. Names and ids are normalized (lowercased, positive ids
// only), and the normalized map is echoed back so the dashboard reflects exactly
// what was stored.
func (p *Proxy) adminRooms(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) != 0 {
		p.writeJSON(w, map[string]any{"error": "not found"})
		return
	}
	school := strings.TrimSpace(r.URL.Query().Get("school"))
	switch r.Method {
	case http.MethodGet:
		m := p.roomOverrides(school)
		if m == nil {
			m = map[string]int64{}
		}
		p.writeJSON(w, map[string]any{"school": school, "rooms": m, "texts": p.unmappedEventRooms(school, m)})
	case http.MethodPost:
		if school == "" {
			p.writeJSON(w, map[string]any{"error": "school is required"})
			return
		}
		var req struct {
			Rooms map[string]int64 `json:"rooms"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			p.writeJSON(w, map[string]any{"error": "bad request"})
			return
		}
		if err := p.setRoomOverrides(school, req.Rooms); err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		p.audit(r, school, "rooms.save", school, strconv.Itoa(len(req.Rooms))+" overrides")
		m := p.roomOverrides(school)
		if m == nil {
			m = map[string]int64{}
		}
		p.writeJSON(w, map[string]any{"ok": true, "school": school, "rooms": m, "texts": p.unmappedEventRooms(school, m)})
	default:
		p.writeJSON(w, map[string]any{"error": "not found"})
	}
}

// unmappedEventRooms lists the room texts that appear on a school's events but
// have no override yet, so the dashboard can offer them for pinning. A field that
// holds several names ("Aula, Turnhalle") is split the way the resolver splits it,
// and a name already mapped is dropped. Order is stable for the picker.
func (p *Proxy) unmappedEventRooms(school string, overrides map[string]int64) []string {
	if strings.TrimSpace(school) == "" {
		return nil
	}
	fields, err := p.store.EventRoomTexts(school)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		for _, name := range splitElementNames(field) {
			key := strings.ToLower(name)
			if seen[key] || overrides[key] != 0 {
				continue
			}
			seen[key] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// forbidden issues the standard 403 JSON body used across the REST API.
func (p *Proxy) forbidden(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = fmt.Fprint(w, `{"error":"forbidden"}`)
}
