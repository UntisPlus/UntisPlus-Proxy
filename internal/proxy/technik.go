package proxy

// The synthetic Technik aggregate class.
//
// Hand-authored events are private: each row belongs to one student and is only
// ever attached to that student's own timetable. Coordinators still need to see
// them all at once, so the proxy offers one extra class — named "Technik" — whose
// timetable is the union of every student's events. It is a real class in every
// way the app cares about: it appears in masterData.klassen and is answered by
// getTimetable2017 like a class id.
//
// Nothing about it is public. The class is only added to the class list and only
// served to a user holding the "techniker" permission, checked on every request
// so granting or revoking it takes effect without a re-login. Everyone else's
// responses are byte-for-byte what they were before.

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"untis-proxy/internal/store"
)

// technikClassName is the class name shown in the app's class list.
const technikClassName = "Technik"

// technikClassID lives in the same out-of-band space as the synthesized subject,
// room and teacher elements, so it can never collide with a real class. It stays
// below 2^31 deliberately: a class id travels to mobile clients that may hold it
// in a signed 32-bit integer, and a value one past 2^31 would arrive there as a
// negative number the client could not send back. Class ids are their own
// namespace (synthesized *elements* are rooms/teachers/subjects), so sharing the
// base with them is harmless.
const technikClassID int64 = syntheticElementBase

// defaultSchoolTimezone is used to place an aggregate event when the request
// carries no periods to read an offset from. It matches the calendar feeds'
// fallback.
const defaultSchoolTimezone = "Europe/Berlin"

// hasTechniker reports whether a user holds the techniker permission.
func (p *Proxy) hasTechniker(username string) bool {
	return p.hasPerm(username, store.FeatureTechniker)
}

// injectTechnikClass adds the synthetic class to a masterData body when the user
// may see it. It is a wrapper around the existing rewrite so every return path —
// student, teacher, boosted — gets the same treatment.
func (p *Proxy) injectTechnikClass(username string, body []byte) []byte {
	if username == "" || !p.hasTechniker(username) {
		return body
	}
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	result, _ := m["result"].(map[string]any)
	if result == nil {
		return body
	}
	md, _ := result["masterData"].(map[string]any)
	if md == nil {
		return body
	}
	klassen, _ := md["klassen"].([]any)
	for _, k := range klassen {
		km, ok := k.(map[string]any)
		if !ok {
			continue
		}
		if id, ok := jsonID(km["id"]); ok && id == technikClassID {
			// Already present (e.g. the body was rewritten twice): leave it.
			return body
		}
	}
	md["klassen"] = append(klassen, technikClassStub(klassen))
	// Adding a class is a masterData change. The app caches masterData by its
	// timeStamp and ignores a body whose stamp it has already seen, so without
	// this a client that logged in before the permission was granted keeps a
	// class list with no Technik entry even after the proxy adds one.
	bumpMasterTimestamp(md)
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// technikClassStub builds the masterData entry for the aggregate class. The
// date span and department are copied from the first real class so the app's
// active/current-term filters treat it as an ordinary class rather than dropping
// it for having an empty span.
func technikClassStub(klassen []any) map[string]any {
	stub := map[string]any{
		"active":       true,
		"backColor":    nil,
		"departmentId": 0,
		"displayable":  true,
		"foreColor":    nil,
		"id":           technikClassID,
		"longName":     technikClassName,
		"name":         technikClassName,
		"startDate":    "",
		"endDate":      "",
	}
	for _, k := range klassen {
		km, ok := k.(map[string]any)
		if !ok {
			continue
		}
		if s, _ := km["startDate"].(string); s != "" {
			stub["startDate"] = s
		}
		if e, _ := km["endDate"].(string); e != "" {
			stub["endDate"] = e
		}
		if d, ok := km["departmentId"]; ok {
			stub["departmentId"] = d
		}
		break
	}
	return stub
}

// serveTechnikClass answers a getTimetable2017 request for the aggregate class.
// It is the one surface where events from several students are served together,
// which is why it is gated on the techniker permission rather than on being the
// subject of the request.
func (p *Proxy) serveTechnikClass(w http.ResponseWriter, id json.RawMessage, school string, requester *store.User, start, end string) {
	if requester == nil || !p.hasTechniker(requester.Username) {
		p.writeJSONRPCError(w, id, "no right for timetable", -8509)
		return
	}
	if !validDate(start) || !validDate(end) || end < start {
		p.writeJSONRPCError(w, id, "no right for timetable", -8509)
		return
	}
	loc := p.schoolLocation(school)
	md := p.rewrittenMasterData(school, requester.Username)
	// The elements are attached before the label is applied so the label can put
	// the event's own title back in the lesson cell: attachEventElements sets it
	// to the subject ("Technik" for every event), which would leave a class full
	// of identical rows.
	periods := p.technikClassPeriods(school, start, end, loc, md)
	for _, pd := range periods {
		// A home-grown period arrives with an empty `can` list; the app's UI keys
		// on it and hides the lesson topic without READ_LESSONTOPIC. Stamping the
		// requester's capability set makes the row readable, exactly as the other
		// serving paths do.
		p.capsInPeriod(requester.Username, pd)
	}
	if md != nil {
		stripHolidayDates(md, periodDates(periods))
	}
	out := map[string]any{
		"jsonrpc": "2.0",
		"id":      idVal(id),
		"result": map[string]any{
			"timetable": map[string]any{
				"displayableStartDate": start,
				"displayableEndDate":   end,
				"periods":              periods,
			},
		},
	}
	if md != nil {
		out["result"].(map[string]any)["masterData"] = md
	}
	w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(out)
	_, _ = w.Write(b)
}

// technikClassPeriods turns every student's events in the range into periods and
// labels each with the student it belongs to. The label is what makes the merged
// list readable: without it a class full of "Technik" lessons says nothing about
// whose appointment each one is.
func (p *Proxy) technikClassPeriods(school, from, to string, loc *time.Location, md jsonObject) []map[string]any {
	events, err := p.store.AllStudentEventsForRange(school, from, to)
	if err != nil {
		enrichmentFailed("technik events "+school, err)
		return nil
	}
	if len(events) == 0 {
		return nil
	}
	owners := make(map[int64]string, len(events))
	for _, ev := range events {
		owners[ev.ID] = p.studentLabel(ev.Username)
	}
	periods := customEventPeriods(events, loc)
	if md != nil {
		attachEventElements(periods, md, p.roomOverrides(school))
	}
	periods = mergeSharedPeriods(periods, owners)
	tagTechnikClass(periods)
	return periods
}

// tagTechnikClass adds a CLASS element pointing at the aggregate to every period,
// the way each upstream period carries the class it was served for. The class
// timetable view keeps only periods whose elements name the class being viewed,
// so without this reference the app receives the periods and displays none of
// them — an aggregate that looks empty even though the data arrived.
func tagTechnikClass(periods []map[string]any) {
	tagClassElement(periods, technikClassID)
}

// studentLabel resolves a stored (folded) event owner to a human name, falling
// back to the stored key when the account cannot be read. It is best-effort: a
// missing name must not hide the event.
func (p *Proxy) studentLabel(username string) string {
	if u, err := p.store.GetUser(username); err == nil && u != nil {
		if strings.TrimSpace(u.DisplayName) != "" {
			return u.DisplayName
		}
	}
	return username
}

// schoolLocation is the zone aggregate events are expressed in.
func (p *Proxy) schoolLocation(_ string) *time.Location {
	if loc, err := time.LoadLocation(defaultSchoolTimezone); err == nil {
		return loc
	}
	return time.UTC
}
