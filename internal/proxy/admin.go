package proxy

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

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

	path := strings.TrimPrefix(r.URL.Path, "/admin")
	path = strings.Trim(path, "/")
	parts := strings.Split(path, "/")

	switch {
	case len(parts) == 1 && parts[0] == "status" && r.Method == http.MethodGet:
		p.adminStatus(w)
	case parts[0] == "users":
		p.adminUsers(w, r, parts[1:])
	case parts[0] == "perms" && r.Method == http.MethodGet:
		p.adminPerms(w)
	case parts[0] == "pool":
		p.adminPool(w, r, parts[1:])
	case parts[0] == "tokens":
		p.adminTokens(w, r, parts[1:])
	case parts[0] == "schools":
		p.adminSchools(w, r, parts[1:])
	case parts[0] == "search" && r.Method == http.MethodGet:
		p.adminSearch(w, r)
	case parts[0] == "webhooks":
		p.adminWebhooks(w, r, parts[1:])
	case parts[0] == "ntfy":
		p.adminNtfy(w, r, parts[1:])
	case parts[0] == "outbox" && r.Method == http.MethodGet:
		p.adminOutbox(w, r)
	case parts[0] == "recon":
		p.adminRecon(w, r, parts[1:])
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
		"version":    p.opts.School,
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
		"outbox": map[string]int{
			"pending": outbox.Pending,
			"sending": outbox.Sending,
			"dead":    outbox.Dead,
		},
		"global": global,
	})
}

func (p *Proxy) adminPerms(w http.ResponseWriter) {
	perms, err := p.store.AllPerms()
	if err != nil {
		p.writeJSON(w, map[string]any{"error": "store error"})
		return
	}
	p.writeJSON(w, map[string]any{"perms": perms})
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
	if len(out) > 60 {
		out = out[:60]
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
		tok, err := p.store.ClassTokenForElement(school, elType, req.ElementID)
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
		}
		p.writeCalendarResponse(w, r, tok)
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
		p.writeJSON(w, map[string]any{"updated": true, "days": req.Days})
		return
	}
	if r.Method == http.MethodDelete && len(parts) == 1 {
		n, err := p.store.DeleteClassToken(parts[0])
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
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
	if r.Method == http.MethodPost && len(parts) == 1 {
		_ = p.store.UpsertSchool(parts[0])
		p.stateFor(parts[0])
		p.writeJSON(w, map[string]any{"school": parts[0], "registered": true})
		return
	}
	schools, err := p.store.ListSchools()
	if err != nil {
		p.writeJSON(w, map[string]any{"error": "store error"})
		return
	}
	type schoolOut struct {
		Name     string `json:"name"`
		AddedAt  int64  `json:"addedAt"`
		LastSeen int64  `json:"lastSeen"`
	}
	out := make([]schoolOut, 0, len(schools))
	for _, sc := range schools {
		out = append(out, schoolOut{sc.Name, sc.AddedAt.Unix(), sc.LastSeen.Unix()})
	}
	p.writeJSON(w, map[string]any{"schools": out})
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
	if onlyDead := state == "dead"; !onlyDead {
		dead, err := p.store.RecentDeadOutbox(limit)
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		p.writeJSON(w, map[string]any{
			"pending": stats.Pending,
			"sending": stats.Sending,
			"dead":    stats.Dead,
			"failed":  deadSummaries(dead),
		})
		return
	}

	dead, err := p.store.RecentDeadOutbox(limit)
	if err != nil {
		p.writeJSON(w, map[string]any{"error": "store error"})
		return
	}
	p.writeJSON(w, map[string]any{"dead": stats.Dead, "failed": deadSummaries(dead)})
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
		n := &store.NtfyTopic{
			School: req.School, Topic: req.Topic, BaseURL: cleanNtfyBase(req.BaseURL),
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

// reconProgress summarises, for one school, how much of the pooled class list
// the enumeration has covered and how stale the coverage is.
func (p *Proxy) reconProgress(school string) map[string]any {
	target := reconHorizon()
	refresh := p.reconRefreshDays()
	classes, _ := p.store.Pool(school)
	horizons := p.stateFor(school).recon.scanProgress()
	// Before the first sweep of this process the in-memory map is empty, so
	// report what the database recorded rather than "never scanned".
	if stored, err := p.store.ReconScan(school); err == nil {
		for id, until := range stored {
			if _, ok := horizons[id]; !ok {
				horizons[id] = until
			}
		}
	}
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
			out = append(out, map[string]any{
				"username":     u.Username,
				"method":       u.Method,
				"personId":     u.PersonID,
				"personType":   u.PersonType,
				"classId":      u.ClassID,
				"className":    u.ClassName,
				"school":       u.School,
				"admin":        u.Admin,
				"displayName":  u.DisplayName,
				"email":        u.Email,
				"lastSeen":     u.LastSeen.Unix(),
				"permissions":  perms[u.Username],
				"boosted":      p.isBoosted(u.Username),
				"editor":       p.isEditor(u.Username),
				"reconAllowed": p.hasPerm(u.Username, store.FeatureRecon),
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
		if existing != nil {
			existing.Admin = req.Admin
			if req.Password != "" {
				existing.Password = req.Password
				existing.Method = req.Method
			}
			if err := p.store.UpsertUser(existing); err != nil {
				p.writeJSON(w, map[string]any{"error": "store error"})
				return
			}
		} else {
			if err := p.store.SetAdmin(req.Username, req.Admin); err != nil {
				p.writeJSON(w, map[string]any{"error": "store error"})
				return
			}
		}
		if req.Admin {
			_ = p.store.SetPerm(req.Username, store.FeatureBoosted, true)
		}
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
				default:
					err = p.store.SetPerm(username, feature, allowed)
				}
				if err != nil {
					p.writeJSON(w, map[string]any{"error": "store error"})
					return
				}
				p.writeJSON(w, map[string]any{"ok": true})
				return
			}
			if v, ok := req["admin"].(bool); ok {
				if err := p.store.SetAdmin(username, v); err != nil {
					p.writeJSON(w, map[string]any{"error": "store error"})
					return
				}
				p.writeJSON(w, map[string]any{"ok": true})
				return
			}
			p.writeJSON(w, map[string]any{"error": "unknown mutation"})
			return
		}
	}
	p.writeJSON(w, map[string]any{"error": "not found"})
}

// forbidden issues the standard 403 JSON body used across the REST API.
func (p *Proxy) forbidden(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = fmt.Fprint(w, `{"error":"forbidden"}`)
}
