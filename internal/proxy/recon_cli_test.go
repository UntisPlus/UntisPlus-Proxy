package proxy

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"untis-proxy/internal/store"
)

func reconTestStore(t *testing.T, classes ...int64) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "recon.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.SetDefaultSchool("testschool"); err != nil {
		t.Fatalf("default school: %v", err)
	}
	for i, classID := range classes {
		u := &store.User{
			Username: fmt.Sprintf("u%d", classID),
			Password: "x",
			PersonID: int64(1000 + i),
			ClassID:  classID,
			School:   "testschool",
			LastSeen: time.Now(),
		}
		if err := st.UpsertUser(u); err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}
	return st
}

func fakePeriod(els ...struct {
	Type string
	ID   int64
}) map[string]any {
	out := make([]any, 0, len(els))
	for _, el := range els {
		out = append(out, map[string]any{"type": el.Type, "id": float64(el.ID)})
	}
	return map[string]any{"id": float64(1), "elements": out}
}

type el struct {
	Type string
	ID   int64
}

func TestRunReconProgressAndPersistence(t *testing.T) {
	st := reconTestStore(t, 10, 20, 30)
	var calls int64
	progress := make([]ReconProgress, 0)
	var pmu sync.Mutex
	fetch := func(school string, classID int64, start, end string) ([]map[string]any, error) {
		atomic.AddInt64(&calls, 1)
		return []map[string]any{
			fakePeriod(el{"TEACHER", classID}, el{"ROOM", classID + 100}, el{"SUBJECT", classID + 200}),
		}, nil
	}
	opts := ReconOptions{
		School: "testschool", YearStart: "2025-08-01", YearEnd: "2026-07-31",
		Mode: ReconSlow, Concurrency: 2,
		Progress: func(p ReconProgress) {
			pmu.Lock()
			progress = append(progress, p)
			pmu.Unlock()
		},
	}
	res, err := runRecon(context.Background(), st, opts, 2, 0, fetch)
	if err != nil {
		t.Fatalf("runRecon: %v", err)
	}
	if calls != 3 {
		t.Fatalf("fetch calls = %d, want 3 (one chunk per class)", calls)
	}
	if res.Classes != 3 || res.ClassesFailed != 0 {
		t.Fatalf("result = %+v", res)
	}
	if len(progress) != 3 {
		t.Fatalf("progress events = %d, want 3", len(progress))
	}
	last := progress[len(progress)-1]
	if last.ChunksDone != last.ChunksTotal || last.ClassesDone != 3 {
		t.Fatalf("final progress = %+v", last)
	}

	scans, err := st.ReconScan("testschool")
	if err != nil {
		t.Fatalf("recon scan: %v", err)
	}
	target := reconHorizon()
	for _, classID := range []int64{10, 20, 30} {
		if scans[classID] != target {
			t.Fatalf("class %d horizon = %q, want %q", classID, scans[classID], target)
		}
	}
	elems, err := st.LoadReconElements("testschool")
	if err != nil {
		t.Fatalf("load elements: %v", err)
	}
	if len(elems["TEACHER"]) != 3 || len(elems["ROOM"]) != 3 || len(elems["SUBJECT"]) != 3 {
		t.Fatalf("elements = %+v", elems)
	}
}

func TestRunReconFullStartsAtYearStart(t *testing.T) {
	st := reconTestStore(t, 10)
	// A saved horizon at the target would make a resume a no-op.
	target := reconHorizon()
	if err := st.SaveReconScanAt("testschool", 10, target); err != nil {
		t.Fatalf("save horizon: %v", err)
	}
	start, _ := time.Parse("2006-01-02", target)
	yearStart := start.AddDate(0, 0, -27).Format("2006-01-02")

	var chunks int64
	fetch := func(school string, classID int64, s, e string) ([]map[string]any, error) {
		atomic.AddInt64(&chunks, 1)
		return []map[string]any{fakePeriod(el{"TEACHER", 1})}, nil
	}
	opt := ReconOptions{School: "testschool", YearStart: yearStart, YearEnd: target, Mode: ReconHard}

	resume, err := runRecon(context.Background(), st, opt, 1, 0, fetch)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resume.Chunks != 0 || atomic.LoadInt64(&chunks) != 0 {
		t.Fatalf("resume with fresh horizon fetched %d chunks, want 0", resume.Chunks)
	}

	opt.Full = true
	full, err := runRecon(context.Background(), st, opt, 1, 0, fetch)
	if err != nil {
		t.Fatalf("full: %v", err)
	}
	if full.Chunks < 2 {
		t.Fatalf("full chunks = %d, want at least 2 over 28 days", full.Chunks)
	}
	if atomic.LoadInt64(&chunks) != int64(full.Chunks) {
		t.Fatalf("fetched %d chunks, result says %d", chunks, full.Chunks)
	}
}

func TestRunReconFailureKeepsHorizonAndMerges(t *testing.T) {
	st := reconTestStore(t, 10, 20, 30)
	if err := st.SaveReconElements("testschool", map[string][]int64{"TEACHER": {999}}); err != nil {
		t.Fatalf("seed elements: %v", err)
	}
	target := reconHorizon()
	fetch := func(school string, classID int64, s, e string) ([]map[string]any, error) {
		if classID == 20 {
			return nil, fmt.Errorf("upstream down")
		}
		return []map[string]any{fakePeriod(el{"ROOM", classID})}, nil
	}
	opts := ReconOptions{School: "testschool", YearStart: "2025-08-01", YearEnd: "2026-07-31", Mode: ReconHard}
	res, err := runRecon(context.Background(), st, opts, 3, 0, fetch)
	if err != nil {
		t.Fatalf("runRecon: %v", err)
	}
	if res.ClassesFailed != 1 {
		t.Fatalf("failed classes = %d, want 1", res.ClassesFailed)
	}
	scans, _ := st.ReconScan("testschool")
	if _, ok := scans[20]; ok {
		t.Fatalf("failed class 20 recorded horizon %q", scans[20])
	}
	if scans[10] != target || scans[30] != target {
		t.Fatalf("successful horizons = %+v", scans)
	}
	elems, _ := st.LoadReconElements("testschool")
	// The pre-existing element must survive a partial sweep, and the two
	// successful classes must contribute theirs.
	if len(elems["TEACHER"]) != 1 || elems["TEACHER"][0] != 999 {
		t.Fatalf("merged teachers = %+v", elems["TEACHER"])
	}
	if len(elems["ROOM"]) != 2 {
		t.Fatalf("merged rooms = %+v, want 2", elems["ROOM"])
	}
}

func TestRunReconDryRunDoesNotFetchOrWrite(t *testing.T) {
	st := reconTestStore(t, 10, 20)
	var calls int64
	fetch := func(school string, classID int64, s, e string) ([]map[string]any, error) {
		atomic.AddInt64(&calls, 1)
		return nil, fmt.Errorf("must not fetch")
	}
	opts := ReconOptions{
		School: "testschool", YearStart: "2025-08-01", YearEnd: "2026-07-31",
		Mode: ReconHard, Full: true, DryRun: true,
	}
	res, err := runRecon(context.Background(), st, opts, 1, 0, fetch)
	if err != nil {
		t.Fatalf("runRecon: %v", err)
	}
	if calls != 0 {
		t.Fatalf("dry run fetched %d times", calls)
	}
	if res.Chunks == 0 {
		t.Fatal("dry run planned no chunks")
	}
	scans, _ := st.ReconScan("testschool")
	if len(scans) != 0 {
		t.Fatalf("dry run wrote scan progress: %+v", scans)
	}
}

func TestRunReconRespectsConcurrency(t *testing.T) {
	st := reconTestStore(t, 1, 2, 3, 4, 5, 6)
	var inFlight, maxSeen int64
	var mu sync.Mutex
	fetch := func(school string, classID int64, s, e string) ([]map[string]any, error) {
		n := atomic.AddInt64(&inFlight, 1)
		mu.Lock()
		if n > maxSeen {
			maxSeen = n
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt64(&inFlight, -1)
		return []map[string]any{fakePeriod(el{"TEACHER", classID})}, nil
	}
	opts := ReconOptions{School: "testschool", YearStart: "2025-08-01", YearEnd: "2026-07-31", Mode: ReconHard}
	if _, err := runRecon(context.Background(), st, opts, 3, 0, fetch); err != nil {
		t.Fatalf("runRecon: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if maxSeen > 3 {
		t.Fatalf("max in flight = %d, want <= 3", maxSeen)
	}
	if maxSeen < 2 {
		t.Fatalf("max in flight = %d, want parallel fetches", maxSeen)
	}
}
