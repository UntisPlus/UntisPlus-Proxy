package proxy

// The techniker's local absence register for hand-authored appointments.
//
// A proxy event is not a real Untis lesson, so it has no upstream ttId, no class
// register and no absence record to write. The official app still offers its
// absence editor on any period whose `can` list carries READ/WRITE_STUD_ABSENCE,
// and a techniker needs exactly that: a way to tick off who showed up to an
// appointment. Everything here answers the app's four calls for a synthetic
// period entirely from the proxy's own store and never lets them reach Untis:
//
//	getPeriodData2017          the roster and the recorded absences
//	createImmediateAbsence2017 mark a student absent
//	deleteAbsence2017          undo that
//	submitAbsencesChecked2017  record that the register was submitted
//
// The four are intercepted before the editor write gate, because a techniker
// writes a local note, not an Untis record, and therefore needs neither the
// editor flag nor a teacher account. A request for a real period — or from a
// user without the techniker permission — falls straight through to the existing
// path, so nothing about normal absence handling changes.

import (
	"encoding/json"
	"hash/fnv"
	"net/http"
	"strconv"
	"strings"

	"untis-proxy/internal/store"
)

// eventAbsenceIDSpace must match the store's own constant: it is the range the
// synthetic absence ids live in, checked here before a delete is treated as
// local.
const eventAbsenceIDSpace = -3_000_000_000

// syntheticPersonIDBase reserves a range for students whose account has no real
// Untis person id, so a local absence can still name them without colliding with
// a real person.
const syntheticPersonIDBase = -4_000_000_000

// isSyntheticPeriodID reports whether a ttId names a proxy event period.
func isSyntheticPeriodID(id int64) bool { return id < customEventIDSpace }

// isEventAbsenceID reports whether an id names a local event absence.
func isEventAbsenceID(id int64) bool {
	return id <= eventAbsenceIDSpace && id > eventAbsenceIDSpace-1_000_000_000
}

// sharedEventKey is everything a viewer sees about an event except who is
// assigned; two stored events with the same key are one appointment. It mirrors
// sharedPeriodKey, but is computed from the stored row so the register can be
// keyed without first building a period.
func sharedEventKey(ev store.StudentEvent) string {
	return strings.Join([]string{
		ev.Date,
		ev.StartTime,
		ev.EndTime,
		strings.TrimSpace(ev.Title),
		strings.TrimSpace(ev.Subject),
		strings.TrimSpace(ev.Room),
		strings.TrimSpace(ev.Teacher),
		strings.TrimSpace(ev.Description),
	}, "\x00")
}

// eventPerson is one student on an appointment's roster, resolved to the shape
// the app's Person model expects.
type eventPerson struct {
	ID        int64
	Username  string
	KlasseID  int64
	FirstName string
	LastName  string
}

// personJSON is the referencedStudents entry for this student.
func (e eventPerson) personJSON() map[string]any {
	return map[string]any{"id": e.ID, "firstName": e.FirstName, "lastName": e.LastName}
}

// eventAppointment is one appointment's roster plus its recorded absences.
type eventAppointment struct {
	Key      string
	Date     string
	Event    store.StudentEvent
	Roster   []eventPerson
	Absences []store.EventAbsence
	Checked  bool
}

// resolveAppointment turns one stored event into its whole appointment: the
// sibling event rows that share its key (the assigned students) and the local
// register recorded against it.
func (p *Proxy) resolveAppointment(school string, ev store.StudentEvent) eventAppointment {
	appt := eventAppointment{Key: sharedEventKey(ev), Date: ev.Date, Event: ev}
	siblings, err := p.store.AllStudentEventsForRange(school, ev.Date, ev.Date)
	if err != nil {
		enrichmentFailed("event appointment "+school, err)
		siblings = []store.StudentEvent{ev}
	}
	seen := map[int64]bool{}
	for _, s := range siblings {
		if sharedEventKey(s) != appt.Key || seen[s.ID] {
			continue
		}
		seen[s.ID] = true
		appt.Roster = append(appt.Roster, p.eventPersonFor(school, s.Username))
	}
	if len(appt.Roster) == 0 {
		appt.Roster = append(appt.Roster, p.eventPersonFor(school, ev.Username))
	}
	if rows, err := p.store.EventAbsences(school, appt.Key); err == nil {
		appt.Absences = rows
	} else {
		enrichmentFailed("event absences "+school, err)
	}
	if checked, err := p.store.EventAbsenceChecked(school, appt.Key); err == nil {
		appt.Checked = checked
	}
	return appt
}

// eventPersonFor resolves a stored (folded) event owner to the roster shape. A
// missing account degrades to the stored key and a synthesized person id, so a
// student who has never logged in is still listed rather than dropped.
func (p *Proxy) eventPersonFor(school, username string) eventPerson {
	person := eventPerson{Username: username, ID: syntheticPersonID(username)}
	if u, err := p.store.GetUserInSchool(school, username); err == nil && u != nil {
		if u.PersonID > 0 {
			person.ID = u.PersonID
		}
		person.KlasseID = u.ClassID
		first, last := splitDisplayName(u.DisplayName)
		person.FirstName, person.LastName = first, last
		return person
	}
	first, last := splitDisplayName(username)
	person.FirstName, person.LastName = first, last
	return person
}

// splitDisplayName splits a human name into the first name and everything else,
// so a three-part name keeps its surnames together. A single word becomes the
// first name and an empty last name rather than being duplicated.
func splitDisplayName(name string) (string, string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", ""
	}
	if i := strings.IndexByte(name, ' '); i >= 0 {
		return name[:i], strings.TrimSpace(name[i+1:])
	}
	return name, ""
}

// syntheticPersonID derives a stable negative id from a username, so the same
// student keeps the same roster id across requests.
func syntheticPersonID(username string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(strings.ToLower(strings.TrimSpace(username))))
	return syntheticPersonIDBase - int64(h.Sum64()%1_000_000_000)
}

// absenceIDFor derives the synthetic absence id the app will use to delete the
// row. It delegates to the store so the serving path and the lookup agree.
func absenceIDFor(appointmentKey, username string) int64 {
	return store.EventAbsenceID(appointmentKey, username)
}

// eventDateTime renders a stored date and "T14:00" clock as the wall-clock
// instant the app parses, falling back to another instant when the clock is
// missing.
func eventDateTime(date, clock, fallback string) string {
	clock = strings.TrimSpace(clock)
	if date == "" || clock == "" {
		return fallback
	}
	clock = strings.TrimPrefix(clock, "T")
	return date + "T" + clock + "Z"
}

// periodStartDateTime is the appointment's start instant, used as the fallback
// when a stored absence carries no clock.
func periodStartDateTime(ev store.StudentEvent) string {
	return ev.Date + "T" + strings.TrimSpace(ev.StartTime) + "Z"
}

func periodEndDateTime(ev store.StudentEvent) string {
	return ev.Date + "T" + strings.TrimSpace(ev.EndTime) + "Z"
}

// studentAbsenceJSON builds one StudentAbsence the app can render and delete.
func studentAbsenceJSON(person eventPerson, appt eventAppointment, row store.EventAbsence) map[string]any {
	start := eventDateTime(row.Date, row.StartTime, periodStartDateTime(appt.Event))
	end := eventDateTime(row.Date, row.EndTime, periodEndDateTime(appt.Event))
	return map[string]any{
		"id":              absenceIDFor(appt.Key, person.Username),
		"studentId":       person.ID,
		"klasseId":        person.KlasseID,
		"startDateTime":   start,
		"endDateTime":     end,
		"owner":           true,
		"excused":         false,
		"excuse":          nil,
		"absenceReasonId": 0,
		"absenceReason":   "",
		"text":            "",
	}
}

// eventPeriodDataFor builds the PeriodData the absent-register row reads: the
// appointment's roster, its recorded absences and whether the register was
// already submitted.
func (p *Proxy) eventPeriodDataFor(school, username string, periodID int64) (map[string]any, []eventPerson, bool) {
	ev, found, err := p.store.StudentEventByID(school, customEventIDSpace-periodID)
	if err != nil || !found {
		return emptyEventPeriodData(periodID), nil, false
	}
	appt := p.resolveAppointment(school, ev)
	byUsername := make(map[string]eventPerson, len(appt.Roster))
	studentIDs := make([]any, 0, len(appt.Roster))
	for _, person := range appt.Roster {
		byUsername[person.Username] = person
		studentIDs = append(studentIDs, person.ID)
	}
	absences := make([]any, 0, len(appt.Absences))
	for _, row := range appt.Absences {
		person, ok := byUsername[row.Username]
		if !ok {
			continue
		}
		absences = append(absences, studentAbsenceJSON(person, appt, row))
	}
	return eventPeriodData(periodID, eventTopicJSON(periodID, ev), appt.Checked, studentIDs, absences), appt.Roster, true
}

// eventTopicJSON is the lesson topic the app's detail view shows. The event title
// belongs here: the timetable cell carries the subject, so the topic line is
// where the appointment's actual name is read. Every field the app's LessonTopic
// model requires is present, or the client would drop the whole object.
func eventTopicJSON(periodID int64, ev store.StudentEvent) map[string]any {
	return map[string]any{
		"periodId":         periodID,
		"text":             strings.TrimSpace(ev.Title),
		"attachments":      []any{},
		"startDateTime":    periodStartDateTime(ev),
		"endDateTime":      periodEndDateTime(ev),
		"teachingMethodId": 0,
	}
}

// emptyEventPeriodData is a valid PeriodData for a period whose event could not
// be resolved, so the app still receives an entry instead of an absent one.
func emptyEventPeriodData(periodID int64) map[string]any {
	return eventPeriodData(periodID, nil, false, []any{}, []any{})
}

func eventPeriodData(periodID int64, topic map[string]any, checked bool, studentIDs, absences []any) map[string]any {
	return map[string]any{
		"ttId":                   periodID,
		"absenceChecked":         checked,
		"studentIds":             studentIDs,
		"absences":               absences,
		"classRegEvents":         nil,
		"exemptions":             nil,
		"prioritizedAttendances": nil,
		"text":                   nil,
		"topic":                  topic,
		"homeWorks":              []any{},
		"seatingPlan":            nil,
		"classRoles":             nil,
		"channel":                nil,
		"can": []any{
			readStudAbsenceCap, writeStudAbsenceCap, readLessonTopicCap,
			"READ_HOMEWORK", "READ_CLASSROLE", "READ_PERIODINFO",
		},
	}
}

// handleEventAbsenceCall intercepts the four absence-register calls for a
// techniker. It returns true only when it recognised the call as a proxy-event
// one and answered it; anything else falls through to the normal Untis path.
func (p *Proxy) handleEventAbsenceCall(w http.ResponseWriter, id json.RawMessage, school, method, username string, body []byte) bool {
	if username == "" || !p.hasTechniker(username) {
		return false
	}
	switch strings.ToLower(method) {
	case "getperioddata2017":
		return p.serveEventPeriodData(w, id, school, username, body)
	case "createimmediateabsence2017":
		return p.createEventAbsence(w, id, school, username, body)
	case "deleteabsence2017":
		return p.deleteEventAbsence(w, id, school, username, body)
	case "submitabsenceschecked2017":
		return p.submitEventAbsencesChecked(w, id, school, username, body)
	}
	return false
}

// serveEventPeriodData answers getPeriodData2017 for the synthetic ids in the
// request, forwarding any real ids to Untis and merging both halves. It reports
// false when the request carries no synthetic id, so a normal call is untouched.
func (p *Proxy) serveEventPeriodData(w http.ResponseWriter, id json.RawMessage, school, username string, body []byte) bool {
	ids, ok := parseTTIds(body)
	if !ok {
		return false
	}
	var synthetic, real []int64
	for _, v := range ids {
		if isSyntheticPeriodID(v) {
			synthetic = append(synthetic, v)
		} else {
			real = append(real, v)
		}
	}
	if len(synthetic) == 0 {
		return false
	}
	dataByTTID := map[string]any{}
	personSeen := map[int64]bool{}
	persons := make([]any, 0)
	for _, periodID := range synthetic {
		pd, roster, _ := p.eventPeriodDataFor(school, username, periodID)
		dataByTTID[strconv.FormatInt(periodID, 10)] = pd
		for _, person := range roster {
			if personSeen[person.ID] {
				continue
			}
			personSeen[person.ID] = true
			persons = append(persons, person.personJSON())
		}
	}
	// Real ids on the same call (a techniker's own mixed timetable) keep going to
	// Untis and are folded in, so one request still answers a mixed set.
	if len(real) > 0 {
		if realBody, err := withTTIds(body, real); err == nil {
			if b, _, err := p.forwardIntern(school, "getPeriodData2017", username, realBody); err == nil {
				upData, upPersons := upstreamPeriodData(b)
				for k, v := range upData {
					dataByTTID[k] = v
				}
				persons = append(persons, upPersons...)
			}
		}
	}
	p.writeJSONRPCResult(w, id, map[string]any{
		"dataByTTId":         dataByTTID,
		"referencedStudents": persons,
	})
	return true
}

// createEventAbsence records a student absent from an appointment. It answers
// with the created StudentAbsence, whose id the app will later delete with.
func (p *Proxy) createEventAbsence(w http.ResponseWriter, id json.RawMessage, school, username string, body []byte) bool {
	var req struct {
		Params []struct {
			PeriodID  int64  `json:"periodId"`
			StudentID int64  `json:"studentId"`
			StartTime string `json:"startTime"`
			EndTime   string `json:"endTime"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &req) != nil || len(req.Params) == 0 {
		return false
	}
	call := req.Params[0]
	if !isSyntheticPeriodID(call.PeriodID) {
		return false
	}
	ev, found, err := p.store.StudentEventByID(school, customEventIDSpace-call.PeriodID)
	if err != nil || !found {
		p.writeJSONRPCError(w, id, "unknown period", -8509)
		return true
	}
	appt := p.resolveAppointment(school, ev)
	person, ok := personByID(appt.Roster, call.StudentID)
	if !ok {
		p.writeJSONRPCError(w, id, "unknown student", -8509)
		return true
	}
	row := store.EventAbsence{
		Date:      ev.Date,
		StartTime: call.StartTime,
		EndTime:   call.EndTime,
	}
	if _, err := p.store.SetEventAbsence(school, appt.Key, ev.Date, person.Username, call.StartTime, call.EndTime, username); err != nil {
		p.writeJSONRPCError(w, id, "store error", -1)
		return true
	}
	// Marking an already-absent student again is a correction, not a new mark.
	action := store.EventAbsenceActionCreated
	if eventHasAbsence(appt.Absences, person.Username) {
		action = store.EventAbsenceActionEdited
	}
	p.logAbsenceChange(school, appt.Key, ev.Date, person.Username, action, username,
		absenceWindowDetail(call.StartTime, call.EndTime))
	p.writeJSONRPCResult(w, id, map[string]any{
		"absences": []any{studentAbsenceJSON(person, appt, row)},
	})
	return true
}

// deleteEventAbsence removes a local absence. It reports false for an id that is
// not a local one, so a delete of a real absence still forwards to Untis.
func (p *Proxy) deleteEventAbsence(w http.ResponseWriter, id json.RawMessage, school, username string, body []byte) bool {
	var req struct {
		Params []struct {
			AbsenceID int64 `json:"absenceId"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &req) != nil || len(req.Params) == 0 {
		return false
	}
	absenceID := req.Params[0].AbsenceID
	if !isEventAbsenceID(absenceID) {
		return false
	}
	row, found, err := p.store.EventAbsenceByID(school, absenceID)
	if err != nil {
		p.writeJSONRPCError(w, id, "store error", -1)
		return true
	}
	if found {
		if _, err := p.store.DeleteEventAbsence(school, row.AppointmentKey, row.Username); err != nil {
			p.writeJSONRPCError(w, id, "store error", -1)
			return true
		}
		p.logAbsenceChange(school, row.AppointmentKey, row.Date, row.Username, store.EventAbsenceActionDeleted, username,
			absenceWindowDetail(row.StartTime, row.EndTime))
	}
	p.writeJSONRPCResult(w, id, map[string]any{"success": true})
	return true
}

// submitEventAbsencesChecked records that the register was submitted. It reports
// false when no synthetic period is named, so a real teacher's submit is
// untouched.
func (p *Proxy) submitEventAbsencesChecked(w http.ResponseWriter, id json.RawMessage, school, username string, body []byte) bool {
	var req struct {
		Params []struct {
			PeriodIDs []int64 `json:"periodIds"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &req) != nil || len(req.Params) == 0 {
		return false
	}
	handled := false
	for _, periodID := range req.Params[0].PeriodIDs {
		if !isSyntheticPeriodID(periodID) {
			continue
		}
		ev, found, err := p.store.StudentEventByID(school, customEventIDSpace-periodID)
		if err != nil || !found {
			continue
		}
		if _, err := p.store.SetEventAbsenceChecked(school, sharedEventKey(ev), username); err != nil {
			p.writeJSONRPCError(w, id, "store error", -1)
			return true
		}
		p.logAbsenceChange(school, sharedEventKey(ev), ev.Date, "", store.EventAbsenceActionSubmitted, username, "")
		handled = true
	}
	if !handled {
		return false
	}
	p.writeJSONRPCResult(w, id, map[string]any{})
	return true
}

// personByID finds the roster member the app named, matched by the id it was
// given for them.
func personByID(roster []eventPerson, id int64) (eventPerson, bool) {
	for _, person := range roster {
		if person.ID == id {
			return person, true
		}
	}
	return eventPerson{}, false
}

// logAbsenceChange records one change in the append-only register changelog. A
// logging failure is not surfaced: the change itself already happened, and the
// register — not the log — is the source of truth.
func (p *Proxy) logAbsenceChange(school, appointmentKey, date, username, action, actor, detail string) {
	_ = p.store.LogEventAbsenceChange(school, appointmentKey, date, username, action, detail, actor)
}

// eventHasAbsence reports whether the appointment's register already marks a
// student absent, so a repeated mark is logged as a correction.
func eventHasAbsence(rows []store.EventAbsence, username string) bool {
	for _, a := range rows {
		if a.Username == username {
			return true
		}
	}
	return false
}

// absenceWindowDetail renders an absence's window for the changelog.
func absenceWindowDetail(start, end string) string {
	return clockLabel(start) + "–" + clockLabel(end)
}

// clockLabel drops the leading "T" the app prefixes a time with, so the log and
// the dashboard read a plain "14:00" whatever form the client sent.
func clockLabel(clock string) string {
	clock = strings.TrimPrefix(strings.TrimSpace(clock), "T")
	if clock == "" {
		return "—"
	}
	return clock
}

// parseTTIds pulls the synthetic-or-real period ids out of a getPeriodData2017
// body.
func parseTTIds(body []byte) ([]int64, bool) {
	var req struct {
		Params []struct {
			TTIds []int64 `json:"ttIds"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &req) != nil || len(req.Params) == 0 {
		return nil, false
	}
	return req.Params[0].TTIds, true
}

// withTTIds returns the body with the period id set replaced, so the real half of
// a mixed request can be forwarded to Untis without the synthetic ids it cannot
// know.
func withTTIds(body []byte, ids []int64) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	params, _ := doc["params"].([]any)
	if len(params) == 0 {
		return body, nil
	}
	p0, _ := params[0].(map[string]any)
	if p0 == nil {
		return body, nil
	}
	idsAny := make([]any, 0, len(ids))
	for _, v := range ids {
		idsAny = append(idsAny, v)
	}
	p0["ttIds"] = idsAny
	return json.Marshal(doc)
}

// upstreamPeriodData reads the dataByTTId map and referencedStudents list out of
// a getPeriodData2017 answer, tolerating any shape it does not recognise.
func upstreamPeriodData(b []byte) (map[string]any, []any) {
	var doc map[string]any
	if json.Unmarshal(b, &doc) != nil {
		return nil, nil
	}
	result, _ := doc["result"].(map[string]any)
	if result == nil {
		return nil, nil
	}
	data, _ := result["dataByTTId"].(map[string]any)
	persons, _ := result["referencedStudents"].([]any)
	return data, persons
}

// forwardIntern mirrors the proxy's normal passthrough: a class-scoped method an
// editor calls runs as the boosted teacher, and the response is only decorated
// with the requester's personal fields when no rewrite happened.
func (p *Proxy) forwardIntern(school, method, username string, body []byte) ([]byte, int, error) {
	out := body
	rewritten := false
	if owner := p.classScopedOwner(school, username, method); owner != nil {
		if swapped, err := p.rewriteAuthForOwner(school, body, owner); err == nil {
			out = swapped
			rewritten = true
		}
	}
	b, status, _, err := p.untis.RawIntern(school, p.untis.SchoolCookie(school), method, out)
	if err != nil {
		return nil, status, err
	}
	if !rewritten && username != "" {
		b = p.enrichPersonalResponse(b, school, method, username)
	}
	return b, status, nil
}

// writeJSONRPCResult writes a JSON-RPC success envelope, echoing the request id.
func (p *Proxy) writeJSONRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	p.writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": idVal(id), "result": result})
}
