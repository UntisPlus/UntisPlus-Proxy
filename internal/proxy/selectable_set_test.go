package proxy

// The set of selectable teachers/rooms/subjects the app offers must be exactly
// the set reconstruction can serve: the elements the pooled classes actually
// reference. The school-wide name catalog must never leak into it - when it
// did, every teacher and room in the school showed up as selectable while only
// a handful had a timetable to load.

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"untis-proxy/internal/session"
	"untis-proxy/internal/store"
	"untis-proxy/internal/untis"
)

// The master data a school serves lists every teacher, room and subject; the
// pooled class below references exactly one of each.
const catalogMasterData = `{"jsonrpc":"2.0","id":"upstream","result":{
	"sessionId":"upstreamsid","personId":7,"personType":5,"klasseId":5000,
	"masterData":{
		"timeStamp":1,
		"klassen":[{"id":5000,"name":"10b"},{"id":4420,"name":"10c"}],
		"teachers":[
			{"id":5009,"name":"A. Hartley"},
			{"id":224,"firstName":"Ada","lastName":"Lovelace"},
			{"id":225,"name":"Turing A"}
		],
		"rooms":[{"id":169,"name":"R101"},{"id":170,"name":"R102"}],
		"subjects":[{"id":7,"longName":"Mathematik 3"},{"id":8,"name":"Sport"}]
	}}}`

func scanOneClass(t *testing.T, f *fakeUpstream, p *Proxy) {
	t.Helper()
	f.setTimetable(t, []map[string]any{reconPeriod(10, "2026-09-21", "08:00", "08:45", 5009, 169, 7)})
	seedClassOwner(t, p.store, "owen", 7, 5000)
	ys, ye := schoolYearRange(time.Now())
	p.StartRecon("testschool", ys, ye)
	deadline := time.Now().Add(10 * time.Second)
	for {
		elems, err := p.store.LoadReconElements("testschool")
		if err != nil {
			t.Fatalf("LoadReconElements: %v", err)
		}
		if len(elems["TEACHER"]) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !p.stateFor("testschool").recon.has("TEACHER", 5009) {
		t.Fatal("scan never registered the class's teacher")
	}
}

func assertOnlyScannedSelectable(t *testing.T, flags mdFlags) {
	t.Helper()
	for _, tc := range []struct {
		typ string
		id  int64
	}{
		{"TEACHER", 5009}, {"ROOM", 169}, {"SUBJECT", 7},
	} {
		var got bool
		switch tc.typ {
		case "TEACHER":
			got = flags.Teachers[tc.id]
		case "ROOM":
			got = flags.Rooms[tc.id]
		case "SUBJECT":
			got = flags.Subjects[tc.id]
		}
		if !got {
			t.Errorf("referenced %s %d is not selectable: %+v", tc.typ, tc.id, flags)
		}
	}
	for _, tc := range []struct {
		typ string
		id  int64
	}{
		{"TEACHER", 224}, {"TEACHER", 225}, {"ROOM", 170}, {"SUBJECT", 8},
	} {
		var got bool
		switch tc.typ {
		case "TEACHER":
			got = flags.Teachers[tc.id]
		case "ROOM":
			got = flags.Rooms[tc.id]
		case "SUBJECT":
			got = flags.Subjects[tc.id]
		}
		if got {
			t.Errorf("%s %d is not referenced by any pooled class but is selectable", tc.typ, tc.id)
		}
	}
}

// Logging in persists the whole school catalog, which must not turn the
// unreferenced teachers/rooms/subjects into selectable ones.
func TestCatalogNamesDoNotBecomeSelectable(t *testing.T) {
	f := &fakeUpstream{userDataBody: catalogMasterData}
	p, st := newFakeProxyUpstream(t, f)
	scanOneClass(t, f, p)
	seedKeyUser(t, st)
	if err := st.SetPerm("owen", store.FeatureRecon, true); err != nil {
		t.Fatalf("grant recon: %v", err)
	}

	assertOnlyScannedSelectable(t, decodeMDFlags(t, mdLogin(t, p)))

	// every catalog name is still available for lookup
	for _, tc := range []struct {
		typ, want string
		id        int64
	}{
		{"TEACHER", "A. Hartley", 5009},
		{"TEACHER", "Turing A", 225},
		{"ROOM", "R102", 170},
		{"SUBJECT", "Sport", 8},
	} {
		if got := st.ElementName("testschool", tc.typ, tc.id); got != tc.want {
			t.Errorf("ElementName(%s,%d) = %q, want %q", tc.typ, tc.id, got, tc.want)
		}
	}
	if elems, err := st.LoadReconElements("testschool"); err != nil {
		t.Fatalf("LoadReconElements: %v", err)
	} else {
		for _, typ := range []string{"TEACHER", "ROOM", "SUBJECT"} {
			for _, id := range elems[typ] {
				if id != 5009 && id != 169 && id != 7 {
					t.Errorf("catalog-only %s %d leaked into the reconstructed set %v", typ, id, elems[typ])
				}
			}
		}
	}
}

// The same must hold after a restart, i.e. the persisted set is the reference
// set and the catalog stays out of it.
func TestSelectableSetSurvivesRestartClean(t *testing.T) {
	f := &fakeUpstream{userDataBody: catalogMasterData}
	p, _ := newFakeProxyUpstream(t, f)
	scanOneClass(t, f, p)
	mdLogin(t, p) // a login persists the full catalog before the restart

	// restart against the same database
	srv := httptest.NewTLSServer(f.handler())
	defer srv.Close()
	host := srv.URL[len("https://"):]
	cl := untis.New(untis.Config{Server: host, School: "testschool", HTTPClient: srv.Client()})
	p2 := New(p.store, cl, session.NewManager(5*time.Minute), Options{School: "testschool"})
	p2.LoadRecon("testschool")

	elems, err := p2.store.LoadReconElements("testschool")
	if err != nil {
		t.Fatalf("LoadReconElements: %v", err)
	}
	if len(elems["TEACHER"]) != 1 || elems["TEACHER"][0] != 5009 {
		t.Errorf("persisted teacher set = %v, want [5009] only", elems["TEACHER"])
	}
	if len(elems["ROOM"]) != 1 || elems["ROOM"][0] != 169 {
		t.Errorf("persisted room set = %v, want [169] only", elems["ROOM"])
	}

	seedKeyUser(t, p2.store)
	if err := p2.store.SetPerm("owen", store.FeatureRecon, true); err != nil {
		t.Fatalf("grant recon: %v", err)
	}
	assertOnlyScannedSelectable(t, decodeMDFlags(t, mdLogin(t, p2)))
}

// Every selectable element must actually return periods; an element the app can
// open but that serves nothing is the bug this whole change is about.
func TestSelectableElementsAllLoadForTheRequestedWeek(t *testing.T) {
	f := &fakeUpstream{userDataBody: catalogMasterData}
	p, st := newFakeProxyUpstream(t, f)
	// two classes, so the second teacher/room/subject is referenced too
	f.setTimetable(t, []map[string]any{
		reconPeriod(10, "2026-09-21", "08:00", "08:45", 5009, 169, 7),
		reconPeriod(11, "2026-09-22", "10:00", "10:45", 224, 170, 8),
	})
	seedClassOwner(t, st, "owen", 7, 5000)
	seedClassOwner(t, st, "dee", 8, 4420)
	ys, ye := schoolYearRange(time.Now())
	p.StartRecon("testschool", ys, ye)
	deadline := time.Now().Add(10 * time.Second)
	for {
		elems, _ := st.LoadReconElements("testschool")
		if len(elems["TEACHER"]) >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	seedKeyUser(t, st)
	if err := st.SetPerm("owen", store.FeatureRecon, true); err != nil {
		t.Fatalf("grant recon: %v", err)
	}
	flags := decodeMDFlags(t, mdLogin(t, p))

	// teacher 225 is in no pooled class: it must not be offered
	if flags.Teachers[225] {
		t.Error("teacher 225 is unreferenced but offered")
	}
	// and every offered teacher must serve a week
	for id, selectable := range flags.Teachers {
		if !selectable {
			continue
		}
		rec := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
			ttParam(id, "TEACHER", "2026-09-21", "2026-09-27", "owen")))
		if errCode(t, rec) != 0 {
			t.Errorf("selectable teacher %d refused: %d", id, errCode(t, rec))
			continue
		}
		var resp struct {
			Result struct {
				Timetable struct {
					Periods []map[string]any `json:"periods"`
				} `json:"timetable"`
			} `json:"result"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("bad json: %v (body=%s)", err, rec.Body.String())
		}
		if len(resp.Result.Timetable.Periods) == 0 {
			t.Errorf("selectable teacher %d returns an empty week", id)
		}
	}
}
