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
