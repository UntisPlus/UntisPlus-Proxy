package proxy

// Admin CRUD for per-student custom events.
//
// The API is admin-only. The *read* side is not here: events reach a student only
// through the surfaces that already know who is asking (the ICS feed for a student
// token, the /week page for one, and a student's own getTimetable2017), so there is
// deliberately no "list events" endpoint a token alone could reach.

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"untis-proxy/internal/store"
)

// studentEventPayload is one event on the wire for the admin UI.
type studentEventPayload struct {
	School      string `json:"school"`
	EventID     int64  `json:"eventId"`
	Username    string `json:"username"`
	Date        string `json:"date"`
	StartTime   string `json:"startTime"`
	EndTime     string `json:"endTime"`
	Title       string `json:"title"`
	Subject     string `json:"subject"`
	Room        string `json:"room"`
	Teacher     string `json:"teacher"`
	Description string `json:"description"`
	CreatedBy   string `json:"createdBy"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
	Revision    int64  `json:"revision"`
}

func studentEventJSON(ev store.StudentEvent) studentEventPayload {
	return studentEventPayload{
		School: ev.School, EventID: ev.ID, Username: ev.Username,
		Date: ev.Date, StartTime: ev.StartTime, EndTime: ev.EndTime,
		Title: ev.Title, Subject: ev.Subject, Room: ev.Room, Teacher: ev.Teacher,
		Description: ev.Description, CreatedBy: ev.CreatedBy,
		CreatedAt: ev.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: ev.UpdatedAt.UTC().Format(time.RFC3339),
		Revision:  ev.Revision,
	}
}

// handleStudentEvents serves the admin event API.
//
//	GET    /api/admin/events?username=…    list (a username is required)
//	POST   /api/admin/events               create
//	PATCH  /api/admin/events/{id}          edit one field or more
//	DELETE /api/admin/events/{id}          remove
func (p *Proxy) handleStudentEvents(w http.ResponseWriter, r *http.Request) {
	// The gate runs before the method is dispatched and before any body is read, so
	// a non-admin's response is the same whatever they asked for.
	u := p.sessionUser(r)
	if u == nil {
		writeJSONError(w, http.StatusUnauthorized, "not logged in")
		return
	}
	if !p.isAdmin(u.Username) {
		p.forbidden(w)
		return
	}
	actor := u.Username
	school := r.URL.Query().Get("school")
	if school == "" {
		school = p.opts.School
	}
	var eventID int64
	if id := r.PathValue("id"); id != "" {
		parsed, err := strconv.ParseInt(id, 10, 64)
		if err != nil || parsed <= 0 {
			writeJSONError(w, http.StatusBadRequest, "bad event id")
			return
		}
		eventID = parsed
	}

	switch r.Method {
	case http.MethodGet:
		username := r.URL.Query().Get("username")
		if username == "" {
			// A list with no subject would either dump one student's events under a
			// different student's key or silently return everyone's. Requiring the
			// username keeps the response unambiguously one student's.
			writeJSONError(w, http.StatusBadRequest, "username is required")
			return
		}
		events, err := p.store.StudentEvents(school, username)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "store error")
			return
		}
		out := make([]studentEventPayload, 0, len(events))
		for _, ev := range events {
			out = append(out, studentEventJSON(ev))
		}
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].Date != out[j].Date {
				return out[i].Date < out[j].Date
			}
			return out[i].StartTime < out[j].StartTime
		})
		p.writeJSON(w, map[string]any{"school": school, "username": username, "events": out})

	case http.MethodPost:
		var req struct {
			Username string `json:"username"`
			// Usernames carries the same event for several students at once. It is
			// one row per student, not a shared group row: a group row would have
			// to be joined against every reader to decide what they may see, and a
			// single row per student is the only shape that cannot leak one
			// student's entry to another.
			Usernames   []string `json:"usernames"`
			Date        string   `json:"date"`
			StartTime   string   `json:"startTime"`
			EndTime     string   `json:"endTime"`
			Title       string   `json:"title"`
			Subject     string   `json:"subject"`
			Room        string   `json:"room"`
			Teacher     string   `json:"teacher"`
			Description string   `json:"description"`
		}
		if !decodeJSON(readBody(r), &req) {
			writeJSONError(w, http.StatusBadRequest, "bad request")
			return
		}
		targets := eventTargets(req.Username, req.Usernames)
		if len(targets) == 0 {
			writeJSONError(w, http.StatusBadRequest, "username is required")
			return
		}
		if bad := validateStudentEvent(targets[0], req.Date, req.StartTime, req.EndTime, req.Title); bad != "" {
			writeJSONError(w, http.StatusBadRequest, bad)
			return
		}
		// Every event shows a lesson in the app, and these are Technik appointments,
		// so an unnamed subject defaults to Technik. The admin can change it per
		// event; the default is what stops the lesson cell from coming back blank.
		if strings.TrimSpace(req.Subject) == "" {
			req.Subject = customSubjectDefault
		}
		// Every target is checked before anything is written, so a batch either
		// lands for all of them or for none: a half-applied batch would leave an
		// admin believing a slot is covered when it is not.
		for _, u := range targets {
			if !p.userExists(school, u) {
				// Checked so an event cannot be filed against a username nothing will
				// ever read, which is how a typo becomes an event that silently never
				// appears on anyone's timetable.
				writeJSONError(w, http.StatusNotFound, "no such student in this school: "+u)
				return
			}
		}
		// A student cannot be in two places at once, so an event that already sits
		// in the same slot is the same appointment entered twice. It is skipped
		// rather than created a second time, and the skip is reported so the admin
		// can see the slot was already covered instead of silently getting a
		// duplicate. Skips are not errors: the rest of the batch still lands.
		created := make([]map[string]any, 0, len(targets))
		skipped := make([]map[string]any, 0)
		var firstEv store.StudentEvent
		for _, u := range targets {
			if dup, found, err := p.store.StudentEventDuplicate(school, u, req.Date, req.StartTime, req.EndTime); err != nil {
				writeJSONError(w, http.StatusInternalServerError, "store error")
				return
			} else if found {
				skipped = append(skipped, map[string]any{"username": u, "eventId": dup.ID, "reason": "duplicate"})
				continue
			}
			ev, err := p.store.CreateStudentEvent(school, u, store.NewStudentEvent{
				Date: req.Date, StartTime: req.StartTime, EndTime: req.EndTime,
				Title: req.Title, Subject: req.Subject, Room: req.Room,
				Teacher: req.Teacher, Description: req.Description,
			}, actor)
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, "store error")
				return
			}
			if firstEv.ID == 0 {
				firstEv = ev
			}
			created = append(created, map[string]any{"eventId": ev.ID, "username": ev.Username})
			p.announceStudentEventChange(school, u, "created")
		}
		if len(targets) == 1 && len(created) == 1 {
			// A single student keeps the original response shape, so an existing
			// client that reads eventId still works.
			p.writeJSON(w, studentEventJSON(firstEv))
			return
		}
		p.writeJSON(w, map[string]any{"school": school, "created": created, "skipped": skipped})

	case http.MethodPatch:
		if eventID == 0 {
			writeJSONError(w, http.StatusBadRequest, "missing event id")
			return
		}
		var req struct {
			Date        *string `json:"date"`
			StartTime   *string `json:"startTime"`
			EndTime     *string `json:"endTime"`
			Title       *string `json:"title"`
			Subject     *string `json:"subject"`
			Room        *string `json:"room"`
			Teacher     *string `json:"teacher"`
			Description *string `json:"description"`
		}
		if !decodeJSON(readBody(r), &req) {
			writeJSONError(w, http.StatusBadRequest, "bad request")
			return
		}
		// Validate the merged result, not just the patch: changing only the start
		// time can make a stored event nonsensical, and the row is what is checked
		// before it is written.
		existing, found, err := p.store.StudentEventByID(school, eventID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "store error")
			return
		}
		if !found {
			writeJSONError(w, http.StatusNotFound, "no such event")
			return
		}
		merged := store.StudentEventPatch{
			Date: req.Date, StartTime: req.StartTime, EndTime: req.EndTime,
			Title: req.Title, Subject: req.Subject, Room: req.Room,
			Teacher: req.Teacher, Description: req.Description,
		}
		bad := validateStudentEventPatch(existing, merged)
		if bad != "" {
			writeJSONError(w, http.StatusBadRequest, bad)
			return
		}
		ev, found, err := p.store.UpdateStudentEvent(school, eventID, merged)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "store error")
			return
		}
		if !found {
			writeJSONError(w, http.StatusNotFound, "no such event")
			return
		}
		p.announceStudentEventChange(school, ev.Username, "updated")
		p.writeJSON(w, studentEventJSON(ev))

	case http.MethodDelete:
		if eventID == 0 {
			writeJSONError(w, http.StatusBadRequest, "missing event id")
			return
		}
		// Read before deleting so the notification names the right student.
		existing, found, err := p.store.StudentEventByID(school, eventID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "store error")
			return
		}
		if !found {
			writeJSONError(w, http.StatusNotFound, "no such event")
			return
		}
		deleted, err := p.store.DeleteStudentEvent(school, eventID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "store error")
			return
		}
		if deleted {
			p.announceStudentEventChange(school, existing.Username, "deleted")
		}
		p.writeJSON(w, map[string]any{"deleted": deleted})

	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// eventAbsencePayload is one recorded absence on the wire for the admin UI.
type eventAbsencePayload struct {
	School         string `json:"school"`
	AppointmentKey string `json:"appointmentKey"`
	Date           string `json:"date"`
	Username       string `json:"username"`
	DisplayName    string `json:"displayName"`
	StartTime      string `json:"startTime"`
	EndTime        string `json:"endTime"`
	CreatedBy      string `json:"createdBy"`
	UpdatedAt      string `json:"updatedAt"`
}

// eventRosterPayload is one assigned student on an appointment, with whether the
// register marks them absent. A student on the roster with absent=false is the
// "was there" side of the register, which is otherwise invisible: the absence
// table only ever held the absent ones.
type eventRosterPayload struct {
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Absent      bool   `json:"absent"`
	StartTime   string `json:"startTime,omitempty"`
	EndTime     string `json:"endTime,omitempty"`
	UpdatedAt   string `json:"updatedAt,omitempty"`
	UpdatedBy   string `json:"updatedBy,omitempty"`
}

// eventAppointmentPayload is one appointment with its whole roster, so the
// dashboard can show who is present and who is not, and whether the register was
// submitted, without a second request per student.
type eventAppointmentPayload struct {
	AppointmentKey string               `json:"appointmentKey"`
	Date           string               `json:"date"`
	StartTime      string               `json:"startTime"`
	EndTime        string               `json:"endTime"`
	Title          string               `json:"title"`
	Subject        string               `json:"subject"`
	Room           string               `json:"room"`
	Teacher        string               `json:"teacher"`
	Checked        bool                 `json:"checked"`
	Roster         []eventRosterPayload `json:"roster"`
}

// eventAbsenceLogPayload is one line of the append-only register changelog.
type eventAbsenceLogPayload struct {
	ID       int64  `json:"id"`
	Date     string `json:"date"`
	Username string `json:"username"`
	Action   string `json:"action"`
	Detail   string `json:"detail"`
	Actor    string `json:"actor"`
	At       string `json:"at"`
}

// displayNameFor resolves the readable name for a stored (folded) account,
// falling back to the key itself when the student has never logged in.
func (p *Proxy) displayNameFor(school, username string) string {
	if u, err := p.store.GetUserInSchool(school, username); err == nil && u != nil && strings.TrimSpace(u.DisplayName) != "" {
		return u.DisplayName
	}
	return username
}

// handleEventAbsences serves the admin read of the techniker absence register: the
// appointments in the window with their whole roster (present and absent) and
// whether each register was submitted. Proxy-local, never written to Untis.
//
//	GET /api/admin/events/absences?school=&from=YYYY-MM-DD&to=YYYY-MM-DD
//
// The window is optional and defaults to everything, so an admin who lands on the
// section sees the register without first choosing dates.
func (p *Proxy) handleEventAbsences(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	user := p.sessionUser(r)
	if user == nil {
		writeJSONError(w, http.StatusUnauthorized, "not logged in")
		return
	}
	if !p.isAdmin(user.Username) {
		p.forbidden(w)
		return
	}
	school, from, to := p.absenceWindow(r)
	events, err := p.store.AllStudentEventsForRange(school, from, to)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store error")
		return
	}
	appointments := make([]eventAppointmentPayload, 0)
	flat := make([]eventAbsencePayload, 0)
	index := map[string]int{}
	for _, ev := range events {
		key := sharedEventKey(ev)
		gi, ok := index[key]
		if !ok {
			gi = len(appointments)
			index[key] = gi
			appointments = append(appointments, eventAppointmentPayload{
				AppointmentKey: key, Date: ev.Date, StartTime: ev.StartTime, EndTime: ev.EndTime,
				Title: ev.Title, Subject: ev.Subject, Room: ev.Room, Teacher: ev.Teacher,
			})
		}
		appt := &appointments[gi]
		display := p.displayNameFor(school, ev.Username)
		appt.Roster = append(appt.Roster, eventRosterPayload{Username: ev.Username, DisplayName: display})
	}
	for i := range appointments {
		appt := &appointments[i]
		absences, err := p.store.EventAbsences(school, appt.AppointmentKey)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "store error")
			return
		}
		byUser := make(map[string]store.EventAbsence, len(absences))
		for _, a := range absences {
			byUser[a.Username] = a
		}
		for j := range appt.Roster {
			member := &appt.Roster[j]
			a, absent := byUser[member.Username]
			if !absent {
				continue
			}
			member.Absent = true
			member.StartTime = a.StartTime
			member.EndTime = a.EndTime
			member.UpdatedAt = a.UpdatedAt.UTC().Format(time.RFC3339)
			member.UpdatedBy = p.displayNameFor(school, a.CreatedBy)
			flat = append(flat, eventAbsencePayload{
				School: school, AppointmentKey: appt.AppointmentKey, Date: appt.Date,
				Username: member.Username, DisplayName: member.DisplayName,
				StartTime: a.StartTime, EndTime: a.EndTime,
				CreatedBy: a.CreatedBy, UpdatedAt: a.UpdatedAt.UTC().Format(time.RFC3339),
			})
		}
		if checked, err := p.store.EventAbsenceChecked(school, appt.AppointmentKey); err == nil {
			appt.Checked = checked
		}
	}
	p.writeJSON(w, map[string]any{
		"school": school, "from": from, "to": to,
		"appointments": appointments, "absences": flat,
	})
}

// absenceWindow reads the optional school/from/to query, defaulting the school to
// the configured one and the window to everything.
func (p *Proxy) absenceWindow(r *http.Request) (school, from, to string) {
	school = r.URL.Query().Get("school")
	if school == "" {
		school = p.opts.School
	}
	from = r.URL.Query().Get("from")
	to = r.URL.Query().Get("to")
	if !validDate(from) {
		from = "1970-01-01"
	}
	if !validDate(to) {
		to = "9999-12-31"
	}
	return school, from, to
}

// handleEventAbsenceEdit is the admin's correction of one recorded absence:
// changing its window and/or moving it to another assigned student. Both are
// logged, so a correction is visible in the changelog like any other change.
//
//	PATCH /api/admin/events/absences
//	{ "school":…, "appointmentKey":…, "date":…, "username":…,
//	  "startTime":…, "endTime":…, "newUsername":… }
func (p *Proxy) handleEventAbsenceEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	user := p.sessionUser(r)
	if user == nil {
		writeJSONError(w, http.StatusUnauthorized, "not logged in")
		return
	}
	if !p.isAdmin(user.Username) {
		p.forbidden(w)
		return
	}
	var req struct {
		School         string `json:"school"`
		AppointmentKey string `json:"appointmentKey"`
		Date           string `json:"date"`
		Username       string `json:"username"`
		StartTime      string `json:"startTime"`
		EndTime        string `json:"endTime"`
		NewUsername    string `json:"newUsername"`
	}
	if !decodeJSON(readBody(r), &req) {
		writeJSONError(w, http.StatusBadRequest, "bad request")
		return
	}
	if req.School == "" {
		req.School = p.opts.School
	}
	if req.AppointmentKey == "" || req.Username == "" || !validDate(req.Date) {
		writeJSONError(w, http.StatusBadRequest, "appointmentKey, date and username are required")
		return
	}
	// The appointment must still exist on that date, or the edit would resurrect
	// an absence for an event that is gone.
	found := false
	roster := map[string]bool{}
	for _, ev := range p.upstreamDayEvents(req.School, req.Date) {
		if sharedEventKey(ev) != req.AppointmentKey {
			continue
		}
		found = true
		roster[ev.Username] = true
	}
	if !found {
		writeJSONError(w, http.StatusNotFound, "no such appointment on that date")
		return
	}
	absences, err := p.store.EventAbsences(req.School, req.AppointmentKey)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store error")
		return
	}
	var current store.EventAbsence
	exists := false
	for _, a := range absences {
		if a.Username == eventKeyFold(req.Username) {
			current, exists = a, true
			break
		}
	}
	if !exists {
		writeJSONError(w, http.StatusNotFound, "no such absence on that appointment")
		return
	}
	start, end := req.StartTime, req.EndTime
	if strings.TrimSpace(start) == "" {
		start = current.StartTime
	}
	if strings.TrimSpace(end) == "" {
		end = current.EndTime
	}
	target := current.Username
	if req.NewUsername != "" && eventKeyFold(req.NewUsername) != current.Username {
		if !roster[eventKeyFold(req.NewUsername)] {
			writeJSONError(w, http.StatusBadRequest, "target student is not assigned to this appointment")
			return
		}
		target = eventKeyFold(req.NewUsername)
	}
	if target != current.Username {
		if _, err := p.store.DeleteEventAbsence(req.School, req.AppointmentKey, current.Username); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "store error")
			return
		}
	}
	if _, err := p.store.SetEventAbsence(req.School, req.AppointmentKey, current.Date, target, start, end, user.Username); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store error")
		return
	}
	detail := clockLabel(start) + "–" + clockLabel(end)
	if target != current.Username {
		detail = p.displayNameFor(req.School, current.Username) + " → " + p.displayNameFor(req.School, target) + " (" + detail + ")"
	}
	_ = p.store.LogEventAbsenceChange(req.School, req.AppointmentKey, current.Date, target, store.EventAbsenceActionEdited, detail, user.Username)
	p.writeJSON(w, map[string]any{"success": true, "appointmentKey": req.AppointmentKey})
}

// handleEventAbsenceLog serves the append-only register changelog. There is no
// DELETE counterpart by design: the history is not editable from the UI.
//
//	GET /api/admin/events/absences/log?school=&from=&to=
func (p *Proxy) handleEventAbsenceLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	user := p.sessionUser(r)
	if user == nil {
		writeJSONError(w, http.StatusUnauthorized, "not logged in")
		return
	}
	if !p.isAdmin(user.Username) {
		p.forbidden(w)
		return
	}
	school, from, to := p.absenceWindow(r)
	rows, err := p.store.EventAbsenceLog(school, from, to)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store error")
		return
	}
	out := make([]eventAbsenceLogPayload, 0, len(rows))
	for _, e := range rows {
		out = append(out, eventAbsenceLogPayload{
			ID: e.ID, Date: e.Date, Username: p.displayNameFor(school, e.Username),
			Action: e.Action, Detail: e.Detail, Actor: e.Actor,
			At: e.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	p.writeJSON(w, map[string]any{"school": school, "from": from, "to": to, "log": out})
}

// upstreamDayEvents is the stored events on one date, for resolving an
// appointment by its key. A store error degrades to nil: the caller then reports
// the appointment as gone rather than editing blind.
func (p *Proxy) upstreamDayEvents(school, date string) []store.StudentEvent {
	events, err := p.store.AllStudentEventsForRange(school, date, date)
	if err != nil {
		return nil
	}
	return events
}

// eventKeyFold mirrors the store's event key fold so the handler can compare a
// typed username against a stored one without importing the rule.
func eventKeyFold(username string) string {
	return foldUmlauts(strings.ToLower(strings.TrimSpace(username)))
}

// eventTargets resolves the create request's target students: the single
// username, or the usernames list, de-duplicated with order kept so the response
// reads in the order the admin picked them.
func eventTargets(username string, usernames []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(u string) {
		u = strings.TrimSpace(u)
		key := strings.ToLower(u)
		if u == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, u)
	}
	add(username)
	for _, u := range usernames {
		add(u)
	}
	return out
}

// validateStudentEvent checks a create request. Returns "" when acceptable, or the
// message to send back.
func validateStudentEvent(username, date, start, end, title string) string {
	if strings.TrimSpace(username) == "" {
		return "username is required"
	}
	if strings.TrimSpace(title) == "" {
		return "title is required"
	}
	if !validDate(date) {
		return "date must be YYYY-MM-DD"
	}
	return validateEventTimes(start, end)
}

func validateEventTimes(start, end string) string {
	// time.Parse accepts one- or two-digit hours, so "9:00" is valid input and
	// reaches here. Comparing the strings would then rank "10:00" below "9:00"
	// and reject a perfectly ordinary 09:00–10:00 event while accepting an
	// inverted 10:00–09:00 one, so the two values are compared as times.
	startT, err := time.Parse("15:04", start)
	if err != nil {
		return "startTime and endTime must be HH:MM"
	}
	endT, err := time.Parse("15:04", end)
	if err != nil {
		return "startTime and endTime must be HH:MM"
	}
	if !endT.After(startT) {
		// Rejected rather than silently collapsed at render time: an admin who typed
		// this has made a mistake, and quietly showing the event at the wrong length
		// hides it.
		return "endTime must be after startTime"
	}
	return ""
}

// validateStudentEventPatch validates the merged result of an edit.
func validateStudentEventPatch(before store.StudentEvent, patch store.StudentEventPatch) string {
	date, start, end, title := before.Date, before.StartTime, before.EndTime, before.Title
	if patch.Date != nil {
		date = *patch.Date
	}
	if patch.StartTime != nil {
		start = *patch.StartTime
	}
	if patch.EndTime != nil {
		end = *patch.EndTime
	}
	if patch.Title != nil {
		title = *patch.Title
	}
	if !validDate(date) {
		return "date must be YYYY-MM-DD"
	}
	if strings.TrimSpace(title) == "" {
		return "title is required"
	}
	return validateEventTimes(start, end)
}

// userExists reports whether the username is a student of this school. The events
// table has no foreign key to users, so this is what keeps a typo from creating
// an event nobody will ever see.
//
// It requires PersonType 5 (student): a teacher or admin account is a known user,
// but an event filed against one would sit in the table forever, because teacher
// timetables have no viewer and so are never overlaid.
func (p *Proxy) userExists(school, username string) bool {
	u, err := p.store.GetUserInSchool(school, strings.TrimSpace(username))
	return err == nil && u != nil && u.PersonType == 5
}
