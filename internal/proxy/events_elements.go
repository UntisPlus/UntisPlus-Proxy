package proxy

// The app places a synthesized event by the elements it references — a period
// that references nothing is dropped — and it resolves each element's name from
// the masterData in the same response. An admin types plain text (a subject, a
// room, an organizer), so this file turns that text into element references,
// inventing a catalogue entry for any name the school does not already have.

import (
	"hash/fnv"
	"strings"
)

// customSubjectDefault is the lesson an event shows when the admin did not name
// one. These events are Technik appointments, so "Technik" is the sensible
// default; it stays editable per event.
const customSubjectDefault = "Technik"

// syntheticElementBase keeps ids invented here out of the range of real Untis
// ids (which are small positive numbers). An id in this space can never be one
// the school itself issued, so a synthesized element cannot shadow a real one.
const syntheticElementBase int64 = 2_000_000_000

// attachEventElements gives every custom period the SUBJECT, ROOM and TEACHER
// elements the app resolves, and adds a catalogue entry to masterData for each
// name that is not already there. It reports whether masterData changed, which
// the caller turns into a timestamp bump so a cached client refetches.
//
// roomOverrides pins a room name to an id the admin configured (see roommap.go);
// it wins over the catalogue and is consulted only for rooms.
//
// The subject is always attached (defaulted to "Technik") because that is the
// lesson the app shows; room and teacher are attached only when the event names
// one. An event is still served with no masterData at all, just without elements.
func attachEventElements(periods []map[string]any, md jsonObject, roomOverrides map[string]int64) bool {
	if len(periods) == 0 {
		return false
	}
	subjects := elementNameIndex(md["subjects"])
	rooms := elementNameIndex(md["rooms"])
	roomIDs := elementIDSet(md["rooms"])
	teachers := elementNameIndex(md["teachers"])
	changed := false

	for _, pd := range periods {
		subject := strings.TrimSpace(asString(pd["subject"]))
		if subject == "" {
			subject = customSubjectDefault
		}
		elems := make([]any, 0, 4)
		if id, added := ensureElement(md, "SUBJECT", "subjects", subject, subjects); id != 0 {
			elems = append(elems, elementRef("SUBJECT", id))
			changed = changed || added
		}
		for _, name := range splitElementNames(asString(pd["room"])) {
			if id, added := resolveRoom(md, name, rooms, roomIDs, roomOverrides); id != 0 {
				elems = append(elems, elementRef("ROOM", id))
				changed = changed || added
			}
		}
		for _, name := range splitElementNames(asString(pd["teacher"])) {
			if id, added := ensureElement(md, "TEACHER", "teachers", name, teachers); id != 0 {
				elems = append(elems, elementRef("TEACHER", id))
				changed = changed || added
			}
		}
		pd["elements"] = elems
		// Keep the lesson cell on the resolved subject even for an event stored
		// before the subject field, or imported without one.
		if text, ok := pd["text"].(map[string]any); ok {
			text["lesson"] = subject
		}
	}
	return changed
}

// elementNameIndex maps a masterData list's lowercased short and long names to
// ids.
//
// A name can resolve to more than one id: a school keeps retired elements
// alongside the current ones, and a short name and a long name can meet (room
// "01-Aula" with long name "Aula" versus a retired room plain "Aula"). An event
// must reference the id the *real* lessons use, or it lands under a second room
// with the same name and looks empty. Active entries therefore win over inactive
// ones regardless of list order, and among equals the first stays — stated as a
// rule rather than left to whatever order upstream happened to use.
func elementNameIndex(v any) map[string]int64 {
	idx := map[string]int64{}
	active := map[string]bool{}
	for _, obj := range jsonArray(v) {
		id, ok := jsonID(obj["id"])
		if !ok || id == 0 {
			continue
		}
		isActive := true
		if a, ok := obj["active"].(bool); ok {
			isActive = a
		}
		for _, name := range []string{asString(obj["name"]), asString(obj["longName"])} {
			key := strings.ToLower(strings.TrimSpace(name))
			if key == "" {
				continue
			}
			if _, seen := idx[key]; !seen || (isActive && !active[key]) {
				idx[key] = id
				active[key] = isActive
			}
		}
	}
	return idx
}

// ensureElement resolves a name to an id, inventing and appending a catalogue
// entry when the school has none. It reports the id and whether it added an
// entry (so the caller knows masterData changed).
func ensureElement(md jsonObject, elType, listKey, name string, idx map[string]int64) (int64, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, false
	}
	key := strings.ToLower(name)
	if id, ok := idx[key]; ok {
		return id, false
	}
	id := syntheticElementID(elType, name)
	idx[key] = id
	md[listKey] = append(jsonArray(md[listKey]), syntheticElement(elType, id, name))
	return id, true
}

// resolveRoom turns an event's room text into an id. An admin-configured
// override wins, because it exists precisely to name the id the real lessons use
// when the catalogue cannot be trusted to. The override's room is added to the
// response catalogue when the school does not already list that id, so the app
// can still resolve a name for it; otherwise the catalogue (active entry first)
// decides, and an unknown name is synthesized as usual.
func resolveRoom(md jsonObject, name string, idx map[string]int64, ids map[int64]bool, overrides map[string]int64) (int64, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, false
	}
	key := strings.ToLower(name)
	if id, ok := overrides[key]; ok && id != 0 {
		idx[key] = id
		if !ids[id] {
			ids[id] = true
			md["rooms"] = append(jsonArray(md["rooms"]), syntheticElement("ROOM", id, name))
			return id, true
		}
		return id, false
	}
	return ensureElement(md, "ROOM", "rooms", name, idx)
}

// elementIDSet collects the ids a masterData list already carries, so a room can
// be checked for existence by id rather than by name.
func elementIDSet(v any) map[int64]bool {
	ids := map[int64]bool{}
	for _, obj := range jsonArray(v) {
		if id, ok := jsonID(obj["id"]); ok && id != 0 {
			ids[id] = true
		}
	}
	return ids
}

// syntheticElementID derives a stable id for an invented element. It depends only
// on the type and the name, so the same event served twice references the same
// id.
func syntheticElementID(elType, name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(elType))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(strings.ToLower(strings.TrimSpace(name))))
	return syntheticElementBase + int64(h.Sum64()%1_000_000_000)
}

// elementRef is the reference shape a period carries.
func elementRef(elType string, id int64) map[string]any {
	return map[string]any{"type": elType, "id": id, "orgId": id}
}

// tagClassElement prepends a CLASS reference to every period, the way upstream
// names the class a period belongs to. A client view scoped to a class keeps
// only the periods whose elements name that class, so a synthesized period
// without this reference is delivered but rendered nowhere. A zero id is a
// no-op: an owner without a known class must not fabricate one.
func tagClassElement(periods []map[string]any, classID int64) {
	if classID == 0 {
		return
	}
	ref := elementRef("CLASS", classID)
	for _, pd := range periods {
		els, _ := pd["elements"].([]any)
		pd["elements"] = append([]any{ref}, els...)
	}
}

// syntheticElement builds a masterData catalogue entry in the same shape upstream
// sends, so the app resolves the name exactly as it would a real element.
func syntheticElement(elType string, id int64, name string) map[string]any {
	switch elType {
	case "SUBJECT":
		return map[string]any{
			"active": true, "backColor": nil, "departmentIds": []any{},
			"displayAllowed": true, "foreColor": nil, "id": id,
			"longName": name, "name": name,
		}
	case "ROOM":
		return map[string]any{
			"active": true, "backColor": nil, "departmentId": 0,
			"displayAllowed": true, "foreColor": nil, "id": id,
			"longName": name, "name": name,
		}
	case "TEACHER":
		return map[string]any{
			"active": true, "backColor": nil, "departmentIds": []any{},
			"displayAllowed": true, "entryDate": nil, "exitDate": nil,
			"firstName": "", "foreColor": nil, "id": id,
			"lastName": name, "name": name,
		}
	}
	return map[string]any{"active": true, "displayAllowed": true, "id": id, "longName": name, "name": name}
}

// splitElementNames splits an admin's free text into the separate names it holds.
// A comma joins several rooms or teachers; a slash joins the same in the transit
// calendar's "Veranstalter" column ("Abc/Def").
func splitElementNames(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	fields := strings.FieldsFunc(s, func(r rune) bool {
		switch r {
		case ',', '/', ';', '|':
			return true
		}
		return false
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// asString reads a string field without panicking on a missing or non-string one.
func asString(v any) string {
	s, _ := v.(string)
	return s
}
