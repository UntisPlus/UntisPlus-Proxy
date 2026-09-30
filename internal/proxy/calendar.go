package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
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

	// ── class ──
	if req.ClassID > 0 {
		ok, err := p.store.PoolContains(school, req.ClassID)
		if err != nil || !ok {
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

	// Access control: must have recon or boosted for teacher/room/subject.
	hasRecon, _ := p.store.ReconAccess(u.Username, elType)
	hasBoosted, _ := p.store.BoostedAccess(u.Username)
	if !hasRecon && !hasBoosted {
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

// writeCalendarResponse writes the standard calendar token JSON response.
func (p *Proxy) writeCalendarResponse(w http.ResponseWriter, r *http.Request, tok *store.ClassToken) {
	md := p.masterData(tok.School)
	name := ""
	switch tok.ElementType {
	case "CLASS":
		name = elementName(md, "CLASS", tok.ElementID)
	case "STUDENT":
		u, _ := p.store.UserByPersonID(tok.ElementID)
		if u != nil && u.DisplayName != "" {
			name = u.DisplayName
		}
	case "TEACHER":
		name = elementName(md, "TEACHER", tok.ElementID)
	case "ROOM":
		name = elementName(md, "ROOM", tok.ElementID)
	case "SUBJECT":
		name = elementName(md, "SUBJECT", tok.ElementID)
	}
	resp := map[string]any{
		"type":      strings.ToLower(tok.ElementType),
		"id":        tok.ElementID,
		"name":      name,
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
	p.writeJSON(w, resp)
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
	if err != nil || tok == nil {
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
