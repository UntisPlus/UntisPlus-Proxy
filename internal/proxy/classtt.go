package proxy

// Class timetable fast path.
//
// The app's first request of the day used to be forwarded to WebUntis every
// time: the 2017 cache was disabled, and the change poller already fetched the
// same class but stored only a flattened snapshot. This file keeps the class
// periods the poller sees, indexed by day, so an app request for any range the
// poller has already covered is answered without an upstream call. The exact
// upstream body is cached as well, because it preserves fields this proxy does
// not model.
//
// Nothing viewer-specific is stored. The cached data is the class's timetable;
// master data, capabilities and personal events are applied per request after
// the cache read, exactly as they were on the upstream path.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

// maxClassDayEntries bounds the retained history for one class. The poller only
// ever writes a week or two, so this is headroom for a client asking for a
// wider range, not a growing archive.
const maxClassDayEntries = 180

// maxClassDaySpanDays bounds a range the day cache is willing to store or
// serve. The cache is a warm-up for the week the app opens, not an archive; a
// term-long calendar export must not copy a year of periods into memory.
const maxClassDaySpanDays = 31

type classFetchCall struct {
	done   chan struct{}
	body   []byte
	status int
	err    error
}

type classDayKey struct {
	school  string
	classID int64
}

type cachedPeriod struct {
	raw   json.RawMessage
	start string
	id    int64
}

type classDay struct {
	periods []cachedPeriod
	at      time.Time
}

type classDayCache struct {
	mu   sync.RWMutex
	days map[classDayKey]map[string]classDay
}

func newClassDayCache() *classDayCache {
	return &classDayCache{days: map[classDayKey]map[string]classDay{}}
}

func parseYMD(s string) (time.Time, bool) {
	if len(s) < 10 {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02", s[:10])
	return t, err == nil
}

// putWindow records every day in [start,end] as observed at `at`, including
// days with no periods. Without the empty-day markers a range containing a
// weekend or a holiday could never be considered fully covered.
func (c *classDayCache) putWindow(school string, classID int64, start, end string, periods []map[string]any, at time.Time) {
	s, ok1 := parseYMD(start)
	e, ok2 := parseYMD(end)
	if !ok1 || !ok2 || e.Before(s) {
		return
	}
	byDay := map[string][]cachedPeriod{}
	for _, pd := range periods {
		raw, err := json.Marshal(pd)
		if err != nil {
			continue
		}
		startDT, _ := pd["startDateTime"].(string)
		day := startDT
		if len(day) > 10 {
			day = day[:10]
		}
		if len(day) != 10 {
			// No usable start time: it cannot be placed on a day, and guessing
			// would put a lesson in the wrong slot.
			continue
		}
		id, _ := jsonID(pd["id"])
		byDay[day] = append(byDay[day], cachedPeriod{raw: raw, start: startDT, id: id})
	}

	c.mu.Lock()
	key := classDayKey{school: school, classID: classID}
	days := c.days[key]
	if days == nil {
		days = map[string]classDay{}
		c.days[key] = days
	}
	for d := s; !d.After(e); d = d.AddDate(0, 0, 1) {
		ds := d.Format("2006-01-02")
		days[ds] = classDay{periods: byDay[ds], at: at}
	}
	if len(days) > maxClassDayEntries {
		dates := make([]string, 0, len(days))
		for d := range days {
			dates = append(dates, d)
		}
		sort.Strings(dates)
		for _, d := range dates[:len(dates)-maxClassDayEntries] {
			delete(days, d)
		}
	}
	c.mu.Unlock()
}

// lookup returns the periods for a fully covered, fresh range. A partial or
// stale range is a miss: serving half a timetable would be worse than paying
// for the upstream request.
func (c *classDayCache) lookup(school string, classID int64, start, end string, oldest time.Time) ([]map[string]any, bool) {
	s, ok1 := parseYMD(start)
	e, ok2 := parseYMD(end)
	if !ok1 || !ok2 || e.Before(s) {
		return nil, false
	}
	var collected []cachedPeriod
	c.mu.RLock()
	days := c.days[classDayKey{school: school, classID: classID}]
	if days == nil {
		c.mu.RUnlock()
		return nil, false
	}
	for d := s; !d.After(e); d = d.AddDate(0, 0, 1) {
		day, ok := days[d.Format("2006-01-02")]
		if !ok || day.at.Before(oldest) {
			c.mu.RUnlock()
			return nil, false
		}
		collected = append(collected, day.periods...)
	}
	c.mu.RUnlock()

	seen := map[string]bool{}
	out := make([]map[string]any, 0, len(collected))
	for _, cp := range collected {
		key := cp.start
		if cp.id != 0 {
			key = fmt.Sprintf("%d|%s", cp.id, cp.start)
		} else {
			key = string(cp.raw)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		var pd map[string]any
		if json.Unmarshal(cp.raw, &pd) == nil {
			out = append(out, pd)
		}
	}
	// Upstream returns periods chronologically. Restore that order because a
	// client may render the array as-is.
	sort.SliceStable(out, func(i, j int) bool {
		si, _ := out[i]["startDateTime"].(string)
		sj, _ := out[j]["startDateTime"].(string)
		if si != sj {
			return si < sj
		}
		ii, _ := jsonID(out[i]["id"])
		ij, _ := jsonID(out[j]["id"])
		return ii < ij
	})
	return out, true
}

func (c *classDayCache) invalidate(school string, classID int64) {
	c.mu.Lock()
	delete(c.days, classDayKey{school: school, classID: classID})
	c.mu.Unlock()
}

// dayWindowSpan reports the inclusive day count of [start,end] and whether the
// range is short enough for the day cache. A malformed or inverted range is
// rejected rather than guessed at.
func dayWindowSpan(start, end string) (int, bool) {
	s, ok1 := parseYMD(start)
	e, ok2 := parseYMD(end)
	if !ok1 || !ok2 || e.Before(s) {
		return 0, false
	}
	days := int(e.Sub(s).Hours()/24) + 1
	if days < 1 || days > maxClassDaySpanDays {
		return 0, false
	}
	return days, true
}

// cachedClassPeriods serves a fully covered, fresh range from the day cache.
func (p *Proxy) cachedClassPeriods(school string, classID int64, start, end string) ([]map[string]any, bool) {
	if _, ok := dayWindowSpan(start, end); !ok {
		return nil, false
	}
	return p.classDays.lookup(school, classID, start, end, time.Now().Add(-p.opts.TTL))
}

// rememberClassPeriods records a fetched window so the next request for it is
// answered locally. It is called at the classPeriods level, never inside the
// chunk fetcher: a recon sweep fetches a school year in chunks and must not
// fill the day cache with data no app request will read.
func (p *Proxy) rememberClassPeriods(school string, classID int64, start, end string, periods []map[string]any) {
	if _, ok := dayWindowSpan(start, end); !ok {
		return
	}
	p.classDays.putWindow(school, classID, start, end, periods, time.Now())
}

// classTimetableCacheKey is intentionally school-scoped. The disabled cache
// this replaces was keyed only by class and dates, which would have served one
// school's timetable to another in a multi-school deployment.
func classTimetableCacheKey(school string, classID int64, start, end string) string {
	return fmt.Sprintf("2017|%s|%d|%s|%s", school, classID, start, end)
}

// studentTimetableCacheKey mirrors classTimetableCacheKey for a personal
// timetable. The id is the person, not the class: two students in the same
// class have different personal schedules. The "student" segment keeps the key
// space disjoint from class keys even when ids happen to overlap.
func studentTimetableCacheKey(school string, personID int64, start, end string) string {
	return fmt.Sprintf("2017|student|%s|%d|%s|%s", school, personID, start, end)
}

// studentBodyFromDays builds the JSON-RPC envelope around the personal day
// cache. Like classBodyFromDays it omits master data, which is substituted per
// viewer on the way out.
func (p *Proxy) studentBodyFromDays(school string, personID int64, start, end string) ([]byte, bool) {
	if !p.hasRawMasterData(school) {
		return nil, false
	}
	oldest := time.Now().Add(-p.opts.TTL)
	periods, ok := p.studentDays.lookup(school, personID, start, end, oldest)
	if !ok {
		return nil, false
	}
	out := map[string]any{
		"jsonrpc": "2.0",
		"id":      nil,
		"result": map[string]any{
			"timetable": map[string]any{
				"displayableStartDate": start,
				"displayableEndDate":   end,
				"periods":              periods,
			},
		},
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, false
	}
	return b, true
}

// cachedStudentBody checks the exact response cache first, then the personal
// day cache. The same master-data gate as the class path applies: a cached body
// may carry the fetching account's element flags, so it is only safe to serve
// once the requester's own copy can be substituted.
func (p *Proxy) cachedStudentBody(school string, personID int64, start, end string) ([]byte, bool) {
	if !p.hasRawMasterData(school) {
		return nil, false
	}
	if b, ok := p.tt.Get(studentTimetableCacheKey(school, personID, start, end)); ok {
		return b, true
	}
	return p.studentBodyFromDays(school, personID, start, end)
}

// rememberStudentPeriods records a fetched personal window so a later request
// for a covered range is answered locally. Only app-sized ranges are kept; a
// long calendar export must not copy months of lessons into memory.
func (p *Proxy) rememberStudentPeriods(school string, personID int64, start, end string, periods []map[string]any) {
	if _, ok := dayWindowSpan(start, end); !ok {
		return
	}
	p.studentDays.putWindow(school, personID, start, end, periods, time.Now())
}

// periodsFromBody decodes result.timetable.periods from a raw JSON-RPC
// response, returning nil when the body is not a timetable answer.
func periodsFromBody(raw []byte) []map[string]any {
	var resp struct {
		Result struct {
			Timetable struct {
				Periods []map[string]any `json:"periods"`
			} `json:"timetable"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &resp) != nil {
		return nil
	}
	return resp.Result.Timetable.Periods
}

func (p *Proxy) hasRawMasterData(school string) bool {
	p.mdRawMu.Lock()
	defer p.mdRawMu.Unlock()
	return len(p.mdRaw[school]) > 0
}

// classBodyFromDays builds the JSON-RPC envelope around the day cache. It
// deliberately omits master data: writeClassTimetable inserts the requester's
// own copy, so one viewer's element flags can never be cached for another.
func (p *Proxy) classBodyFromDays(school string, classID int64, start, end string) ([]byte, bool) {
	if !p.hasRawMasterData(school) {
		return nil, false
	}
	oldest := time.Now().Add(-p.opts.TTL)
	periods, ok := p.classDays.lookup(school, classID, start, end, oldest)
	if !ok {
		return nil, false
	}
	out := map[string]any{
		"jsonrpc": "2.0",
		"id":      nil,
		"result": map[string]any{
			"timetable": map[string]any{
				"displayableStartDate": start,
				"displayableEndDate":   end,
				"periods":              periods,
			},
		},
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, false
	}
	return b, true
}

// cachedClassBody checks the exact response cache first, then the day cache.
// The exact body preserves upstream fields this proxy does not model.
func (p *Proxy) cachedClassBody(school string, classID int64, start, end string) ([]byte, bool) {
	// A cached body may carry an upstream masterData block produced for whoever
	// performed the fetch. Serving it without being able to substitute the
	// requester's own copy would leak that viewer's element flags, so the cache
	// is only usable once the persisted raw body makes the substitution possible.
	if !p.hasRawMasterData(school) {
		return nil, false
	}
	if b, ok := p.tt.Get(classTimetableCacheKey(school, classID, start, end)); ok {
		return b, true
	}
	return p.classBodyFromDays(school, classID, start, end)
}

// fetchClassBody collapses concurrent cold requests for the same class/range
// into one upstream call. flightKey may include the requester for paths whose
// upstream authentication is the requester's own, so a malformed auth block
// cannot deny the fetch to a classmate.
func (p *Proxy) fetchClassBody(cacheKey, flightKey string, fetch func() ([]byte, int, error)) ([]byte, int, error) {
	if b, ok := p.tt.Get(cacheKey); ok {
		return b, http.StatusOK, nil
	}

	p.flightMu.Lock()
	if p.flights == nil {
		p.flights = map[string]*classFetchCall{}
	}
	if call, ok := p.flights[flightKey]; ok {
		p.flightMu.Unlock()
		<-call.done
		return call.body, call.status, call.err
	}
	call := &classFetchCall{done: make(chan struct{})}
	p.flights[flightKey] = call
	p.flightMu.Unlock()

	b, status, err := fetch()
	// A school server can answer an HTTP 200 with a JSON-RPC error. Caching that
	// for the TTL would keep replaying a transient refusal long after it cleared.
	if err == nil && status == http.StatusOK && len(b) > 0 && jsonrpcError(b) == "" {
		p.tt.Put(cacheKey, b)
	}
	call.body, call.status, call.err = b, status, err
	p.flightMu.Lock()
	delete(p.flights, flightKey)
	p.flightMu.Unlock()
	close(call.done)
	return b, status, err
}

// patchJSONRPCID makes a cached body answer the request that is reading it. A
// cached response was produced for an earlier request and may carry that
// request's id; JSON-RPC clients are entitled to match on it.
func patchJSONRPCID(b []byte, id any) []byte {
	var doc map[string]any
	if json.Unmarshal(b, &doc) != nil {
		return b
	}
	doc["id"] = id
	out, err := json.Marshal(doc)
	if err != nil {
		return b
	}
	return out
}

// writeClassTimetable is the single exit for class timetable responses. Cache
// entries are viewer-neutral, so every per-viewer field is applied here:
// master data flags, capabilities, personal events and the synthetic Technik
// class.
func (p *Proxy) writeClassTimetable(w http.ResponseWriter, school, viewer string, id json.RawMessage, raw []byte, pr timetableParams, ownClass, fromCache bool, status int) {
	b := raw
	if !fromCache && !p.hasRawMasterData(school) {
		// The upstream answer carries a masterData block for the account that
		// fetched it. Keep it as the school's raw copy so this and later cache
		// hits can substitute each requester's own view instead of falling back
		// to an upstream fetch forever.
		p.cacheRawMasterData(school, raw)
	}
	if fromCache {
		// A cached body always needs the requester's own master data and id.
		b = p.withRequesterMasterData(school, viewer, b)
		b = patchJSONRPCID(b, idVal(id))
	} else if !ownClass {
		// The pool path already did this before caching; keep the uncached
		// path byte-compatible with upstream for clients/tests that compare.
		b = p.withRequesterMasterData(school, viewer, b)
	}
	b = p.viewerBody(viewer, b)
	if ownClass {
		b = p.decorateStudentEvents(b, school, viewer, pr.StartDate, pr.EndDate)
		b = p.injectTechnikClass(viewer, b)
	}
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
