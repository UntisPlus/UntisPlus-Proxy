package proxy

import (
	"context"
	"fmt"
	"sync"
	"time"

	"untis-proxy/internal/store"
	"untis-proxy/internal/untis"
)

// This file holds the operator-facing recon sweep used by `untisctl recon`.
// It reuses the server's fetch and element-extraction code so a manual sweep
// sees exactly the same upstream data, but it runs in the caller's process and
// can therefore pace itself differently from the background scan.

// ReconMode selects the default concurrency/pacing of a manual sweep.
type ReconMode string

const (
	// ReconHard fetches several classes at once with no added delay. It is the
	// fastest way to fill a stale registry, at the cost of upstream load.
	ReconHard ReconMode = "hard"
	// ReconSlow fetches one class at a time and pauses between requests, so a
	// sweep can run against a busy school server without competing with real
	// timetable loads.
	ReconSlow ReconMode = "slow"
)

// ReconOptions configures one manual recon sweep.
type ReconOptions struct {
	School    string
	YearStart string
	YearEnd   string
	Mode      ReconMode
	// Full ignores the persisted per-class scan horizons and starts at
	// YearStart instead of resuming from the last observed date.
	Full bool
	// DryRun plans the sweep and reports its size without fetching or writing.
	DryRun bool
	// Concurrency and Delay override the mode defaults when > 0 and >= 0
	// respectively. A nil Progress skips callbacks; a non-nil one may be called
	// from several goroutines at once.
	Concurrency int
	Delay       time.Duration
	Progress    func(ReconProgress)
}

// ReconProgress is emitted once per fetched chunk. ChunksDone/ChunksTotal is
// the live percentage; ClassesDone counts fully processed classes, including
// classes that were already at the horizon and therefore needed no fetch.
type ReconProgress struct {
	ChunksDone    int
	ChunksTotal   int
	ClassesDone   int
	ClassesTotal  int
	ClassID       int64
	ClassComplete bool
	Err           error
}

// ReconResult summarises a finished sweep.
type ReconResult struct {
	Classes       int
	ClassesFresh  int
	ClassesFailed int
	Chunks        int
	Elements      map[string]int
	Elapsed       time.Duration
}

func (m ReconMode) defaultConcurrency() int {
	if m == ReconHard {
		return 8
	}
	return 1
}

func (m ReconMode) defaultDelay() time.Duration {
	if m == ReconHard {
		return 0
	}
	return time.Second
}

// SchoolYearRange exposes the server-side school-year bounds to the CLI.
func SchoolYearRange(now time.Time) (string, string) {
	return schoolYearRange(now)
}

type reconFetcher func(school string, classID int64, start, end string) ([]map[string]any, error)

// RunRecon enumerates teachers/rooms/subjects for every pooled class of one
// school and persists the result. Each class is fetched in <=2 week chunks;
// jobs outside the saved horizon (or from the year start with Full) are split
// ahead of time so progress is measured in chunks rather than classes.
func RunRecon(ctx context.Context, st *store.Store, uc *untis.Client, opts ReconOptions) (*ReconResult, error) {
	if opts.School == "" {
		return nil, fmt.Errorf("recon: a school is required")
	}
	if opts.Mode == "" {
		opts.Mode = ReconSlow
	}
	if opts.YearStart == "" || opts.YearEnd == "" {
		ys, ye := schoolYearRange(time.Now())
		if opts.YearStart == "" {
			opts.YearStart = ys
		}
		if opts.YearEnd == "" {
			opts.YearEnd = ye
		}
	}
	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = opts.Mode.defaultConcurrency()
	}
	delay := opts.Delay
	if delay < 0 {
		delay = opts.Mode.defaultDelay()
	}
	p := &Proxy{
		store:   st,
		untis:   uc,
		tt:      newTTCache(time.Minute),
		secrets: map[string]string{},
	}
	fetch := func(school string, classID int64, start, end string) ([]map[string]any, error) {
		// No cache: a sweep touches every chunk once and would otherwise keep
		// responses that nothing will read before they expire.
		return p.fetchClassChunkImpl(school, classID, start, end, false, false)
	}
	return runRecon(ctx, st, opts, concurrency, delay, fetch)
}

type reconJob struct {
	classID int64
	start   string
	end     string
}

func runRecon(ctx context.Context, st *store.Store, opts ReconOptions, concurrency int, delay time.Duration, fetch reconFetcher) (*ReconResult, error) {
	started := time.Now()
	classes, err := st.Pool(opts.School)
	if err != nil {
		return nil, fmt.Errorf("recon: list pool: %w", err)
	}
	if len(classes) == 0 {
		return nil, fmt.Errorf("recon: no pooled classes for school %q", opts.School)
	}
	stored, err := st.ReconScan(opts.School)
	if err != nil {
		return nil, fmt.Errorf("recon: read scan progress: %w", err)
	}

	target := reconHorizon()
	jobs := make([]reconJob, 0, len(classes))
	remaining := make(map[int64]int, len(classes))
	fresh := 0
	for _, c := range classes {
		since := opts.YearStart
		if !opts.Full {
			since = stored[c.ID]
			// A class already at the horizon has nothing left to read. Without
			// this a resume would fetch one target..target chunk per class.
			if since >= target {
				fresh++
				continue
			}
			if since == "" || since > target {
				since = target
			}
			if since < opts.YearStart {
				since = opts.YearStart
			}
		}
		chunks, cerr := chunkDates(since, target)
		if cerr != nil || len(chunks) == 0 {
			chunks, _ = chunkDates(since, opts.YearEnd)
		}
		if len(chunks) == 0 {
			continue
		}
		remaining[c.ID] = len(chunks)
		for _, ck := range chunks {
			jobs = append(jobs, reconJob{classID: c.ID, start: ck[0], end: ck[1]})
		}
	}

	res := &ReconResult{Classes: len(classes), ClassesFresh: fresh, Elements: map[string]int{}}
	if opts.DryRun {
		res.Chunks = len(jobs)
		res.Elapsed = time.Since(started)
		return res, nil
	}
	var mu sync.Mutex
	done := fresh
	failed := make(map[int64]bool)
	completed := make([]int64, 0, len(remaining))
	found := map[string]map[int64]bool{}
	chunksDone := 0
	chunksTotal := len(jobs)

	emit := func(p ReconProgress) {
		if opts.Progress != nil {
			opts.Progress(p)
		}
	}

	finishClass := func(classID int64, ferr error) (bool, bool) {
		remaining[classID]--
		if ferr != nil {
			failed[classID] = true
		}
		if remaining[classID] > 0 {
			return false, ferr != nil
		}
		done++
		classFailed := failed[classID]
		if classFailed {
			res.ClassesFailed++
		} else {
			completed = append(completed, classID)
		}
		return true, classFailed
	}

	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > len(jobs) && len(jobs) > 0 {
		concurrency = len(jobs)
	}
	jobCh := make(chan reconJob)
	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobCh {
				if ctx.Err() != nil {
					return
				}
				periods, ferr := fetch(opts.School, j.classID, j.start, j.end)
				if ferr == nil {
					mu.Lock()
					for _, pd := range periods {
						for _, el := range periodElementIDs(pd) {
							if found[el.Type] == nil {
								found[el.Type] = map[int64]bool{}
							}
							found[el.Type][el.ID] = true
						}
					}
					mu.Unlock()
				}
				mu.Lock()
				chunksDone++
				complete, _ := finishClass(j.classID, ferr)
				p := ReconProgress{
					ChunksDone: chunksDone, ChunksTotal: chunksTotal,
					ClassesDone: done, ClassesTotal: len(classes),
					ClassID: j.classID, ClassComplete: complete, Err: ferr,
				}
				mu.Unlock()
				emit(p)
				if delay > 0 {
					select {
					case <-ctx.Done():
						return
					case <-time.After(delay):
					}
				}
			}
		}()
	}
feed:
	for _, j := range jobs {
		select {
		case <-ctx.Done():
			break feed
		case jobCh <- j:
		}
	}
	close(jobCh)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return res, err
	}
	elems := make(map[string][]int64, len(found))
	for elType, ids := range found {
		for id := range ids {
			elems[elType] = append(elems[elType], id)
		}
		res.Elements[elType] = len(elems[elType])
	}
	// Replacing the persisted set is only safe when the sweep covered every
	// pooled class and every one of them succeeded. Otherwise merge, so elements
	// vouched for by a skipped or failed class are not dropped.
	if opts.Full && fresh == 0 && res.ClassesFailed == 0 {
		if err := st.SaveReconElements(opts.School, elems); err != nil {
			return res, fmt.Errorf("recon: save elements: %w", err)
		}
	} else if merged, err := st.LoadReconElements(opts.School); err == nil {
		for elType, ids := range elems {
			for _, id := range ids {
				merged[elType] = append(merged[elType], id)
			}
		}
		if err := st.SaveReconElements(opts.School, merged); err != nil {
			return res, fmt.Errorf("recon: save elements: %w", err)
		}
	} else if err := st.SaveReconElements(opts.School, elems); err != nil {
		return res, fmt.Errorf("recon: save elements: %w", err)
	}
	// Advance the horizons only after the elements are stored. The reverse order
	// would let a failed write make the next run skip classes whose elements
	// were never saved.
	for _, classID := range completed {
		if err := st.SaveReconScanAt(opts.School, classID, target); err != nil {
			return res, fmt.Errorf("recon: save scan progress: %w", err)
		}
	}
	res.Chunks = chunksTotal
	res.Elapsed = time.Since(started)
	return res, nil
}
