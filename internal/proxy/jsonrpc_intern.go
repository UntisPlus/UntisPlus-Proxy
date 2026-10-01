package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"untis-proxy/internal/store"
	"untis-proxy/internal/untis"
)

func (p *Proxy) handleJSONRPCIntern(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	log.Printf("[intern] %s?%s body=%s", r.URL.Path, r.URL.RawQuery, string(body))
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	_ = json.Unmarshal(body, &req)
	school := r.URL.Query().Get("school")
	if school == "" {
		school = p.opts.School
	}

	switch req.Method {
	case "getUserData2017":
		p.keyLogin(w, r, school, body)
	case "getTimetable2017":
		p.getTimetable2017(w, r, school, req.ID, body)
	case "getAppSharedSecret":
		b, status, _, err := p.untis.RawIntern(school, "", "getAppSharedSecret", body)
		if err != nil {
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		var req struct {
			Params []struct {
				UserName string `json:"userName"`
				Password string `json:"password"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		var resp struct {
			Result string `json:"result"`
		}
		_ = json.Unmarshal(b, &resp)
		secret := resp.Result
		if secret == "" && len(req.Params) > 0 {
			// Some apps send the shared secret itself as the "password" (the
			// real server rejects that with -8998). If the value is a valid
			// base32 secret, capture it so we can replay the account.
			if pw := req.Params[0].Password; isBase32Secret(pw) {
				secret = pw
			}
		}
		if secret != "" && len(req.Params) > 0 && req.Params[0].UserName != "" {
			p.secretsMu.Lock()
			p.secrets[strings.ToLower(req.Params[0].UserName)] = secret
			p.secretsMu.Unlock()
			_ = p.store.UpsertSecret(req.Params[0].UserName, secret)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(b)
	default:
		// Info-center style methods (absences, events, messages, ...) are
		// self-authenticating: BetterUntis includes an auth block per request.
		// Forward as-is so the real server validates the OTP and returns only
		// that user's own data (info center always uses the user's own account,
		// never a teacher account).
		if hasAuthBlock(body) {
			m := r.URL.Query().Get("m")
			if m == "" {
				m = req.Method
			}
			// Write methods are gated per editor flag; reading your own
			// absences is always allowed.
			username := extractAuthUser(body)
			if username != "" && isWriteMethod(m) && !p.isEditor(username) {
				p.writeJSONRPCError(w, req.ID, "method not allowed", -32601)
				return
			}
			// Class-scoped editor data exists upstream only for a teacher
			// identity, so editors run those requests as the boosted teacher
			// and keep everything else on their own account.
			out := body
			// Once the credentials have been swapped, the response is the
			// teacher's data rather than the requester's, so it must not be
			// decorated with the requester's personal fields. Homework flags are
			// exactly that kind of field.
			rewritten := false
			if owner := p.classScopedOwner(school, username, m); owner != nil {
				if swapped, err := p.rewriteAuthForOwner(school, body, owner); err == nil {
					out = swapped
					rewritten = true
					log.Printf("[intern] class-scoped %s: %s -> %s", m, username, owner.Username)
				} else {
					log.Printf("[intern] class-scoped %s: rewrite for %s failed: %v", m, owner.Username, err)
				}
			}
			b, status, _, err := p.untis.RawIntern(school, p.untis.SchoolCookie(school), m, out)
			if err != nil {
				p.writeJSONRPCError(w, req.ID, "upstream error", -1)
				return
			}
			if !rewritten && username != "" && homeworkCarryingMethod(m) {
				// username came from the request but was validated by upstream:
				// the response only arrived because those credentials are real.
				// An empty username means the request named nobody, and personal
				// fields are never attached to a response the proxy cannot attribute.
				b = p.enrichHomeWorkResponse(b, school, username)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(b)
			return
		}
		user := p.sessionUser(r)
		if user == nil {
			p.writeJSONRPCError(w, req.ID, "not logged in", -8520)
			return
		}
		m := r.URL.Query().Get("m")
		if m == "" {
			m = req.Method
		}
		// Write methods are gated per editor flag; reading your own
		// absences is always allowed.
		if isWriteMethod(m) && !p.isEditor(user.Username) {
			p.writeJSONRPCError(w, req.ID, "method not allowed", -32601)
			return
		}
		// Class-scoped editor data exists upstream only for a teacher
		// identity; editors run those requests as the boosted teacher.
		run := user
		rewritten := false
		if owner := p.classScopedOwner(school, user.Username, m); owner != nil {
			run = owner
			rewritten = true
		}
		b, status, err := p.escalatedIntern(school, run, m, body)
		if err != nil {
			p.writeJSONRPCError(w, req.ID, "upstream error", -1)
			return
		}
		if !rewritten && homeworkCarryingMethod(m) {
			// The session, not the request body, decided who this is.
			b = p.enrichHomeWorkResponse(b, school, user.Username)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(b)
	}
}

// homeworkCarryingMethod reports whether an upstream response can carry homework
// this proxy decorates.
//
// getHomeWork2017 is the list. getPeriodData2017 is per lesson and may include a
// homeWorks[] of its own, which is the path the .ics export already walks — so
// decorating both means a client sees the same flag whichever one it reads.
func homeworkCarryingMethod(method string) bool {
	switch strings.ToLower(method) {
	case "gethomework2017", "getperioddata2017":
		return true
	}
	return false
}

// hasAuthBlock reports whether a jsonrpc_intern.do request body carries the
// self-authentication block BetterUntis sends on every request.
func hasAuthBlock(body []byte) bool {
	var req struct {
		Params []struct {
			Auth struct {
				User string `json:"user"`
				OTP  any    `json:"otp"`
			} `json:"auth"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return false
	}
	for _, pr := range req.Params {
		if pr.Auth.User != "" {
			return true
		}
	}
	return false
}

// extractAuthUser extracts the username from the self-auth block in the
// request body (if present).
func extractAuthUser(body []byte) string {
	var req struct {
		Params []struct {
			Auth struct {
				User string `json:"user"`
				OTP  any    `json:"otp"`
			} `json:"auth"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return ""
	}
	for _, pr := range req.Params {
		if pr.Auth.User != "" {
			return pr.Auth.User
		}
	}
	return ""
}

func isBase32Secret(s string) bool {
	if len(s) < 16 || len(s)%8 == 4 || len(s)%8 == 1 || len(s)%8 == 5 {
		return false
	}
	for _, c := range s {
		if c == '=' {
			continue
		}
		if c < 'A' || c > 'Z' {
			if c < '2' || c > '7' {
				return false
			}
		}
	}
	return true
}

func (p *Proxy) keyLogin(w http.ResponseWriter, r *http.Request, school string, body []byte) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Params []struct {
			Auth struct {
				User       string          `json:"user"`
				OTP        json.RawMessage `json:"otp"`
				ClientTime int64           `json:"clientTime"`
				Key        string          `json:"key"`
			} `json:"auth"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Params) == 0 {
		p.writeJSONRPCError(w, req.ID, "no username specified", -8502)
		return
	}
	auth := req.Params[0].Auth
	if auth.User == "" {
		p.writeJSONRPCError(w, req.ID, "no username specified", -8502)
		return
	}
	otp := strings.TrimSpace(string(auth.OTP))
	if strings.HasPrefix(otp, "\"") {
		if s, err := strconv.Unquote(otp); err == nil {
			otp = s
		}
	}
	extra := map[string]any{}
	replayKey := auth.Key
	if replayKey == "" {
		replayKey = p.secrets[strings.ToLower(auth.User)]
	}
	if replayKey == "" {
		replayKey, _ = p.store.GetSecret(auth.User)
	}

	// Password fallback: a base32-shaped value in the "otp" field means the
	// submitter pasted the shared secret itself instead of a live 6-digit code
	// (the app setup flow always displays the secret). Use it as the replay key
	// and derive the current TOTP so the real server validates the login. The
	// secret is only persisted on success, so a stale/foreign paste never
	// clobbers a working stored key. Note isBase32Secret requires >=16 chars,
	// so a real 6-digit TOTP can never be mistaken for a secret.
	loginOTP := otp
	if isBase32Secret(otp) {
		replayKey = otp
		loginOTP = untis.TOTP(otp)
	}
	if replayKey != "" {
		extra["key"] = replayKey
	}
	cookie, realBody, err := p.untis.KeyLogin(school, auth.User, loginOTP, auth.ClientTime, extra)
	if err != nil {
		if ue, ok := err.(*untis.UpstreamError); ok {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(ue.Raw)
			return
		}
		p.writeJSONRPCError(w, req.ID, "bad credentials", -8504)
		return
	}

	// A login the upstream accepted is the strongest proof the presented replay
	// key (stored secret or pasted base32 secret) is valid, so record it for
	// future session escalation even if it wasn't captured before.
	if replayKey != "" {
		p.secretsMu.Lock()
		p.secrets[strings.ToLower(auth.User)] = replayKey
		p.secretsMu.Unlock()
		_ = p.store.UpsertSecret(auth.User, replayKey)
	}

	isAnon := auth.User == "#anonymous#"
	info, _ := p.untis.PersonInfo(school, cookie)
	if replayKey == "" {
		if existing, err := p.store.GetUser(auth.User); err == nil && existing != nil && existing.Password != "" {
			replayKey = existing.Password
		}
	}
	// Donation rule: everyone donates their class to the pool, so Pool()/
	// PoolContains()/OwnerForClass see the full set of classes users belong to.
	donateClassID := info.ClassID
	user := &store.User{
		Username:    auth.User,
		Password:    replayKey,
		Method:      "key",
		School:      school,
		PersonID:    info.PersonID,
		PersonType:  info.PersonType,
		ClassID:     donateClassID,
		ClassName:   p.classNameFor(school, cookie, info.ClassID),
		Email:       info.Email,
		DisplayName: info.DisplayName,
	}
	// Defensive identity preservation: if upstream failed to resolve a real
	// person identity for this login (PersonInfo hiccup, degraded response),
	// keep the previously known person/class instead of wiping the row to zeros
	// (which would kick the user's class out of the pool and break ownership).
	if existing, err := p.store.GetUser(auth.User); err == nil && existing != nil {
		if user.PersonID == 0 {
			user.PersonID = existing.PersonID
		}
		if user.PersonType == 0 {
			user.PersonType = existing.PersonType
		}
		if user.ClassID == 0 {
			user.ClassID = existing.ClassID
			user.ClassName = existing.ClassName
		}
		if user.Email == "" {
			user.Email = existing.Email
		}
		if user.DisplayName == "" {
			user.DisplayName = existing.DisplayName
		}
		// Admin and CreatedAt are assigned out-of-band (untisctl users admin,
		// -admin flag); upstream never reports them, so carry them over
		// wholesale instead of zeroing them on every login.
		user.Admin = existing.Admin
		user.CreatedAt = existing.CreatedAt
	}
	// The official anonymous login is only a session: don't persist
	// "#anonymous#" as a pool account or owner.
	if !isAnon {
		_ = p.store.UpsertUser(user)
		_ = p.store.Touch(auth.User)
		p.stateFor(school)
		if p.isNewSchool(school) {
			p.ensureReconScan(school)
		}
	}
	go p.untis.Logout(school, cookie)

	s := p.sessions.New(auth.User, info.ClassID)
	p.setSessionCookies(w, s.ID, school)
	rewritten := p.markPooledElementsDisplayable(school, realBody, auth.User)
	// Cache the raw body, not the rewritten one: the flags depend on the
	// requesting user's permissions and are recomputed on every request.
	p.cacheRawMasterData(school, realBody)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(rewritten)
}

// markPooledElementsDisplayable rewrites the masterData lists in a
// getUserData2017 response so pooled classes are displayable and teachers/rooms/
// subjects the requesting user is allowed to reconstruct are displayAllowed.
// Element reconstruction access is granted per type (TEACHER/ROOM/SUBJECT) via
// a per-user override or the global switch; un-granted types stay hidden.
func (p *Proxy) markPooledElementsDisplayable(school string, body []byte, username string) []byte {
	var resp struct {
		Result struct {
			MasterData struct {
				Klassen []struct {
					ID          int64 `json:"id"`
					Displayable bool  `json:"displayable"`
				} `json:"klassen"`
				Teachers []struct {
					ID             int64 `json:"id"`
					DisplayAllowed bool  `json:"displayAllowed"`
				} `json:"teachers"`
				Rooms []struct {
					ID             int64 `json:"id"`
					DisplayAllowed bool  `json:"displayAllowed"`
				} `json:"rooms"`
				Subjects []struct {
					ID             int64 `json:"id"`
					DisplayAllowed bool  `json:"displayAllowed"`
				} `json:"subjects"`
			} `json:"masterData"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.Result.MasterData.Klassen == nil {
		return body
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	pooled := map[int64]bool{}
	for _, k := range resp.Result.MasterData.Klassen {
		ok, err := p.store.PoolContains(school, k.ID)
		if err == nil && ok {
			pooled[k.ID] = true
		}
	}
	if len(pooled) == 0 {
		return body
	}
	result, _ := m["result"].(map[string]any)
	md, _ := result["masterData"].(map[string]any)

	// Persist element names so `untisctl calendar create --teacher|--room|--subject`
	// can fuzzy-lookup by name; the masterData on every login carries the full set.
	p.persistElementNames(school, md)

	setDisplayable := func(listKey string, want func(id int64) bool, flag string) {
		items, _ := md[listKey].([]any)
		for _, item := range items {
			ci, ok := item.(map[string]any)
			if !ok {
				continue
			}
			idf, ok := ci["id"].(float64)
			if !ok {
				continue
			}
			// Stamp the flag explicitly (true AND false): upstream often serves
			// every element with displayAllowed=true, and promote-only rewriting
			// would leave un-granted elements selectable.
			ci[flag] = want(int64(idf))
		}
	}
	// Teacher accounts (non-students) behave like the stock WebUntis API:
	// leave their teacher/room/subject displayAllowed flags untouched. For
	// students, reconstructed elements are only made displayable to users who
	// hold the recon permission; boosted users see every element.
	requester, _ := p.store.GetUser(username)
	isStudent := requester != nil && requester.PersonType == 5
	boosted := p.isBoosted(username)
	if !isStudent {
		out, err := json.Marshal(m)
		if err != nil {
			return body
		}
		return out
	}
	if boosted {
		// Boosted users can serve every class/teacher/room/subject raw via the
		// saved teacher accounts, so make all of them selectable.
		setDisplayable("klassen", func(id int64) bool { return true }, "displayable")
		setDisplayable("teachers", func(id int64) bool { return true }, "displayAllowed")
		setDisplayable("rooms", func(id int64) bool { return true }, "displayAllowed")
		setDisplayable("subjects", func(id int64) bool { return true }, "displayAllowed")
		out, err := json.Marshal(m)
		if err != nil {
			return body
		}
		return out
	}
	can, _ := p.store.ReconAccess(username, "TEACHER")
	// Everyone (Basic and Recon alike) can at least see the pooled classes.
	setDisplayable("klassen", func(id int64) bool { return pooled[id] }, "displayable")
	setDisplayable("teachers", func(id int64) bool {
		return can && p.stateFor(school).recon.has("TEACHER", id)
	}, "displayAllowed")
	setDisplayable("rooms", func(id int64) bool {
		return can && p.stateFor(school).recon.has("ROOM", id)
	}, "displayAllowed")
	setDisplayable("subjects", func(id int64) bool {
		return can && p.stateFor(school).recon.has("SUBJECT", id)
	}, "displayAllowed")

	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// persistElementNames records teacher/room/subject names into recon_elements so
// CLI fuzzy name lookup works. Runs opportunistically: failures are logged, not
// fatal, and the rewrite continues.
func (p *Proxy) persistElementNames(school string, md map[string]any) {
	if md == nil {
		return
	}
	for _, lt := range []struct{ key, typ string }{
		{"teachers", "TEACHER"}, {"rooms", "ROOM"}, {"subjects", "SUBJECT"},
	} {
		names := map[int64]string{}
		items, _ := md[lt.key].([]any)
		for _, it := range items {
			ci, ok := it.(map[string]any)
			if !ok {
				continue
			}
			idf, _ := ci["id"].(float64)
			if idf == 0 {
				continue
			}
			name, _ := ci["name"].(string)
			if name == "" {
				if n, ok := ci["longName"].(string); ok {
					name = n
				} else if fn, ok := ci["firstName"].(string); ok && fn != "" {
					ln, _ := ci["lastName"].(string)
					name = strings.TrimSpace(fn + " " + ln)
				}
			}
			names[int64(idf)] = name
		}
		if len(names) == 0 {
			continue
		}
		if err := p.store.UpsertElementNames(school, lt.typ, names); err != nil {
			log.Printf("[recon] persist %s names: %v", lt.typ, err)
		}
	}
}

func (p *Proxy) getTimetable2017(w http.ResponseWriter, r *http.Request, school string, id json.RawMessage, body []byte) {
	var req struct {
		Params []struct {
			ID        int64  `json:"id"`
			Type      string `json:"type"`
			StartDate string `json:"startDate"`
			EndDate   string `json:"endDate"`
			Auth      struct {
				User string `json:"user"`
			} `json:"auth"`
		} `json:"params"`
	}
	_ = json.Unmarshal(body, &req)
	if len(req.Params) == 0 {
		p.writeJSONRPCError(w, id, "no username specified", -8502)
		return
	}
	pr := req.Params[0]

	// BetterUntis self-authenticates every request via the auth block, so the
	// requester identity comes from there when present, else the session cookie.
	requesterName := pr.Auth.User
	if requesterName == "" {
		if u := p.sessionUser(r); u != nil {
			requesterName = u.Username
		}
	}
	if requesterName == "" {
		p.writeJSONRPCError(w, id, "not logged in", -8520)
		return
	}
	requester, err := p.store.GetUser(requesterName)
	if err != nil || requester == nil {
		p.writeJSONRPCError(w, id, "not logged in", -8520)
		return
	}

	// boost: serve ALL timetables (class/teacher/room/subject AND the user's own
	// personal student timetable) raw from the saved teacher accounts. This gives
	// boosted users teacher-grade visibility — e.g. unlimited future weeks on
	// their own timetable, which a student account caps at ~1 week. If no
	// teacher account is saved, fall back to the pool path below.
	if p.isBoosted(requesterName) {
		if p.serveRawFromBoostedSource(w, r, school, id, requesterName, body) {
			return
		}
	}

	// Teacher accounts (non-students) without the boosted flag behave exactly
	// like the stock WebUntis API: every timetable request (class, teacher,
	// room, subject, student) is forwarded raw through their own session, never
	// intercepted by the pool or the recon gate.
	if requester.PersonType != 5 {
		b, status, err := p.escalatedIntern(school, requester, "getTimetable2017", body)
		if err != nil {
			p.writeJSONRPCError(w, id, "no right for timetable", -8509)
			return
		}
		b = p.viewerBody(requesterName, b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(b)
		return
	}

	classID := int64(0)
	switch pr.Type {
	case "STUDENT":
		// A student's own timetable: forward untouched. The auth block is
		// validated by upstream directly, so even a session-only account
		// (no replayable key) can read its own timetable.
		if requester.PersonType == 5 && requester.PersonID == pr.ID {
			b, status, _, err := p.untis.RawIntern(school, p.untis.SchoolCookie(school), "getTimetable2017", body)
			if err != nil {
				p.writeJSONRPCError(w, id, "no right for timetable", -8509)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(b)
			return
		}
		// Another student's id: resolve it to their class and serve it via
		// the pool, which requires a replayable owner.
		u, err := p.store.UserByPersonID(pr.ID)
		if err == nil && u != nil {
			classID = u.ClassID
		}
	case "CLASS":
		classID = pr.ID
		// A student's own class: forward untouched. Upstream validates the
		// auth block directly, so a session-only account (no replayable
		// key) can still view its own class timetable.
		if requester.PersonType == 5 && pr.ID == requester.ClassID {
			b, status, _, err := p.untis.RawIntern(school, p.untis.SchoolCookie(school), "getTimetable2017", body)
			if err != nil {
				p.writeJSONRPCError(w, id, "no right for timetable", -8509)
				return
			}
			b = p.viewerBody(requesterName, b)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(b)
			return
		}
	case "TEACHER", "ROOM", "SUBJECT":
		p.serveElementTimetable(w, r, school, id, pr, body)
		return
	default:
		b, status, _, err := p.untis.RawIntern(school, p.untis.SchoolCookie(school), "getTimetable2017", body)
		if err != nil {
			p.writeJSONRPCError(w, id, "no right for timetable", -8509)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(b)
		return
	}

	if classID == 0 {
		p.writeJSONRPCError(w, id, "no right for timetable", -8509)
		return
	}
	ok, err := p.store.PoolContains(school, classID)
	if err != nil || !ok {
		p.writeJSONRPCError(w, id, "no right for timetable", -8509)
		return
	}
	owner, err := p.store.OwnerForClass(school, classID)
	if err != nil || owner == nil {
		p.writeJSONRPCError(w, id, "no right for timetable", -8509)
		return
	}

	key := fmt.Sprintf("2017|%d|%s|%s", classID, pr.StartDate, pr.EndDate)
	if false {
		if b, ok := p.tt.Get(key); ok {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(b)
			return
		}
	}

	newBody := body
	if requester.Username != owner.Username {
		newBody, err = p.rewriteAuthForOwner(school, body, owner)
		if err != nil {
			p.writeJSONRPCError(w, id, "no right for timetable", -8509)
			return
		}
	}
	b, status, _, err := p.untis.RawIntern(school, p.untis.SchoolCookie(school), "getTimetable2017", newBody)
	if err != nil {
		p.writeJSONRPCError(w, id, "no right for timetable", -8509)
		return
	}
	if requester.Username != owner.Username {
		b = p.withRequesterMasterData(school, requester.Username, b)
	}
	b = p.viewerBody(requesterName, b)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// withRequesterMasterData swaps the forwarded body's result.masterData for the
// requester's own view. Forwarding under the class owner's auth means upstream
// returns the owner's masterData, so a recon user who opens a pooled class gets
// a body with every teacher/room/subject displayAllowed=false and the app
// replaces its working picker catalog with that empty one.
func (p *Proxy) withRequesterMasterData(school, username string, b []byte) []byte {
	md := p.rewrittenMasterData(school, username)
	if md == nil {
		return b
	}
	var doc map[string]any
	if json.Unmarshal(b, &doc) != nil {
		return b
	}
	result, _ := doc["result"].(map[string]any)
	if result == nil {
		return b
	}
	result["masterData"] = md
	out, err := json.Marshal(doc)
	if err != nil {
		return b
	}
	return out
}

// serveRawFromBoostedSource serves timetable requests (CLASS/STUDENT/TEACHER/
// ROOM/SUBJECT) raw from the saved teacher accounts when the requester is
// boosted. It picks the first available teacher source account and forwards the
// request with rewritten auth to that account.
func (p *Proxy) serveRawFromBoostedSource(w http.ResponseWriter, r *http.Request, school string, id json.RawMessage, requesterName string, body []byte) bool {
	owner := p.boostedSource(school)
	if owner == nil {
		return false
	}
	newBody, err := p.rewriteAuthForOwner(school, body, owner)
	if err != nil {
		p.writeJSONRPCError(w, id, "no right for timetable", -8509)
		return true
	}
	b, status, _, err := p.untis.RawIntern(school, p.untis.SchoolCookie(school), "getTimetable2017", newBody)
	if err != nil {
		p.writeJSONRPCError(w, id, "no right for timetable", -8509)
		return true
	}
	// The teacher account answers with its full capability set. Without the
	// editor flag the boosted user gets visibility, never write access.
	b = p.viewerBody(requesterName, b)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
	return true
}

// boostedSource returns the most recently active saved teacher account
// (non-student with a replayable credential), or nil if none is available.
func (p *Proxy) boostedSource(school string) *store.User {
	sources, err := p.store.BoostedSourceAccounts(school)
	if err != nil || len(sources) == 0 {
		return nil
	}
	return sources[0]
}

// serveElementTimetable answers a TEACHER/ROOM/SUBJECT getTimetable2017 request
// by reconstructing the element's schedule from the pooled classes' timetables.
// Only elements that appear in pooled data are served; everything else gets the
// same -8509 the real server would return.
func (p *Proxy) serveElementTimetable(w http.ResponseWriter, r *http.Request, school string, id json.RawMessage, pr struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	StartDate string `json:"startDate"`
	EndDate   string `json:"endDate"`
	Auth      struct {
		User string `json:"user"`
	} `json:"auth"`
}, body []byte) {
	requesterName := pr.Auth.User
	if requesterName == "" {
		if u := p.sessionUser(r); u != nil {
			requesterName = u.Username
		}
	}
	if requesterName == "" {
		p.writeJSONRPCError(w, id, "not logged in", -8520)
		return
	}
	requester, err := p.store.GetUser(requesterName)
	if err != nil || requester == nil {
		p.writeJSONRPCError(w, id, "not logged in", -8520)
		return
	}
	// A user's own teacher timetable (their own person id) is stock WebUntis
	// behavior: forward it raw through their own account, never gated by recon.
	if pr.Type == "TEACHER" && requester.PersonID == pr.ID {
		b, status, err := p.escalatedIntern(school, requester, "getTimetable2017", body)
		if err != nil {
			p.writeJSONRPCError(w, id, "no right for timetable", -8509)
			return
		}
		b = p.viewerBody(requesterName, b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(b)
		return
	}
	// Reconstructed teacher/room/subject timetables are gated per element type
	// (individual override or global switch); nothing is enabled by default.
	allowed, _ := p.store.ReconAccess(requester.Username, pr.Type)
	if !allowed {
		p.writeJSONRPCError(w, id, "no right for timetable", -8509)
		return
	}
	if pr.StartDate == "" || pr.EndDate == "" {
		p.writeJSONRPCError(w, id, "no right for timetable", -8509)
		return
	}
	if !p.stateFor(school).recon.has(pr.Type, pr.ID) {
		p.writeJSONRPCError(w, id, "no right for timetable", -8509)
		return
	}
	periods, err := p.elementPeriods(school, pr.Type, pr.ID, pr.StartDate, pr.EndDate)
	if err != nil {
		p.writeJSONRPCError(w, id, "no right for timetable", -8509)
		return
	}
	for _, pd := range periods {
		p.capsInPeriod(requesterName, pd)
	}
	out := map[string]any{
		"jsonrpc": "2.0",
		"id":      idVal(id),
		"result": map[string]any{
			"timetable": map[string]any{
				"displayableStartDate": pr.StartDate,
				"displayableEndDate":   pr.EndDate,
				"periods":              periods,
			},
		},
	}
	// The real server always includes result.masterData (with a fresh timeStamp)
	// in getTimetable2017 responses; the app uses it to detect that its cached
	// masterData is stale and refetch it. Without this the app keeps its old
	// cached copy where rooms/teachers have displayAllowed=false, so they never
	// appear. Embed the rewritten masterData with a bumped timestamp instead.
	if md := p.rewrittenMasterData(school, requesterName); md != nil {
		out["result"].(map[string]any)["masterData"] = md
	}
	w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(out)
	_, _ = w.Write(b)
}

// cacheRawMasterData stores the untouched getUserData2017 result body so
// displayAllowed can be recomputed per request.
func (p *Proxy) cacheRawMasterData(school string, body []byte) {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return
	}
	if result, _ := m["result"].(map[string]any); result != nil {
		if md, _ := result["masterData"].(map[string]any); md != nil {
			p.mdRawMu.Lock()
			if p.mdRaw == nil {
				p.mdRaw = map[string][]byte{}
			}
			p.mdRaw[school] = append([]byte(nil), body...)
			p.mdRawMu.Unlock()
			return
		}
	}
}

// rewrittenMasterData returns the cached masterData for a school with
// displayAllowed stamped for that specific user and a timeStamp bumped so
// clients treat it as newer than anything they cached before. Recomputing per
// request is what makes a granted or revoked recon permission take effect
// without a fresh login, and keeps a boosted user's flags out of everyone
// else's menus.
func (p *Proxy) rewrittenMasterData(school, username string) map[string]any {
	p.mdRawMu.Lock()
	raw := p.mdRaw[school]
	p.mdRawMu.Unlock()
	if len(raw) == 0 {
		return nil
	}
	if username != "" {
		// markPooledElementsDisplayable decodes its own copy, so the cached
		// raw body stays pristine for the next request.
		if rewritten := p.markPooledElementsDisplayable(school, raw, username); !bytes.Equal(rewritten, raw) {
			raw = rewritten
		}
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	result, _ := m["result"].(map[string]any)
	if result == nil {
		return nil
	}
	md, _ := result["masterData"].(map[string]any)
	if md == nil {
		return nil
	}
	md["timeStamp"] = time.Now().UnixMilli()
	return md
}

// rewriteAuthForOwner replaces the auth block in a getTimetable2017 request with
// a fresh OTP for the class owner, so the real server serves the class timetable.
func (p *Proxy) rewriteAuthForOwner(school string, body []byte, owner *store.User) ([]byte, error) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	params, ok := req["params"].([]any)
	if !ok || len(params) == 0 {
		return nil, fmt.Errorf("no params")
	}
	p0, ok := params[0].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("bad param")
	}

	var otp, secret string
	switch owner.Method {
	case "key":
		secret = owner.Password
		if secret == "" {
			secret, _ = p.store.GetSecret(owner.Username)
		}
	default:
		secBody, _ := json.Marshal(map[string]any{
			"id": "untis-proxy", "jsonrpc": "2.0", "method": "getAppSharedSecret",
			"params": []any{map[string]any{"userName": owner.Username, "password": owner.Password}},
		})
		b, _, _, err := p.untis.RawIntern(school, "", "getAppSharedSecret", secBody)
		if err != nil {
			return nil, err
		}
		var resp struct {
			Result string `json:"result"`
		}
		if err := json.Unmarshal(b, &resp); err != nil || resp.Result == "" {
			return nil, fmt.Errorf("no shared secret for %s", owner.Username)
		}
		secret = resp.Result
	}
	otp = untis.TOTP(secret)

	p0["auth"] = map[string]any{
		"user":       owner.Username,
		"otp":        otp,
		"clientTime": time.Now().UnixMilli(),
	}
	return json.Marshal(req)
}
