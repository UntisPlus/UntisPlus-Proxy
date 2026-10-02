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
			Username    string `json:"username"`
			Date        string `json:"date"`
			StartTime   string `json:"startTime"`
			EndTime     string `json:"endTime"`
			Title       string `json:"title"`
			Subject     string `json:"subject"`
			Room        string `json:"room"`
			Teacher     string `json:"teacher"`
			Description string `json:"description"`
		}
		if !decodeJSON(readBody(r), &req) {
			writeJSONError(w, http.StatusBadRequest, "bad request")
			return
		}
		if bad := validateStudentEvent(req.Username, req.Date, req.StartTime, req.EndTime, req.Title); bad != "" {
			writeJSONError(w, http.StatusBadRequest, bad)
			return
		}
		if !p.userExists(school, req.Username) {
			// Checked so an event cannot be filed against a username nothing will
			// ever read, which is how a typo becomes an event that silently never
			// appears on anyone's timetable.
			writeJSONError(w, http.StatusNotFound, "no such user in this school")
			return
		}
		ev, err := p.store.CreateStudentEvent(school, req.Username, store.NewStudentEvent{
			Date: req.Date, StartTime: req.StartTime, EndTime: req.EndTime,
			Title: req.Title, Subject: req.Subject, Room: req.Room,
			Teacher: req.Teacher, Description: req.Description,
		}, actor)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "store error")
			return
		}
		p.announceStudentEventChange(school, ev.Username, "created")
		p.writeJSON(w, studentEventJSON(ev))

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
