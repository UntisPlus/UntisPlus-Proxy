package proxy

// Tests for the techniker's local absence register on hand-authored events. The
// register is entirely proxy-local, so the two failure modes that matter are
// drawing it where it does not belong (a real period, or a non-techniker) and
// losing a recorded absence somewhere between the app and the store.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"untis-proxy/internal/store"
)

// TestAbsenceCapsOnlyOnSyntheticPeriodsForTechniker pins the capability gate: the
// absence register may only open on a proxy event period, and only for a user
// who holds the techniker permission. A real Untis period must never grow the
// capabilities, because the absence recorded there would be a real one.
func TestAbsenceCapsOnlyOnSyntheticPeriodsForTechniker(t *testing.T) {
	p, st := eventProxy(t)
	synthetic := map[string]any{"customEventId": int64(1), "can": []any{"READ_LESSONTOPIC"}}
	real := map[string]any{"can": []any{"READ_LESSONTOPIC"}}

	p.capsInPeriod("dee", synthetic)
	p.capsInPeriod("dee", real)
	if hasCap(toCanList(synthetic["can"]), writeStudAbsenceCap) {
		t.Error("a non-techniker was offered the absence register")
	}
	if hasCap(toCanList(real["can"]), writeStudAbsenceCap) {
		t.Error("a real period was offered the absence register")
	}

	if err := st.SetPerm("dee", store.FeatureTechniker, true); err != nil {
		t.Fatalf("grant techniker: %v", err)
	}
	p.capsInPeriod("dee", synthetic)
	p.capsInPeriod("dee", real)
	if !hasCap(toCanList(synthetic["can"]), readStudAbsenceCap) || !hasCap(toCanList(synthetic["can"]), writeStudAbsenceCap) {
		t.Errorf("techniker caps = %v, want read and write absence", synthetic["can"])
	}
	if hasCap(toCanList(real["can"]), readStudAbsenceCap) {
		t.Error("a real period was offered the absence register to a techniker")
	}
}

// TestTechnikAggregateEventPeriodOffersAbsenceRegister runs the same gate through
// the whole surface a techniker actually loads, so a capability lost between the
// aggregate builder and the wire is caught here.
func TestTechnikAggregateEventPeriodOffersAbsenceRegister(t *testing.T) {
	p, st := eventProxy(t)
	if err := st.SetPerm("dee", store.FeatureTechniker, true); err != nil {
		t.Fatalf("grant techniker: %v", err)
	}
	seedEvent(t, st, "dee", testEvent())

	rec := internReq(t, p, "getTimetable2017", "dee",
		internBody("getTimetable2017", ttParam(technikClassID, "CLASS", "2026-10-01", "2026-10-07", "dee")))
	found := false
	for _, pd := range periodsOfResponse(t, rec) {
		if !isCustomPeriod(pd) {
			continue
		}
		found = true
		if !hasCap(toCanList(pd["can"]), writeStudAbsenceCap) {
			t.Errorf("aggregate period caps = %v, want the absence register", pd["can"])
		}
	}
	if !found {
		t.Fatalf("no event period in the aggregate: %s", rec.Body.String())
	}
}

// TestEventAbsenceRoundTrip drives the app's three writes plus the read against
// one appointment shared by two students, which is the case the register exists
// for: the coordinator ticks off who actually showed up.
func TestEventAbsenceRoundTrip(t *testing.T) {
	p, st := eventProxy(t)
	if err := st.SetPerm("dee", store.FeatureTechniker, true); err != nil {
		t.Fatalf("grant techniker: %v", err)
	}
	dee := seedEvent(t, st, "dee", func() store.NewStudentEvent {
		ev := testEvent()
		ev.Title = "Einschulung E-Phase"
		return ev
	}())
	seedEvent(t, st, "sam", func() store.NewStudentEvent {
		ev := testEvent()
		ev.Title = "Einschulung E-Phase"
		return ev
	}()) // identical fields: one appointment
	periodID := customEventID(dee)
	key := sharedEventKey(dee)

	read := internReq(t, p, "getPeriodData2017", "dee",
		internBody("getPeriodData2017", map[string]any{"ttIds": []int64{periodID}, "auth": authMap("dee")}))
	data, _ := decodeResult(t, read.Body.Bytes())["dataByTTId"].(map[string]any)
	pd, _ := data[strconv.FormatInt(periodID, 10)].(map[string]any)
	if pd == nil {
		t.Fatalf("no period data for the synthetic id: %s", read.Body.String())
	}
	if got := len(pd["studentIds"].([]any)); got != 2 {
		t.Errorf("studentIds = %d, want both assigned students", got)
	}
	if checked, _ := pd["absenceChecked"].(bool); checked {
		t.Error("a fresh appointment reports its register submitted")
	}
	// The event title is the lesson topic in the detail view, not the timetable
	// cell's lesson text (which carries the subject).
	topic, _ := pd["topic"].(map[string]any)
	if topic == nil || topic["text"] != "Einschulung E-Phase" {
		t.Errorf("topic = %v, want the event title as the lesson topic", pd["topic"])
	}

	create := internReq(t, p, "createImmediateAbsence2017", "dee",
		internBody("createImmediateAbsence2017", map[string]any{
			"periodId": periodID, "studentId": 10, "startTime": "T14:00", "endTime": "T15:00", "auth": authMap("dee"),
		}))
	createdAbsences, _ := decodeResult(t, create.Body.Bytes())["absences"].([]any)
	if len(createdAbsences) != 1 {
		t.Fatalf("create returned %d absences, want 1: %s", len(createdAbsences), create.Body.String())
	}
	absence, _ := createdAbsences[0].(map[string]any)
	if id, _ := jsonID(absence["studentId"]); id != 10 {
		t.Errorf("created absence studentId = %v, want 10", absence["studentId"])
	}
	absenceID, _ := jsonID(absence["id"])
	if !isEventAbsenceID(absenceID) {
		t.Fatalf("absence id = %d, want a synthetic absence id", absenceID)
	}
	if rows, _ := st.EventAbsences("testschool", key); len(rows) != 1 {
		t.Errorf("store holds %d rows after create, want 1", len(rows))
	}

	del := internReq(t, p, "deleteAbsence2017", "dee",
		internBody("deleteAbsence2017", map[string]any{"absenceId": absenceID, "auth": authMap("dee")}))
	if ok, _ := decodeResult(t, del.Body.Bytes())["success"].(bool); !ok {
		t.Errorf("delete did not report success: %s", del.Body.String())
	}
	if rows, _ := st.EventAbsences("testschool", key); len(rows) != 0 {
		t.Errorf("store holds %d rows after delete, want 0", len(rows))
	}

	submit := internReq(t, p, "submitAbsencesChecked2017", "dee",
		internBody("submitAbsencesChecked2017", map[string]any{"periodIds": []int64{periodID}, "auth": authMap("dee")}))
	if submit.Code != 200 || decodeResult(t, submit.Body.Bytes()) == nil {
		t.Fatalf("submit did not answer with a result: %d %s", submit.Code, submit.Body.String())
	}
	if checked, _ := st.EventAbsenceChecked("testschool", key); !checked {
		t.Error("the register was not reported checked")
	}

	// Every step landed in the append-only changelog, newest first.
	entries, err := st.EventAbsenceLog("testschool", "1970-01-01", "9999-12-31")
	if err != nil {
		t.Fatalf("EventAbsenceLog: %v", err)
	}
	actions := make([]string, 0, len(entries))
	for _, e := range entries {
		actions = append(actions, e.Action)
	}
	if got := strings.Join(actions, ","); got != "submitted,deleted,created" {
		t.Errorf("log actions = %q, want submitted,deleted,created", got)
	}
}

// TestEventAbsenceFallsThroughForNonTechniker: without the permission none of the
// four calls is ours to answer, so each must report that it was not intercepted.
func TestEventAbsenceFallsThroughForNonTechniker(t *testing.T) {
	p, st := eventProxy(t)
	periodID := customEventID(seedEvent(t, st, "dee", testEvent()))
	cases := []struct {
		method string
		body   string
	}{
		{"getPeriodData2017", internBody("getPeriodData2017", map[string]any{"ttIds": []int64{periodID}, "auth": authMap("dee")})},
		{"createImmediateAbsence2017", internBody("createImmediateAbsence2017", map[string]any{"periodId": periodID, "studentId": 10, "auth": authMap("dee")})},
		{"submitAbsencesChecked2017", internBody("submitAbsencesChecked2017", map[string]any{"periodIds": []int64{periodID}, "auth": authMap("dee")})},
	}
	for _, tc := range cases {
		if p.handleEventAbsenceCall(nil, json.RawMessage(`"t"`), "testschool", tc.method, "dee", []byte(tc.body)) {
			t.Errorf("%s was intercepted for a non-techniker", tc.method)
		}
	}
}

// TestAdminEventAbsencesList covers the dashboard read: an admin sees each
// appointment with its roster and absence, a non-admin does not, and the date
// window is honored.
func TestAdminEventAbsencesList(t *testing.T) {
	p, st := adminEventProxy(t)
	for _, d := range []string{"2026-10-01", "2026-10-08"} {
		ev, err := st.CreateStudentEvent("testschool", "dee", store.NewStudentEvent{
			Date: d, StartTime: "14:00", EndTime: "15:00", Title: "Technik", Subject: "Technik",
		}, "adm")
		if err != nil {
			t.Fatalf("seed event %s: %v", d, err)
		}
		if _, err := st.SetEventAbsence("testschool", sharedEventKey(ev), d, "dee", "T14:00", "T15:00", "tech1"); err != nil {
			t.Fatalf("seed absence %s: %v", d, err)
		}
	}

	all := doAdmin(t, p, "adm", http.MethodGet, "/api/admin/events/absences?school=testschool", "")
	if all.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", all.Code, all.Body.String())
	}
	var out struct {
		Absences     []map[string]any `json:"absences"`
		Appointments []struct {
			Date    string           `json:"date"`
			Checked bool             `json:"checked"`
			Roster  []map[string]any `json:"roster"`
		} `json:"appointments"`
	}
	if err := json.Unmarshal(all.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if len(out.Absences) != 2 || len(out.Appointments) != 2 {
		t.Fatalf("got %d absences and %d appointments, want both: %s", len(out.Absences), len(out.Appointments), all.Body.String())
	}
	if len(out.Appointments[0].Roster) != 1 || out.Appointments[0].Roster[0]["absent"] != true {
		t.Errorf("roster = %v, want one absent student", out.Appointments[0].Roster)
	}
	if out.Appointments[0].Checked {
		t.Error("an unsubmitted register reports checked")
	}

	windowed := doAdmin(t, p, "adm", http.MethodGet, "/api/admin/events/absences?school=testschool&from=2026-10-01&to=2026-10-02", "")
	var win struct {
		Absences []map[string]any `json:"absences"`
	}
	_ = json.Unmarshal(windowed.Body.Bytes(), &win)
	if len(win.Absences) != 1 || win.Absences[0]["date"] != "2026-10-01" {
		t.Errorf("windowed = %v, want only the 10-01 absence", win.Absences)
	}

	if rec := doAdmin(t, p, "dee", http.MethodGet, "/api/admin/events/absences?school=testschool", ""); rec.Code != http.StatusForbidden {
		t.Errorf("non-admin status = %d, want 403", rec.Code)
	}
	if rec := doAdmin(t, p, "", http.MethodGet, "/api/admin/events/absences?school=testschool", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no-session status = %d, want 401", rec.Code)
	}
}

// TestAdminEventAbsenceEditAndLog covers the admin correction path end to end: a
// window change and a move to another assigned student both land, and every step
// is written to the append-only log, which itself has no delete.
func TestAdminEventAbsenceEditAndLog(t *testing.T) {
	p, st := adminEventProxy(t)
	ev, err := st.CreateStudentEvent("testschool", "dee", store.NewStudentEvent{
		Date: "2026-10-01", StartTime: "14:00", EndTime: "15:00", Title: "Technik", Subject: "Technik",
	}, "adm")
	if err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := st.CreateStudentEvent("testschool", "sam", store.NewStudentEvent{
		Date: "2026-10-01", StartTime: "14:00", EndTime: "15:00", Title: "Technik", Subject: "Technik",
	}, "adm"); err != nil {
		t.Fatalf("seed sam event: %v", err)
	}
	key := sharedEventKey(ev)
	if _, err := st.SetEventAbsence("testschool", key, "2026-10-01", "dee", "T14:00", "T15:00", "tech1"); err != nil {
		t.Fatalf("seed absence: %v", err)
	}

	body := `{"school":"testschool","appointmentKey":` + jsonString(key) + `,"date":"2026-10-01","username":"dee","startTime":"T14:30","endTime":"T15:30","newUsername":"sam"}`
	rec := doAdmin(t, p, "adm", http.MethodPatch, "/api/admin/events/absences", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	rows, _ := st.EventAbsences("testschool", key)
	if len(rows) != 1 || rows[0].Username != "sam" || rows[0].StartTime != "T14:30" {
		t.Fatalf("after edit = %+v, want sam at T14:30", rows)
	}

	log := doAdmin(t, p, "adm", http.MethodGet, "/api/admin/events/absences/log?school=testschool", "")
	var out struct {
		Log []map[string]any `json:"log"`
	}
	if err := json.Unmarshal(log.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad log json: %v", err)
	}
	if len(out.Log) != 1 || out.Log[0]["action"] != "edited" {
		t.Fatalf("log = %v, want one edited entry", out.Log)
	}
	// The last log line must never be deletable: the route does not exist.
	if del := doAdmin(t, p, "adm", http.MethodDelete, "/api/admin/events/absences/log", ""); del.Code == http.StatusOK {
		t.Errorf("delete on the changelog returned 200; it must not be removable")
	}
}

// jsonString renders s as a JSON string literal for hand-built request bodies.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestEventAbsenceFallsThroughForRealIDs: a techniker's ordinary Untis calls must
// still reach Untis. Only ids in the synthetic ranges may be answered locally.
func TestEventAbsenceFallsThroughForRealIDs(t *testing.T) {
	p, st := eventProxy(t)
	if err := st.SetPerm("dee", store.FeatureTechniker, true); err != nil {
		t.Fatalf("grant techniker: %v", err)
	}
	const realPeriod int64 = 123456
	cases := []struct {
		method string
		body   string
	}{
		{"getPeriodData2017", internBody("getPeriodData2017", map[string]any{"ttIds": []int64{realPeriod}, "auth": authMap("dee")})},
		{"createImmediateAbsence2017", internBody("createImmediateAbsence2017", map[string]any{"periodId": realPeriod, "studentId": 10, "auth": authMap("dee")})},
		{"deleteAbsence2017", internBody("deleteAbsence2017", map[string]any{"absenceId": 42, "auth": authMap("dee")})},
		{"submitAbsencesChecked2017", internBody("submitAbsencesChecked2017", map[string]any{"periodIds": []int64{realPeriod}, "auth": authMap("dee")})},
	}
	for _, tc := range cases {
		if p.handleEventAbsenceCall(nil, json.RawMessage(`"t"`), "testschool", tc.method, "dee", []byte(tc.body)) {
			t.Errorf("%s was intercepted for a real id", tc.method)
		}
	}
}
