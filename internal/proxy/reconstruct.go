package proxy

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"untis-proxy/internal/store"
)

// elementDB tracks which teachers/rooms/subjects actually appear in the pooled
// classes' timetables. Only those are made displayable in client apps and are
// served reconstructed timetables for.
type elementDB struct {
	mu        sync.RWMutex
	teachers  map[int64]bool
	rooms     map[int64]bool
	subjects  map[int64]bool
	scanUntil map[int64]string
}

func newElementDB() *elementDB {
	return &elementDB{
		teachers:  map[int64]bool{},
		rooms:     map[int64]bool{},
		subjects:  map[int64]bool{},
		scanUntil: map[int64]string{},
	}
}

// snapshot returns the current element sets as type -> [ids], suitable for
// persisting on shutdown.
func (e *elementDB) snapshot() map[string][]int64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := map[string][]int64{}
	for id := range e.teachers {
		out["TEACHER"] = append(out["TEACHER"], id)
	}
	for id := range e.rooms {
		out["ROOM"] = append(out["ROOM"], id)
	}
	for id := range e.subjects {
		out["SUBJECT"] = append(out["SUBJECT"], id)
	}
	return out
}

// counts returns the number of known elements per type.
func (e *elementDB) counts() map[string]int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return map[string]int{
		"TEACHER": len(e.teachers),
		"ROOM":    len(e.rooms),
		"SUBJECT": len(e.subjects),
	}
}

// seedFrom merges in a previously-persisted set of known elements. It is a warm
// start only: the background scan re-runs on boot and revalidates (and removes
// nothing) once it fetches fresh timetables.
func (e *elementDB) seedFrom(elems map[string][]int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for t, ids := range elems {
		for _, id := range ids {
			switch t {
			case "TEACHER":
				e.teachers[id] = true
			case "ROOM":
				e.rooms[id] = true
			case "SUBJECT":
				e.subjects[id] = true
			}
		}
	}
}

// mergeFrom adds every element of other to e without removing anything. Used by
// a partial recon sweep, where the skipped classes' elements must survive.
func (e *elementDB) mergeFrom(other *elementDB) {
	other.mu.RLock()
	defer other.mu.RUnlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	for id := range other.teachers {
		e.teachers[id] = true
	}
	for id := range other.rooms {
		e.rooms[id] = true
	}
	for id := range other.subjects {
		e.subjects[id] = true
	}
}

// setScanHorizon records how far one class has been scanned.
func (e *elementDB) setScanHorizon(classID int64, until string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.scanUntil[classID] = until
}

// setScanUntil replaces the per-class scan horizons with a stored set.
func (e *elementDB) setScanUntil(horizons map[int64]string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, until := range horizons {
		e.scanUntil[id] = until
	}
}

// scanProgress returns a copy of the per-class scan horizons.
func (e *elementDB) scanProgress() map[int64]string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make(map[int64]string, len(e.scanUntil))
	for id, until := range e.scanUntil {
		out[id] = until
	}
	return out
}

func (e *elementDB) has(elType string, id int64) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	switch elType {
	case "TEACHER":
		return e.teachers[id]
	case "ROOM":
		return e.rooms[id]
	case "SUBJECT":
		return e.subjects[id]
	}
	return false
}

// jsonrpcError returns the message of a JSON-RPC error object, or "" when the
// body is not an error answer.
func jsonrpcError(body []byte) string {
	var resp struct {
		Error struct {
			Code    any    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return ""
	}
	if resp.Error.Message == "" {
		return ""
	}
	return fmt.Sprintf("code %v: %s", resp.Error.Code, resp.Error.Message)
}

// upstreamError turns a non-2xx school-server response into an error. The
// untis client returns the status separately and no transport error for a 4xx or
// 5xx, so without this a school-server outage parses as "this class has zero
// periods" — which would mark every cached period REMOVED and fire a change
// storm at every subscriber.
func upstreamError(what string, status int, body []byte) error {
	if status >= 200 && status < 300 {
		return nil
	}
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 160 {
		snippet = snippet[:160] + "..."
	}
	if snippet == "" {
		snippet = "<empty body>"
	}
	return fmt.Errorf("%s: upstream returned %d: %s", what, status, snippet)
}

// fetchClassChunk fetches one class timetable over a single (<=2 week) chunk,
// using the class owner's account. Results are cached by class+range.
func (p *Proxy) fetchClassChunk(school string, classID int64, start, end string) ([]map[string]any, error) {
	return p.fetchClassChunkMode(school, classID, start, end, false)
}

// fetchClassChunkFresh is fetchClassChunk but forces an upstream fetch,
// bypassing the in-memory TTL cache. Used by the change-detection poller so it
// always sees current data.
func (p *Proxy) fetchClassChunkFresh(school string, classID int64, start, end string) ([]map[string]any, error) {
	return p.fetchClassChunkMode(school, classID, start, end, true)
}

func (p *Proxy) fetchClassChunkMode(school string, classID int64, start, end string, fresh bool) ([]map[string]any, error) {
	key := fmt.Sprintf("recon|%s|%d|%s|%s", school, classID, start, end)
	if !fresh {
		if v, ok := p.tt.Get(key); ok {
			var out []map[string]any
			if err := json.Unmarshal(v, &out); err == nil {
				return out, nil
			}
		}
	}
	owner, err := p.store.OwnerForClass(school, classID)
	if err != nil || owner == nil {
		return nil, fmt.Errorf("no owner for class %d", classID)
	}
	body, _ := json.Marshal(map[string]any{
		"id": "untis-proxy-recon", "jsonrpc": "2.0", "method": "getTimetable2017",
		"params": []any{map[string]any{
			"id": classID, "type": "CLASS",
			"startDate": start, "endDate": end,
			"masterDataTimestamp": 0, "timetableTimestamp": 0, "timetableTimestamps": []any{},
		}},
	})
	newBody, err := p.rewriteAuthForOwner(school, body, owner)
	if err != nil {
		return nil, err
	}
	b, status, _, err := p.untis.RawIntern(school, "", "getTimetable2017", newBody)
	if err != nil {
		return nil, err
	}
	if err := upstreamError(fmt.Sprintf("class %d timetable", classID), status, b); err != nil {
		return nil, err
	}
	var resp struct {
		Result struct {
			Timetable struct {
				Periods []map[string]any `json:"periods"`
			} `json:"timetable"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, err
	}
	if resp.Result.Timetable.Periods == nil && jsonrpcError(b) != "" {
		return nil, fmt.Errorf("class %d timetable: upstream jsonrpc error: %s", classID, jsonrpcError(b))
	}
	raw, _ := json.Marshal(resp.Result.Timetable.Periods)
	p.tt.Put(key, raw)
	return resp.Result.Timetable.Periods, nil
}

func chunkDates(start, end string) ([][2]string, error) {
	s, err := time.Parse("2006-01-02", start)
	if err != nil {
		return nil, err
	}
	e, err := time.Parse("2006-01-02", end)
	if err != nil {
		return nil, err
	}
	if e.Before(s) {
		return nil, fmt.Errorf("end before start")
	}
	var out [][2]string
	for {
		chunkEnd := s.AddDate(0, 0, 13)
		if chunkEnd.After(e) {
			chunkEnd = e
		}
		out = append(out, [2]string{s.Format("2006-01-02"), chunkEnd.Format("2006-01-02")})
		if chunkEnd.Equal(e) {
			break
		}
		s = chunkEnd.AddDate(0, 0, 1)
	}
	return out, nil
}

// classPeriods returns all periods for a class over a (possibly long) range,
// fetched in <=2 week chunks and deduplicated by period id.
//
// Periods are deduplicated by their unique period id, NOT by lesson id: a
// double (or longer) lesson is represented by the school server as several
// distinct period entries that share one lessonId. Deduping on lessonId would
// collapse those consecutive time slots into a single one.
func (p *Proxy) classPeriods(school string, classID int64, start, end string) ([]map[string]any, error) {
	chunks, err := chunkDates(start, end)
	if err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	var out []map[string]any
	for _, c := range chunks {
		ps, err := p.fetchClassChunk(school, classID, c[0], c[1])
		if err != nil {
			return nil, err
		}
		for _, pd := range ps {
			pid, _ := pd["id"].(float64)
			if seen[int64(pid)] {
				continue
			}
			seen[int64(pid)] = true
			out = append(out, pd)
		}
	}
	return out, nil
}

// classPeriodsFresh is classPeriods but forces fresh upstream data (no cache).
func (p *Proxy) classPeriodsFresh(school string, classID int64, start, end string) ([]map[string]any, error) {
	chunks, err := chunkDates(start, end)
	if err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	var out []map[string]any
	for _, c := range chunks {
		ps, err := p.fetchClassChunkFresh(school, classID, c[0], c[1])
		if err != nil {
			return nil, err
		}
		for _, pd := range ps {
			pid, _ := pd["id"].(float64)
			if seen[int64(pid)] {
				continue
			}
			seen[int64(pid)] = true
			out = append(out, pd)
		}
	}
	return out, nil
}

// studentPeriods returns a student's personal timetable over a range by querying
// WebUntis with element type STUDENT for the given person id, using the
// student's own account. This reflects the student's individual subject
// enrollment (e.g. Wahlpflicht group splits), so it can differ from the full
// class timetable. Results are cached in the shared TTL cache.
func (p *Proxy) studentPeriods(school string, personID int64, start, end string) ([]map[string]any, error) {
	u, err := p.store.UserByPersonID(personID)
	if err != nil || u == nil {
		return nil, fmt.Errorf("no user for person %d", personID)
	}
	chunks, err := chunkDates(start, end)
	if err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	var out []map[string]any
	for _, c := range chunks {
		ps, err := p.fetchStudentChunk(school, u, personID, c[0], c[1])
		if err != nil {
			return nil, err
		}
		for _, pd := range ps {
			pid, _ := pd["id"].(float64)
			if seen[int64(pid)] {
				continue
			}
			seen[int64(pid)] = true
			out = append(out, pd)
		}
	}
	return out, nil
}

// fetchStudentChunk fetches one chunk of a student's personal timetable.
func (p *Proxy) fetchStudentChunk(school string, u *store.User, personID int64, start, end string) ([]map[string]any, error) {
	key := fmt.Sprintf("student|%s|%d|%s|%s", school, personID, start, end)
	if v, ok := p.tt.Get(key); ok {
		var out []map[string]any
		if err := json.Unmarshal(v, &out); err == nil {
			return out, nil
		}
	}
	body, _ := json.Marshal(map[string]any{
		"id": "untis-proxy-student", "jsonrpc": "2.0", "method": "getTimetable2017",
		"params": []any{map[string]any{
			"id": personID, "type": "STUDENT",
			"startDate": start, "endDate": end,
			"masterDataTimestamp": 0, "timetableTimestamp": 0, "timetableTimestamps": []any{},
		}},
	})
	newBody, err := p.rewriteAuthForOwner(school, body, u)
	if err != nil {
		return nil, err
	}
	b, _, err := p.escalatedIntern(school, u, "getTimetable2017", newBody)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result struct {
			Timetable struct {
				Periods []map[string]any `json:"periods"`
			} `json:"timetable"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(resp.Result.Timetable.Periods)
	p.tt.Put(key, raw)
	return resp.Result.Timetable.Periods, nil
}

func periodElementIDs(pd map[string]any) []struct {
	Type string
	ID   int64
} {
	var out []struct {
		Type string
		ID   int64
	}
	els, _ := pd["elements"].([]any)
	for _, el := range els {
		m, ok := el.(map[string]any)
		if !ok {
			continue
		}
		t, _ := m["type"].(string)
		idf, _ := m["id"].(float64)
		if t == "" {
			continue
		}
		out = append(out, struct {
			Type string
			ID   int64
		}{t, int64(idf)})
	}
	return out
}

// elementPeriods reconstructs the timetable for one teacher/room/subject by
// merging the pooled classes' timetables over the range. Periods are deduped by
// (start, end, subject) so a teacher teaching two classes at once appears once.
func (p *Proxy) elementPeriods(school, elType string, elID int64, start, end string) ([]map[string]any, error) {
	classes, err := p.store.Pool(school)
	if err != nil {
		return nil, err
	}
	type key struct {
		start, end, subject string
	}
	seen := map[key]bool{}
	var out []map[string]any
	for _, c := range classes {
		ps, err := p.classPeriods(school, c.ID, start, end)
		if err != nil {
			continue
		}
		for _, pd := range ps {
			matched := false
			sub := ""
			for _, el := range periodElementIDs(pd) {
				if el.Type == elType && el.ID == elID {
					matched = true
				}
				if el.Type == "SUBJECT" {
					sub = fmt.Sprintf("%d", el.ID)
				}
			}
			if !matched {
				continue
			}
			st, _ := pd["startDateTime"].(string)
			en, _ := pd["endDateTime"].(string)
			k := key{st, en, sub}
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, pd)
		}
	}
	return out, nil
}

// fetchElementRaw fetches a teacher/room/subject timetable directly from the
// upstream WebUntis server via a boosted source account.  This returns the raw
// upstream periods (no reconstruction, no caching) — the same data a boosted
// user sees in the app.
func (p *Proxy) fetchElementRaw(school, elType string, elID int64, start, end string) ([]map[string]any, error) {
	owner := p.boostedSource(school)
	if owner == nil {
		return nil, fmt.Errorf("no boosted source account available")
	}
	// Build a getTimetable2017 body for the element
	var elTypeUpstream string
	switch elType {
	case "TEACHER":
		elTypeUpstream = "TEACHER"
	case "ROOM":
		elTypeUpstream = "ROOM"
	case "SUBJECT":
		elTypeUpstream = "SUBJECT"
	default:
		return nil, fmt.Errorf("unsupported element type %q", elType)
	}
	body, _ := json.Marshal(map[string]any{
		"id": "untis-proxy-cal", "jsonrpc": "2.0", "method": "getTimetable2017",
		"params": []any{map[string]any{
			"id": elID, "type": elTypeUpstream,
			"startDate": start, "endDate": end,
			"masterDataTimestamp": 0, "timetableTimestamp": 0, "timetableTimestamps": []any{},
		}},
	})
	newBody, err := p.rewriteAuthForOwner(school, body, owner)
	if err != nil {
		return nil, err
	}
	b, status, _, err := p.untis.RawIntern(school, "", "getTimetable2017", newBody)
	if err != nil {
		return nil, err
	}
	if err := upstreamError(fmt.Sprintf("%s %d timetable", elTypeUpstream, elID), status, b); err != nil {
		return nil, err
	}
	var resp struct {
		Result struct {
			Timetable struct {
				Periods []map[string]any `json:"periods"`
			} `json:"timetable"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, err
	}
	return resp.Result.Timetable.Periods, nil
}

// reconHorizonDays is how far ahead of today the recon enumeration reaches.
const reconHorizonDays = 21

// defaultReconRefresh is the default staleness threshold: a class whose stored
// scan horizon is still within this many days of the target is left alone.
const defaultReconRefresh = 21

// reconHorizon returns the date the enumeration aims to cover.
func reconHorizon() string {
	return time.Now().AddDate(0, 0, reconHorizonDays).Format("2006-01-02")
}

// reconRefreshDays is the configured staleness threshold for resuming a scan.
func (p *Proxy) reconRefreshDays() int {
	if p.opts.ReconRefreshDays > 0 {
		return p.opts.ReconRefreshDays
	}
	return defaultReconRefresh
}

// reconStale reports whether a class scanned up to `since` must be re-scanned to
// reach `target`: true when it was never scanned, or when its horizon has fallen
// more than the refresh window behind the target.
func reconStale(since, target string, refreshDays int) bool {
	if since == "" {
		return true
	}
	s, err := time.Parse("2006-01-02", since)
	if err != nil {
		return true
	}
	t, err := time.Parse("2006-01-02", target)
	if err != nil {
		return true
	}
	return s.AddDate(0, 0, refreshDays).Before(t)
}

// scanClass enumerates teachers/rooms/subjects for one class from `since` up to
// the bounded horizon (today + reconHorizonDays), fetching week-by-week (the
// real server caps ranges at ~2 weeks). Resuming from the stored scan horizon is
// what keeps a restart from re-fetching the whole year from the school server.
// Observed elements accumulate into dst, which the caller swaps into the serving
// registry once every pooled class has been scanned, so the registry never
// retains elements that are not currently reconstructible from pooled classes.
func (p *Proxy) scanClass(school string, classID int64, since, yearEnd string, dst *elementDB) {
	horizon := reconHorizon()
	if since == "" || since > horizon {
		since = horizon
	}
	chunks, err := chunkDates(since, horizon)
	if err != nil || len(chunks) == 0 {
		chunks, _ = chunkDates(since, yearEnd)
	}
	if len(chunks) == 0 {
		return
	}
	for _, c := range chunks {
		ps, err := p.fetchClassChunk(school, classID, c[0], c[1])
		if err != nil {
			log.Printf("[recon] class %d %s..%s: %v", classID, c[0], c[1], err)
			continue
		}
		for _, pd := range ps {
			for _, el := range periodElementIDs(pd) {
				switch el.Type {
				case "TEACHER":
					dst.mu.Lock()
					dst.teachers[el.ID] = true
					dst.mu.Unlock()
				case "ROOM":
					dst.mu.Lock()
					dst.rooms[el.ID] = true
					dst.mu.Unlock()
				case "SUBJECT":
					dst.mu.Lock()
					dst.subjects[el.ID] = true
					dst.mu.Unlock()
				}
			}
		}
	}
	p.stateFor(school).recon.setScanHorizon(classID, horizon)
	_ = p.store.SaveReconScanAt(school, classID, horizon)
}

// StartRecon kicks off background enumeration of known teachers/rooms/subjects
// across all pooled classes. Classes whose stored scan horizon is still within
// the refresh window are skipped and keep contributing the elements found
// earlier, so a restart costs no school-server traffic at all. A class that has
// fallen stale is re-scanned from its stored horizon, not from the year start.
//
// The registry is replaced — dropping elements that no longer appear in any
// pooled class — only when every pooled class was re-scanned in this sweep, i.e.
// when the result is complete. A partial sweep merges into the live registry,
// because recon_elements records no per-class attribution, so a partial sweep
// cannot tell which of the skipped classes still vouches for an element.
func (p *Proxy) StartRecon(school, yearStart, yearEnd string) {
	p.stateFor(school)
	classes, err := p.store.Pool(school)
	if err != nil {
		log.Printf("[recon] no pool: %v", err)
		return
	}
	if len(classes) == 0 {
		return
	}
	force := p.opts.ForceRescan || p.rescanReq.Swap(false)
	target := reconHorizon()
	refresh := p.reconRefreshDays()
	stored, err := p.store.ReconScan(school)
	if err != nil {
		log.Printf("[recon] read scan progress: %v", err)
		stored = map[int64]string{}
	}
	if force {
		// Drop the recorded progress so this and later sweeps cannot resume.
		if err := p.store.ClearReconScan(school); err != nil {
			log.Printf("[recon] clear scan progress: %v", err)
		}
		stored = map[int64]string{}
		log.Printf("[recon] forced full rescan of %d classes", len(classes))
	}
	// Surface the stored horizons in the admin status while the sweep runs.
	p.stateFor(school).recon.setScanUntil(stored)

	type scanJob struct {
		classID int64
		since   string
	}
	jobs := make([]scanJob, 0, len(classes))
	skipped := 0
	for _, c := range classes {
		since := stored[c.ID]
		if !force && !reconStale(since, target, refresh) {
			skipped++
			continue
		}
		if since < yearStart {
			since = yearStart
		}
		jobs = append(jobs, scanJob{classID: c.ID, since: since})
	}
	if len(jobs) == 0 {
		log.Printf("[recon] %d/%d classes already scanned through %s, nothing to do",
			skipped, len(classes), target)
		return
	}
	go func() {
		// Rebuild the registry from what is reachable in the pooled classes on
		// this boot. The persisted snapshot is only a warm start: replacing
		// (not merging) after a full sweep drops elements that no longer appear
		// in any pooled class, keeping the selectable set in sync with what
		// reconstruction can actually serve.
		fresh := newElementDB()
		if skipped > 0 {
			// Partial sweep: start from the persisted set so the classes we skip
			// keep contributing their elements.
			if elems, err := p.store.LoadReconElements(school); err == nil {
				fresh.seedFrom(elems)
			}
		}
		// stagger to avoid a burst of simultaneous requests
		for i, j := range jobs {
			time.Sleep(time.Duration(i) * 400 * time.Millisecond)
			p.scanClass(school, j.classID, j.since, yearEnd, fresh)
		}
		e := p.stateFor(school).recon
		if skipped == 0 {
			// Full sweep: the result is complete, so replace and prune.
			e.mu.Lock()
			e.teachers = fresh.teachers
			e.rooms = fresh.rooms
			e.subjects = fresh.subjects
			e.mu.Unlock()
		} else {
			e.mergeFrom(fresh)
		}
		log.Printf("[recon] enumeration done: %d/%d classes re-scanned, %d teachers, %d rooms, %d subjects",
			len(jobs), len(classes), e.counts()["TEACHER"], e.counts()["ROOM"], e.counts()["SUBJECT"])
		p.PersistRecon(school)
	}()
}

// LoadRecon restores the persisted element set so reconstruction requests are
// answered immediately on boot, before the background scan finishes. The scan
// then re-runs and refreshes the fresh state. It is only ever a warm start and
// not served as a replacement for fresh data.
func (p *Proxy) LoadRecon(school string) {
	elems, err := p.store.LoadReconElements(school)
	if err != nil || len(elems) == 0 {
		return
	}
	p.stateFor(school).recon.seedFrom(elems)
	log.Printf("[recon] restored snapshot (%s): %d teachers, %d rooms, %d subjects",
		school, len(elems["TEACHER"]), len(elems["ROOM"]), len(elems["SUBJECT"]))
}

// PersistRecon writes the current recon element set (and per-class scan
// progress) to persistent storage. Call on graceful shutdown.
func (p *Proxy) PersistRecon(school string) {
	if err := p.store.SaveReconElements(school, p.stateFor(school).recon.snapshot()); err != nil {
		log.Printf("[recon] persist snapshot: %v", err)
		return
	}
	log.Printf("[recon] saved snapshot")
}

// weekRange returns the Monday..Friday range containing the given date.
func weekRange(date string) (string, string, error) {
	d, err := time.Parse("2006-01-02", date)
	if err != nil {
		return "", "", err
	}
	wd := int(d.Weekday())
	if wd == 0 {
		wd = 7
	}
	monday := d.AddDate(0, 0, -(wd - 1))
	return monday.Format("2006-01-02"), monday.AddDate(0, 0, 4).Format("2006-01-02"), nil
}

// weeklyPeriod converts a JSON-RPC timetable period into the weekly-data REST
// format, returning the converted period and the element IDs it references.
func weeklyPeriod(pd map[string]any) (map[string]any, []int64, error) {
	st, _ := pd["startDateTime"].(string)
	en, _ := pd["endDateTime"].(string)
	if st == "" || en == "" {
		return nil, nil, fmt.Errorf("no times")
	}
	stT, err := time.Parse("2006-01-02T15:04Z07:00", st)
	if err != nil {
		stT, err = time.Parse("2006-01-02T15:04:05Z07:00", st)
		if err != nil {
			return nil, nil, err
		}
	}
	enT, err := time.Parse("2006-01-02T15:04Z07:00", en)
	if err != nil {
		enT, err = time.Parse("2006-01-02T15:04:05Z07:00", en)
		if err != nil {
			return nil, nil, err
		}
	}

	elTypes := map[string]int{"CLASS": 1, "TEACHER": 2, "SUBJECT": 3, "ROOM": 4}
	els := make([]map[string]any, 0)
	var ids []int64
	for _, el := range periodElementIDs(pd) {
		t, ok := elTypes[el.Type]
		if !ok {
			continue
		}
		els = append(els, map[string]any{
			"type": t, "id": el.ID, "orgId": el.ID,
			"missing": false, "state": "REGULAR",
		})
		ids = append(ids, el.ID)
	}
	if len(els) == 0 {
		return nil, nil, fmt.Errorf("no elements")
	}

	lessonID, _ := pd["lessonId"].(float64)
	periodID, _ := pd["id"].(float64)
	isRegular := true
	if is, ok := pd["is"].([]any); ok && len(is) > 0 {
		if s, ok := is[0].(string); ok {
			isRegular = s == "REGULAR"
		}
	}

	return map[string]any{
		"id":                int64(periodID),
		"lessonId":          int64(lessonID),
		"lessonNumber":      0,
		"lessonCode":        "LESSON",
		"lessonText":        textField(pd, "lesson"),
		"periodText":        textField(pd, "period"),
		"hasPeriodText":     textField(pd, "period") != "",
		"periodInfo":        textField(pd, "info"),
		"periodAttachments": []any{},
		"substText":         textField(pd, "substitution"),
		"date":              ymdInt(stT),
		"startTime":         hmInt(stT),
		"endTime":           hmInt(enT),
		"elements":          els,
		"hasInfo":           textField(pd, "info") != "",
		"code":              0,
		"cellState":         "STANDARD",
		"priority":          5,
		"is":                map[string]any{"standard": isRegular, "event": false},
		"roomCapacity":      0,
		"studentCount":      0,
		"debugInfo":         fmt.Sprintf("%d,0,0/%d,0", int64(lessonID), int64(periodID)),
	}, ids, nil
}

func textField(pd map[string]any, field string) string {
	if m, ok := pd["text"].(map[string]any); ok {
		if v, ok := m[field].(string); ok {
			return v
		}
	}
	return ""
}

func ymdInt(t time.Time) int {
	return t.Year()*10000 + int(t.Month())*100 + t.Day()
}

func hmInt(t time.Time) int {
	return t.Hour()*100 + t.Minute()
}
