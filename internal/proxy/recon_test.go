package proxy

// Reconstruction: the element enumeration scan (StartRecon/scanClass), the
// warm start from persistence (LoadRecon/PersistRecon), and the two element
// reconstruction paths (student personal timetables and teacher/room/subject
// schedules merged out of the pooled classes).

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"untis-proxy/internal/store"
)

// reconPeriod is one upstream period referencing a teacher, room and subject.
func reconPeriod(id float64, day, start, end string, teacher, room, subject int64) map[string]any {
	return map[string]any{
		"id":            id,
		"startDateTime": day + "T" + start + ":00Z",
		"endDateTime":   day + "T" + end + ":00Z",
		"startTime":     day + " " + start + ":00",
		"endTime":       day + " " + end + ":00",
		"elements":      []any{map[string]any{"id": teacher, "type": "TEACHER"}, map[string]any{"id": room, "type": "ROOM"}, map[string]any{"id": subject, "type": "SUBJECT"}},
	}
}

func seedClassOwner(t *testing.T, st *store.Store, username string, personID, classID int64) {
	t.Helper()
	if err := st.UpsertUser(&store.User{
		Username: username, Method: "key", School: "testschool", Password: "replayable",
		PersonType: 5, PersonID: personID, ClassID: classID, ClassName: "10b",
	}); err != nil {
		t.Fatalf("seed class owner %s: %v", username, err)
	}
}

func TestStartReconEnumeratesAndPersistsElements(t *testing.T) {
	f := &fakeUpstream{}
	f.setTimetable(t, []map[string]any{reconPeriod(10, "2026-09-21", "08:00", "08:45", 5009, 169, 7)})
	p, st := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st, "owen", 7, 5000)

	ys, ye := schoolYearRange(time.Now())
	p.StartRecon("testschool", ys, ye)

	// the scan runs detached; wait for it to persist the discovered elements
	var elems map[string][]int64
	deadline := time.Now().Add(10 * time.Second)
	for {
		var err error
		elems, err = st.LoadReconElements("testschool")
		if err != nil {
			t.Fatalf("LoadReconElements: %v", err)
		}
		if len(elems["TEACHER"]) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	for _, tc := range []struct {
		typ string
		id  int64
	}{
		{"TEACHER", 5009},
		{"ROOM", 169},
		{"SUBJECT", 7},
	} {
		if !containsInt(elems[tc.typ], tc.id) {
			t.Errorf("persisted %s %d missing, got %v", tc.typ, tc.id, elems[tc.typ])
		}
		if !p.stateFor("testschool").recon.has(tc.typ, tc.id) {
			t.Errorf("in-memory recon set missing %s %d", tc.typ, tc.id)
		}
	}
	// a stale element from a previous boot must be dropped, not merged: seed one
	// before scanning and assert it is gone afterwards.
	p2f := &fakeUpstream{}
	p2f.setTimetable(t, []map[string]any{reconPeriod(11, "2026-09-21", "10:00", "10:45", 224, 170, 8)})
	p2, st2 := newFakeProxyUpstream(t, p2f)
	if err := st2.SaveReconElements("testschool", map[string][]int64{"TEACHER": {999}}); err != nil {
		t.Fatalf("seed stale element: %v", err)
	}
	p2.LoadRecon("testschool")
	if !p2.stateFor("testschool").recon.has("TEACHER", 999) {
		t.Error("LoadRecon must warm-start the in-memory set from the persisted snapshot")
	}
	seedClassOwner(t, st2, "owen", 7, 5000)
	p2.StartRecon("testschool", ys, ye)
	deadline = time.Now().Add(10 * time.Second)
	for p2.stateFor("testschool").recon.has("TEACHER", 999) {
		if time.Now().After(deadline) {
			t.Fatal("scan never replaced the stale element set")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !p2.stateFor("testschool").recon.has("TEACHER", 224) {
		t.Error("scan did not register the freshly scanned teacher 224")
	}
}

func TestStartReconWithoutPoolDoesNothing(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	ys, ye := schoolYearRange(time.Now())
	p.StartRecon("testschool", ys, ye)
	elems, err := st.LoadReconElements("testschool")
	if err != nil {
		t.Fatalf("LoadReconElements: %v", err)
	}
	if len(elems) != 0 {
		t.Errorf("recon elements = %v, want none for an empty pool", elems)
	}
	if n := len(f.Forwards()); n != 0 {
		t.Errorf("upstream forwards = %d, want 0", n)
	}
}

func TestLoadReconAndPersistReconRoundTrip(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.SaveReconElements("testschool", map[string][]int64{"ROOM": {169}}); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	p.LoadRecon("testschool")
	if !p.stateFor("testschool").recon.has("ROOM", 169) {
		t.Fatal("LoadRecon did not seed the room set")
	}

	// PersistRecon writes the current in-memory set. The store deliberately
	// never deletes rows (recon_elements also holds element names for elements
	// outside the scanned pool), so the seeded room row must survive.
	p.stateFor("testschool").recon.seedFrom(map[string][]int64{"TEACHER": {5009}})
	p.PersistRecon("testschool")
	elems, err := st.LoadReconElements("testschool")
	if err != nil {
		t.Fatalf("LoadReconElements: %v", err)
	}
	if !containsInt(elems["TEACHER"], 5009) {
		t.Errorf("PersistRecon did not write the teacher set: %v", elems)
	}
	if !containsInt(elems["ROOM"], 169) {
		t.Errorf("PersistRecon must not delete pre-existing element rows: %v", elems)
	}
}

func TestLoadReconIgnoresEmptySnapshot(t *testing.T) {
	p, _ := newTestProxy(t)
	p.LoadRecon("testschool") // no rows: must not panic or invent elements
	if got := p.stateFor("testschool").recon.counts(); got["TEACHER"] != 0 {
		t.Errorf("counts = %v, want empty after loading nothing", got)
	}
}

func TestStudentPeriodsUsesTheStudentsOwnAccount(t *testing.T) {
	f := &fakeUpstream{}
	f.setTimetable(t, []map[string]any{reconPeriod(20, "2026-09-22", "09:00", "09:45", 5009, 169, 7)})
	p, st := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st, "evan", 9, 5000)

	periods, err := p.studentPeriods("testschool", 9, "2026-09-21", "2026-09-27")
	if err != nil {
		t.Fatalf("studentPeriods: %v", err)
	}
	if len(periods) != 1 {
		t.Fatalf("periods = %d, want 1: %v", len(periods), periods)
	}

	// the request must go upstream as a STUDENT request for that person
	found := false
	for _, fw := range f.Forwards() {
		if fw.Method == "getTimetable2017" {
			if strings.Contains(string(fw.Body), `"type":"STUDENT"`) && strings.Contains(string(fw.Body), `"id":9`) {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("no STUDENT getTimetable2017 forward for person 9, forwards = %d", len(f.Forwards()))
	}

	// second call is served from the range cache
	before := len(f.Forwards())
	if _, err := p.studentPeriods("testschool", 9, "2026-09-21", "2026-09-27"); err != nil {
		t.Fatalf("cached studentPeriods: %v", err)
	}
	if after := len(f.Forwards()); after != before {
		t.Errorf("cached call made %d extra upstream requests, want 0", after-before)
	}
}

func TestStudentPeriodsWithoutMatchingUserFails(t *testing.T) {
	f := &fakeUpstream{}
	p, _ := newFakeProxyUpstream(t, f)
	if _, err := p.studentPeriods("testschool", 4242, "2026-09-21", "2026-09-27"); err == nil {
		t.Error("studentPeriods for an unknown person must fail, not return an empty timetable")
	}
}

func TestElementPeriodsMergesAndDedupesPooledClasses(t *testing.T) {
	f := &fakeUpstream{}
	// every class forward returns the same lesson, so two pooled classes must
	// collapse into a single reconstructed period
	f.setTimetable(t, []map[string]any{
		reconPeriod(30, "2026-09-21", "08:00", "08:45", 5009, 169, 7),
		reconPeriod(31, "2026-09-22", "10:00", "10:45", 224, 170, 7),
	})
	p, st := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st, "owen", 7, 5000)
	seedClassOwner(t, st, "dee", 8, 4420)

	periods, err := p.elementPeriods("testschool", "TEACHER", 5009, "2026-09-21", "2026-09-27")
	if err != nil {
		t.Fatalf("elementPeriods: %v", err)
	}
	if len(periods) != 1 {
		t.Errorf("periods = %d, want 1 (the same lesson seen in both classes dedupes): %v", len(periods), periods)
	}

	// a teacher who is not in any pooled lesson yields nothing
	none, err := p.elementPeriods("testschool", "TEACHER", 999, "2026-09-21", "2026-09-27")
	if err != nil {
		t.Fatalf("elementPeriods: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("periods for unknown teacher 999 = %v, want none", none)
	}
}

func TestFetchElementRawUsesBoostedSource(t *testing.T) {
	f := &fakeUpstream{}
	f.setTimetable(t, []map[string]any{reconPeriod(40, "2026-09-21", "08:00", "08:45", 5009, 169, 7)})
	p, st := newFakeProxyUpstream(t, f)
	seedTeacherSource(t, st, "mrTeacher", 5009)

	periods, err := p.fetchElementRaw("testschool", "TEACHER", 5009, "2026-09-21", "2026-09-27")
	if err != nil {
		t.Fatalf("fetchElementRaw: %v", err)
	}
	if len(periods) != 1 {
		t.Fatalf("periods = %d, want 1", len(periods))
	}
	found := false
	for _, fw := range f.Forwards() {
		if strings.Contains(string(fw.Body), `"type":"TEACHER"`) && strings.Contains(string(fw.Body), `"id":5009`) {
			found = true
		}
	}
	if !found {
		t.Errorf("no teacher getTimetable2017 forward for 5009, forwards = %d", len(f.Forwards()))
	}
}

func TestFetchElementRawErrorPaths(t *testing.T) {
	p, _ := newTestProxy(t)
	if _, err := p.fetchElementRaw("testschool", "TEACHER", 5009, "2026-09-21", "2026-09-27"); err == nil {
		t.Error("fetchElementRaw without a boosted source account must fail")
	}
	f := &fakeUpstream{}
	p2, st2 := newFakeProxyUpstream(t, f)
	seedTeacherSource(t, st2, "mrTeacher", 5009)
	if _, err := p2.fetchElementRaw("testschool", "CLASS", 5000, "2026-09-21", "2026-09-27"); err == nil {
		t.Error("fetchElementRaw for an unsupported element type must fail")
	}
}

func TestRESTWeeklyTeacherReconstructedFromPool(t *testing.T) {
	f := &fakeUpstream{}
	f.setTimetable(t, []map[string]any{reconPeriod(50, "2026-09-21", "08:00", "08:45", 5009, 169, 7)})
	p, st := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st, "owen", 7, 5000)
	seedSessionUser(t, st, "sam", 5, 11, 5000)
	if err := st.SetPerm("sam", store.FeatureRecon, true); err != nil {
		t.Fatalf("grant recon: %v", err)
	}
	if err := st.SaveReconElements("testschool", map[string][]int64{"TEACHER": {5009}}); err != nil {
		t.Fatalf("save recon elements: %v", err)
	}

	rec := restReq(t, p, "sam", http.MethodGet,
		"/WebUntis/api/public/timetable/weekly/data?elementType=2&elementId=5009&date=2026-09-21&school=testschool", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"5009"`) {
		t.Errorf("response has no elementPeriods entry for teacher 5009: %s", body)
	}
	// every referenced element is offered back to the client with its type id
	for _, want := range []string{`"type":2`, `"type":3`, `"type":4`} {
		if !strings.Contains(body, want) {
			t.Errorf("response missing %s (teacher/subject/room type ids): %s", want, body)
		}
	}
	if n := len(f.Rests()); n != 0 {
		t.Errorf("rest forwards = %d, want 0 (reconstruction must not hit the REST API)", n)
	}
}

func TestRESTWeeklyUnknownElementForbidden(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st, "owen", 7, 5000)
	seedSessionUser(t, st, "sam", 5, 11, 5000)
	if err := st.SetPerm("sam", store.FeatureRecon, true); err != nil {
		t.Fatalf("grant recon: %v", err)
	}
	// teacher 5009 was never reconstructed
	rec := restReq(t, p, "sam", http.MethodGet,
		"/WebUntis/api/public/timetable/weekly/data?elementType=2&elementId=5009&date=2026-09-21&school=testschool", "", "")
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a non-reconstructed element", rec.Code)
	}
}

func containsInt(xs []int64, want int64) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// TestReconResumesFromStoredScanHorizon: a restart must not re-fetch the whole
// year. A class already scanned through the current horizon is skipped without a
// single upstream request, and its discovered elements stay available.
func TestReconResumesFromStoredScanHorizon(t *testing.T) {
	f := &fakeUpstream{}
	f.setTimetable(t, []map[string]any{reconPeriod(10, "2026-09-21", "08:00", "08:45", 5009, 169, 7)})
	p, st := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st, "owen", 7, 5000)

	ys, ye := schoolYearRange(time.Now())
	p.StartRecon("testschool", ys, ye)
	waitFor(t, 10*time.Second, func() bool {
		elems, _ := st.LoadReconElements("testschool")
		return len(elems["TEACHER"]) == 1
	}, "first scan to find teacher 5009")

	scanned, err := st.ReconScan("testschool")
	if err != nil {
		t.Fatalf("ReconScan: %v", err)
	}
	if scanned[5000] == "" {
		t.Fatalf("scan horizon not recorded, got %v", scanned)
	}
	before := f.TimetableCalls()
	if before == 0 {
		t.Fatal("first scan made no upstream timetable calls, test is not measuring anything")
	}

	// Fresh in-memory state over the same database, as after a restart.
	p2 := newFakeProxyUpstreamWithStore(t, f, st)
	p2.LoadRecon("testschool")
	if !p2.stateFor("testschool").recon.has("TEACHER", 5009) {
		t.Error("warm start did not restore teacher 5009 from the persisted snapshot")
	}
	p2.StartRecon("testschool", ys, ye)
	// Nothing is stale, so the sweep returns synchronously without fetching.
	if got := f.TimetableCalls(); got != before {
		t.Errorf("restart re-fetched the school server: %d extra calls (total %d)", got-before, got)
	}
	if !p2.stateFor("testschool").recon.has("TEACHER", 5009) {
		t.Error("skipped sweep dropped the elements of the skipped class")
	}
}

// TestReconReScansOnlyWhenStale: once a class's recorded horizon falls outside
// the refresh window it is re-scanned, resuming from the stored date rather than
// the year start.
func TestReconReScansOnlyWhenStale(t *testing.T) {
	f := &fakeUpstream{}
	f.setTimetable(t, []map[string]any{reconPeriod(10, "2026-09-21", "08:00", "08:45", 5009, 169, 7)})
	p, st := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st, "owen", 7, 5000)

	stale := time.Now().AddDate(0, 0, -reconHorizonDays-2).Format("2006-01-02")
	if err := st.SaveReconScanAt("testschool", 5000, stale); err != nil {
		t.Fatalf("SaveReconScanAt: %v", err)
	}
	ys, ye := schoolYearRange(time.Now())
	p.StartRecon("testschool", ys, ye)
	waitFor(t, 10*time.Second, func() bool {
		elems, _ := st.LoadReconElements("testschool")
		return len(elems["TEACHER"]) == 1
	}, "stale class to be re-scanned")

	after, _ := st.ReconScan("testschool")
	if after[5000] == stale {
		t.Errorf("scan horizon still %q, want the current target %q", after[5000], reconHorizon())
	}
	if f.TimetableCalls() == 0 {
		t.Error("stale class was not re-scanned")
	}
}

// TestReconForceRescanIgnoresStoredHorizon: RequestRescan (the admin escape
// hatch) re-enumerates every class even when nothing is stale.
func TestReconForceRescanIgnoresStoredHorizon(t *testing.T) {
	f := &fakeUpstream{}
	f.setTimetable(t, []map[string]any{reconPeriod(10, "2026-09-21", "08:00", "08:45", 5009, 169, 7)})
	p, st := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st, "owen", 7, 5000)

	if err := st.SaveReconScanAt("testschool", 5000, reconHorizon()); err != nil {
		t.Fatalf("SaveReconScanAt: %v", err)
	}
	ys, ye := schoolYearRange(time.Now())
	p.RequestRescan()
	p.StartRecon("testschool", ys, ye)
	waitFor(t, 10*time.Second, func() bool { return f.TimetableCalls() > 0 },
		"forced rescan to hit the upstream")
}

// TestReconStaleIsHorizonWindowAware: reconStale is the whole resume decision,
// so pin its boundaries.
func TestReconStaleIsHorizonWindowAware(t *testing.T) {
	target := reconHorizon()
	if !reconStale("", target, 21) {
		t.Error("a class that was never scanned must be stale")
	}
	if !reconStale("not-a-date", target, 21) {
		t.Error("an unparseable horizon must be treated as stale")
	}
	fresh := time.Now().Format("2006-01-02")
	if reconStale(fresh, target, 21) {
		t.Error("a class scanned today must not be stale with a 21 day window")
	}
	old := time.Now().AddDate(0, 0, -30).Format("2006-01-02")
	if !reconStale(old, target, 21) {
		t.Error("a class scanned 30 days ago must be stale with a 21 day window")
	}
	if reconStale(old, target, 60) {
		t.Error("a 30 day old horizon must not be stale with a 60 day window")
	}
}

// waitFor polls cond until it holds or the timeout expires, failing with msg.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, msg)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestReconPartialSweepMergesSkippedClasses: when only some classes are stale,
// the sweep must merge the freshly found elements into the persisted set instead
// of replacing it. Replacing would silently drop every element that is only
// reachable through a class this sweep skipped — the exact failure that makes
// teacher/room/subject feeds answer "unknown element" after a routine restart.
func TestReconPartialSweepMergesSkippedClasses(t *testing.T) {
	f := &fakeUpstream{}
	f.setTimetable(t, []map[string]any{reconPeriod(10, "2026-09-21", "08:00", "08:45", 5009, 169, 7)})
	p, st := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st, "owen", 7, 5000)
	seedClassOwner(t, st, "mira", 8, 4420)

	// 5000 is fresh and will be skipped; 4420 is stale and will be re-scanned.
	if err := st.SaveReconScanAt("testschool", 5000, reconHorizon()); err != nil {
		t.Fatalf("SaveReconScanAt(5000): %v", err)
	}
	stale := time.Now().AddDate(0, 0, -reconHorizonDays-2).Format("2006-01-02")
	if err := st.SaveReconScanAt("testschool", 4420, stale); err != nil {
		t.Fatalf("SaveReconScanAt(4420): %v", err)
	}
	// Teacher 999 is only reachable through the skipped class, so a replacing
	// sweep would lose it.
	if err := st.SaveReconElements("testschool", map[string][]int64{
		"TEACHER": {999},
		"ROOM":    {},
		"SUBJECT": {},
	}); err != nil {
		t.Fatalf("SaveReconElements: %v", err)
	}

	ys, ye := schoolYearRange(time.Now())
	p.StartRecon("testschool", ys, ye)
	waitFor(t, 10*time.Second, func() bool {
		scan, _ := st.ReconScan("testschool")
		return scan[4420] != "" && scan[4420] != stale
	}, "the stale class to be re-scanned")

	elems, err := st.LoadReconElements("testschool")
	if err != nil {
		t.Fatalf("LoadReconElements: %v", err)
	}
	if !containsID(elems["TEACHER"], 999) {
		t.Errorf("partial sweep dropped the skipped class's teacher 999: %v", elems["TEACHER"])
	}
	if !containsID(elems["TEACHER"], 5009) {
		t.Errorf("partial sweep did not add the freshly scanned teacher 5009: %v", elems["TEACHER"])
	}
	if !containsID(elems["ROOM"], 169) {
		t.Errorf("partial sweep did not add the freshly scanned room 169: %v", elems["ROOM"])
	}
	if !p.stateFor("testschool").recon.has("TEACHER", 999) {
		t.Error("in-memory registry lost the skipped class's teacher 999")
	}
}

func containsID(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
