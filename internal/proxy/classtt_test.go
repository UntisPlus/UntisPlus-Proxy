package proxy

// Tests for the class timetable fast path: response caching, the day cache, the
// single-flight fetch, and the poll window that keeps the warm cache aligned
// with what the app asks for.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"untis-proxy/internal/store"
)

// seedRawMasterData installs a persisted getUserData2017 body in the proxy, as
// a real login would, so the class cache is allowed to substitute the
// requester's master data on a cache hit.
func seedRawMasterData(t *testing.T, p *Proxy) {
	t.Helper()
	p.cacheRawMasterData("testschool", []byte(`{"jsonrpc":"2.0","id":"md","result":{"masterData":{
		"teachers":[{"id":42,"name":"Persisted"}],
		"rooms":[],"subjects":[],"klassen":[{"id":5000,"name":"5a"}],"absenceReasons":[]}}}`))
	if !p.hasRawMasterData("testschool") {
		t.Fatal("raw master data was not installed")
	}
}

func cachedPeriods(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var resp struct {
		Result struct {
			Timetable struct {
				Periods []map[string]any `json:"periods"`
			} `json:"timetable"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v (body %s)", err, rec.Body.String())
	}
	return resp.Result.Timetable.Periods
}

const cacheTestTimetable = `{"jsonrpc":"2.0","id":"upstream","result":{
	"timetable":{"displayableStartDate":"2026-11-09","displayableEndDate":"2026-11-15","periods":[
		{"id":1,"startDateTime":"2026-11-09T08:00:00Z","endDateTime":"2026-11-09T08:45:00Z",
		 "text":{"lesson":"Mathe"},
		 "elements":[{"id":5009,"type":"TEACHER"}],
		 "can":["READ_HOMEWORK","WRITE_LESSONTOPIC"]}]}}}`

// TestClassTimetableSecondRequestIsServedFromCache: the first request pays for
// the upstream call, the second must not, and the cached answer must carry the
// current request's id.
func TestClassTimetableSecondRequestIsServedFromCache(t *testing.T) {
	f := &fakeUpstream{}
	f.setTimetableBody(cacheTestTimetable)
	p, st := newFakeProxyUpstream(t, f)
	seedRawMasterData(t, p)
	seedSessionUser(t, st, "dee", 5, 7, 5000)

	first := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(5000, "CLASS", "2026-11-09", "2026-11-15", "dee")))
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, body %s", first.Code, first.Body.String())
	}
	if got := f.TimetableCalls(); got != 1 {
		t.Fatalf("first upstream calls = %d, want 1", got)
	}

	second := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(5000, "CLASS", "2026-11-09", "2026-11-15", "dee")))
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, body %s", second.Code, second.Body.String())
	}
	if got := f.TimetableCalls(); got != 1 {
		t.Errorf("upstream calls after second request = %d, want 1 (cache hit)", got)
	}
	var resp struct {
		ID     string `json:"id"`
		Result struct {
			MasterData map[string]any `json:"masterData"`
		} `json:"result"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &resp); err != nil {
		t.Fatalf("second body: %v (%s)", err, second.Body.String())
	}
	if resp.ID != "t" {
		t.Errorf("cached response id = %q, want the request id %q", resp.ID, "t")
	}
	if resp.Result.MasterData == nil {
		t.Error("cached response has no requester masterData")
	}
}

// TestClassTimetableCacheHitIsDecoratedPerViewer: the cached body carries one
// viewer's capability list; a second viewer must still get their own.
func TestClassTimetableCacheHitIsDecoratedPerViewer(t *testing.T) {
	f := &fakeUpstream{}
	f.setTimetableBody(cacheTestTimetable)
	p, st := newFakeProxyUpstream(t, f)
	seedRawMasterData(t, p)
	seedSessionUser(t, st, "dee", 5, 7, 5000)
	seedSessionUser(t, st, "ed", 5, 8, 5000)
	if err := st.SetPerm("ed", store.FeatureEditor, true); err != nil {
		t.Fatalf("grant editor: %v", err)
	}

	dee := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(5000, "CLASS", "2026-11-09", "2026-11-15", "dee")))
	ed := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(5000, "CLASS", "2026-11-09", "2026-11-15", "ed")))
	if got := f.TimetableCalls(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
	deeCaps := periodCaps(t, dee)
	if hasCap(deeCaps, "WRITE_LESSONTOPIC") {
		t.Errorf("basic viewer kept a write capability from the cache: %v", deeCaps)
	}
	if !hasCap(deeCaps, readLessonTopicCap) {
		t.Errorf("basic viewer lost the lesson topic: %v", deeCaps)
	}
	edCaps := periodCaps(t, ed)
	if !hasCap(edCaps, "WRITE_LESSONTOPIC") {
		t.Errorf("editor lost a write capability on a cache hit: %v", edCaps)
	}
}

// TestClassDayCacheServesCoveredRange: a range covered by a fetched week is
// answered without any exact-body cache entry, which is what the poller warms.
func TestClassDayCacheServesCoveredRange(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	seedRawMasterData(t, p)
	seedSessionUser(t, st, "dee", 5, 7, 5000)

	p.rememberClassPeriods("testschool", 5000, "2026-11-09", "2026-11-15", []map[string]any{
		{"id": float64(1), "startDateTime": "2026-11-09T08:00:00Z", "endDateTime": "2026-11-09T08:45:00Z",
			"text": map[string]any{"lesson": "Mathe"}},
		{"id": float64(2), "startDateTime": "2026-11-10T08:00:00Z", "endDateTime": "2026-11-10T08:45:00Z",
			"text": map[string]any{"lesson": "Deutsch"}},
	})

	rec := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(5000, "CLASS", "2026-11-10", "2026-11-10", "dee")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := f.TimetableCalls(); got != 0 {
		t.Errorf("upstream calls = %d, want 0 (day cache covers the range)", got)
	}
	periods := cachedPeriods(t, rec)
	if len(periods) != 1 {
		t.Fatalf("periods = %d, want the one Tuesday lesson", len(periods))
	}
	if got, _ := periods[0]["startDateTime"].(string); !strings.HasPrefix(got, "2026-11-10") {
		t.Errorf("period start = %q, want a Tuesday lesson", got)
	}
}

// TestClassDayCacheRequiresCompleteCoverage: half a week in the cache must not
// be served as a whole week.
func TestClassDayCacheRequiresCompleteCoverage(t *testing.T) {
	p, _ := newFakeProxyUpstream(t, &fakeUpstream{})
	seedRawMasterData(t, p)
	p.rememberClassPeriods("testschool", 5000, "2026-11-09", "2026-11-11", []map[string]any{
		{"id": float64(1), "startDateTime": "2026-11-09T08:00:00Z", "endDateTime": "2026-11-09T08:45:00Z"},
	})
	if _, ok := p.classBodyFromDays("testschool", 5000, "2026-11-09", "2026-11-13"); ok {
		t.Error("a partially covered range was served from the day cache")
	}
}

// TestFetchClassBodyCollapsesConcurrentMisses: a cold burst for one class must
// produce one upstream flight, not one per phone.
func TestFetchClassBodyCollapsesConcurrentMisses(t *testing.T) {
	f := &fakeUpstream{timetableDelay: 100 * time.Millisecond}
	f.setTimetableBody(cacheTestTimetable)
	p, st := newFakeProxyUpstream(t, f)
	seedRawMasterData(t, p)
	seedSessionUser(t, st, "dee", 5, 7, 5000)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
				ttParam(5000, "CLASS", "2026-11-09", "2026-11-15", "dee")))
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, body %s", rec.Code, rec.Body.String())
			}
		}()
	}
	wg.Wait()
	if got := f.TimetableCalls(); got != 1 {
		t.Errorf("upstream calls = %d, want 1 (single flight)", got)
	}
}

// TestClassTimetableCacheKeyIsSchoolScoped guards the key shape the poller uses
// for invalidation and the multi-school separation.
func TestClassTimetableCacheKeyIsSchoolScoped(t *testing.T) {
	a := classTimetableCacheKey("school-a", 5000, "2026-11-09", "2026-11-15")
	b := classTimetableCacheKey("school-b", 5000, "2026-11-09", "2026-11-15")
	if a == b {
		t.Fatalf("cache keys collide across schools: %q", a)
	}
	if !strings.HasPrefix(a, "2017|school-a|5000|") {
		t.Errorf("cache key = %q, want 2017|school|class|start|end", a)
	}
}

// TestPollWindowCoversTheRequestedSchoolWeek pins the window the poller warms:
// Monday..Sunday of the current week, extended by a today+4 lookahead when that
// reaches further.
func TestPollWindowCoversTheRequestedSchoolWeek(t *testing.T) {
	cases := []struct {
		name      string
		now       time.Time
		wantStart string
		wantEnd   string
	}{
		{"monday includes the sunday just gone", time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC), "2026-11-08", "2026-11-15"},
		{"wednesday", time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC), "2026-11-09", "2026-11-15"},
		{"friday lookahead passes sunday", time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC), "2026-11-09", "2026-11-17"},
		{"sunday", time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC), "2026-11-09", "2026-11-19"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			start, end := pollWindow(c.now)
			if start != c.wantStart || end != c.wantEnd {
				t.Errorf("pollWindow(%s) = %s..%s, want %s..%s",
					c.now.Format("2006-01-02"), start, end, c.wantStart, c.wantEnd)
			}
		})
	}
}

// TestMasterDataServedFromPersistedRawWithoutUpstream: after a restart, the
// persisted body must answer immediately instead of making the first request
// wait for getUserData2017.
func TestMasterDataServedFromPersistedRawWithoutUpstream(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	_ = st.SetDefaultSchool("testschool")
	if err := st.SetSetting(masterDataSettingPrefix+"testschool", `{"jsonrpc":"2.0","id":"md","result":{"masterData":{
		"teachers":[{"id":42,"name":"Persisted"}],"rooms":[],"subjects":[],"klassen":[],"absenceReasons":[]}}}`); err != nil {
		t.Fatalf("seed setting: %v", err)
	}
	p := newFakeProxyUpstreamWithStore(t, &fakeUpstream{}, st)

	md := p.masterData("testschool")
	if md == nil {
		t.Fatal("masterData returned nil from a persisted body")
	}
	if got := md.teachers[42]; got != "Persisted" {
		t.Errorf("teacher 42 = %q, want the persisted name", got)
	}
}

// TestBoostedClassTimetableServedFromWarmCache: a boosted user used to bypass
// every class cache and pay for a teacher fetch on each request. A class
// timetable is class-wide data, so the poller-warmed day cache answers it.
func TestBoostedClassTimetableServedFromWarmCache(t *testing.T) {
	f := &fakeUpstream{}
	f.setTimetableBody(cacheTestTimetable)
	p, st := newFakeProxyUpstream(t, f)
	seedRawMasterData(t, p)
	seedTeacherSource(t, st, "mrTeacher", 5009)
	seedSessionUser(t, st, "dee", 5, 7, 5000)
	if err := st.SetBoostedFlag("dee", true); err != nil {
		t.Fatalf("boost dee: %v", err)
	}
	// What the poller leaves behind after its tick.
	p.rememberClassPeriods("testschool", 5000, "2026-11-09", "2026-11-15", []map[string]any{
		{"id": float64(1), "startDateTime": "2026-11-09T08:00:00Z", "endDateTime": "2026-11-09T08:45:00Z",
			"text": map[string]any{"lesson": "Mathe"},
			"can":  []any{"READ_LESSONTOPIC", "WRITE_LESSONTOPIC"}},
	})

	rec := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(5000, "CLASS", "2026-11-09", "2026-11-15", "dee")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := f.TimetableCalls(); got != 0 {
		t.Errorf("upstream calls = %d, want 0 (warm day cache)", got)
	}
	periods := cachedPeriods(t, rec)
	if len(periods) != 1 {
		t.Fatalf("periods = %d, want 1", len(periods))
	}
	caps := periodCaps(t, rec)
	if hasCap(caps, "WRITE_LESSONTOPIC") {
		t.Errorf("boosted non-editor kept a write capability: %v", caps)
	}
	if !hasCap(caps, readLessonTopicCap) {
		t.Errorf("boosted user lost the lesson topic: %v", caps)
	}
}

// TestBoostedClassTimetableMissFillsSharedCache: a cold boosted request still
// pays for one teacher fetch, but it must land in the cache so the next request
// (boosted or not) is free.
func TestBoostedClassTimetableMissFillsSharedCache(t *testing.T) {
	f := &fakeUpstream{}
	f.setTimetableBody(cacheTestTimetable)
	p, st := newFakeProxyUpstream(t, f)
	seedRawMasterData(t, p)
	seedTeacherSource(t, st, "mrTeacher", 5009)
	seedSessionUser(t, st, "dee", 5, 7, 5000)
	if err := st.SetBoostedFlag("dee", true); err != nil {
		t.Fatalf("boost dee: %v", err)
	}

	first := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(5000, "CLASS", "2026-11-09", "2026-11-15", "dee")))
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, body %s", first.Code, first.Body.String())
	}
	if got := f.TimetableCalls(); got != 1 {
		t.Fatalf("first upstream calls = %d, want 1", got)
	}
	second := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(5000, "CLASS", "2026-11-09", "2026-11-15", "dee")))
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, body %s", second.Code, second.Body.String())
	}
	if got := f.TimetableCalls(); got != 1 {
		t.Errorf("upstream calls after second request = %d, want 1 (cache hit)", got)
	}
	if _, ok := p.tt.Get(classTimetableCacheKey("testschool", 5000, "2026-11-09", "2026-11-15")); !ok {
		t.Error("boosted class fetch did not fill the shared class cache")
	}
}

// TestBoostedStudentTimetableServedFromWarmCache: a boosted student's personal
// timetable can be answered from the poller-warmed day cache. The cached data is
// viewer-neutral; capabilities are replaced per viewer.
func TestBoostedStudentTimetableServedFromWarmCache(t *testing.T) {
	f := &fakeUpstream{}
	f.setTimetableBody(cacheTestTimetable)
	p, st := newFakeProxyUpstream(t, f)
	seedRawMasterData(t, p)
	seedTeacherSource(t, st, "mrTeacher", 5009)
	seedSessionUser(t, st, "dee", 5, 7, 5000)
	if err := st.SetBoostedFlag("dee", true); err != nil {
		t.Fatalf("boost dee: %v", err)
	}
	// What the warmer leaves behind: the whole week, so the app's Monday..Saturday
	// range is covered too.
	p.rememberStudentPeriods("testschool", 7, "2026-11-09", "2026-11-15", []map[string]any{
		{"id": float64(1), "startDateTime": "2026-11-09T08:00:00Z", "endDateTime": "2026-11-09T08:45:00Z",
			"text": map[string]any{"lesson": "Mathe"},
			"can":  []any{"READ_LESSONTOPIC", "WRITE_LESSONTOPIC"}},
	})

	rec := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(7, "STUDENT", "2026-11-09", "2026-11-14", "dee")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := f.TimetableCalls(); got != 0 {
		t.Errorf("upstream calls = %d, want 0 (warm personal day cache)", got)
	}
	periods := cachedPeriods(t, rec)
	if len(periods) != 1 {
		t.Fatalf("periods = %d, want 1", len(periods))
	}
	caps := periodCaps(t, rec)
	if hasCap(caps, "WRITE_LESSONTOPIC") {
		t.Errorf("boosted non-editor kept a write capability: %v", caps)
	}
	if !hasCap(caps, readLessonTopicCap) {
		t.Errorf("boosted user lost the lesson topic: %v", caps)
	}
}

// TestBoostedStudentTimetableMissFillsSharedCache: a cold personal request pays
// for one teacher fetch, then both the exact range and any range covered by the
// fetched days are answered locally.
func TestBoostedStudentTimetableMissFillsSharedCache(t *testing.T) {
	f := &fakeUpstream{}
	f.setTimetableBody(cacheTestTimetable)
	p, st := newFakeProxyUpstream(t, f)
	seedRawMasterData(t, p)
	seedTeacherSource(t, st, "mrTeacher", 5009)
	seedSessionUser(t, st, "dee", 5, 7, 5000)
	if err := st.SetBoostedFlag("dee", true); err != nil {
		t.Fatalf("boost dee: %v", err)
	}

	first := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(7, "STUDENT", "2026-11-09", "2026-11-15", "dee")))
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, body %s", first.Code, first.Body.String())
	}
	if got := f.TimetableCalls(); got != 1 {
		t.Fatalf("first upstream calls = %d, want 1", got)
	}
	if _, ok := p.tt.Get(studentTimetableCacheKey("testschool", 7, "2026-11-09", "2026-11-15")); !ok {
		t.Error("boosted student fetch did not fill the shared response cache")
	}

	second := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(7, "STUDENT", "2026-11-09", "2026-11-15", "dee")))
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, body %s", second.Code, second.Body.String())
	}
	if got := f.TimetableCalls(); got != 1 {
		t.Errorf("upstream calls after exact repeat = %d, want 1", got)
	}

	covered := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(7, "STUDENT", "2026-11-10", "2026-11-12", "dee")))
	if covered.Code != http.StatusOK {
		t.Fatalf("covered status = %d, body %s", covered.Code, covered.Body.String())
	}
	if got := f.TimetableCalls(); got != 1 {
		t.Errorf("upstream calls after covered range = %d, want 1 (day cache)", got)
	}
}

// TestWarmBoostedStudentsFetchesAppWeeksOnce: the warmer pre-fetches the five
// weeks the app opens with, and skips every range that is already warm.
func TestWarmBoostedStudentsFetchesAppWeeksOnce(t *testing.T) {
	f := &fakeUpstream{}
	f.setTimetableBody(cacheTestTimetable)
	p, st := newFakeProxyUpstream(t, f)
	seedRawMasterData(t, p)
	seedTeacherSource(t, st, "mrTeacher", 5009)
	seedSessionUser(t, st, "dee", 5, 7, 5000)
	if err := st.SetBoostedFlag("dee", true); err != nil {
		t.Fatalf("boost dee: %v", err)
	}

	if got := len(studentWarmRanges(time.Now())); got != 5 {
		t.Fatalf("warm ranges = %d, want 5", got)
	}
	p.warmBoostedStudents("testschool")
	if got := f.TimetableCalls(); got != 5 {
		t.Fatalf("first warm upstream calls = %d, want 5", got)
	}
	p.warmBoostedStudents("testschool")
	if got := f.TimetableCalls(); got != 5 {
		t.Errorf("second warm upstream calls = %d, want 5 (all ranges warm)", got)
	}
}

// TestWarmTimetableKeepsElementNames: a warm response must not rewrite the
// persisted element-name catalogue. That rewrite used to run inside the
// per-request masterData decoration, so every cache hit paid for hundreds of
// UPSERTs that could not possibly change anything between two logins.
func TestWarmTimetableKeepsElementNames(t *testing.T) {
	f := &fakeUpstream{}
	f.setTimetableBody(cacheTestTimetable)
	p, st := newFakeProxyUpstream(t, f)
	seedRawMasterData(t, p)
	seedSessionUser(t, st, "dee", 5, 7, 5000)

	body := internBody("getTimetable2017", ttParam(5000, "CLASS", "2026-11-09", "2026-11-09", "dee"))
	if rec := internReq(t, p, "getTimetable2017", "", body); rec.Code != http.StatusOK {
		t.Fatalf("first status = %d, body %s", rec.Code, rec.Body.String())
	}
	// The login is allowed to seed the catalogue; a later warm request is not.
	if err := st.UpsertElementNames("testschool", "TEACHER", map[int64]string{42: "Sentinel"}); err != nil {
		t.Fatalf("seed sentinel: %v", err)
	}
	if rec := internReq(t, p, "getTimetable2017", "", body); rec.Code != http.StatusOK {
		t.Fatalf("warm status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := st.ElementName("testschool", "TEACHER", 42); got != "Sentinel" {
		t.Errorf("warm timetable rewrote element names: got %q, want Sentinel", got)
	}
}

// TestViewerMasterDataCacheTracksPermissions: the cached per-viewer masterData
// is keyed by the permissions that shape it, so granting a flag changes the
// next response even though the raw catalogue did not change.
func TestViewerMasterDataCacheTracksPermissions(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	p.cacheRawMasterData("testschool", []byte(`{"jsonrpc":"2.0","id":"md","result":{"masterData":{
		"teachers":[],"rooms":[],"subjects":[],
		"klassen":[{"id":5000,"name":"5a"},{"id":6000,"name":"6b"}],
		"absenceReasons":[]}}}`))
	seedSessionUser(t, st, "dee", 5, 7, 5000)

	displayable := func(b []byte) map[int64]bool {
		t.Helper()
		var md struct {
			Klassen []struct {
				ID          int64 `json:"id"`
				Displayable bool  `json:"displayable"`
			} `json:"klassen"`
		}
		if err := json.Unmarshal(b, &md); err != nil {
			t.Fatalf("bad masterData: %v", err)
		}
		out := map[int64]bool{}
		for _, k := range md.Klassen {
			out[k.ID] = k.Displayable
		}
		return out
	}

	basic := displayable(p.viewerMasterDataBytes("testschool", "dee"))
	if !basic[5000] || basic[6000] {
		t.Errorf("basic viewer flags = %v, want only the pooled class 5000", basic)
	}
	if err := st.SetBoostedFlag("dee", true); err != nil {
		t.Fatalf("boost: %v", err)
	}
	boosted := displayable(p.viewerMasterDataBytes("testschool", "dee"))
	if !boosted[5000] || !boosted[6000] {
		t.Errorf("boosted viewer flags = %v, want every class", boosted)
	}
}

// TestViewerMasterDataCacheDropsOnFreshCopy: a new login carries a new
// catalogue, so cached per-viewer copies derived from the old one must not be
// served.
func TestViewerMasterDataCacheDropsOnFreshCopy(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	seedRawMasterData(t, p)
	seedSessionUser(t, st, "dee", 5, 7, 5000)

	before := string(p.viewerMasterDataBytes("testschool", "dee"))
	p.cacheRawMasterData("testschool", []byte(`{"jsonrpc":"2.0","id":"md","result":{"masterData":{
		"teachers":[{"id":42,"name":"Fresh"}],"rooms":[],"subjects":[],
		"klassen":[{"id":5000,"name":"5a"}],"absenceReasons":[]}}}`))
	after := string(p.viewerMasterDataBytes("testschool", "dee"))
	if before == after {
		t.Fatal("viewer masterData cache survived a fresh raw copy")
	}
	if !strings.Contains(after, `"Fresh"`) {
		t.Errorf("viewer masterData did not pick up the fresh copy: %s", after)
	}
}

// TestRenderCachedTimetableKeepsUnknownFields: the single-pass renderer may only
// replace the fields it owns. Anything else the real server sent must survive
// byte-for-byte in value.
func TestRenderCachedTimetableKeepsUnknownFields(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	seedRawMasterData(t, p)
	seedSessionUser(t, st, "dee", 5, 7, 5000)

	raw := []byte(`{"jsonrpc":"2.0","id":"old","result":{"keep":{"a":1},"timetable":{"periods":[
		{"id":1,"can":["READ_HOMEWORK","WRITE_LESSONTOPIC"]}]},"masterData":{"klassen":[]}}}`)
	out := p.renderCachedTimetable("testschool", "dee", "new", raw, false, "", "")
	var doc struct {
		ID     string `json:"id"`
		Result struct {
			Keep struct {
				A int `json:"a"`
			} `json:"keep"`
			Timetable struct {
				Periods []struct {
					Can []string `json:"can"`
				} `json:"periods"`
			} `json:"timetable"`
			MasterData struct {
				Teachers []struct {
					Name string `json:"name"`
				} `json:"teachers"`
			} `json:"masterData"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("bad render: %v (%s)", err, out)
	}
	if doc.ID != "new" {
		t.Errorf("id = %q, want new", doc.ID)
	}
	if doc.Result.Keep.A != 1 {
		t.Errorf("unknown result field was dropped: %+v", doc.Result.Keep)
	}
	if len(doc.Result.MasterData.Teachers) != 1 || doc.Result.MasterData.Teachers[0].Name != "Persisted" {
		t.Errorf("masterData was not replaced with the viewer copy: %+v", doc.Result.MasterData)
	}
	if len(doc.Result.Timetable.Periods) != 1 {
		t.Fatalf("periods = %d, want 1", len(doc.Result.Timetable.Periods))
	}
	caps := doc.Result.Timetable.Periods[0].Can
	for _, c := range caps {
		if c == "WRITE_LESSONTOPIC" {
			t.Errorf("basic viewer kept a write capability: %v", caps)
		}
	}
	found := false
	for _, c := range caps {
		if c == readLessonTopicCap {
			found = true
		}
	}
	if !found {
		t.Errorf("capabilities = %v, want %s", caps, readLessonTopicCap)
	}
}
