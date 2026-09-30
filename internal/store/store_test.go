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
