package proxy

// Absence enrichment: derived metadata plus the student's own private note.
//
// Untis' absence records are rich but unreadable as they stand. They carry a
// class *id*, a reason *code* and a date range, and nothing that says which day
// of the week it was or which lesson it displaced. Everything here is added
// alongside the upstream fields, never in place of them.
//
// Two rules shape the whole file:
//
//   - No note is ever attached to somebody else's absence. The note is looked up
//     by (school, viewer, absence id), and an entry whose own `studentId`
//     disagrees with the viewer is left completely undecorated. That check is
//     what keeps the class-register path — which is replayed as a boosted
//     teacher — from carrying a student's private text into a teacher's view.
//   - Derived metadata degrades to absent, never to a guess. An absence that
//     overlaps no lesson, or more than one, has no single subject, and reporting
//     the first candidate anyway would put a wrong subject on a student's record.

import (
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"untis-proxy/internal/store"
)

// absenceCarryingMethod reports whether a response is the student's own absence
// list.
//
// getStudentAbsences2017 is the only such method, and it is deliberately not in
// classScopedMethods: it identifies the student from the auth block, so it is
// never replayed as a teacher. Enriching this method alone is what keeps notes
// off every editor and teacher surface — the guarantee is structural, there is no
// code path from a teacher's response to a note lookup.
func absenceCarryingMethod(method string) bool {
	return strings.EqualFold(method, "getStudentAbsences2017")
}

// absenceCandidateKeys are the result fields that have been observed to hold the
// absence list. The first is by far the most likely; the rest cost nothing to
// try and mean a wrong guess degrades to "no enrichment" instead of a broken
// feature.
var absenceCandidateKeys = []string{"absences", "absenceList", "absenceEntries", "entries", "days"}

// absenceDiagnostics throttles the "found no absence list" log, so a shape
// mismatch is reported once an hour per school rather than on every poll.
type absenceDiagnostics struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (d *absenceDiagnostics) note(school, msg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.last == nil {
		d.last = map[string]time.Time{}
	}
	now := time.Now()
	if prev, ok := d.last[school]; ok && now.Sub(prev) < time.Hour {
		return
	}
	d.last[school] = now
	log.Printf("[absence] %s: %s", school, msg)
}

// absenceTimeLayouts are the two shapes a wall-clock timestamp arrives in: the
// absence records use an RFC3339-ish local time, the pooled periods a plain
// space-separated one. Both are local wall clock, so they are compared as such
// and never converted through a zone.
var absenceTimeLayouts = []string{"2006-01-02T15:04", "2006-01-02 15:04"}

// parseAbsenceTime parses either timestamp layout, reporting whether it parsed.
func parseAbsenceTime(s string) (time.Time, bool) {
	for _, layout := range absenceTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// findAbsenceList locates the absence array in a decoded result.
//
// It tries the known keys first and then falls back to recognising the array by
// its contents, because the key name is not something this proxy can rely on
// being told. An entry is recognisable: it has a numeric id and a startDateTime.
func findAbsenceList(result jsonObject) ([]jsonObject, string) {
	for _, c := range absenceContainers(result) {
		for _, key := range absenceCandidateKeys {
			if arr, ok := c.obj[key].([]any); ok && arrayLooksLikeAbsences(arr) {
				return jsonArray(arr), c.label + key
			}
		}
		// Last resort within this container: any array that is recognisably absences.
		for _, key := range sortedObjectKeys(c.obj) {
			arr, ok := c.obj[key].([]any)
			if !ok || !arrayLooksLikeAbsences(arr) {
				continue
			}
			return jsonArray(arr), c.label + key
		}
	}
	return nil, ""
}

// absenceContainer is one object that might hold the absence list.
type absenceContainer struct {
	obj   jsonObject
	label string
}

// absenceContainers returns the result itself and everything exactly one level
// below it that is an object — reached either directly or as an element of a
// top-level array, which is the shape a per-day wrapper takes.
func absenceContainers(result jsonObject) []absenceContainer {
	out := []absenceContainer{{obj: result}}
	for _, key := range sortedObjectKeys(result) {
		switch v := result[key].(type) {
		case jsonObject:
			out = append(out, absenceContainer{obj: v, label: key + "."})
		case []any:
			for i, item := range v {
				if obj, ok := item.(jsonObject); ok {
					out = append(out, absenceContainer{obj: obj, label: key + "[" + itoa(i) + "]."})
				}
			}
		}
	}
	return out
}

// itoa avoids pulling strconv in for what is only a diagnostic label.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

// arrayLooksLikeAbsences reports whether an array's first element carries the
// fields every absence has. An empty array cannot be recognised, so an empty
// result is left alone — there is nothing to enrich either way.
func arrayLooksLikeAbsences(arr []any) bool {
	if len(arr) == 0 {
		return false
	}
	first, ok := arr[0].(jsonObject)
	if !ok {
		return false
	}
	if _, ok := jsonID(first["id"]); !ok {
		return false
	}
	_, hasStart := first["startDateTime"].(string)
	return hasStart
}

// sortedObjectKeys gives map iteration a stable order, so decoration and the shape
// fallback behave identically on every run.
func sortedObjectKeys(m jsonObject) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// enrichAbsenceResponse decorates a getStudentAbsences2017 response with derived
// metadata and the viewer's private note. Any problem returns the original bytes,
// because a student's absence list is more useful undecorated than wrong.
func (p *Proxy) enrichAbsenceResponse(raw []byte, school, viewer string) []byte {
	if viewer == "" || school == "" {
		return raw
	}
	user, err := p.store.GetUser(viewer)
	if err != nil {
		enrichmentFailed("absence "+viewer, err)
		return raw
	}
	if user == nil {
		return raw
	}
	notes, err := p.store.AbsenceNotes(school, viewer)
	if err != nil {
		enrichmentFailed("absence notes "+viewer, err)
		notes = map[int64]store.AbsenceNote{}
	}
	// With no notes at all and no way to read them, the request still deserves its
	// derived metadata, so a store failure downgrades the note, not the response.
	md := p.masterData(school)
	return enrichJSON(raw, func(result jsonObject) {
		list, _ := findAbsenceList(result)
		if list == nil {
			p.absenceDiag.note(school, "no absence list found in a getStudentAbsences2017 result "+
				"(result fields: "+strings.Join(sortedObjectKeys(result), ", ")+")")
			return
		}
		subjects := p.absenceSubjects(school, user, list)
		for _, abs := range list {
			key, ok := jsonID(abs["id"])
			if !ok {
				continue
			}
			if !absenceBelongsTo(abs, user) {
				// Somebody else's record: no note, no derived metadata, nothing.
				continue
			}
			if note, has := notes[key]; has {
				abs["note"] = note.Note
				abs["noteUpdatedAt"] = note.UpdatedAt.UTC().Format(time.RFC3339)
			}
			abs["derived"] = p.deriveAbsence(abs, md, subjects[key])
		}
	})
}

// absenceBelongsTo reports whether an absence entry is the viewer's own.
//
// `studentId` is on every upstream entry, so identity can be checked against the
// response itself rather than taken on trust from the request. When the field is
// missing the entry is left undecorated: the viewer is then only as trustworthy
// as the request that named them, and a private note deserves better.
func absenceBelongsTo(abs jsonObject, user *store.User) bool {
	raw, present := abs["studentId"]
	if !present {
		// No identity on the entry. A missing studentId means the only thing
		// connecting this record to the viewer is the request, and a private note
		// deserves better than that, so the entry is left undecorated.
		return false
	}
	sid, ok := jsonID(raw)
	if !ok {
		return false
	}
	return sid == user.PersonID
}

// absenceSubjects resolves, per absence key, the subject of the lesson it
// displaced — but only where that lesson is unambiguous.
//
// The snapshot is queried once per (class, date) rather than once per absence, so
// a term's worth of absences in one response costs a handful of queries.
func (p *Proxy) absenceSubjects(school string, user *store.User, list []jsonObject) map[int64]string {
	out := map[int64]string{}
	type group struct {
		classID int64
		date    string
	}
	// The key is read once, here, and carried alongside the entry. Re-reading it
	// per group would have to re-check a value already known to be an integer.
	type entry struct {
		key int64
		abs jsonObject
	}
	byGroup := map[group][]entry{}
	order := []group{}
	for _, abs := range list {
		key, ok := jsonID(abs["id"])
		if !ok {
			continue
		}
		start, _ := abs["startDateTime"].(string)
		t, ok := parseAbsenceTime(start)
		if !ok {
			continue
		}
		classID := user.ClassID
		if raw, ok := jsonID(abs["klasseId"]); ok && raw != 0 {
			classID = raw
		}
		if classID == 0 {
			continue
		}
		g := group{classID: classID, date: t.Format("2006-01-02")}
		if _, seen := byGroup[g]; !seen {
			order = append(order, g)
		}
		byGroup[g] = append(byGroup[g], entry{key: key, abs: abs})
	}
	for _, g := range order {
		periods, err := p.store.ClassPeriodsOnDate(school, g.classID, g.date)
		if err != nil {
			enrichmentFailed("absence periods "+g.date, err)
			continue
		}
		for _, e := range byGroup[g] {
			if subject, ok := soleOverlappingSubject(e.abs, periods); ok {
				out[e.key] = subject
			}
		}
	}
	return out
}

// soleOverlappingSubject returns the subject of the single lesson the absence
// overlaps.
//
// More than one overlap means the absence covered several lessons — the normal
// case for a whole-day absence — and there is no honest single answer, so the
// subject is left out instead of being guessed from the first period of the day.
func soleOverlappingSubject(abs jsonObject, periods []store.PeriodRow) (string, bool) {
	startStr, _ := abs["startDateTime"].(string)
	endStr, _ := abs["endDateTime"].(string)
	start, ok := parseAbsenceTime(startStr)
	if !ok {
		return "", false
	}
	// A missing, unparseable or inverted end leaves the window empty, and a
	// half-open empty window overlaps no period — so the absence reports no
	// subject. That is the honest answer for a malformed range; widening it to
	// the start instant would name a lesson the record does not claim.
	end := start
	if e, ok := parseAbsenceTime(endStr); ok && e.After(start) {
		end = e
	}
	var match *store.PeriodRow
	for i := range periods {
		pStart, ok1 := parseAbsenceTime(periods[i].Start)
		pEnd, ok2 := parseAbsenceTime(periods[i].End)
		if !ok1 || !ok2 {
			continue
		}
		if periods[i].Subject == "" {
			continue
		}
		// Half-open overlap, so an absence ending exactly when a lesson starts does
		// not claim that lesson.
		if pStart.Before(end) && start.Before(pEnd) {
			if match != nil {
				return "", false
			}
			match = &periods[i]
		}
	}
	if match == nil {
		return "", false
	}
	return match.Subject, true
}

// deriveAbsence builds the derived block: what the absence was for, on which day,
// and why. Every field is omitted rather than defaulted when it cannot be known,
// so the client can tell "not derivable" from "empty".
func (p *Proxy) deriveAbsence(abs jsonObject, md *masterDataCache, subject string) jsonObject {
	out := jsonObject{}
	if classID, ok := jsonID(abs["klasseId"]); ok && classID != 0 {
		out["classId"] = classID
		if md != nil {
			if name, ok := md.klassen[classID]; ok && name != "" {
				out["className"] = name
			}
		}
	}
	if startStr, ok := abs["startDateTime"].(string); ok {
		if t, ok := parseAbsenceTime(startStr); ok {
			out["weekday"] = t.Weekday().String()
			out["date"] = t.Format("2006-01-02")
		}
	}
	if subject != "" {
		out["subject"] = subject
	}
	if reasonID, ok := jsonID(abs["absenceReasonId"]); ok && md != nil {
		if text, ok := md.absenceReasons[reasonID]; ok && text != "" {
			out["reason"] = text
		}
	}
	// Nothing derivable is not worth an empty object on the wire.
	if len(out) == 0 {
		return nil
	}
	return out
}

// absenceNotePayload is one note on the wire.
type absenceNotePayload struct {
	AbsenceKey int64  `json:"absenceKey"`
	Note       string `json:"note"`
	UpdatedAt  string `json:"updatedAt"`
}

// maxAbsenceNoteLen bounds a note. The field is free text a student types on a
// phone; this is not a limit anyone should notice, but it keeps one row from
// holding megabytes and keeps the response honest about what it stores.
const maxAbsenceNoteLen = 2000

// handleAbsenceNotes reads and writes the viewer's own absence notes.
//
//	GET  /api/absence/notes[?school=<name>]      every note the student has written
//	POST /api/absence/notes   {"absenceKey":123,"note":"…"}
//	POST /api/absence/notes   {"absenceKey":123,"note":""}   clears the note
//
// The viewer comes from the session cookie and from nowhere else. A `username` in
// the body is ignored, so there is no way to read or write another student's
// notes.
func (p *Proxy) handleAbsenceNotes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodPost:
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	user := p.sessionUser(r)
	if user == nil {
		writeJSONError(w, http.StatusUnauthorized, "not logged in")
		return
	}
	school := r.URL.Query().Get("school")
	if school == "" {
		school = p.opts.School
	}

	if r.Method == http.MethodGet {
		notes, err := p.store.AbsenceNotes(school, user.Username)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "store error")
			return
		}
		out := make([]absenceNotePayload, 0, len(notes))
		for _, n := range notes {
			out = append(out, absenceNotePayload{
				AbsenceKey: n.Key,
				Note:       n.Note,
				UpdatedAt:  n.UpdatedAt.UTC().Format(time.RFC3339),
			})
		}
		// Sorted so the response is stable regardless of map order.
		sort.Slice(out, func(i, j int) bool { return out[i].AbsenceKey < out[j].AbsenceKey })
		p.writeJSON(w, map[string]any{"school": school, "notes": out})
		return
	}

	var req struct {
		AbsenceKey int64  `json:"absenceKey"`
		Note       string `json:"note"`
		// Accepted and ignored on purpose; see the doc comment.
		Username string `json:"username"`
	}
	if !decodeJSON(readBody(r), &req) {
		writeJSONError(w, http.StatusBadRequest, "bad request")
		return
	}
	if req.AbsenceKey <= 0 {
		writeJSONError(w, http.StatusBadRequest, "absenceKey is required")
		return
	}
	note := strings.TrimSpace(req.Note)
	if len(note) > maxAbsenceNoteLen {
		writeJSONError(w, http.StatusBadRequest, "note is too long")
		return
	}
	// An empty note means "no note", which is a delete rather than a stored blank.
	if note == "" {
		if err := p.store.ClearAbsenceNote(school, user.Username, req.AbsenceKey); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "store error")
			return
		}
		p.writeJSON(w, map[string]any{"absenceKey": req.AbsenceKey, "note": nil})
		return
	}
	at, err := p.store.SetAbsenceNote(school, user.Username, req.AbsenceKey, note)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store error")
		return
	}
	p.writeJSON(w, absenceNotePayload{
		AbsenceKey: req.AbsenceKey,
		Note:       note,
		UpdatedAt:  at.UTC().Format(time.RFC3339),
	})
}
