package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"untis-proxy/internal/store"
)

// icsEscape escapes a string for an iCalendar TEXT value.
func icsEscape(s string) string {
	r := strings.NewReplacer("\\", "\\\\", ",", "\\,", ";", "\\;", "\n", "\\n")
	return r.Replace(s)
}

// newCalendarToken generates a cryptographically random opaque token.
func newCalendarToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// calendarTokenMaxAge bounds how long a calendar subscription token stays
// valid. The token is a bearer credential with no other guard, so an operator
// may want it to age out. 0 (the default) preserves the long-lived behaviour a
// subscribed calendar expects: the token is good until it is revoked.
var calendarTokenMaxAge time.Duration

// SetCalendarTokenMaxAge sets the maximum age of a calendar subscription token.
// A non-positive value disables expiry.
func SetCalendarTokenMaxAge(d time.Duration) { calendarTokenMaxAge = d }

// tokenExpired reports whether a token has outlived the configured max age.
// With expiry disabled every token is valid.
func tokenExpired(tok *store.ClassToken) bool {
	if calendarTokenMaxAge <= 0 || tok == nil {
		return false
	}
	return time.Since(time.Unix(tok.CreatedAt, 0)) > calendarTokenMaxAge
}

// handleCalendarToken creates (or returns the existing) calendar subscription
// token. It supports class, personal/student, teacher, room, and subject
// timetables. It requires a valid session.
func (p *Proxy) handleCalendarToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	u := p.sessionUser(r)
	if u == nil {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}
	var req struct {
		School    string `json:"school"`
		ClassID   int64  `json:"classId"`
		PersonID  int64  `json:"personId"`
		Personal  bool   `json:"personal"`
		TeacherID int64  `json:"teacherId"`
		RoomID    int64  `json:"roomId"`
		SubjectID int64  `json:"subjectId"`
		Timezone  string `json:"timezone"`
		Days      int    `json:"days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
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

	// Count how many element targets are set (exactly one required).
	nTargets := 0
	if req.Personal || req.PersonID > 0 {
		nTargets++
	}
	if req.ClassID > 0 {
		nTargets++
	}
	if req.TeacherID > 0 {
		nTargets++
	}
	if req.RoomID > 0 {
		nTargets++
	}
	if req.SubjectID > 0 {
		nTargets++
	}
	if nTargets != 1 {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"exactly one of classId, personal/personId, teacherId, roomId, subjectId is required"}`))
		return
	}

	// ── personal / student ──
	if req.Personal || req.PersonID > 0 {
		pid := req.PersonID
		if pid == 0 {
			pid = u.PersonID
		}
		if pid <= 0 || u.PersonID != pid {
			p.forbidden(w)
			return
		}
		tok, err := p.store.ClassTokenForPerson(school, pid)
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		if tok == nil {
			now := time.Now().Unix()
			tok = &store.ClassToken{
				Token: newCalendarToken(), School: school,
				PersonID: pid, ElementType: "STUDENT", ElementID: pid,
				Timezone: tz, Days: days, CreatedAt: now, LastAccess: now,
				CreatedBy: u.Username,
			}
			if err := p.store.CreateClassToken(tok); err != nil {
				p.writeJSON(w, map[string]any{"error": "store error"})
				return
			}
		}
		p.writeCalendarResponse(w, r, tok)
		return
	}

	// ── Technik aggregate class ──
	// The synthetic Technik class is not in the pool, so it needs its own
	// element type. It is only visible (and only subscribable) to a techniker.
	if req.ClassID == technikClassID {
		if !p.hasTechniker(u.Username) {
			p.forbidden(w)
			return
		}
		tok, err := p.store.ClassTokenForElement(school, "TECHNIK", technikClassID)
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		if tok == nil {
			now := time.Now().Unix()
			tok = &store.ClassToken{
				Token: newCalendarToken(), School: school,
				ClassID: technikClassID, ElementType: "TECHNIK", ElementID: technikClassID,
				Timezone: tz, Days: days, CreatedAt: now, LastAccess: now,
				CreatedBy: u.Username,
			}
			if err := p.store.CreateClassToken(tok); err != nil {
				p.writeJSON(w, map[string]any{"error": "store error"})
				return
			}
		}
		p.writeCalendarResponse(w, r, tok)
		return
	}

	// ── class ──
	if req.ClassID > 0 {
		if !p.canMintClass(school, u, req.ClassID) {
			p.forbidden(w)
			return
		}
		tok, err := p.store.ClassTokenForClass(school, req.ClassID)
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		if tok == nil {
			now := time.Now().Unix()
			tok = &store.ClassToken{
				Token: newCalendarToken(), School: school,
				ClassID: req.ClassID, ElementType: "CLASS", ElementID: req.ClassID,
				Timezone: tz, Days: days, CreatedAt: now, LastAccess: now,
				CreatedBy: u.Username,
			}
			if err := p.store.CreateClassToken(tok); err != nil {
				p.writeJSON(w, map[string]any{"error": "store error"})
				return
			}
		}
		p.writeCalendarResponse(w, r, tok)
		return
	}

	// ── teacher / room / subject ──
	var elType string
	var elID int64
	switch {
	case req.TeacherID > 0:
		elType = "TEACHER"
		elID = req.TeacherID
	case req.RoomID > 0:
		elType = "ROOM"
		elID = req.RoomID
	case req.SubjectID > 0:
		elType = "SUBJECT"
		elID = req.SubjectID
	}

	// Access control mirrors the app's visibility: recon grants the
	// reconstructed elements, boosted grants everything raw, and a staff
	// (non-student) account keeps stock WebUntis visibility.
	if !p.canMintElement(u, elType) {
		p.forbidden(w)
		return
	}

	tok, err := p.store.ClassTokenForElement(school, elType, elID)
	if err != nil {
		p.writeJSON(w, map[string]any{"error": "store error"})
		return
	}
	if tok == nil {
		now := time.Now().Unix()
		tok = &store.ClassToken{
			Token: newCalendarToken(), School: school,
			ElementType: elType, ElementID: elID,
			Timezone: tz, Days: days, CreatedAt: now, LastAccess: now,
			CreatedBy: u.Username,
		}
		if err := p.store.CreateClassToken(tok); err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
	}
	p.writeCalendarResponse(w, r, tok)
}

// calendarTokenName resolves the display name of a token's target element.
func (p *Proxy) calendarTokenName(tok *store.ClassToken) string {
	if tok.ElementType == "TECHNIK" {
		return technikClassName
	}
	md := p.masterData(tok.School)
	switch tok.ElementType {
	case "CLASS":
		return elementName(md, "CLASS", tok.ElementID)
	case "STUDENT":
		if u, _ := p.store.UserByPersonID(tok.ElementID); u != nil && u.DisplayName != "" {
			return u.DisplayName
		}
	case "TEACHER", "ROOM", "SUBJECT":
		return elementName(md, tok.ElementType, tok.ElementID)
	}
	return ""
}

// calendarTokenView renders one token in the shape both the mint response and
// the self-service list share, so the two can never drift apart.
func (p *Proxy) calendarTokenView(r *http.Request, tok *store.ClassToken) map[string]any {
	resp := map[string]any{
		"type":      strings.ToLower(tok.ElementType),
		"id":        tok.ElementID,
		"name":      p.calendarTokenName(tok),
		"token":     tok.Token,
		"url":       p.calendarURL(r, tok.Token),
		"timezone":  tok.Timezone,
		"days":      tok.Days,
		"horizon":   time.Now().AddDate(0, 0, tok.Days).Format("2006-01-02"),
		"created":   tok.CreatedAt,
		"lastUsed":  tok.LastAccess,
		"createdBy": tok.CreatedBy,
	}
	if tok.ClassID > 0 {
		resp["classId"] = tok.ClassID
	}
	if tok.PersonID > 0 {
		resp["personId"] = tok.PersonID
	}
	return resp
}

// writeCalendarResponse writes the standard calendar token JSON response.
func (p *Proxy) writeCalendarResponse(w http.ResponseWriter, r *http.Request, tok *store.ClassToken) {
	p.writeJSON(w, p.calendarTokenView(r, tok))
}

// calendarOption is one subscribable timetable target, as listed by the options
// endpoint and picked by the app's "add calendar" dialog.
type calendarOption struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// canMintClass reports whether a user may mint a calendar token for a class.
// The synthetic Technik aggregate needs the techniker permission; staff
// (non-student) accounts keep stock WebUntis visibility; everyone else needs
// the class to be in the pool or the boosted flag.
func (p *Proxy) canMintClass(school string, u *store.User, classID int64) bool {
	if classID == technikClassID {
		return p.hasTechniker(u.Username)
	}
	if u.PersonType != 5 || p.isBoosted(u.Username) {
		return true
	}
	ok, _ := p.store.PoolContains(school, classID)
	return ok
}

// canMintElement reports whether a user may mint a calendar token for a
// teacher/room/subject: recon, boosted, or a staff (non-student) account.
func (p *Proxy) canMintElement(u *store.User, elType string) bool {
	if u.PersonType != 5 || p.isBoosted(u.Username) {
		return true
	}
	can, _ := p.store.ReconAccess(u.Username, elType)
	return can
}

// calendarOptions lists the calendar targets the user can see in the app. It
// mirrors the masterData display flags the proxy writes at login:
//   - everyone: the pooled classes, plus the personal timetable
//   - recon:    the teachers/rooms/subjects the recon scan actually enumerated
//   - boosted:  every class/teacher/room/subject
//   - techniker: the synthetic Technik class
//   - staff (non-student) accounts: stock visibility, i.e. every element
func (p *Proxy) calendarOptions(school string, u *store.User) map[string]any {
	var teachersMD, roomsMD, subjectsMD, klassenMD map[int64]string
	if md := p.masterData(school); md != nil {
		teachersMD, roomsMD, subjectsMD, klassenMD = md.teachers, md.rooms, md.subjects, md.klassen
	}
	boosted := p.isBoosted(u.Username)
	staff := u.PersonType != 5
	all := boosted || staff

	classSet := map[int64]string{}
	if pooled, err := p.store.Pool(school); err == nil {
		for _, c := range pooled {
			classSet[c.ID] = c.Name
		}
	}
	if all {
		for id, name := range klassenMD {
			if _, ok := classSet[id]; !ok || classSet[id] == "" {
				classSet[id] = name
			}
		}
	}

	teacherSet := map[int64]string{}
	roomSet := map[int64]string{}
	subjectSet := map[int64]string{}
	if all {
		teacherSet, roomSet, subjectSet = teachersMD, roomsMD, subjectsMD
	} else if can, _ := p.store.ReconAccess(u.Username, "TEACHER"); can {
		snap := p.stateFor(school).recon.snapshot()
		for _, id := range snap["TEACHER"] {
			if name := teachersMD[id]; name != "" {
				teacherSet[id] = name
			}
		}
		for _, id := range snap["ROOM"] {
			if name := roomsMD[id]; name != "" {
				roomSet[id] = name
			}
		}
		for _, id := range snap["SUBJECT"] {
			if name := subjectsMD[id]; name != "" {
				subjectSet[id] = name
			}
		}
	}

	resp := map[string]any{
		"school":  school,
		"class":   optionList(classSet),
		"teacher": optionList(teacherSet),
		"room":    optionList(roomSet),
		"subject": optionList(subjectSet),
	}
	if u.PersonID > 0 {
		resp["personal"] = map[string]any{"personId": u.PersonID, "name": u.DisplayName}
	}
	if p.hasTechniker(u.Username) {
		resp["technik"] = calendarOption{ID: technikClassID, Name: technikClassName}
	}
	return resp
}

// optionList flattens a collector map into a name-sorted slice, so the app's
// pickers render in a stable order.
func optionList(set map[int64]string) []calendarOption {
	out := make([]calendarOption, 0, len(set))
	for id, name := range set {
		out = append(out, calendarOption{ID: id, Name: name})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// requestSchool resolves the school a session request targets, matching every
// other session endpoint: an explicit ?school= wins, then the default.
func (p *Proxy) requestSchool(r *http.Request) string {
	if s := r.URL.Query().Get("school"); s != "" {
		return s
	}
	return p.opts.School
}

// handleCalendarOptions lists the calendar targets the signed-in user can
// subscribe to, mirroring what they can already see in the app.
func (p *Proxy) handleCalendarOptions(w http.ResponseWriter, r *http.Request) {
	u := p.sessionUser(r)
	if u == nil {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}
	p.writeJSON(w, p.calendarOptions(p.requestSchool(r), u))
}

// handleCalendarTokens lists only the tokens the requesting user created, so
// the self-service page is scoped to its owner.
func (p *Proxy) handleCalendarTokens(w http.ResponseWriter, r *http.Request) {
	u := p.sessionUser(r)
	if u == nil {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}
	toks, err := p.store.ListClassTokensByUser(u.Username)
	if err != nil {
		p.writeJSON(w, map[string]any{"error": "store error"})
		return
	}
	out := make([]map[string]any, 0, len(toks))
	for _, tok := range toks {
		out = append(out, p.calendarTokenView(r, tok))
	}
	p.writeJSON(w, out)
}

// calendarOwnedToken loads the token named in the request path and confirms the
// session user created it. A missing token and a foreign one are indistinguishable
// to the caller: neither is theirs to touch.
func (p *Proxy) calendarOwnedToken(r *http.Request, u *store.User) (*store.ClassToken, bool) {
	token := strings.TrimSuffix(r.PathValue("token"), ".ics")
	tok, err := p.store.ClassTokenByToken(token)
	if err != nil || tok == nil {
		return nil, false
	}
	if tok.CreatedBy != u.Username {
		return nil, false
	}
	return tok, true
}

// handleCalendarDelete revokes one of the caller's own calendar subscriptions.
func (p *Proxy) handleCalendarDelete(w http.ResponseWriter, r *http.Request) {
	u := p.sessionUser(r)
	if u == nil {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}
	tok, ok := p.calendarOwnedToken(r, u)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
		return
	}
	if _, err := p.store.DeleteClassToken(tok.Token); err != nil {
		p.writeJSON(w, map[string]any{"error": "store error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCalendarUpdate changes the lookahead window of one of the caller's own
// calendar subscriptions. Days are clamped exactly like mint.
func (p *Proxy) handleCalendarUpdate(w http.ResponseWriter, r *http.Request) {
	u := p.sessionUser(r)
	if u == nil {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}
	tok, ok := p.calendarOwnedToken(r, u)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
		return
	}
	var req struct {
		Days int `json:"days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	days := req.Days
	if days <= 0 {
		days = 30
	}
	if days > 365 {
		days = 365
	}
	if _, err := p.store.UpdateClassTokenDays(tok.Token, days); err != nil {
		p.writeJSON(w, map[string]any{"error": "store error"})
		return
	}
	tok.Days = days
	p.writeCalendarResponse(w, r, tok)
}

// handleCalendarICS serves the .ics subscription feed for a token. It is public
// by design (anyone with the opaque token can read the timetable), so the token
// must be treated as a password.
// feedTarget is one resolved calendar subscription: the element's display name
// plus the periods behind it. Both the .ics feed and the /week page are built
// from it, so they can never drift apart.
type feedTarget struct {
	School  string
	Name    string
	Token   string
	Periods []map[string]any
	Version int64
}

// feedLocation is the zone a feed's periods are expressed in: the token's, or the
// offset the fetched periods actually carry if the token names a zone this binary
// has no data for. An unreadable zone falls back to UTC rather than failing the
// feed — the events would land on the wrong wall clock, but the lessons are still
// correct and that is the lesser failure.
func feedLocation(tok *store.ClassToken, periods []map[string]any) *time.Location {
	if tok.Timezone != "" {
		if loc, err := time.LoadLocation(tok.Timezone); err == nil {
			return loc
		}
	}
	for _, pd := range periods {
		if t, err := parsePeriodTime(rawString(pd, "startDateTime")); err == nil {
			_, offset := t.Zone()
			return time.FixedZone("feed", offset)
		}
	}
	return time.UTC
}

// resolveFeed resolves a calendar token to its element and fetches the periods
// for [from, to]. Access is token-based, exactly like the .ics feed.
func (p *Proxy) resolveFeed(tok *store.ClassToken, from, to string) (feedTarget, error) {
	md := p.masterData(tok.School)
	out := feedTarget{School: tok.School, Token: tok.Token}
	var err error

	// Determine element type: prefer element_type (new tokens), fall back to
	// legacy class_id / person_id columns for old tokens.
	elType := tok.ElementType
	if elType == "" {
		if tok.PersonID > 0 {
			elType = "STUDENT"
		} else if tok.ClassID > 0 {
			elType = "CLASS"
		}
	}

	switch elType {
	case "STUDENT":
		personID := tok.ElementID
		if personID == 0 {
			personID = tok.PersonID
		}
		u, perr := p.store.UserByPersonID(personID)
		out.Name = "Mein Stundenplan"
		if perr == nil && u != nil {
			if u.DisplayName != "" {
				out.Name = u.DisplayName
			}
			out.Version = p.store.ClassVersion(tok.School, u.ClassID)
		}
		out.Periods, err = p.studentPeriods(tok.School, personID, from, to)
		// A student's own events ride along on their feed. This branch is the only
		// one that has a username, which is the whole reason a class, teacher, room
		// or subject feed cannot carry anyone's events: there is nowhere to read
		// them from. A lookup failure (no user row for this person) is not fatal —
		// the real periods still resolve.
		if perr == nil && u != nil && u.Username != "" {
			out.Periods = append(out.Periods,
				p.studentEventPeriods(tok.School, u.Username, from, to, feedLocation(tok, out.Periods))...)
		}
	case "CLASS":
		classID := tok.ElementID
		if classID == 0 {
			classID = tok.ClassID
		}
		out.Name = elementName(md, "CLASS", classID)
		if out.Name == "" {
			out.Name = fmt.Sprintf("class-%d", classID)
		}
		out.Periods, err = p.classPeriods(tok.School, classID, from, to)
		out.Version = p.store.ClassVersion(tok.School, classID)
	case "TECHNIK":
		out.Name = technikClassName
		// The aggregate spans every student's events, exactly like the app's
		// Technik class surface; the raw (unrewritten) masterData is enough to
		// resolve the room/teacher names the periods reference.
		out.Periods = p.technikClassPeriods(tok.School, from, to, p.schoolLocation(tok.School), p.rewrittenMasterData(tok.School, ""))
		out.Version = p.store.SchoolVersion(tok.School)
	case "TEACHER", "ROOM", "SUBJECT":
		out.Name = elementName(md, elType, tok.ElementID)
		if out.Name == "" {
			out.Name = fmt.Sprintf("%s-%d", strings.ToLower(elType), tok.ElementID)
		}
		out.Periods, err = p.fetchElementRaw(tok.School, elType, tok.ElementID, from, to)
		// These feeds span every pooled class, so the school-wide change
		// counter is the closest monotonic version available.
		out.Version = p.store.SchoolVersion(tok.School)
	default:
		return out, fmt.Errorf("token %s has unsupported element type %q", tok.Token, elType)
	}
	if err != nil {
		return out, err
	}
	if out.Version == 0 {
		out.Version = p.store.SchoolVersion(tok.School)
	}
	return out, nil
}

// icsFilename returns the download name advertised for a resolved feed.
func icsFilename(tok *store.ClassToken) string {
	elType := tok.ElementType
	if elType == "" {
		if tok.PersonID > 0 {
			elType = "STUDENT"
		} else if tok.ClassID > 0 {
			elType = "CLASS"
		}
	}
	switch elType {
	case "STUDENT":
		id := tok.ElementID
		if id == 0 {
			id = tok.PersonID
		}
		return fmt.Sprintf("untis-%d.ics", id)
	case "CLASS":
		id := tok.ElementID
		if id == 0 {
			id = tok.ClassID
		}
		return fmt.Sprintf("untis-%d.ics", id)
	case "TEACHER":
		return fmt.Sprintf("untis-teacher-%d.ics", tok.ElementID)
	case "ROOM":
		return fmt.Sprintf("untis-room-%d.ics", tok.ElementID)
	case "SUBJECT":
		return fmt.Sprintf("untis-subject-%d.ics", tok.ElementID)
	}
	return "untis.ics"
}

func (p *Proxy) handleCalendarICS(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSuffix(r.PathValue("token"), ".ics")
	tok, err := p.store.ClassTokenByToken(token)
	if err != nil || tok == nil || tokenExpired(tok) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	_ = p.store.TouchClassToken(token, time.Now().Unix())

	now := time.Now()
	lookahead := tok.Days
	if lookahead <= 0 {
		lookahead = 30
	}
	end := now.AddDate(0, 0, lookahead)
	tz := tok.Timezone
	if tz == "" {
		tz = "Europe/Berlin"
	}
	feed, err := p.resolveFeed(tok, now.Format("2006-01-02"), end.Format("2006-01-02"))
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"fetch failed"}`))
		return
	}
	md := p.masterData(tok.School)
	calName := feed.Name
	filename := icsFilename(tok)

	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\n")
	b.WriteString("VERSION:2.0\r\n")
	b.WriteString("PRODID:-//untis-api//Calendar//EN\r\n")
	b.WriteString("CALSCALE:GREGORIAN\r\n")
	b.WriteString("X-WR-CALNAME:" + icsEscape("Untis "+calName) + "\r\n")
	b.WriteString("X-WR-CALDESC:" + icsEscape("Untis timetable for "+calName) + "\r\n")
	for _, pd := range feed.Periods {
		// An event carries plain text where a real period carries element ids, so
		// it has its own renderer rather than being forced through buildICSVEVENT.
		if isCustomPeriod(pd) {
			b.WriteString(buildCustomICSVEVENT(pd, tz, now))
			continue
		}
		b.WriteString(buildICSVEVENT(pd, md, tz, feed.Version, now))
	}
	b.WriteString("END:VCALENDAR\r\n")

	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	_, _ = w.Write([]byte(b.String()))
}

// buildICSVEVENT renders one period as a VEVENT with full lesson details. seq is
// the feed's change counter and stamp the feed generation time: together they
// let calendar clients notice that an unchanged-UID event was moved.
func buildICSVEVENT(pd map[string]any, md *masterDataCache, tz string, seq int64, stamp time.Time) string {
	st, _ := pd["startDateTime"].(string)
	en, _ := pd["endDateTime"].(string)
	stT, err := parsePeriodTime(st)
	if err != nil {
		return ""
	}
	enT, err := parsePeriodTime(en)
	if err != nil {
		return ""
	}
	periodID, _ := pd["id"].(float64)

	// Collect all elements by type
	var subjects, teachers, rooms, classes []string
	var subjectID int64
	for _, el := range periodElementIDs(pd) {
		name := elementName(md, el.Type, el.ID)
		if name == "" {
			name = fmt.Sprintf("#%d", el.ID)
		}
		switch el.Type {
		case "SUBJECT":
			subjects = append(subjects, name)
			if subjectID == 0 {
				subjectID = el.ID
			}
		case "TEACHER":
			teachers = append(teachers, name)
		case "ROOM":
			rooms = append(rooms, name)
		case "CLASS":
			classes = append(classes, name)
		}
	}

	subject := strings.Join(subjects, ", ")
	if subject == "" {
		subject = textField(pd, "subject")
	}

	// Build summary: subject · teacher(s)
	summary := subject
	if len(teachers) > 0 {
		summary += " · " + strings.Join(teachers, ", ")
	}

	// Build LOCATION: all rooms
	location := strings.Join(rooms, ", ")

	// Build description with full lesson info
	var desc []string

	// Classes
	if len(classes) > 0 {
		desc = append(desc, "Klasse: "+strings.Join(classes, ", "))
	}

	// Teachers
	if len(teachers) > 0 {
		desc = append(desc, "Lehrer: "+strings.Join(teachers, ", "))
	}

	// Rooms (all)
	if len(rooms) > 0 {
		desc = append(desc, "Raum: "+strings.Join(rooms, ", "))
	}

	// Check for room change: orgId != id
	for _, el := range periodElementIDs(pd) {
		if el.Type == "ROOM" {
			els, _ := pd["elements"].([]any)
			for _, e := range els {
				em, _ := e.(map[string]any)
				if em["type"] == "ROOM" {
					idF, _ := em["id"].(float64)
					orgF, _ := em["orgId"].(float64)
					if int64(idF) == el.ID && int64(orgF) != int64(idF) && int64(orgF) != 0 {
						orgName := elementName(md, "ROOM", int64(orgF))
						if orgName == "" {
							orgName = fmt.Sprintf("#%d", int64(orgF))
						}
						desc = append(desc, "Raumänderung: "+orgName+" → "+strings.Join(rooms, ", "))
					}
				}
			}
		}
	}

	// Subject
	if subject != "" {
		desc = append(desc, "Fach: "+subject)
	}

	// Exam
	if exam, ok := pd["exam"].(map[string]any); ok && exam != nil {
		examType, _ := exam["examtype"].(string)
		examName, _ := exam["name"].(string)
		examText, _ := exam["text"].(string)
		examLabel := "Klausur"
		if examType != "" {
			examLabel = examType
		}
		examDesc := examLabel
		if examName != "" {
			examDesc += " (" + examName + ")"
		}
		if examText != "" {
			examDesc += ": " + examText
		}
		desc = append(desc, "⚠ "+examDesc)
	}

	// Lesson topic
	if t := textField(pd, "lesson"); t != "" {
		desc = append(desc, "Thema: "+t)
	}

	// Substitution
	if t := textField(pd, "substitution"); t != "" {
		desc = append(desc, "Vertretung: "+t)
	}

	// Info
	if t := textField(pd, "info"); t != "" {
		desc = append(desc, "Info: "+t)
	}

	// Homework
	if hwList, ok := pd["homeWorks"].([]any); ok && len(hwList) > 0 {
		for _, hw := range hwList {
			hwMap, ok := hw.(map[string]any)
			if !ok {
				continue
			}
			hwText, _ := hwMap["text"].(string)
			hwDue, _ := hwMap["dueDate"].(string)
			hwEntry := "Hausaufgabe"
			if hwDue != "" {
				hwEntry += " (bis " + hwDue + ")"
			}
			if hwText != "" {
				hwEntry += ": " + hwText
			}
			desc = append(desc, hwEntry)
		}
	}

	// Status flags (EXAM, CANCELLED, etc.)
	if isList, ok := pd["is"].([]any); ok {
		for _, v := range isList {
			if s, ok := v.(string); ok && s != "REGULAR" {
				desc = append(desc, "Status: "+s)
			}
		}
	}

	description := strings.Join(desc, "\n")

	var b strings.Builder
	b.WriteString("BEGIN:VEVENT\r\n")
	b.WriteString("DTSTAMP:" + stamp.UTC().Format("20060102T150405Z") + "\r\n")
	b.WriteString(fmt.Sprintf("UID:%d@untis-api\r\n", int64(periodID)))
	b.WriteString(fmt.Sprintf("SEQUENCE:%d\r\n", seq))
	b.WriteString(fmt.Sprintf("DTSTART;TZID=%s:%s\r\n", tz, stT.Format("20060102T150405")))
	b.WriteString(fmt.Sprintf("DTEND;TZID=%s:%s\r\n", tz, enT.Format("20060102T150405")))
	b.WriteString("SUMMARY:" + icsEscape(summary) + "\r\n")
	if location != "" {
		b.WriteString("LOCATION:" + icsEscape(location) + "\r\n")
	}
	if description != "" {
		b.WriteString("DESCRIPTION:" + icsEscape(description) + "\r\n")
	}
	b.WriteString("TRANSP:OPAQUE\r\n")
	b.WriteString("CLASS:PUBLIC\r\n")
	b.WriteString("END:VEVENT\r\n")
	return b.String()
}

// parsePeriodTime parses a WebUntis RFC3339-like period timestamp.
func parsePeriodTime(s string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02T15:04Z07:00", "2006-01-02T15:04:05Z07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse time %q", s)
}

// calendarURL builds the absolute .ics subscription URL for a token.
func (p *Proxy) calendarURL(r *http.Request, token string) string {
	scheme := "https"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = "localhost"
	}
	return scheme + "://" + host + "/api/calendar/" + token + ".ics"
}
