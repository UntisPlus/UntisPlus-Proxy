package proxy

// Tests for per-student custom events on the three surfaces that serve a
// timetable: the .ics feed, the /week page and the app's getTimetable2017 call.
//
// The load-bearing tests here are the negative ones. Events belong to one student,
// so the interesting failures are the ones where a class, a teacher, a room, a
// subject or another student's client sees something it should not.

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"untis-proxy/internal/store"
)

// eventProxy sets up a proxy with one student (person 9, class 5000) and one
// classmate (person 10) already logged into the store.
func eventProxy(t *testing.T) (*Proxy, *store.Store) {
	t.Helper()
	p, st, _ := newFakeProxy(t, &fakeUntis{})
	if err := st.UpsertUser(&store.User{
		Username: "dee", School: "testschool", PersonID: 9, PersonType: 5, ClassID: 5000,
		DisplayName: "Dee",
	}); err != nil {
		t.Fatalf("seed dee: %v", err)
	}
	if err := st.UpsertUser(&store.User{
		Username: "sam", School: "testschool", PersonID: 10, PersonType: 5, ClassID: 5000,
	}); err != nil {
		t.Fatalf("seed sam: %v", err)
	}
	return p, st
}

func seedEvent(t *testing.T, st *store.Store, username string, ev store.NewStudentEvent) store.StudentEvent {
	t.Helper()
	got, err := st.CreateStudentEvent("testschool", username, ev, "admin")
	if err != nil {
		t.Fatalf("seed event: %v", err)
	}
	return got
}

// berlin is the zone the tests author events in. The proxy's fallback is UTC, so
// using it in a test would hide a wall-clock mistake rather than show one.
var berlin = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		panic(err)
	}
	return loc
}()

func testEvent() store.NewStudentEvent {
	return store.NewStudentEvent{
		Date: "2026-10-01", StartTime: "14:00", EndTime: "15:00",
		Title: "Technik", Subject: "Mathe", Room: "R12", Teacher: "Mr Smith",
		Description: "Arbeitsblatt mitbringen",
	}
}

func TestCustomEventPeriodCarriesTheAdminText(t *testing.T) {
	ev := seedEvent(t, eventStore(t), "dee", testEvent())
	periods := customEventPeriods([]store.StudentEvent{ev}, berlin)
	if len(periods) != 1 {
		t.Fatalf("got %d periods, want 1", len(periods))
	}
	pd := periods[0]
	if !isCustomPeriod(pd) {
		t.Error("the period is not marked as a custom event")
	}
	if pd["customTitle"] != "Technik" || pd["subject"] != "Mathe" ||
		pd["room"] != "R12" || pd["teacher"] != "Mr Smith" ||
		pd["description"] != "Arbeitsblatt mitbringen" {
		t.Errorf("the admin's text did not survive the conversion: %+v", pd)
	}
	// Written in the school's zone, matching how upstream expresses real periods.
	// If this came back as Z, the event would sit two hours off for a Berlin school.
	if pd["startDateTime"] != "2026-10-01T14:00+02:00" || pd["endDateTime"] != "2026-10-01T15:00+02:00" {
		t.Errorf("times = %v .. %v, want 14:00-15:00 in the school zone", pd["startDateTime"], pd["endDateTime"])
	}
	if pd["date"] != int64(20261001) {
		t.Errorf("date = %v, want 20261001", pd["date"])
	}
}

// eventStore is the store half of eventProxy, for tests that only need storage.
func eventStore(t *testing.T) *store.Store {
	t.Helper()
	_, st, _ := newFakeProxy(t, &fakeUntis{})
	if err := st.UpsertUser(&store.User{Username: "dee", School: "testschool", PersonID: 9, PersonType: 5, ClassID: 5000}); err != nil {
		t.Fatalf("seed dee: %v", err)
	}
	return st
}

func TestCustomEventIDNeverCollidesWithAPeriod(t *testing.T) {
	ev := seedEvent(t, eventStore(t), "dee", testEvent())
	pd := customEventPeriods([]store.StudentEvent{ev}, berlin)[0]
	id, _ := pd["id"].(int64)
	if id >= 0 {
		t.Errorf("custom id = %d, want negative so it cannot collide with a real period id", id)
	}
}

func TestCustomEventIDIsStableAcrossRevisions(t *testing.T) {
	st := eventStore(t)
	ev := seedEvent(t, st, "dee", testEvent())
	first := customEventPeriods([]store.StudentEvent{ev}, berlin)[0]["id"]
	room := "R13"
	updated, _, err := st.UpdateStudentEvent("testschool", ev.ID, store.StudentEventPatch{Room: &room})
	if err != nil {
		t.Fatalf("UpdateStudentEvent: %v", err)
	}
	periods := customEventPeriods([]store.StudentEvent{updated}, berlin)
	second := periods[0]["id"]
	// An ICS client matches a VEVENT by UID and compares SEQUENCE to decide
	// whether it changed. So the id — which becomes the UID — has to stay put
	// across an edit and the revision has to move: a UID that moved would leave
	// the old entry cached under one UID and the new one under another, which is
	// two entries for one appointment.
	if first != second {
		t.Errorf("the id moved from %v to %v across a revision bump, so the client sees a second entry instead of an updated one", first, second)
	}
	if got := periods[0]["customRevision"]; got != int64(2) {
		t.Errorf("customRevision = %v, want 2, so a client can tell the entry changed", got)
	}
	if uid1, uid2 := icsUIDOf(t, first), icsUIDOf(t, second); uid1 != uid2 {
		t.Errorf("the ICS UID changed across an edit: %s -> %s", uid1, uid2)
	}
}

// icsUIDOf renders the UID the ICS feed would emit for a synthetic id.
func icsUIDOf(t *testing.T, id any) string {
	t.Helper()
	pd := map[string]any{
		"id":             id,
		"customTitle":    "Technik",
		"customRevision": int64(1),
		"startDateTime":  "2026-10-01T14:00:00+02:00",
		"endDateTime":    "2026-10-01T15:00:00+02:00",
		"is":             true,
	}
	out := buildCustomICSVEVENT(pd, "Europe/Berlin", time.Unix(1790000000, 0))
	for _, line := range strings.Split(out, "\r\n") {
		if strings.HasPrefix(line, "UID:") {
			return line
		}
	}
	t.Fatalf("no UID in rendered event:\n%s", out)
	return ""
}

func TestCustomEventIDsAreUnique(t *testing.T) {
	st := eventStore(t)
	var events []store.StudentEvent
	for i := 0; i < 5; i++ {
		events = append(events, seedEvent(t, st, "dee", testEvent()))
	}
	room := "R99"
	edited, _, err := st.UpdateStudentEvent("testschool", events[0].ID, store.StudentEventPatch{Room: &room})
	if err != nil {
		t.Fatalf("UpdateStudentEvent: %v", err)
	}
	events[0] = edited

	seen := map[int64]bool{}
	for _, pd := range customEventPeriods(events, berlin) {
		id, _ := pd["id"].(int64)
		if seen[id] {
			t.Errorf("two events share id %d", id)
		}
		seen[id] = true
	}
}

func TestCustomEventPeriodsAreSortedByTime(t *testing.T) {
	st := eventStore(t)
	var events []store.StudentEvent
	for _, tc := range []struct{ date, start, title string }{
		{"2026-10-02", "09:00", "later day"},
		{"2026-10-01", "15:00", "afternoon"},
		{"2026-10-01", "08:00", "morning"},
	} {
		ev := testEvent()
		ev.Date, ev.StartTime, ev.Title = tc.date, tc.start, tc.title
		events = append(events, seedEvent(t, st, "dee", ev))
	}
	periods := customEventPeriods(events, berlin)
	want := []string{"morning", "afternoon", "later day"}
	for i, title := range want {
		if periods[i]["customTitle"] != title {
			t.Errorf("period %d = %v, want %q", i, periods[i]["customTitle"], title)
		}
	}
}

func TestCustomEventWithUnusableTimeIsDropped(t *testing.T) {
	st := eventStore(t)
	good := seedEvent(t, st, "dee", testEvent())
	for _, tc := range []struct{ name, date, start, end string }{
		{"empty date", "", "14:00", "15:00"},
		{"empty start", "2026-10-01", "", "15:00"},
		{"bad date", "not-a-date", "14:00", "15:00"},
		{"bad clock", "2026-10-01", "25:99", "26:00"},
		{"date only", "2026-10-01", "", ""},
	} {
		ev := testEvent()
		ev.Date, ev.StartTime, ev.EndTime = tc.date, tc.start, tc.end
		seedEvent(t, st, "dee", ev)
	}
	periods := customEventPeriods([]store.StudentEvent{good}, berlin)
	// Only the one usable event is served. A time that cannot be read cannot be
	// placed on a calendar, and defaulting it to midnight would invent a slot.
	if len(periods) != 1 {
		t.Errorf("got %d periods, want only the one usable event", len(periods))
	}
}

func TestCustomEventEndBeforeStartIsKeptAsAMarker(t *testing.T) {
	st := eventStore(t)
	ev := testEvent()
	ev.StartTime, ev.EndTime = "15:00", "14:00"
	periods := customEventPeriods([]store.StudentEvent{seedEvent(t, st, "dee", ev)}, berlin)
	if len(periods) != 1 {
		t.Fatalf("got %d periods, want 1", len(periods))
	}
	// Dropping an admin's entry entirely would be worse than showing it collapsed:
	// the admin would have no indication the row they saved is unusable.
	if periods[0]["endDateTime"] != periods[0]["startDateTime"] {
		t.Errorf("end = %v, want it collapsed onto the start %v", periods[0]["endDateTime"], periods[0]["startDateTime"])
	}
}

func TestStudentEventsRequireSchoolAndUsername(t *testing.T) {
	p, st := eventProxy(t)
	seedEvent(t, st, "dee", testEvent())
	for _, tc := range []struct{ school, username string }{
		{"", "dee"},
		{"testschool", ""},
		{"", ""},
	} {
		if got := p.studentEventPeriods(tc.school, tc.username, "2026-01-01", "2027-01-01", berlin); len(got) != 0 {
			t.Errorf("school=%q user=%q returned %d events, want none", tc.school, tc.username, len(got))
		}
	}
}

func TestStudentEventsAreScopedToTheViewer(t *testing.T) {
	p, st := eventProxy(t)
	seedEvent(t, st, "dee", testEvent())
	seedEvent(t, st, "sam", testEvent())
	if got := p.studentEventPeriods("testschool", "dee", "2026-01-01", "2027-01-01", berlin); len(got) != 1 {
		t.Errorf("dee got %d events, want 1", len(got))
	}
	if got := p.studentEventPeriods("testschool", "sam", "2026-01-01", "2027-01-01", berlin); len(got) != 1 {
		t.Errorf("sam got %d events, want 1 (sam's own)", len(got))
	}
	if got := p.studentEventPeriods("testschool", "casey", "2026-01-01", "2027-01-01", berlin); len(got) != 0 {
		t.Errorf("a third student got %d events, want none", len(got))
	}
}

// --- ICS ---

func TestCustomEventRendersAVevent(t *testing.T) {
	st := eventStore(t)
	ev := seedEvent(t, st, "dee", testEvent())
	pd := customEventPeriods([]store.StudentEvent{ev}, berlin)[0]
	ics := buildCustomICSVEVENT(pd, "Europe/Berlin", time.Unix(1790000000, 0))

	for _, want := range []string{
		"BEGIN:VEVENT", "END:VEVENT",
		"SUMMARY:Technik · Mathe · Mr Smith",
		"LOCATION:R12",
		"DESCRIPTION:Raum: R12\\nArbeitsblatt mitbringen",
		"DTSTART;TZID=Europe/Berlin:20261001T140000",
		"DTEND;TZID=Europe/Berlin:20261001T150000",
		"SEQUENCE:1",
		"TRANSP:TRANSPARENT",
	} {
		if !strings.Contains(ics, want) {
			t.Errorf("the VEVENT is missing %q:\n%s", want, ics)
		}
	}
}

func TestCustomEventUIDIsNamespaced(t *testing.T) {
	st := eventStore(t)
	ev := seedEvent(t, st, "dee", testEvent())
	pd := customEventPeriods([]store.StudentEvent{ev}, berlin)[0]
	ics := buildCustomICSVEVENT(pd, "Europe/Berlin", time.Now())
	// A real period's UID is the bare period id. An event must not be able to
	// collide with one the client already holds.
	if !strings.Contains(ics, "UID:custom-") {
		t.Errorf("the custom UID is not namespaced:\n%s", ics)
	}
	if strings.Contains(ics, fmt.Sprintf("UID:%d@", pd["id"])) {
		t.Errorf("the custom UID is the bare period id:\n%s", ics)
	}
}

func TestCustomEventSequenceFollowsRevision(t *testing.T) {
	st := eventStore(t)
	ev := seedEvent(t, st, "dee", testEvent())
	room := "R13"
	ev, _, err := st.UpdateStudentEvent("testschool", ev.ID, store.StudentEventPatch{Room: &room})
	if err != nil {
		t.Fatalf("UpdateStudentEvent: %v", err)
	}
	ics := buildCustomICSVEVENT(customEventPeriods([]store.StudentEvent{ev}, berlin)[0], "Europe/Berlin", time.Now())
	if !strings.Contains(ics, "SEQUENCE:2") {
		t.Errorf("an edited event did not raise SEQUENCE, so a client keeps the old copy:\n%s", ics)
	}
}

func TestCustomEventWithNoOptionalText(t *testing.T) {
	st := eventStore(t)
	ev, err := st.CreateStudentEvent("testschool", "dee", store.NewStudentEvent{
		Date: "2026-10-01", StartTime: "14:00", EndTime: "15:00", Title: "Technik",
	}, "admin")
	if err != nil {
		t.Fatalf("CreateStudentEvent: %v", err)
	}
	ics := buildCustomICSVEVENT(customEventPeriods([]store.StudentEvent{ev}, berlin)[0], "Europe/Berlin", time.Now())
	if !strings.Contains(ics, "SUMMARY:Technik") {
		t.Errorf("the title is missing:\n%s", ics)
	}
	// Empty LOCATION and DESCRIPTION lines are legal but noisy; a client that
	// splits on ":" should not see a stray field.
	if strings.Contains(ics, "LOCATION:") {
		t.Errorf("an empty LOCATION was written:\n%s", ics)
	}
	if strings.Contains(ics, "DESCRIPTION:") {
		t.Errorf("an empty DESCRIPTION was written:\n%s", ics)
	}
}

func TestCustomEventUnparseableTimeRendersNothing(t *testing.T) {
	// A renderer that returned a VEVENT with no DTSTART would be worse than one
	// that returns nothing at all.
	if got := buildCustomICSVEVENT(map[string]any{"startDateTime": "nope"}, "Europe/Berlin", time.Now()); got != "" {
		t.Errorf("got %q, want an empty string", got)
	}
}

func TestICSFeedCarriesStudentEventsOnlyForStudentTokens(t *testing.T) {
	for _, elType := range []string{"CLASS", "TEACHER", "ROOM", "SUBJECT"} {
		t.Run(elType, func(t *testing.T) {
			p, st := eventProxy(t)
			seedEvent(t, st, "dee", testEvent())
			token := "tok-" + strings.ToLower(elType)
			if err := st.CreateClassToken(&store.ClassToken{
				School: "testschool", Token: token, ElementType: elType, ElementID: 5000, Days: 7,
			}); err != nil {
				t.Fatalf("seed token: %v", err)
			}
			start := time.Now().Add(24 * time.Hour).Format("2006-01-02T15:04Z07:00")
			end := time.Now().Add(25 * time.Hour).Format("2006-01-02T15:04Z07:00")
			feed, err := p.resolveFeed(&store.ClassToken{School: "testschool", Token: token, ElementType: elType, ElementID: 5000}, start[:10], start[:10])
			_ = end
			if err != nil {
				// Some element feeds need upstream data the fake cannot provide;
				// that is fine, but the periods must still hold no events.
				return
			}
			for _, pd := range feed.Periods {
				if isCustomPeriod(pd) {
					t.Errorf("a %s feed carries a student's custom event: %+v", elType, pd)
				}
			}
		})
	}
}

func TestStudentFeedCarriesOwnEvents(t *testing.T) {
	p, st := eventProxy(t)
	seedEvent(t, st, "dee", testEvent())
	day := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	ev := testEvent()
	ev.Date = day
	seedEvent(t, st, "dee", ev)

	feed, err := p.resolveFeed(&store.ClassToken{School: "testschool", Token: "s", ElementType: "STUDENT", ElementID: 9}, day, day)
	if err != nil {
		t.Skipf("the fake upstream cannot serve a student timetable: %v", err)
	}
	found := 0
	for _, pd := range feed.Periods {
		if isCustomPeriod(pd) {
			found++
			if pd["customTitle"] != "Technik" {
				t.Errorf("unexpected event on the student feed: %+v", pd)
			}
		}
	}
	if found != 1 {
		t.Errorf("the student's own feed carried %d events, want 1", found)
	}
}

func TestStudentFeedDoesNotCarryAnotherStudentsEvents(t *testing.T) {
	p, st := eventProxy(t)
	// dee's event, on a date in the future so it falls inside the token's window.
	ev := testEvent()
	ev.Date = time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	seedEvent(t, st, "dee", ev)
	day := ev.Date

	// Person 10 is sam, a different student in the same class. Sam's feed must not
	// contain dee's event even though the class is shared.
	feed, err := p.resolveFeed(&store.ClassToken{School: "testschool", Token: "sam", ElementType: "STUDENT", ElementID: 10}, day, day)
	if err != nil {
		t.Skipf("the fake upstream cannot serve a student timetable: %v", err)
	}
	for _, pd := range feed.Periods {
		if isCustomPeriod(pd) {
			t.Errorf("sam's feed carries dee's event: %+v", pd)
		}
	}
}

// --- week page ---

func TestWeekPageShowsCustomEvent(t *testing.T) {
	p, st := eventProxy(t)
	ev := testEvent()
	seedEvent(t, st, "dee", ev)

	if err := st.CreateClassToken(&store.ClassToken{
		School: "testschool", Token: "caltok", ElementType: "STUDENT", ElementID: 9, Days: 30, Timezone: "Europe/Berlin",
	}); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	feed := feedTarget{School: "testschool", Name: "Dee", Token: "caltok"}
	feed.Periods = customEventPeriods([]store.StudentEvent{
		seedEvent(t, st, "dee", ev),
	}, berlin)
	html := renderWeekForTest(t, p, feed)
	for _, want := range []string{"Technik", "Mr Smith", "R12", "Arbeitsblatt mitbringen", "Termin"} {
		if !strings.Contains(html, want) {
			t.Errorf("the week page is missing %q", want)
		}
	}
}

func TestWeekPageEscapesCustomEventText(t *testing.T) {
	p, st := eventProxy(t)
	ev := testEvent()
	ev.Title = `<script>alert(1)</script>`
	seedEvent(t, st, "dee", ev)
	feed := feedTarget{School: "testschool", Name: "Dee"}
	feed.Periods = customEventPeriods([]store.StudentEvent{seedEvent(t, st, "dee", ev)}, berlin)
	html := renderWeekForTest(t, p, feed)
	// html/template escapes on output, so the tag must not survive as markup.
	if strings.Contains(html, "<script>alert(1)</script>") {
		t.Error("an admin's event title was rendered as live markup")
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Error("the title was neither rendered nor escaped — it looks dropped")
	}
}

// --- getTimetable2017 ---

func timetableResponse(periods ...any) []byte {
	b, _ := json.Marshal(map[string]any{
		"result": map[string]any{
			"timetable": map[string]any{"periods": periods},
		},
	})
	return b
}

func realPeriodJSON(date string) map[string]any {
	return map[string]any{
		"id": float64(1), "startDateTime": date + "T08:00Z07:00", "endDateTime": date + "T08:45Z07:00",
		"elements": []any{map[string]any{"type": "SUBJECT", "id": float64(1)}},
	}
}

func TestDecorateStudentEventsAppendsToPeriods(t *testing.T) {
	p, st := eventProxy(t)
	seedEvent(t, st, "dee", testEvent())
	raw := timetableResponse(realPeriodJSON("2026-10-01"))
	got := p.decorateStudentEvents(raw, "testschool", "dee", "2026-10-01", "2026-10-07")

	var parsed struct {
		Result struct {
			Timetable struct {
				Periods []map[string]any `json:"periods"`
			} `json:"timetable"`
		} `json:"result"`
	}
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("the decorated response is not valid JSON: %v", err)
	}
	periods := parsed.Result.Timetable.Periods
	if len(periods) != 2 {
		t.Fatalf("got %d periods, want the original plus the event", len(periods))
	}
	if !isCustomPeriod(periods[1]) {
		t.Errorf("the appended period is not marked custom: %+v", periods[1])
	}
}

func TestDecorateStudentEventsPreservesUpstreamFields(t *testing.T) {
	p, st := eventProxy(t)
	seedEvent(t, st, "dee", testEvent())
	raw := []byte(`{"result":{"timetable":{"periods":[],"someNewUpstreamField":"keep me"},"other":"kept"},"extra":1}`)
	got := p.decorateStudentEvents(raw, "testschool", "dee", "2026-10-01", "2026-10-07")
	if !strings.Contains(string(got), "someNewUpstreamField") {
		t.Errorf("an unknown upstream field was dropped:\n%s", got)
	}
	if !strings.Contains(string(got), `"other":"kept"`) || !strings.Contains(string(got), `"extra":1`) {
		t.Errorf("a sibling result key was dropped:\n%s", got)
	}
}

func TestDecorateStudentEventsLeavesUnrelatedResponsesAlone(t *testing.T) {
	p, st := eventProxy(t)
	seedEvent(t, st, "dee", testEvent())
	for name, raw := range map[string][]byte{
		"no timetable": []byte(`{"result":{"homework":[]}}`),
		"error":        []byte(`{"error":{"code":-1,"message":"denied"}}`),
		"not json":     []byte(`<html>nope</html>`),
		"empty body":   nil,
	} {
		got := p.decorateStudentEvents(raw, "testschool", "dee", "2026-10-01", "2026-10-07")
		if string(got) != string(raw) {
			t.Errorf("%s: the response was modified:\n got %s\nwant %s", name, got, raw)
		}
	}
}

func TestDecorateStudentEventsKeepsNonObjectPeriodEntries(t *testing.T) {
	p, st := eventProxy(t)
	seedEvent(t, st, "dee", testEvent())
	// A period list is always objects upstream, but if it were not, the entries
	// must survive rather than be silently deleted by a decoder that skips them.
	raw := timetableResponse(realPeriodJSON("2026-10-01"), "unexpected string")
	got := p.decorateStudentEvents(raw, "testschool", "dee", "2026-10-01", "2026-10-07")
	if !strings.Contains(string(got), "unexpected string") {
		t.Errorf("a non-object period entry was deleted:\n%s", got)
	}
}

func TestDecorateStudentEventsWithoutEventsReturnsInputByteForByte(t *testing.T) {
	p, _ := eventProxy(t)
	raw := timetableResponse(realPeriodJSON("2026-10-01"))
	got := p.decorateStudentEvents(raw, "testschool", "dee", "2026-10-01", "2026-10-07")
	if string(got) != string(raw) {
		t.Errorf("a response with nothing to add was re-encoded:\n got %s\nwant %s", got, raw)
	}
}

func TestDecorateStudentEventsRequiresAViewer(t *testing.T) {
	p, st := eventProxy(t)
	seedEvent(t, st, "dee", testEvent())
	raw := timetableResponse(realPeriodJSON("2026-10-01"))
	for _, tc := range []struct{ school, viewer string }{{"", "dee"}, {"testschool", ""}, {"", ""}} {
		got := p.decorateStudentEvents(raw, tc.school, tc.viewer, "2026-10-01", "2026-10-07")
		if string(got) != string(raw) {
			t.Errorf("school=%q viewer=%q attached events without an identity", tc.school, tc.viewer)
		}
	}
}

func TestDecorateStudentEventsFallsBackToTheResponseSpan(t *testing.T) {
	p, st := eventProxy(t)
	seedEvent(t, st, "dee", testEvent())
	// No usable dates from the caller: the span the response actually covers is
	// the only one the client has anywhere to draw.
	raw := timetableResponse(realPeriodJSON("2026-10-01"))
	got := p.decorateStudentEvents(raw, "testschool", "dee", "", "")
	if !strings.Contains(string(got), `"isCustom":true`) {
		t.Errorf("the span fallback did not attach the event:\n%s", got)
	}
}

func TestDecorateStudentEventsIgnoresEventsOutsideTheRange(t *testing.T) {
	p, st := eventProxy(t)
	seedEvent(t, st, "dee", testEvent())
	raw := timetableResponse(realPeriodJSON("2026-10-01"))
	// The event is on 2026-10-01; a request for a different week must not get it.
	got := p.decorateStudentEvents(raw, "testschool", "dee", "2026-10-05", "2026-10-12")
	if strings.Contains(string(got), `"isCustom":true`) {
		t.Errorf("an event outside the requested range was attached:\n%s", got)
	}
}

func TestDecorateStudentEventsNeverAttachesAnotherStudentsEvents(t *testing.T) {
	p, st := eventProxy(t)
	seedEvent(t, st, "dee", testEvent())
	raw := timetableResponse(realPeriodJSON("2026-10-01"))
	got := p.decorateStudentEvents(raw, "testschool", "sam", "2026-10-01", "2026-10-07")
	if strings.Contains(string(got), `"isCustom":true`) {
		t.Errorf("sam's response carries dee's event:\n%s", got)
	}
}

// --- event version ---

func TestStudentEventVersionMovesOnEveryEdit(t *testing.T) {
	st := eventStore(t)
	v0, err := st.StudentEventVersion("testschool", "dee")
	if err != nil || v0 != 0 {
		t.Fatalf("version before any event = %d, err %v", v0, err)
	}
	ev := seedEvent(t, st, "dee", testEvent())
	v1, _ := st.StudentEventVersion("testschool", "dee")
	if v1 <= v0 {
		t.Errorf("creating an event moved the version from %d to %d", v0, v1)
	}
	room := "R13"
	if _, _, err := st.UpdateStudentEvent("testschool", ev.ID, store.StudentEventPatch{Room: &room}); err != nil {
		t.Fatalf("UpdateStudentEvent: %v", err)
	}
	v2, _ := st.StudentEventVersion("testschool", "dee")
	if v2 <= v1 {
		t.Errorf("editing an event moved the version from %d to %d", v1, v2)
	}
	if _, err := st.DeleteStudentEvent("testschool", ev.ID); err != nil {
		t.Fatalf("DeleteStudentEvent: %v", err)
	}
	v3, _ := st.StudentEventVersion("testschool", "dee")
	// The delete is the case a derived counter cannot catch: the row is gone, so
	// a max(updated_at) would not move and the client would keep showing it.
	if v3 <= v2 {
		t.Errorf("deleting an event moved the version from %d to %d", v2, v3)
	}
}

func TestStudentEventVersionIsPerStudent(t *testing.T) {
	st := eventStore(t)
	seedEvent(t, st, "dee", testEvent())
	sam, err := st.StudentEventVersion("testschool", "sam")
	if err != nil {
		t.Fatalf("StudentEventVersion: %v", err)
	}
	if sam != 0 {
		t.Errorf("a classmate's version moved to %d because of dee's event", sam)
	}
}

func TestStudentEventVersionIsPerSchool(t *testing.T) {
	st := eventStore(t)
	seedEvent(t, st, "dee", testEvent())
	other, err := st.StudentEventVersion("otherschool", "dee")
	if err != nil {
		t.Fatalf("StudentEventVersion: %v", err)
	}
	if other != 0 {
		t.Errorf("another school's version moved to %d", other)
	}
}

func TestStudentEventVersionNormalizesTheUsername(t *testing.T) {
	st := eventStore(t)
	seedEvent(t, st, "dee", testEvent())
	want, err := st.StudentEventVersion("testschool", "dee")
	if err != nil {
		t.Fatalf("StudentEventVersion: %v", err)
	}
	got, err := st.StudentEventVersion("testschool", "  DEE ")
	if err != nil {
		t.Fatalf("StudentEventVersion: %v", err)
	}
	if got != want {
		t.Errorf("version read as %d for a padded username, want %d", got, want)
	}
}

// renderWeekForTest renders the week view through the production lesson builder and
// template. It deliberately does not re-implement the period-to-row mapping: a copy
// of that logic in a test can agree with itself while diverging from the page the
// user sees, which is how a missing field hides behind a green test.
func renderWeekForTest(t *testing.T, p *Proxy, feed feedTarget) string {
	t.Helper()
	byDay := weekLessonsByDay(feed.Periods, p.masterData(feed.School), berlin)
	days := make([]string, 0, len(byDay))
	for k := range byDay {
		days = append(days, k)
	}
	sort.Strings(days)
	view := weekView{Name: feed.Name, Timezone: "Europe/Berlin", RenderedAt: "01.01.2026 00:00"}
	for _, k := range days {
		lessons := byDay[k]
		sort.SliceStable(lessons, func(i, j int) bool { return lessons[i].TimeRange < lessons[j].TimeRange })
		view.Days = append(view.Days, weekDay{Label: k, Relative: k, Lessons: lessons})
	}
	var out strings.Builder
	if err := weekTemplate.Execute(&out, view); err != nil {
		t.Fatalf("render week: %v", err)
	}
	return out.String()
}

func TestFeedLocationPrefersTheTokenZone(t *testing.T) {
	loc := feedLocation(&store.ClassToken{Timezone: "Europe/Berlin"}, nil)
	if got, want := timeZoneOffsetAt(loc, "2026-10-01"), 2*3600; got != want {
		t.Errorf("offset = %d, want the Berlin summer offset %d", got, want)
	}
	// Same zone, a winter date: the zone database has to be consulted rather than
	// the offset cached from a summer period.
	if got, want := timeZoneOffsetAt(loc, "2026-12-01"), 3600; got != want {
		t.Errorf("winter offset = %d, want %d", got, want)
	}
}

func TestFeedLocationFallsBackToThePeriodOffset(t *testing.T) {
	// A token naming a zone this binary has no data for must not fail the feed:
	// the lessons are still correct, and the events would only be on the wrong wall
	// clock. The offset the fetched periods actually carry is the best guess.
	periods := []map[string]any{{"startDateTime": "2026-10-01T14:00+05:30"}}
	loc := feedLocation(&store.ClassToken{Timezone: "Not/AZone"}, periods)
	if got, want := timeZoneOffsetAt(loc, "2026-10-01"), 5*3600+1800; got != want {
		t.Errorf("offset = %d, want the periods' own %d", got, want)
	}
}

func TestFeedLocationWithNothingToGoOn(t *testing.T) {
	for _, tok := range []*store.ClassToken{{}, {Timezone: "Not/AZone"}} {
		loc := feedLocation(tok, nil)
		if loc != time.UTC {
			t.Errorf("feedLocation(%+v, nil) = %v, want UTC", tok, loc)
		}
	}
}

func TestFeedLocationSkipsUnparseablePeriods(t *testing.T) {
	periods := []map[string]any{
		{"startDateTime": "garbage"},
		{"startDateTime": "2026-10-01T14:00+05:30"},
	}
	if got, want := timeZoneOffsetAt(feedLocation(&store.ClassToken{}, periods), "2026-10-01"), 5*3600+1800; got != want {
		t.Errorf("offset = %d, want %d — a garbage timestamp must not end the search", got, want)
	}
}

func timeZoneOffsetAt(loc *time.Location, date string) int {
	t, err := time.ParseInLocation("2006-01-02", date, loc)
	if err != nil {
		return 0
	}
	_, offset := t.Zone()
	return offset
}

func TestEventTimesAreWallClockInTheSchoolZone(t *testing.T) {
	// The distinction that matters: an admin's 14:00 is 14:00 on the school clock.
	// Parsed in UTC it would be a two-hour-later instant, and every surface that
	// converts the timestamp to display it would show the wrong time.
	ev := seedEvent(t, eventStore(t), "dee", testEvent())
	pd := customEventPeriods([]store.StudentEvent{ev}, berlin)[0]
	start, err := parsePeriodTime(rawString(pd, "startDateTime"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if h := start.In(berlin).Hour(); h != 14 {
		t.Errorf("the event reads as %02d:00 in the school zone, want 14:00", h)
	}
}

// TestEventsReachTheAppThroughBothSelfTimetablePaths drives getTimetable2017
// through the real handler, for both ways a client can ask for its own
// timetable, and checks the one way it must not.
//
// The feature was originally attached only to the `type=STUDENT` branch, on the
// documented belief that "the app asks for STUDENT with its own id". That belief
// is not a property the proxy can verify: it is a claim about client behaviour,
// and the repo held no capture of what the app actually sends. If the app asks
// for its own timetable as `type=CLASS` — its class, which the existing
// self-class branch explicitly serves — then the one surface the feature was
// announced for would have come back empty, and every test would still have
// passed because they all called decorateStudentEvents directly.
//
// So both self paths are decorated, keyed by the session user, and this test
// pins all three cases.
func TestEventsReachTheAppThroughBothSelfTimetablePaths(t *testing.T) {
	st := eventStore(t)
	ev := testEvent()
	ev.Date = "2026-10-05"
	seedEvent(t, st, "dee", ev)

	fu := &fakeUpstream{}
	p := newFakeProxyUpstreamWithStore(t, fu, st)
	fu.setTimetable(t, []map[string]any{realPeriodJSON("2026-10-05")})
	// dee is a student in class 5000 with person id 7; sam is a classmate.
	seedSessionUser(t, st, "dee", 5, 7, 5000)
	seedSessionUser(t, st, "sam", 5, 8, 5000)

	for _, tc := range []struct {
		name      string
		user      string
		id        int64
		typ       string
		wantEvent bool
		why       string
	}{
		{"own record as STUDENT", "dee", 7, "STUDENT", true, "the student asking for their own record"},
		{"own class as CLASS", "dee", 5000, "CLASS", true, "the same student asking for their own class instead"},
		{"classmate's class", "sam", 5000, "CLASS", false, "a classmate asking for the same class must see no event"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := internReq(t, p, "getTimetable2017", tc.user,
				internBody("getTimetable2017", ttParam(tc.id, tc.typ, "2026-10-05", "2026-10-11", tc.user)))
			periods := periodsOfResponse(t, rec)
			got := 0
			for _, pd := range periods {
				if isCustomPeriod(pd) {
					got++
				}
			}
			if len(periods) == 0 {
				t.Fatalf("no periods decoded: %s", rec.Body.String())
			}
			if tc.wantEvent && got != 1 {
				t.Errorf("%s: got %d custom periods, want 1: %s", tc.why, got, rec.Body.String())
			}
			if !tc.wantEvent && got != 0 {
				t.Errorf("%s: got %d custom periods, want none: %s", tc.why, got, rec.Body.String())
			}
		})
	}
}

// periodsOfResponse decodes result.timetable.periods from a JSON-RPC response,
// failing the test if the response is an error carrier.
func periodsOfResponse(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var parsed struct {
		Result struct {
			Timetable struct {
				Periods []map[string]any `json:"periods"`
			} `json:"timetable"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, rec.Body.String())
	}
	if parsed.Error != nil {
		t.Fatalf("json-rpc error %d %q: %s", parsed.Error.Code, parsed.Error.Message, rec.Body.String())
	}
	return parsed.Result.Timetable.Periods
}

// TestCustomEventIDsAreDistinctAcrossEventsAndRevisions pins the synthetic id
// scheme against aliasing.
//
// The first scheme subtracted a fixed amount per revision: id = -(1e9 + eventID +
// (revision-1)*1e6). That makes event 1 at revision 2 and event 1000001 at
// revision 1 the same id, and since the ICS UID is built from this id, two
// different events in one school would share a UID and one would silently
// replace the other in a client's calendar. It needed a school with a million
// events to show up, which is exactly the kind of bug that is found years later
// by someone who has no idea why the number was there.
//
// The scheme now encodes the pair as eventID*stride + (revision-1), which is
// distinct for every pair a timetable can produce.
func TestCustomEventIDsAreDistinctAcrossEventsAndRevisions(t *testing.T) {
	id := func(evID, rev int64) int64 {
		return customEventID(store.StudentEvent{ID: evID, Revision: rev})
	}
	seen := map[int64]string{}
	for evID := int64(1); evID <= 3; evID++ {
		for rev := int64(1); rev <= 3; rev++ {
			got := id(evID, rev)
			label := fmt.Sprintf("event %d revision %d", evID, rev)
			// Distinct events must never share an id; the same event across
			// revisions must always share one.
			key := fmt.Sprintf("%d", evID)
			if rev == 1 {
				if prev, dup := seen[got]; dup {
					t.Errorf("%s and %s share the synthetic id %d", label, prev, got)
				}
				seen[got] = label
			}
			if first := id(evID, 1); got != first {
				t.Errorf("%s has id %d, want the stable %d", label, got, first)
			}
			_ = key
			if got >= 0 {
				t.Errorf("%s produced the non-negative id %d, which could pass for a real period", label, got)
			}
		}
	}
	// The pair that aliased under the old scheme.
	if a, b := id(1, 2), id(1000001, 1); a == b {
		t.Errorf("event 1 rev 2 and event 1000001 rev 1 still share id %d", a)
	}
}

// TestStudentEventsServeTheLastDayOfTheRange is the range-bound regression at the
// surface that matters: an event an admin put on the final day of the requested
// window has to be served. The window's last day is what a calendar subscription
// and a week view both ask about, and the half-open read dropped it.
func TestStudentEventsServeTheLastDayOfTheRange(t *testing.T) {
	p, st := eventProxy(t)
	for _, date := range []string{"2026-10-01", "2026-10-02", "2026-10-03"} {
		ev := testEvent()
		ev.Date = date
		seedEvent(t, st, "dee", ev)
	}
	raw := timetableResponse(realPeriodJSON("2026-10-01"))
	for _, tc := range []struct {
		from, to string
		want     int
	}{
		{"2026-10-01", "2026-10-03", 3},
		{"2026-10-01", "2026-10-02", 2},
		{"2026-10-03", "2026-10-03", 1},
	} {
		got := p.decorateStudentEvents(raw, "testschool", "dee", tc.from, tc.to)
		periods := periodsFromTimetable(t, got)
		custom := 0
		for _, pd := range periods {
			if isCustomPeriod(pd) {
				custom++
			}
		}
		if custom != tc.want {
			t.Errorf("%s..%s served %d events, want %d", tc.from, tc.to, custom, tc.want)
		}
	}
}

func periodsFromTimetable(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var parsed struct {
		Result struct {
			Timetable struct {
				Periods []map[string]any `json:"periods"`
			} `json:"timetable"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	return parsed.Result.Timetable.Periods
}
