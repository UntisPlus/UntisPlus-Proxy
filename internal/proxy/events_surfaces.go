package proxy

// Sharing custom events beyond the two private surfaces.
//
// A hand-authored event belongs to one student, so it is attached to that
// student's own timetable and to the Technik aggregate. A coordinator also wants
// to see it where it happens: on the room's timetable and on the organiser's.
// Those two surfaces are shared, so an event is only ever added for a viewer who
// holds the techniker permission — the caller checks that — which is why a room
// timetable normally carries no events at all.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// elementEventPeriods returns the custom-event periods that belong on a room or
// teacher timetable: those whose name for that element resolves to the requested
// id. It is a pure filter — the events are attached to the response catalogue by
// appendEventPeriods, the same call the personal and aggregate surfaces use.
//
// The name is resolved exactly as attachEventElements resolves it, so a room
// pinned by an admin override and a room matched from the school catalogue agree.
// A nil masterData (a cold cache) yields nothing: without the catalogue a room
// name cannot be turned into the id the request named.
func (p *Proxy) elementEventPeriods(school, elType string, elID int64, from, to string, loc *time.Location, md jsonObject) []map[string]any {
	if md == nil || elID == 0 || (elType != "ROOM" && elType != "TEACHER") {
		return nil
	}
	events, err := p.store.AllStudentEventsForRange(school, from, to)
	if err != nil {
		enrichmentFailed("element events "+school, err)
		return nil
	}
	if len(events) == 0 {
		return nil
	}
	rooms := elementNameIndex(md["rooms"])
	teachers := elementNameIndex(md["teachers"])
	overrides := p.roomOverrides(school)
	periods := customEventPeriods(events, loc)
	owners := make(map[int64]string, len(events))
	for _, ev := range events {
		owners[ev.ID] = p.studentLabel(ev.Username)
	}
	kept := make([]map[string]any, 0, len(periods))
	for _, pd := range periods {
		if periodNamesElement(pd, elType, rooms, teachers, overrides) == elID {
			kept = append(kept, pd)
		}
	}
	return mergeSharedPeriods(kept, owners)
}

// periodNamesElement resolves the room or teacher text a period carries and
// returns the id it will reference, or 0 when it names no such element. It reads
// the same way attachEventElements does, without mutating the catalogue.
func periodNamesElement(pd map[string]any, elType string, rooms, teachers map[string]int64, overrides map[string]int64) int64 {
	for _, name := range splitElementNames(asString(pd[strings.ToLower(elType)])) {
		switch elType {
		case "ROOM":
			if id := resolveRoomID(name, rooms, overrides); id != 0 {
				return id
			}
		case "TEACHER":
			if id := resolveTeacherID(name, teachers); id != 0 {
				return id
			}
		}
	}
	return 0
}

// resolveRoomID is the id a room name will take once attached: an admin override,
// else the catalogue entry, else the synthetic id a new element gets.
func resolveRoomID(name string, rooms map[string]int64, overrides map[string]int64) int64 {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0
	}
	key := strings.ToLower(name)
	if id, ok := overrides[key]; ok && id != 0 {
		return id
	}
	if id, ok := rooms[key]; ok {
		return id
	}
	return syntheticElementID("ROOM", name)
}

// resolveTeacherID is the id a teacher name will take once attached: the
// catalogue entry, else the synthetic id a new element gets.
func resolveTeacherID(name string, teachers map[string]int64) int64 {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0
	}
	if id, ok := teachers[strings.ToLower(name)]; ok {
		return id
	}
	return syntheticElementID("TEACHER", name)
}

// masterDataOf peeks at result.masterData of a timetable body without keeping the
// rest. It is used to resolve element names against the catalogue the response
// itself will carry.
func masterDataOf(raw []byte) jsonObject {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	result, _ := m["result"].(map[string]any)
	if result == nil {
		return nil
	}
	md, _ := result["masterData"].(jsonObject)
	return md
}

// periodReferencesElement reports whether a period carries the given element in
// the `elements` list the app resolves names from.
func periodReferencesElement(pd map[string]any, elType string, elID int64) bool {
	for _, el := range jsonArray(pd["elements"]) {
		if asString(el["type"]) != elType {
			continue
		}
		if id, ok := jsonID(el["id"]); ok && id == elID {
			return true
		}
	}
	return false
}

// mergeSharedPeriods collapses events that are the same appointment staffed by
// several students into one period, then labels it with every assigned student.
//
// The import and the admin UI store one row per assigned technician, so a single
// event staffed by five people arrives as five near-identical events. On a
// personal timetable that is correct — a student sees their own row — but on a
// shared surface (the Technik class, a room, an organiser) it would draw the same
// appointment five times. Two events collapse only when every field a viewer sees
// matches (date, times, title, subject, room, teacher, description); anything
// that actually differs stays a separate row.
func mergeSharedPeriods(periods []map[string]any, owners map[int64]string) []map[string]any {
	if len(periods) == 0 {
		return periods
	}
	type group struct {
		pd     map[string]any
		labels []string
		seen   map[string]bool
	}
	index := make(map[string]int, len(periods))
	groups := make([]*group, 0, len(periods))
	for _, pd := range periods {
		key := sharedPeriodKey(pd)
		gi, ok := index[key]
		if !ok {
			gi = len(groups)
			index[key] = gi
			groups = append(groups, &group{pd: pd, seen: map[string]bool{}})
		}
		g := groups[gi]
		if evID, ok := jsonID(pd["customEventId"]); ok {
			if label := owners[evID]; label != "" && !g.seen[label] {
				g.seen[label] = true
				g.labels = append(g.labels, label)
			}
		}
	}
	if len(groups) == len(periods) {
		// Nothing collapsed; label in place.
		for _, g := range groups {
			labelSharedPeriod(g.pd, g.labels)
		}
		return periods
	}
	merged := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		labelSharedPeriod(g.pd, g.labels)
		merged = append(merged, g.pd)
	}
	return merged
}

// sharedPeriodKey is everything a viewer sees about an event except who is
// assigned; two events with the same key are one appointment.
func sharedPeriodKey(pd map[string]any) string {
	return fmt.Sprintf("%v\x00%v\x00%v\x00%s\x00%s\x00%s\x00%s\x00%s",
		pd["date"], pd["startTime"], pd["endTime"],
		strings.TrimSpace(asString(pd["customTitle"])),
		strings.TrimSpace(asString(pd["subject"])),
		strings.TrimSpace(asString(pd["room"])),
		strings.TrimSpace(asString(pd["teacher"])),
		strings.TrimSpace(asString(pd["description"])))
}

// labelSharedPeriod puts the event's own title in the lesson cell and the
// assigned students plus description in the info line, so a shared view reads as
// a schedule of who is where rather than a column of identical subjects.
func labelSharedPeriod(pd map[string]any, labels []string) {
	if len(labels) == 0 {
		return
	}
	joined := strings.Join(labels, ", ")
	pd["student"] = joined
	text, _ := pd["text"].(map[string]any)
	if text == nil {
		return
	}
	if title := strings.TrimSpace(asString(pd["customTitle"])); title != "" {
		text["lesson"] = title
	}
	if desc := strings.TrimSpace(asString(pd["description"])); desc != "" {
		text["info"] = joined + " · " + desc
	} else {
		text["info"] = joined
	}
}
