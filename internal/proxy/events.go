package proxy

// Per-student custom events: an admin-authored appointment on one student's own
// schedule, served everywhere that student's timetable is served.
//
// The privacy rule here is the same one the rest of the personal features follow,
// and it is structural rather than a check: an event belongs to a *viewer*, so it
// is only ever attached on a path where the proxy has already resolved who the
// viewer is. A class, teacher, room or subject feed has no viewer, so those feeds
// never carry anyone's events — not because they filter them out, but because
// there is no code path that could add them.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"untis-proxy/internal/store"
)

// A synthesized period is deliberately indistinguishable from a normal lesson on
// the wire. The official app marks editor-created events with isCustom and
// renders them with a distinct look — and a class view, which a group of people
// shares, has no reason to show an editor's private event, so it drops the
// period. The Technik events are meant to be ordinary lessons for now, so the
// marker is not sent. The proxy's own renderers (the .ics feed and the /week
// page) detect the event through the customEventId field instead, and the app
// ignores what it does not know.

// customEventIDSpace keeps synthetic ids from colliding with real period ids.
//
// Every surface keys on `id`, and the ICS feed puts the raw id in a UID. A real
// period id is a small positive number, so negative ids are free — and being
// negative rather than large means an id that has accidentally been left at zero,
// or that arrived from a caller who did not set one, still cannot masquerade as a
// real lesson.
const customEventIDSpace = -1_000_000_000

// customEventID derives the synthetic id for one event.
//
// The id is derived from the event alone and is the same on every revision, which
// is what RFC 5545 requires of the UID built from it: a client matches a VEVENT
// by UID and uses SEQUENCE to decide whether it changed. An id that moved on edit
// would leave the client holding the old entry under one UID and the new one
// under another — two entries for one appointment — so the revision travels in
// SEQUENCE and in customRevision instead, where a client can act on it.
//
// Event ids are unique, so no two events can land on the same synthetic id. An
// earlier scheme also folded in the revision, subtracting a fixed amount per
// revision, which aliased event 1 at revision 2 onto event 1000001 at revision 1:
// one id, and one ICS UID, shared by two different events.
func customEventID(ev store.StudentEvent) int64 {
	return customEventIDSpace - ev.ID
}

// customEventPeriods turns stored events into WebUntis-shaped periods so the
// existing period renderers — the ICS feed, the /week page, the app — can carry
// them without each one reimplementing the mapping.
//
// An event the store returned but whose times cannot be read is dropped rather
// than guessed at: an event at an unknown time cannot be placed on a calendar,
// and placing it at midnight would be a fabrication the client cannot detect.
func customEventPeriods(events []store.StudentEvent, loc *time.Location) []map[string]any {
	if loc == nil {
		loc = time.UTC
	}
	out := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		start, ok := eventTime(ev.Date, ev.StartTime, loc)
		if !ok {
			continue
		}
		end, ok := eventTime(ev.Date, ev.EndTime, loc)
		if !ok {
			continue
		}
		// An end at or before the start would occupy no time at all. Falling back
		// to the start keeps the event visible as a marker rather than dropping it,
		// which is the lesser of the two surprises for an admin-authored entry.
		if !end.After(start) {
			end = start
		}
		// The lesson cell carries what the appointment needs — the admin's
		// description ("Laptop, Beamer, Stehpult"). The event's own title is shown
		// as the lesson topic in the detail view, and the subject is not repeated in
		// the cell. An event with no description falls back to its subject so the
		// cell is never blank. Who the appointment is assigned to is filled on the
		// serving surface (the personal timetable and the shared surfaces both add
		// it), not here.
		lesson := strings.TrimSpace(ev.Description)
		if lesson == "" {
			lesson = strings.TrimSpace(ev.Subject)
		}
		if lesson == "" {
			lesson = customSubjectDefault
		}
		out = append(out, map[string]any{
			"id":       customEventID(ev),
			"lessonId": 0,
			// WebUntis labels a period's wall-clock time with a literal "Z" — it
			// is not a UTC instant, and the app parses exactly that shape. A real
			// offset (what this used to emit) is dropped by the app, so the
			// aggregate and room/teacher surfaces looked empty while the student's
			// own timetable, whose zone is read from the real periods and therefore
			// produced "Z", worked. The wall time is kept exactly, matching how the
			// same event is written on every surface and how the ICS feed reads it.
			"startDateTime":  start.Format("2006-01-02T15:04") + "Z",
			"endDateTime":    end.Format("2006-01-02T15:04") + "Z",
			"foreColor":      "#000000",
			"backColor":      "#FFF3DE",
			"innerForeColor": "#000000",
			"innerBackColor": "#000000",
			"text": map[string]any{
				"lesson":       lesson,
				"substitution": "",
				"info":         "",
				"attachments":  []any{},
			},
			"elements":       []any{},
			"can":            []any{"READ_LESSONTOPIC", "READ_HOMEWORK", "READ_CLASSROLE", "READ_PERIODINFO"},
			"is":             []any{"REGULAR"},
			"homeWorks":      []any{},
			"exam":           nil,
			"isOnlinePeriod": false,
			"blockHash":      ev.ID,
			// The fields below are not part of an upstream period and are meant
			// only for the proxy's own renderers (the .ics feed and the /week
			// page); the official app ignores what it does not know.
			"customEventId":  ev.ID,
			"customRevision": ev.Revision,
			"customTitle":    ev.Title,
			"subject":        ev.Subject,
			"teacher":        ev.Teacher,
			"room":           ev.Room,
			"description":    ev.Description,
			"date":           dateInt(ev.Date),
			"startTime":      hmInt(start),
			"endTime":        hmInt(end),
		})
	}
	// Sorted by instant so a client merging events into real periods sees them in
	// schedule order rather than in whatever order the admin created them.
	sort.SliceStable(out, func(i, j int) bool {
		si, _ := out[i]["startDateTime"].(string)
		sj, _ := out[j]["startDateTime"].(string)
		if si != sj {
			return si < sj
		}
		ei, _ := out[i]["endDateTime"].(string)
		ej, _ := out[j]["endDateTime"].(string)
		return ei < ej
	})
	return out
}

// eventTime combines the stored date and HH:MM into an instant. The two are
// stored separately because a date is a value in its own right (the admin picks
// one from a calendar) and the range read filters on it.
func eventTime(date, clock string, loc *time.Location) (time.Time, bool) {
	if date == "" || clock == "" {
		return time.Time{}, false
	}
	// Parsed in the school's zone, not UTC: an admin writing "14:00" means 14:00 on
	// the school clock. Built as a wall clock in that location rather than
	// time.Parse, which would label the same digits UTC and shift the event by the
	// zone's offset on every surface that converts the timestamp.
	t, err := time.ParseInLocation("2006-01-02 15:04", date+" "+clock, loc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// dateInt is the YYYYMMDD integer form the app expects on a period.
func dateInt(date string) int64 {
	var y, m, d int
	if _, err := fmt.Sscanf(date, "%4d-%2d-%2d", &y, &m, &d); err != nil {
		return 0
	}
	return int64(y)*10000 + int64(m)*100 + int64(d)
}

// studentEventPeriods loads and converts a viewer's events for a date range.
//
// school and viewer are both required: without a school there is nothing to read,
// and without a viewer there is no identity to key on. Returning nothing in that
// case is what keeps a class feed from ever picking up a student's events.
func (p *Proxy) studentEventPeriods(school, username, from, to string, loc *time.Location) []map[string]any {
	if school == "" || username == "" {
		return nil
	}
	events, err := p.store.StudentEventsForRange(school, username, from, to)
	if err != nil {
		// An unreadable event table costs the events, nothing else: the real
		// timetable still resolves and the client still gets its lessons.
		enrichmentFailed("student events "+username, err)
		return nil
	}
	periods := customEventPeriods(events, loc)
	p.labelAppointmentRoster(school, from, to, periods)
	return periods
}

// labelAppointmentRoster fills the substitution line of a viewer's own event
// periods with every student assigned to the same appointment, so they see who
// else is booked and not only themselves. It reads the whole range once, groups
// the stored rows by sharedEventKey, and matches each period back through its
// customEventId; a read failure leaves the events themselves intact.
func (p *Proxy) labelAppointmentRoster(school, from, to string, periods []map[string]any) {
	if len(periods) == 0 {
		return
	}
	events, err := p.store.AllStudentEventsForRange(school, from, to)
	if err != nil {
		enrichmentFailed("event roster "+school, err)
		return
	}
	names := make(map[string][]string, len(periods))
	seen := make(map[string]map[string]bool)
	keyOf := make(map[int64]string, len(events))
	for _, ev := range events {
		key := sharedEventKey(ev)
		keyOf[ev.ID] = key
		if seen[key] == nil {
			seen[key] = map[string]bool{}
		}
		name := p.displayNameFor(school, ev.Username)
		if !seen[key][name] {
			seen[key][name] = true
			names[key] = append(names[key], name)
		}
	}
	for _, pd := range periods {
		id, ok := jsonID(pd["customEventId"])
		if !ok {
			continue
		}
		assigned := names[keyOf[id]]
		if len(assigned) == 0 {
			continue
		}
		if text, ok := pd["text"].(map[string]any); ok {
			text["substitution"] = strings.Join(assigned, ", ")
		}
	}
}

// decorateStudentEvents appends a viewer's custom events to a timetable result.
//
// It goes through enrichJSON so every one of that helper's guarantees applies:
// unknown upstream fields survive, an error response is left alone, any failure
// returns the original bytes, and a response that gains nothing is returned
// un-re-encoded rather than reformatted.
func (p *Proxy) decorateStudentEvents(raw []byte, school, username, from, to string) []byte {
	if school == "" || username == "" {
		return raw
	}
	// The zone comes from the response's own periods. A school is not on a single
	// offset across the year, so it is read from the data being served rather than
	// assumed; a DST boundary inside the range is handled by the zone database.
	loc := responseLocation(raw)
	// The range the caller asked for is preferred, but a request without usable
	// dates still deserves the events for the span it actually returned.
	if !validDate(from) || !validDate(to) {
		from, to = responseSpan(raw)
	}
	if !validDate(from) || !validDate(to) {
		return raw
	}
	// from and to are both inclusive dates here, matching upstream's endDate, so a
	// one-day response is simply from == to and needs no widening.
	if to < from {
		return raw
	}
	periods := p.studentEventPeriods(school, username, from, to, loc)
	if len(periods) == 0 {
		return raw
	}
	// The viewer's own class timetable keeps the periods addressed to them, so an
	// event served on a CLASS request is tagged with the Technik class the same
	// way the aggregate is.
	return p.appendEventPeriods(raw, school, username, periods, technikClassID)
}

// appendEventPeriods merges custom-event periods into a timetable result: it
// stamps the viewer's capabilities, attaches the subject/room/teacher elements
// the app resolves names from, cuts the event dates out of the holiday list, and
// advances the masterData timestamp when any of that changed it. classID, when
// non-zero, is prepended as a CLASS element (used for the viewer's own
// timetable); the shared room/teacher surfaces pass zero.
//
// It goes through enrichJSON so every one of that helper's guarantees applies:
// unknown upstream fields survive, an error response is left alone, any failure
// returns the original bytes, and a response that gains nothing is returned
// un-re-encoded rather than reformatted.
func (p *Proxy) appendEventPeriods(raw []byte, school, username string, periods []map[string]any, classID int64) []byte {
	if len(periods) == 0 {
		return raw
	}
	return enrichJSON(raw, func(result jsonObject) {
		tt, ok := result["timetable"].(jsonObject)
		if !ok {
			return
		}
		existing, _ := tt["periods"].([]any)
		// jsonArray drops any entry that is not an object, so it is used only to
		// check. Rebuilding the list from the original slice keeps every entry
		// upstream sent: a period list is always all objects, but a response
		// carrying something else would otherwise have it silently deleted, which
		// is the one failure this module must never cause.
		if len(jsonArray(tt["periods"])) != len(existing) {
			return
		}
		merged := make([]any, 0, len(existing)+len(periods))
		merged = append(merged, existing...)
		for _, ev := range periods {
			// A synthesized period arrives with an empty `can` list and the app
			// hides the lesson line without READ_LESSONTOPIC, so give it the same
			// capability set a viewer of that timetable would carry. This mirrors
			// how every other serving path stamps the requester's caps.
			p.capsInPeriod(username, ev)
			merged = append(merged, ev)
		}
		tt["periods"] = merged
		md, hasMD := result["masterData"].(jsonObject)
		changed := false
		if hasMD {
			// The app places a period by the elements it references and drops one
			// that references nothing, so the event is given a subject, room and
			// teacher element. Any element whose name is not in the school's
			// catalogue is synthesized into masterData, so the app has a name to
			// resolve and the event is visible.
			changed = attachEventElements(periods, md, p.roomOverrides(school))
			// A client that hides a holiday week hides the event with it. The break is
			// declared in masterData, which the client reads to decide the day is not a
			// school day, so the event dates are cut out of that list: this is the one
			// place the decision can be reversed without the client's cooperation.
			if stripHolidayDates(md, periodDates(periods)) {
				changed = true
			}
		}
		if classID != 0 {
			tagClassElement(periods, classID)
			// A period tagged with the synthetic Technik class references a class
			// the viewer's masterData may not carry. Add the label-only stub so the
			// app can resolve the row; the aggregate data stays gated elsewhere.
			if classID == technikClassID && hasMD && ensureTechnikClass(md) {
				changed = true
			}
		}
		// The client caches master data by its timestamp. Changing it (a stripped
		// holiday, a synthesized element, the Technik label) without moving the
		// timestamp would leave a client that already holds this version serving
		// the old list, so the version is advanced. It is derived from upstream's
		// own value, so repeated requests stay in step.
		if hasMD && changed {
			bumpMasterTimestamp(md)
		}
	})
}

// periodDates collects the distinct YYYY-MM-DD dates the served events fall on.
func periodDates(periods []map[string]any) map[string]bool {
	out := make(map[string]bool, len(periods))
	for _, pd := range periods {
		v, ok := pd["date"].(int64)
		if !ok {
			continue
		}
		out[fmt.Sprintf("%04d-%02d-%02d", v/10000, v/100%100, v%100)] = true
	}
	return out
}

// stripHolidayDates removes the given dates from a masterData holiday list,
// splitting a range around a date in its middle and dropping it when it is the
// last day of the range. It reports whether anything changed.
//
// The first fragment of a split keeps the original id; later fragments get a
// derived id so two holidays are never confused for one. Ids generated this way
// depend only on the original id and position, so the same strip on the same
// response produces the same list every time.
func stripHolidayDates(md jsonObject, dates map[string]bool) bool {
	if len(dates) == 0 {
		return false
	}
	holidays, ok := md["holidays"].([]any)
	if !ok || len(holidays) == 0 {
		return false
	}
	out := make([]any, 0, len(holidays))
	changed := false
	for _, h := range holidays {
		hm, ok := h.(jsonObject)
		if !ok {
			out = append(out, h)
			continue
		}
		start, _ := hm["startDate"].(string)
		end, _ := hm["endDate"].(string)
		if !validDate(start) || !validDate(end) || end < start {
			out = append(out, h)
			continue
		}
		var cut []string
		for d := range dates {
			if d >= start && d <= end {
				cut = append(cut, d)
			}
		}
		if len(cut) == 0 {
			out = append(out, h)
			continue
		}
		sort.Strings(cut)
		changed = true
		segStart := start
		idx := 0
		for _, d := range cut {
			if d > segStart {
				out = append(out, holidayFragment(hm, segStart, dayBefore(d), idx))
				idx++
			}
			segStart = dayAfter(d)
		}
		if segStart <= end {
			out = append(out, holidayFragment(hm, segStart, end, idx))
		}
	}
	if changed {
		md["holidays"] = out
	}
	return changed
}

// holidayFragment copies a holiday with a new span. idx 0 keeps the original id
// so the common "trim the start or end" case is untouched.
func holidayFragment(h jsonObject, start, end string, idx int) jsonObject {
	frag := make(jsonObject, len(h))
	for k, v := range h {
		frag[k] = v
	}
	frag["startDate"] = start
	frag["endDate"] = end
	if idx > 0 {
		if id, ok := jsonID(h["id"]); ok {
			frag["id"] = id*1000 + int64(idx)
		}
	}
	return frag
}

func dayBefore(date string) string { return shiftDay(date, -1) }
func dayAfter(date string) string  { return shiftDay(date, 1) }

func shiftDay(date string, days int) string {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return date
	}
	return t.AddDate(0, 0, days).Format("2006-01-02")
}

// bumpMasterTimestamp advances a masterData version in place, leaving a value of
// an unexpected type alone rather than dropping it.
func bumpMasterTimestamp(md jsonObject) {
	switch v := md["timeStamp"].(type) {
	case json.Number:
		if n, err := v.Int64(); err == nil {
			md["timeStamp"] = json.Number(strconv.FormatInt(n+1, 10))
		}
	case float64:
		md["timeStamp"] = v + 1
	}
}

// buildCustomICSVEVENT renders a hand-authored event as a VEVENT.
//
// It is a separate renderer rather than a branch inside buildICSVEVENT because
// the two have genuinely different contracts. A real period is derived from its
// elements — subject, teacher and room are looked up in master data by id — where
// an event carries plain text an admin typed, with no ids to resolve. Sharing the
// renderer would mean encoding text into ids and looking it back up.
func buildCustomICSVEVENT(pd map[string]any, tz string, stamp time.Time) string {
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
	periodID, _ := pd["id"].(int64)

	summary, _ := pd["customTitle"].(string)
	if summary == "" {
		summary = "Termin"
	}
	// A subject or teacher an admin wrote is context, not the event's name, so it
	// goes after the title rather than replacing it.
	if subject, _ := pd["subject"].(string); subject != "" {
		summary += " · " + subject
	}
	if teacher, _ := pd["teacher"].(string); teacher != "" {
		summary += " · " + teacher
	}

	var desc []string
	if room, _ := pd["room"].(string); room != "" {
		desc = append(desc, "Raum: "+room)
	}
	if d, _ := pd["description"].(string); d != "" {
		desc = append(desc, d)
	}

	var b strings.Builder
	b.WriteString("BEGIN:VEVENT\r\n")
	b.WriteString("DTSTAMP:" + stamp.UTC().Format("20060102T150405Z") + "\r\n")
	// The uid is namespaced so a client can never confuse an event with a real
	// lesson it already holds under the bare period id.
	b.WriteString(fmt.Sprintf("UID:custom-%d@untis-api\r\n", periodID))
	// SEQUENCE follows the event's revision, so an edit raises it and the client
	// treats the VEVENT as updated. A real period keeps the class-wide version.
	b.WriteString(fmt.Sprintf("SEQUENCE:%d\r\n", customRevision(pd)))
	b.WriteString(fmt.Sprintf("DTSTART;TZID=%s:%s\r\n", tz, stT.Format("20060102T150405")))
	b.WriteString(fmt.Sprintf("DTEND;TZID=%s:%s\r\n", tz, enT.Format("20060102T150405")))
	b.WriteString("SUMMARY:" + icsEscape(summary) + "\r\n")
	if room, _ := pd["room"].(string); room != "" {
		b.WriteString("LOCATION:" + icsEscape(room) + "\r\n")
	}
	if len(desc) > 0 {
		b.WriteString("DESCRIPTION:" + icsEscape(strings.Join(desc, "\n")) + "\r\n")
	}
	// TRANSP:TRANSPARENT marks the event as not occupying the time. A hand-authored
	// appointment alongside real lessons should not be treated as a booked lesson
	// by a client that colours the calendar by availability.
	b.WriteString("TRANSP:TRANSPARENT\r\n")
	b.WriteString("CLASS:PUBLIC\r\n")
	b.WriteString("END:VEVENT\r\n")
	return b.String()
}

// customRevision reads the revision back off a synthesized period.
func customRevision(pd map[string]any) int64 {
	if v, ok := jsonID(pd["customRevision"]); ok && v > 0 {
		return v
	}
	return 1
}

// validDate reports whether s is a plain YYYY-MM-DD date.
func validDate(s string) bool {
	if len(s) != len("2006-01-02") {
		return false
	}
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

// responseSpan derives the date range a timetable response covers from the periods
// it contains. Used when the request did not carry usable dates: the events added
// then match the span the client actually received, which is the only range it has
// anywhere to draw.
func responseSpan(raw []byte) (string, string) {
	var res struct {
		Result struct {
			Timetable struct {
				Periods []struct {
					StartDateTime string `json:"startDateTime"`
				} `json:"periods"`
			} `json:"timetable"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", ""
	}
	var from, to string
	for _, pd := range res.Result.Timetable.Periods {
		d, ok := pd.StartDateTime, len(pd.StartDateTime) >= len("2006-01-02")
		if !ok {
			continue
		}
		day := d[:len("2006-01-02")]
		if !validDate(day) {
			continue
		}
		if from == "" || day < from {
			from = day
		}
		if to == "" || day > to {
			to = day
		}
	}
	return from, to
}

// responseLocation reads the UTC offset the response's periods are expressed in.
// Falling back to UTC is safe because a response with no readable period has no
// span either, so no event can be placed from it.
func responseLocation(raw []byte) *time.Location {
	var res struct {
		Result struct {
			Timetable struct {
				Periods []struct {
					StartDateTime string `json:"startDateTime"`
				} `json:"periods"`
			} `json:"timetable"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return time.UTC
	}
	for _, pd := range res.Result.Timetable.Periods {
		t, err := parsePeriodTime(pd.StartDateTime)
		if err != nil {
			continue
		}
		_, offset := t.Zone()
		return time.FixedZone("school", offset)
	}
	return time.UTC
}

// isCustomPeriod reports whether a period is a hand-authored event.
func isCustomPeriod(pd map[string]any) bool {
	_, ok := pd["customEventId"]
	return ok
}

// announceStudentEventChange tells a student's connected clients their schedule
// changed, without saying what changed.
//
// The signal goes only to that student's own subscribers. It deliberately does not
// enter the class outbox or the webhook/ntfy fan-out: those are read by everyone
// with access to the class, so a student's private appointment announced there
// would reach every classmate. An admin who wants a class-wide heads-up about a
// school event writes it as an entry in their own account instead.
func (p *Proxy) announceStudentEventChange(school, username, reason string) {
	if school == "" || username == "" || p.hub == nil {
		return
	}
	version, err := p.store.StudentEventVersion(school, username)
	if err != nil {
		enrichmentFailed("student event version "+username, err)
		return
	}
	p.hub.publishUser(school, username, studentEventMsg{
		School: school, Username: username, EventVersion: version, Reason: reason,
	})
}
