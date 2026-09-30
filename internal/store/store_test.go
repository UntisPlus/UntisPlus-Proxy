package store

import (
	"path/filepath"
	"testing"
)

func TestReplaceClassSnapshotDiff(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if got := st.ClassVersion("s", 1); got != 0 {
		t.Fatalf("initial version = %d, want 0", got)
	}

	// initial snapshot: 2 periods
	v1 := []PeriodRow{
		{PeriodID: 100, Start: "a1", End: "b1", Subject: "S1"},
		{PeriodID: 200, Start: "a2", End: "b2", Subject: "S2"},
	}
	changed, err := st.ReplaceClassSnapshot("s", 1, v1, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if changed != 2 {
		t.Fatalf("first write changed=%d, want 2", changed)
	}
	if got := st.ClassVersion("s", 1); got != 1 {
		t.Fatalf("version after first write = %d, want 1", got)
	}

	// unchanged snapshot: no change
	changed, err = st.ReplaceClassSnapshot("s", 1, v1, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if changed != 0 {
		t.Fatalf("unchanged write changed=%d, want 0", changed)
	}
	if got := st.ClassVersion("s", 1); got != 1 {
		t.Fatalf("version after unchanged write = %d, want 1", got)
	}

	// one CHANGED, one REMOVED, one ADDED
	v2 := []PeriodRow{
		{PeriodID: 100, Start: "a1", End: "b1", Subject: "S1-NEW"}, // changed
		{PeriodID: 300, Start: "a3", End: "b3", Subject: "S3"},     // added
	}
	changed, err = st.ReplaceClassSnapshot("s", 1, v2, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if changed != 3 {
		t.Fatalf("diff write changed=%d, want 3 (1 changed + 1 added + 1 removed)", changed)
	}
	if got := st.ClassVersion("s", 1); got != 2 {
		t.Fatalf("version after diff write = %d, want 2", got)
	}

	pending, cur, err := st.PendingChanges("s", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if cur != 2 {
		t.Fatalf("pending current = %d, want 2", cur)
	}
	kinds := map[int64]string{}
	for _, r := range pending {
		kinds[r.PeriodID] = r.Kind
	}
	if kinds[100] != "CHANGED" {
		t.Fatalf("period 100 kind = %q, want CHANGED", kinds[100])
	}
	if kinds[300] != "ADDED" {
		t.Fatalf("period 300 kind = %q, want ADDED", kinds[300])
	}
	if _, ok := kinds[200]; !ok {
		t.Fatalf("period 200 (removed) missing from pending: %+v", kinds)
	}
	// after consuming up to 2, nothing pending
	pending2, cur2, err := st.PendingChanges("s", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if cur2 != 2 {
		t.Fatalf("current after consume = %d, want 2", cur2)
	}
	if len(pending2) != 0 {
		t.Fatalf("pending after consume = %d, want 0", len(pending2))
	}
}

func TestDropRemovedBefore(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// snapshot with one past period (started yesterday) and one today
	v1 := []PeriodRow{
		{PeriodID: 1, Start: "2026-08-28T10:00Z", End: "2026-08-28T10:45Z", Subject: "A"},
		{PeriodID: 2, Start: "2026-08-29T10:00Z", End: "2026-08-29T10:45Z", Subject: "B"},
	}
	if _, err := st.ReplaceClassSnapshot("s", 2, v1, 1, ""); err != nil {
		t.Fatal(err)
	}
	// next poll: no periods (both left the window); drop before today (2026-08-29).
	// Period 1 (2026-08-28) should silently disappear; period 2 (2026-08-29) removed+reported.
	changed, err := st.ReplaceClassSnapshot("s", 2, nil, 2, "2026-08-29")
	if err != nil {
		t.Fatal(err)
	}
	if changed != 1 {
		t.Fatalf("changed = %d, want 1 (only the today period reported as removed)", changed)
	}
	pending, _, _ := st.PendingChanges("s", 2, 1)
	if len(pending) != 1 || pending[0].PeriodID != 2 || pending[0].Kind != "REMOVED" {
		t.Fatalf("pending = %+v, want only period 2 REMOVED", pending)
	}
}

// TestRemovedIsReportedOnce covers a cancellation that has not happened yet.
// The removal loop must not re-report it on every later poll, or a single
// cancelled lesson sends one notification per poll until its start date passes.
func TestRemovedIsReportedOnce(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// today is 2026-08-29. A lesson four days out is the furthest a period can
	// be in the fetch window, so it is the longest this can repeat for.
	const today = "2026-08-29"
	future := []PeriodRow{
		{PeriodID: 7, Start: "2026-09-02T08:00Z", End: "2026-09-02T08:45Z", Subject: "Mathe", Room: "R204"},
	}
	if _, err := st.ReplaceClassSnapshot("s", 2, future, 1, ""); err != nil {
		t.Fatal(err)
	}

	// The cancellation. This is the one and only time it may be reported.
	changed, err := st.ReplaceClassSnapshot("s", 2, nil, 2, today)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 1 {
		t.Fatalf("cancellation: changed = %d, want 1", changed)
	}
	pending, ver, err := st.PendingChanges("s", 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].PeriodID != 7 || pending[0].Kind != "REMOVED" {
		t.Fatalf("cancellation: pending = %+v, want period 7 REMOVED", pending)
	}
	if ver != 2 {
		t.Fatalf("version = %d, want 2", ver)
	}

	// Every later poll sees the same thing: still absent upstream. Nothing changed.
	for i := int64(3); i <= 8; i++ {
		changed, err := st.ReplaceClassSnapshot("s", 2, nil, i, today)
		if err != nil {
			t.Fatal(err)
		}
		if changed != 0 {
			pending, _, _ := st.PendingChanges("s", 2, i-1)
			t.Fatalf("poll %d: changed = %d and %d rows pending, want 0/0 — the same cancellation is being re-reported",
				i, changed, len(pending))
		}
		if ver := st.ClassVersion("s", 2); ver != 2 {
			t.Fatalf("poll %d: version = %d, want 2 (must not advance)", i, ver)
		}
	}

	// The record itself must survive, so clients asking for the full history
	// still learn the lesson was cancelled. It is hidden from PendingChanges
	// only because its mod version is no longer current.
	all, _, err := st.PendingChanges("s", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Kind != "REMOVED" {
		t.Fatalf("full history = %+v, want the REMOVED row to still be readable", all)
	}
	if all[0].ModVer != 2 {
		t.Errorf("REMOVED row mod version = %d, want 2 (the version it was first reported at)", all[0].ModVer)
	}
}

// TestRemovedAgedOutIsStillDropped guards the interaction with the window: a
// removal whose start date has passed should still disappear rather than linger.
func TestRemovedAgedOutIsStillDropped(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, err := st.ReplaceClassSnapshot("s", 2, []PeriodRow{
		{PeriodID: 7, Start: "2026-08-28T08:00Z", End: "2026-08-28T08:45Z", Subject: "Mathe"},
	}, 1, ""); err != nil {
		t.Fatal(err)
	}
	// cancelled, and it started yesterday
	if changed, err := st.ReplaceClassSnapshot("s", 2, nil, 2, "2026-08-29"); err != nil {
		t.Fatal(err)
	} else if changed != 0 {
		t.Fatalf("yesterday's removal should not be reported, changed = %d", changed)
	}
	if all, _, _ := st.PendingChanges("s", 2, 0); len(all) != 0 {
		t.Fatalf("full history = %+v, want empty", all)
	}
}

func TestClassTokenCreatedByRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	tok := &ClassToken{
		Token: "abc123", School: "testschool", ClassID: 5000,
		ElementType: "TEACHER", ElementID: 112, Timezone: "Europe/Berlin",
		Days: 30, CreatedAt: 12345, CreatedBy: "jdoe",
	}
	if err := st.CreateClassToken(tok); err != nil {
		t.Fatalf("create token: %v", err)
	}
	tokens, err := st.ListClassTokens()
	if err != nil {
		t.Fatalf("list tokens: %v", err)
	}
	found := false
	for _, x := range tokens {
		if x.Token != "abc123" {
			continue
		}
		found = true
		if x.CreatedBy != "jdoe" || x.ElementType != "TEACHER" {
			t.Errorf("createdBy=%q elementType=%q, want jdoe/TEACHER", x.CreatedBy, x.ElementType)
		}
	}
	if !found {
		t.Fatal("token not found after round-trip")
	}
}

func TestNtfyBaseOverrideRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	id, err := st.AddNtfyTopic(&NtfyTopic{
		School: "testschool", ClassID: 5000, Topic: "myschool",
		BaseURL: "https://ntfy.example.org", Enabled: true, CreatedBy: "bschneider",
	})
	if err != nil {
		t.Fatalf("add topic: %v", err)
	}
	topics, err := st.ListNtfyTopics("")
	if err != nil {
		t.Fatalf("list topics: %v", err)
	}
	if len(topics) != 1 || topics[0].ID != id {
		t.Fatalf("unexpected topics: %+v", topics)
	}
	if topics[0].BaseURL != "https://ntfy.example.org" {
		t.Errorf("baseUrl = %q, want https://ntfy.example.org", topics[0].BaseURL)
	}
	if topics[0].CreatedBy != "bschneider" {
		t.Errorf("createdBy = %q, want bschneider", topics[0].CreatedBy)
	}
}

func TestElementTargetRoundTripAndBackfill(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// legacy class-targeted rows are backfilled to CLASS/class_id on open
	if _, err := st.db.Exec(`INSERT INTO ntfy_topics (school, class_id, topic, enabled, created_by, created_at) VALUES ('s',5000,'legacy',1,'x',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO webhooks (school, class_id, url, enabled, created_by, created_at) VALUES ('s',5000,'https://l',1,'x',1)`); err != nil {
		t.Fatal(err)
	}
	topics, err := st.ListNtfyTopics("")
	if err != nil {
		t.Fatal(err)
	}
	if et, eid := topics[0].Target(); et != "CLASS" || eid != 5000 {
		t.Errorf("legacy topic Target() = %q/%d, want CLASS/5000", et, eid)
	}
	hooks, err := st.ListWebhooks("")
	if err != nil {
		t.Fatal(err)
	}
	if et, eid := hooks[0].Target(); et != "CLASS" || eid != 5000 {
		t.Errorf("legacy hook Target() = %q/%d, want CLASS/5000", et, eid)
	}

	// modern element targets round-trip
	if _, err := st.AddNtfyTopic(&NtfyTopic{School: "s", ElementType: "ROOM", ElementID: 169, Topic: "rooms", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddWebhook(&Webhook{School: "s", ElementType: "STUDENT", ElementID: 5005, URL: "https://st", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	topics, _ = st.ListNtfyTopics("")
	hooks, _ = st.ListWebhooks("")
	foundRoom, foundStudent := false, false
	for _, t := range topics {
		if et, eid := t.Target(); et == "ROOM" && eid == 169 {
			foundRoom = true
		}
	}
	for _, h := range hooks {
		if et, eid := h.Target(); et == "STUDENT" && eid == 5005 {
			foundStudent = true
		}
	}
	if !foundRoom || !foundStudent {
		t.Errorf("element targets not round-tripped: topics=%+v hooks=%+v", topics, hooks)
	}

	// ClassForPerson resolves a student's class for matching
	if err := st.UpsertUser(&User{Username: "dee", PersonID: 5005, PersonType: 5, ClassID: 5000}); err != nil {
		t.Fatal(err)
	}
	if cid, err := st.ClassForPerson("s", 5005); err != nil || cid != 5000 {
		t.Errorf("ClassForPerson = %d/%v, want 5000/nil", cid, err)
	}
	if _, err := st.ClassForPerson("s", 999); err == nil {
		t.Error("ClassForPerson for unknown person should error")
	}
}

func TestSnapshotTeacherColumn(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, err := st.ReplaceClassSnapshot("s", 1, []PeriodRow{
		{PeriodID: 7, Subject: "M", Room: "Aula", Teacher: "Müller", Start: "2026-09-07 08:00"},
	}, 1, "2026-09-07"); err != nil {
		t.Fatal(err)
	}
	rows, err := st.LoadClassSnapshot("s", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Teacher != "Müller" || rows[0].Room != "Aula" {
		t.Errorf("snapshot row = %+v, want teacher Müller/room Aula", rows)
	}
	pending, _, err := st.PendingChanges("s", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Teacher != "Müller" {
		t.Errorf("pending row = %+v, want teacher Müller", pending)
	}
}
