package store

// The poller fetches a sliding [today-1, today+4] window. One new day enters
// that window at every midnight, and before the window marker existed those
// lessons were indistinguishable from lessons added upstream: every subscriber
// got a burst of "new lesson" notifications once a day (01:00 or 02:00 local
// when the container runs in UTC). These tests pin the two halves of the fix:
// a lesson on a never-before-observed date is baseline data, while a lesson
// added inside the already-observed range is still announced.

import (
	"path/filepath"
	"testing"
)

func openWindowTest(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "window.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// captureEnqueuer records the rows a change would have notified.
func captureEnqueuer() (Enqueuer, *[][]PeriodRow) {
	var calls [][]PeriodRow
	return func(rows []PeriodRow, put func(string, int64, []byte) error) error {
		calls = append(calls, append([]PeriodRow(nil), rows...))
		return nil
	}, &calls
}

func TestSnapshotWindowSuppressesHorizonDiscovery(t *testing.T) {
	st := openWindowTest(t)
	enqueue, calls := captureEnqueuer()

	day1 := []PeriodRow{{
		PeriodID: 10, Kind: "ADDED", Start: "2026-09-05 08:00", End: "2026-09-05 08:45", Subject: "Math",
	}}
	changed, err := st.ApplyClassSnapshotWindow("s", 5000, day1, 1, "", "2026-09-05", "2026-09-05", enqueue)
	if err != nil {
		t.Fatalf("first window poll: %v", err)
	}
	if changed != 1 {
		t.Fatalf("first window poll changed=%d, want 1 (fresh baseline is announced)", changed)
	}
	if len(*calls) != 1 {
		t.Fatalf("enqueue calls = %d, want 1", len(*calls))
	}

	// Midnight: the window now reaches 2026-09-06, so period 20 has entered
	// view for the first time. It must be stored, but not announced.
	day2 := []PeriodRow{
		{PeriodID: 10, Kind: "ADDED", Start: "2026-09-05 08:00", End: "2026-09-05 08:45", Subject: "Math"},
		{PeriodID: 20, Kind: "ADDED", Start: "2026-09-06 08:00", End: "2026-09-06 08:45", Subject: "Bio"},
	}
	changed, err = st.ApplyClassSnapshotWindow("s", 5000, day2, 2, "", "2026-09-05", "2026-09-06", enqueue)
	if err != nil {
		t.Fatalf("horizon poll: %v", err)
	}
	if changed != 0 {
		t.Fatalf("horizon poll changed=%d, want 0 (newly visible day is not a change)", changed)
	}
	if len(*calls) != 1 {
		t.Fatalf("enqueue calls = %d, want 1 (horizon discovery must not notify)", len(*calls))
	}
	if v := st.ClassVersion("s", 5000); v != 1 {
		t.Errorf("ClassVersion = %d, want 1 (horizon discovery must not bump)", v)
	}
	if got := st.ObservedWindowEnd("s", 5000); got != "2026-09-06" {
		t.Errorf("ObservedWindowEnd = %q, want 2026-09-06 (stored even without a change)", got)
	}

	snap, err := st.LoadClassSnapshot("s", 5000)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	var found *PeriodRow
	for i := range snap {
		if snap[i].PeriodID == 20 {
			found = &snap[i]
		}
	}
	if found == nil {
		t.Fatal("horizon lesson was not stored in the snapshot")
	}
	if found.Kind != "UNCHANGED" {
		t.Errorf("horizon lesson kind = %q, want UNCHANGED", found.Kind)
	}
	if found.ModVer != 1 {
		t.Errorf("horizon lesson mod version = %d, want the pre-change version 1", found.ModVer)
	}

	// A client at the old version must not see the horizon lesson as pending.
	pending, cur, err := st.PendingChanges("s", 5000, 1)
	if err != nil {
		t.Fatalf("pending changes: %v", err)
	}
	if cur != 1 {
		t.Errorf("current version = %d, want 1", cur)
	}
	for _, r := range pending {
		if r.PeriodID == 20 {
			t.Errorf("horizon lesson appears in pending changes: %+v", r)
		}
	}

	// Once the date is inside the observed window, a real change is announced.
	day3 := []PeriodRow{
		{PeriodID: 10, Kind: "ADDED", Start: "2026-09-05 08:00", End: "2026-09-05 08:45", Subject: "Math"},
		{PeriodID: 20, Kind: "ADDED", Start: "2026-09-06 08:00", End: "2026-09-06 08:45", Subject: "Bio", Room: "R112"},
	}
	changed, err = st.ApplyClassSnapshotWindow("s", 5000, day3, 2, "", "2026-09-05", "2026-09-07", enqueue)
	if err != nil {
		t.Fatalf("real change after horizon: %v", err)
	}
	if changed != 1 {
		t.Fatalf("real change changed=%d, want 1", changed)
	}
	last := (*calls)[len(*calls)-1]
	if len(last) != 1 || last[0].PeriodID != 20 || last[0].Kind != "CHANGED" {
		t.Errorf("announced rows = %+v, want one CHANGED period 20", last)
	}
}

// TestSnapshotWindowOnInitiallyEmptyClass: a class whose first observed window
// contained no lessons has version 0. When lessons later appear on a newly
// visible day they are baseline data, but since=0 (the full snapshot) must still
// return them.
func TestSnapshotWindowOnInitiallyEmptyClass(t *testing.T) {
	st := openWindowTest(t)
	enqueue, calls := captureEnqueuer()

	changed, err := st.ApplyClassSnapshotWindow("s", 5003, nil, 1, "", "2026-09-05", "2026-09-05", enqueue)
	if err != nil {
		t.Fatalf("empty first poll: %v", err)
	}
	if changed != 0 || len(*calls) != 0 {
		t.Fatalf("empty first poll changed=%d calls=%d, want 0/0", changed, len(*calls))
	}
	if got := st.ObservedWindowEnd("s", 5003); got != "2026-09-05" {
		t.Fatalf("ObservedWindowEnd = %q, want 2026-09-05", got)
	}

	day2 := []PeriodRow{{
		PeriodID: 50, Kind: "ADDED", Start: "2026-09-06 08:00", End: "2026-09-06 08:45", Subject: "Chem",
	}}
	changed, err = st.ApplyClassSnapshotWindow("s", 5003, day2, 1, "", "2026-09-05", "2026-09-06", enqueue)
	if err != nil {
		t.Fatalf("horizon poll: %v", err)
	}
	if changed != 0 || len(*calls) != 0 {
		t.Fatalf("horizon poll changed=%d calls=%d, want 0/0 (baseline, not a change)", changed, len(*calls))
	}
	if v := st.ClassVersion("s", 5003); v != 0 {
		t.Errorf("ClassVersion = %d, want 0 (nothing to announce)", v)
	}

	all, _, err := st.PendingChanges("s", 5003, 0)
	if err != nil {
		t.Fatalf("full snapshot: %v", err)
	}
	if len(all) != 1 || all[0].PeriodID != 50 {
		t.Errorf("full snapshot = %+v, want the baseline period 50", all)
	}
	if pending, _, _ := st.PendingChanges("s", 5003, 1); len(pending) != 0 {
		t.Errorf("since=1 pending = %+v, want none", pending)
	}
}

func TestSnapshotWindowAnnouncesAdditionInsideObservedRange(t *testing.T) {
	st := openWindowTest(t)
	enqueue, calls := captureEnqueuer()

	day1 := []PeriodRow{{
		PeriodID: 10, Kind: "ADDED", Start: "2026-09-04 08:00", End: "2026-09-04 08:45", Subject: "Math",
	}}
	if _, err := st.ApplyClassSnapshotWindow("s", 5001, day1, 1, "", "2026-09-04", "2026-09-05", enqueue); err != nil {
		t.Fatalf("first poll: %v", err)
	}

	// 2026-09-05 was already inside the observed window, so a lesson appearing
	// there is a real addition (a substitution, a new exam block, ...).
	day2 := []PeriodRow{
		{PeriodID: 10, Kind: "ADDED", Start: "2026-09-04 08:00", End: "2026-09-04 08:45", Subject: "Math"},
		{PeriodID: 30, Kind: "ADDED", Start: "2026-09-05 10:00", End: "2026-09-05 10:45", Subject: "Physics"},
	}
	changed, err := st.ApplyClassSnapshotWindow("s", 5001, day2, 2, "", "2026-09-04", "2026-09-06", enqueue)
	if err != nil {
		t.Fatalf("addition poll: %v", err)
	}
	if changed != 1 {
		t.Fatalf("addition changed=%d, want 1 (date was already observed)", changed)
	}
	last := (*calls)[len(*calls)-1]
	if len(last) != 1 || last[0].PeriodID != 30 || last[0].Kind != "ADDED" {
		t.Errorf("announced rows = %+v, want one ADDED period 30", last)
	}
}

func TestSnapshotLegacyStillAnnouncesEveryUnseenPeriod(t *testing.T) {
	st := openWindowTest(t)
	enqueue, calls := captureEnqueuer()

	day1 := []PeriodRow{{PeriodID: 10, Kind: "ADDED", Start: "2026-09-05 08:00", End: "2026-09-05 08:45"}}
	if _, err := st.ApplyClassSnapshot("s", 5002, day1, 1, "", enqueue); err != nil {
		t.Fatalf("legacy first poll: %v", err)
	}
	day2 := []PeriodRow{
		{PeriodID: 10, Kind: "ADDED", Start: "2026-09-05 08:00", End: "2026-09-05 08:45"},
		{PeriodID: 40, Kind: "ADDED", Start: "2026-09-09 08:00", End: "2026-09-09 08:45"},
	}
	changed, err := st.ApplyClassSnapshot("s", 5002, day2, 2, "", enqueue)
	if err != nil {
		t.Fatalf("legacy second poll: %v", err)
	}
	if changed != 1 {
		t.Fatalf("legacy changed=%d, want 1 (no window marker, old behaviour)", changed)
	}
	last := (*calls)[len(*calls)-1]
	if len(last) != 1 || last[0].PeriodID != 40 || last[0].Kind != "ADDED" {
		t.Errorf("announced rows = %+v, want one ADDED period 40", last)
	}
	if got := st.ObservedWindowEnd("s", 5002); got != "" {
		t.Errorf("legacy call recorded observed_end %q, want empty", got)
	}
}

// TestSnapshotWindowSuppressesEarlierHorizonDiscovery pins the mirror image of
// the midnight case: when the app starts asking for the whole school week, the
// window start moves backwards and lessons on the newly visible earlier days are
// baseline data, not lessons added upstream.
func TestSnapshotWindowSuppressesEarlierHorizonDiscovery(t *testing.T) {
	st := openWindowTest(t)
	enqueue, calls := captureEnqueuer()

	// Sunday 2026-09-06 was not in the window yet; Monday was.
	monday := []PeriodRow{
		{PeriodID: 10, Kind: "ADDED", Start: "2026-09-07 08:00", End: "2026-09-07 08:45", Subject: "Math"},
	}
	changed, err := st.ApplyClassSnapshotWindow("s", 5004, monday, 1, "", "2026-09-07", "2026-09-13", enqueue)
	if err != nil {
		t.Fatalf("first window poll: %v", err)
	}
	if changed != 1 {
		t.Fatalf("first window poll changed=%d, want 1 (fresh baseline is announced)", changed)
	}

	// The window widens to include Sunday. Its lessons have been there all
	// along; they were simply outside the fetch range.
	widened := []PeriodRow{
		{PeriodID: 11, Kind: "ADDED", Start: "2026-09-06 18:00", End: "2026-09-06 18:45", Subject: "Sport"},
		{PeriodID: 10, Kind: "ADDED", Start: "2026-09-07 08:00", End: "2026-09-07 08:45", Subject: "Math"},
	}
	changed, err = st.ApplyClassSnapshotWindow("s", 5004, widened, 2, "", "2026-09-06", "2026-09-13", enqueue)
	if err != nil {
		t.Fatalf("widened poll: %v", err)
	}
	if changed != 0 {
		t.Fatalf("widened poll changed=%d, want 0 (newly visible earlier day is not a change)", changed)
	}
	if len(*calls) != 1 {
		t.Fatalf("enqueue calls = %d, want 1 (widening must not notify)", len(*calls))
	}
	if v := st.ClassVersion("s", 5004); v != 1 {
		t.Errorf("ClassVersion = %d, want 1 (widening must not bump)", v)
	}
	if got := st.ObservedWindowStart("s", 5004); got != "2026-09-06" {
		t.Errorf("ObservedWindowStart = %q, want 2026-09-06 (stored even without a change)", got)
	}

	snap, err := st.LoadClassSnapshot("s", 5004)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	for _, r := range snap {
		if r.PeriodID == 11 && (r.Kind != "UNCHANGED" || r.ModVer != 1) {
			t.Errorf("widened lesson = %+v, want UNCHANGED at pre-change version 1", r)
		}
	}
	if pending, _, _ := st.PendingChanges("s", 5004, 1); len(pending) != 0 {
		t.Errorf("since=1 pending = %+v, want none (widening is baseline)", pending)
	}

	// Once Sunday is inside the observed window, a real addition there is
	// announced normally.
	realAdd := []PeriodRow{
		{PeriodID: 11, Kind: "ADDED", Start: "2026-09-06 18:00", End: "2026-09-06 18:45", Subject: "Sport"},
		{PeriodID: 12, Kind: "ADDED", Start: "2026-09-06 09:00", End: "2026-09-06 09:45", Subject: "Kunst"},
		{PeriodID: 10, Kind: "ADDED", Start: "2026-09-07 08:00", End: "2026-09-07 08:45", Subject: "Math"},
	}
	changed, err = st.ApplyClassSnapshotWindow("s", 5004, realAdd, 2, "", "2026-09-06", "2026-09-13", enqueue)
	if err != nil {
		t.Fatalf("real addition: %v", err)
	}
	if changed != 1 {
		t.Fatalf("real addition changed=%d, want 1", changed)
	}
	last := (*calls)[len(*calls)-1]
	if len(last) != 1 || last[0].PeriodID != 12 || last[0].Kind != "ADDED" {
		t.Errorf("announced rows = %+v, want one ADDED period 12", last)
	}
}

// TestSnapshotWindowLegacyRowDoesNotBurstOnUpgrade covers a row written before
// observed_start existed. Its near edge is unknown, so the first widened poll
// must treat earlier days as baseline instead of announcing a whole week, and it
// must persist the real start so normal detection resumes immediately after.
func TestSnapshotWindowLegacyRowDoesNotBurstOnUpgrade(t *testing.T) {
	st := openWindowTest(t)
	enqueue, calls := captureEnqueuer()

	monday := []PeriodRow{
		{PeriodID: 10, Kind: "ADDED", Start: "2026-09-07 08:00", End: "2026-09-07 08:45", Subject: "Math"},
	}
	if _, err := st.ApplyClassSnapshotWindow("s", 5005, monday, 1, "", "2026-09-07", "2026-09-13", enqueue); err != nil {
		t.Fatalf("first window poll: %v", err)
	}
	if _, err := st.db.Exec(`UPDATE timetable_versions SET observed_start='' WHERE school='s' AND class_id=5005`); err != nil {
		t.Fatalf("simulate pre-upgrade row: %v", err)
	}

	widened := []PeriodRow{
		{PeriodID: 11, Kind: "ADDED", Start: "2026-09-06 18:00", End: "2026-09-06 18:45", Subject: "Sport"},
		{PeriodID: 10, Kind: "ADDED", Start: "2026-09-07 08:00", End: "2026-09-07 08:45", Subject: "Math"},
	}
	changed, err := st.ApplyClassSnapshotWindow("s", 5005, widened, 2, "2026-09-08", "2026-09-06", "2026-09-13", enqueue)
	if err != nil {
		t.Fatalf("widened legacy poll: %v", err)
	}
	if changed != 0 {
		t.Fatalf("widened legacy poll changed=%d, want 0 (no upgrade burst)", changed)
	}
	if len(*calls) != 1 {
		t.Fatalf("enqueue calls = %d, want 1", len(*calls))
	}
	if got := st.ObservedWindowStart("s", 5005); got != "2026-09-06" {
		t.Errorf("ObservedWindowStart = %q, want the real window start 2026-09-06", got)
	}

	// Detection resumes on the next poll: the earlier day is now observed.
	add := append([]PeriodRow(nil), widened...)
	add = append(add, PeriodRow{PeriodID: 12, Kind: "ADDED", Start: "2026-09-06 09:00", End: "2026-09-06 09:45", Subject: "Kunst"})
	changed, err = st.ApplyClassSnapshotWindow("s", 5005, add, 3, "2026-09-08", "2026-09-06", "2026-09-13", enqueue)
	if err != nil {
		t.Fatalf("poll after upgrade: %v", err)
	}
	if changed != 1 {
		t.Fatalf("poll after upgrade changed=%d, want 1", changed)
	}
}
