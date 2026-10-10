package proxy

// Tests for the synthetic Technik aggregate class and for the boosted path
// carrying a student's own events. The aggregate is the one surface that serves
// events from several students at once, so the permission gate is load-bearing:
// without it, anyone could read every student's appointments through one class.

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"untis-proxy/internal/store"
)

// referencesClass reports whether a period carries a CLASS element with the id.
func referencesClass(pd map[string]any, id int64) bool {
	for _, el := range jsonArray(pd["elements"]) {
		if asString(el["type"]) != "CLASS" {
			continue
		}
		if got, ok := jsonID(el["id"]); ok && got == id {
			return true
		}
	}
	return false
}

// toCanList reads a decoded period's `can` field as a plain string slice for the
// shared hasCap helper.
func toCanList(v any) []string {
	raw, _ := v.([]any)
	out := make([]string, 0, len(raw))
	for _, c := range raw {
		if s, ok := c.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func TestTechnikClassAggregatesEveryStudentsEvents(t *testing.T) {
	p, st := eventProxy(t)
	if err := st.SetPerm("dee", store.FeatureTechniker, true); err != nil {
		t.Fatalf("grant techniker: %v", err)
	}
	dee := testEvent()
	dee.Date = "2026-10-05"
	sam := testEvent()
	sam.Date = "2026-10-06"
	sam.Title = "Abbau"
	sam.Room = "Aula"
	seedEvent(t, st, "dee", dee)
	seedEvent(t, st, "sam", sam)

	rec := internReq(t, p, "getTimetable2017", "dee",
		internBody("getTimetable2017", ttParam(technikClassID, "CLASS", "2026-10-01", "2026-10-07", "dee")))
	periods := periodsOfResponse(t, rec)
	if len(periods) != 2 {
		t.Fatalf("got %d periods, want both students' events: %s", len(periods), rec.Body.String())
	}
	labels := map[string]bool{}
	for _, pd := range periods {
		if !isCustomPeriod(pd) {
			t.Errorf("a non-event period leaked into the aggregate: %+v", pd)
		}
		if !referencesClass(pd, technikClassID) {
			t.Errorf("a period does not reference the aggregate class, so a class view would hide it: %+v", pd["elements"])
		}
		if !hasCap(toCanList(pd["can"]), readLessonTopicCap) {
			t.Errorf("a period lacks READ_LESSONTOPIC, so the app would hide its lesson text: %+v", pd["can"])
		}
		labels[asString(pd["student"])] = true
	}
	// Without the student on each row a merged class would be a column of
	// identical "Technik" lessons that says nothing about whose they are.
	if !labels["Dee"] {
		t.Errorf("dee's event is not labeled with the display name: %+v", labels)
	}
	if !labels["sam"] {
		t.Errorf("sam's event is not labeled: %+v", labels)
	}
}

// TestTechnikClassMergesAnEventStaffedBySeveralStudents: the import stores one
// row per assigned technician, so a single appointment arrives several times
// with only the student differing. On the shared aggregate it must draw once,
// naming everyone, or the coordinator reads the same event over and over.
func TestTechnikClassMergesAnEventStaffedBySeveralStudents(t *testing.T) {
	p, st := eventProxy(t)
	if err := st.SetPerm("dee", store.FeatureTechniker, true); err != nil {
		t.Fatalf("grant techniker: %v", err)
	}
	seedEvent(t, st, "dee", testEvent())
	seedEvent(t, st, "sam", testEvent())

	rec := internReq(t, p, "getTimetable2017", "dee",
		internBody("getTimetable2017", ttParam(technikClassID, "CLASS", "2026-10-01", "2026-10-01", "dee")))
	periods := periodsOfResponse(t, rec)
	if len(periods) != 1 {
		t.Fatalf("got %d periods, want the two rows merged into one: %s", len(periods), rec.Body.String())
	}
	names := strings.ToLower(asString(periods[0]["student"]))
	if !strings.Contains(names, "dee") || !strings.Contains(names, "sam") {
		t.Errorf("student = %q, want both assigned students", periods[0]["student"])
	}
}

func TestTechnikClassRequiresTheTechnikerPermission(t *testing.T) {
	p, st := eventProxy(t)
	seedEvent(t, st, "dee", testEvent())
	// No permission granted: the class id must be refused, not served empty.
	rec := internReq(t, p, "getTimetable2017", "dee",
		internBody("getTimetable2017", ttParam(technikClassID, "CLASS", "2026-10-01", "2026-10-07", "dee")))
	if code := errCode(t, rec); code != -8509 {
		t.Errorf("code = %d, want -8509 for a user without the techniker permission: %s", code, rec.Body.String())
	}
}

func TestInjectTechnikClassOnlyForHolders(t *testing.T) {
	p, st := eventProxy(t)
	body := []byte(`{"result":{"masterData":{"klassen":[` +
		`{"id":4188,"name":"01aG","displayable":false,"startDate":"2026-08-10","endDate":"2027-06-27"}]}}}`)

	if got := p.injectTechnikClass("dee", body); string(got) != string(body) {
		t.Errorf("a body was rewritten for a user without the permission:\n%s", got)
	}
	if err := st.SetPerm("dee", store.FeatureTechniker, true); err != nil {
		t.Fatalf("grant techniker: %v", err)
	}
	got := p.injectTechnikClass("dee", body)
	if !strings.Contains(string(got), `"`+technikClassName+`"`) {
		t.Errorf("the class was not added:\n%s", got)
	}
	if !strings.Contains(string(got), strconv.FormatInt(technikClassID, 10)) {
		t.Errorf("the class id is missing:\n%s", got)
	}
	// Added twice (login rewrite plus a per-request rewrite) must not duplicate.
	twice := p.injectTechnikClass("dee", got)
	if n := strings.Count(string(twice), strconv.FormatInt(technikClassID, 10)); n != 1 {
		t.Errorf("the class appears %d times, want once:\n%s", n, twice)
	}
}

// TestTechnikClassShowsEventTitleAndRealRoomIDs: in the merged class a row of
// identical "Technik" subjects is useless, so the event's own title has to win
// over the subject; and an event room must reference the id the real lessons use
// ("01-Aula" id 169, not the retired plain "Aula" id 1036) or it lands under a
// second room with the same name that looks empty.
func TestTechnikClassShowsEventTitleAndRealRoomIDs(t *testing.T) {
	p, st := eventProxy(t)
	if err := st.SetPerm("dee", store.FeatureTechniker, true); err != nil {
		t.Fatalf("grant techniker: %v", err)
	}
	// The retired Aula is listed before the live one on purpose: resolution has
	// to prefer the active entry, not the first one.
	p.cacheRawMasterData("testschool", []byte(`{"result":{"masterData":{"timeStamp":1,`+
		`"klassen":[{"id":4188,"name":"01aG","startDate":"2026-08-10","endDate":"2027-06-27"}],`+
		`"subjects":[],"teachers":[],`+
		`"rooms":[{"id":1036,"name":"Aula","longName":"Aula","active":false},`+
		`{"id":169,"name":"01-Aula","longName":"Aula","active":true},`+
		`{"id":238,"name":"CSS","longName":"Christian Stock Stadion","active":true}],`+
		`"holidays":[]}}}`))
	ev := testEvent()
	ev.Date = "2026-10-05"
	ev.Title = "Einschulung E-Phase"
	ev.Subject = "Technik"
	ev.Room = "Aula, CSS"
	seedEvent(t, st, "dee", ev)

	rec := internReq(t, p, "getTimetable2017", "dee",
		internBody("getTimetable2017", ttParam(technikClassID, "CLASS", "2026-10-01", "2026-10-07", "dee")))
	var pd map[string]any
	for _, period := range periodsOfResponse(t, rec) {
		if isCustomPeriod(period) {
			pd = period
		}
	}
	if pd == nil {
		t.Fatalf("no event period in the aggregate: %s", rec.Body.String())
	}
	text, _ := pd["text"].(map[string]any)
	if text["lesson"] != "Arbeitsblatt mitbringen" {
		t.Errorf("lesson = %v, want the description (what the appointment needs)", text["lesson"])
	}
	if text["lesson"] == ev.Subject {
		t.Error("the subject still fills the lesson cell; it must not")
	}
	if text["substitution"] != p.studentLabel("dee") {
		t.Errorf("substitution = %v, want the assigned student %q", text["substitution"], p.studentLabel("dee"))
	}
	if student, _ := pd["student"].(string); student == "" {
		t.Error("the aggregate period names no assigned student")
	}
	roomIDs := map[int64]bool{}
	for _, el := range jsonArray(pd["elements"]) {
		if asString(el["type"]) != "ROOM" {
			continue
		}
		if id, ok := jsonID(el["id"]); ok {
			roomIDs[id] = true
		}
	}
	if !roomIDs[169] || !roomIDs[238] {
		t.Errorf("room ids = %v, want the live Aula 169 and CSS 238", roomIDs)
	}
	if roomIDs[1036] {
		t.Errorf("the retired Aula 1036 was referenced instead of the live room")
	}
}

// TestInjectTechnikClassBumpsTimestamp: the app caches masterData by timeStamp
// and ignores a body stamped the same, so adding the class must move it or a
// logged-in client never learns the class exists.
func TestInjectTechnikClassBumpsTimestamp(t *testing.T) {
	p, st := eventProxy(t)
	if err := st.SetPerm("dee", store.FeatureTechniker, true); err != nil {
		t.Fatalf("grant techniker: %v", err)
	}
	got := p.injectTechnikClass("dee", []byte(`{"result":{"masterData":{"timeStamp":1000,"klassen":[]}}}`))
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("injected body is not JSON: %v", err)
	}
	md, _ := m["result"].(map[string]any)["masterData"].(map[string]any)
	if ts, _ := jsonID(md["timeStamp"]); ts <= 1000 {
		t.Errorf("timeStamp = %v, want it advanced past 1000", md["timeStamp"])
	}
}

// TestTechnikClassIDFitsSigned32Bit guards the id a mobile client may hold in a
// signed 32-bit integer; a value past 2^31 arrives there as a negative number.
func TestTechnikClassIDFitsSigned32Bit(t *testing.T) {
	if technikClassID >= 1<<31 {
		t.Errorf("technikClassID = %d, want it below 2^31", technikClassID)
	}
}

// TestBoostedSelfTimetableStillCarriesOwnEvents is the regression for the boosted
// path: it returned the raw teacher-account response before any event code ran,
// so a boosted student saw their lessons but none of their appointments.
func TestBoostedSelfTimetableStillCarriesOwnEvents(t *testing.T) {
	fu := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, fu)
	seedSessionUser(t, st, "dee", 5, 7, 5000)
	if err := st.UpsertUser(&store.User{
		Username: "owner", School: "testschool", PersonType: 2, PersonID: 100,
		Password: "s3cret", Method: "key",
	}); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	if err := st.SetPerm("dee", store.FeatureBoosted, true); err != nil {
		t.Fatalf("grant boosted: %v", err)
	}
	ev := testEvent()
	ev.Date = "2026-10-05"
	seedEvent(t, st, "dee", ev)
	fu.setTimetable(t, []map[string]any{realPeriodJSON("2026-10-05")})

	rec := internReq(t, p, "getTimetable2017", "dee",
		internBody("getTimetable2017", ttParam(7, "STUDENT", "2026-10-05", "2026-10-11", "dee")))
	periods := periodsOfResponse(t, rec)
	custom := 0
	for _, pd := range periods {
		if isCustomPeriod(pd) {
			custom++
		}
	}
	if custom != 1 {
		t.Errorf("the boosted user's own timetable carried %d events, want 1: %s", custom, rec.Body.String())
	}
}
