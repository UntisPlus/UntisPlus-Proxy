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
	"strings"
	"time"

	"untis-proxy/internal/store"
)

// customEventField marks a period as hand-authored. It is the only thing that
// distinguishes an event from a real lesson on the wire, and it is present on
// every surface rather than derived per renderer, so the app, the .ics feed and
// the /week page all agree.
const customEventField = "isCustom"

// customEventIDSpace keeps synthetic ids from colliding with real period ids.
//
// Every surface keys on `id`, and the ICS feed puts the raw id in a UID. A real
// period id is a small number, so negative ids are free — and being negative
// rather than large means an id that has accidentally been left at zero, or that
// arrived from a caller who did not set one, still cannot masquerade as a real
// lesson. The event's own revision is folded into the low bits so an edited event
// is a different id, which is what an ICS client needs to treat it as a new
// VEVENT rather than an unchanged one.
const customEventIDSpace = -1_000_000_000

// customEventID derives the synthetic id for one event revision.
func customEventID(ev store.StudentEvent) int64 {
	if ev.Revision < 1 {
		return customEventIDSpace - ev.ID
	}
	return customEventIDSpace - ev.ID - (ev.Revision-1)*1_000_000
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
		out = append(out, map[string]any{
			"id":                customEventID(ev),
			"startDateTime":     start.Format("2006-01-02T15:04Z07:00"),
			"endDateTime":       end.Format("2006-01-02T15:04Z07:00"),
			customEventField:    true,
			"customEventId":     ev.ID,
			"customRevision":    ev.Revision,
			"customTitle":       ev.Title,
			"subject":           ev.Subject,
			"teacher":           ev.Teacher,
			"room":              ev.Room,
			"description":       ev.Description,
			"date":              dateInt(ev.Date),
			"startTime":         hmInt(start),
			"endTime":           hmInt(end),
			"lessonText":        ev.Title,
			"lessonCode":        "CUSTOM",
			"lessonNumber":      0,
			"is":                map[string]any{"standard": false, "event": true},
			"cellState":         "CUSTOM",
			"code":              0,
			"priority":          5,
			"hasInfo":           ev.Description != "",
			"periodAttachments": []any{},
			"elements":          []any{},
			"debugInfo":         fmt.Sprintf("custom,%d", ev.ID),
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
	return customEventPeriods(events, loc)
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
	// The range read is half-open, and a response spanning one day collapses to
	// from == to, which would query an empty range and drop the day's events.
	if from == to {
		if t, err := time.ParseInLocation("2006-01-02", from, time.UTC); err == nil {
			to = t.AddDate(0, 0, 1).Format("2006-01-02")
		}
	}
	periods := p.studentEventPeriods(school, username, from, to, loc)
	if len(periods) == 0 {
		return raw
	}
	return enrichJSON(raw, func(result jsonObject) {
		tt, ok := result["timetable"].(jsonObject)
		if !ok {
			return
		}
		raw, _ := tt["periods"].([]any)
		// jsonArray drops any entry that is not an object, so it is used only to
		// check. Rebuilding the list from the original slice keeps every entry
		// upstream sent: a period list is always all objects, but a response
		// carrying something else would otherwise have it silently deleted, which
		// is the one failure this module must never cause.
		if len(jsonArray(tt["periods"])) != len(raw) {
			return
		}
		merged := make([]any, 0, len(raw)+len(periods))
		merged = append(merged, raw...)
		for _, ev := range periods {
			merged = append(merged, ev)
		}
		tt["periods"] = merged
	})
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
	v, _ := pd[customEventField].(bool)
	return v
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
