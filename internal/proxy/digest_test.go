package proxy

// The human-readable change digest: what a subscriber actually reads in a push
// notification or the X-Untis-Summary header, and what makes it a diff rather
// than a count.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"untis-proxy/internal/store"
)

func TestDigestDescribesAMovedRoom(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.SaveMasterNames("testschool", "CLASS", map[int64]string{5000: "10b"}); err != nil {
		t.Fatalf("seed class name: %v", err)
	}
	rows := []store.PeriodRow{
		{PeriodID: 10, Kind: "CHANGED", Start: "2026-09-29T08:00:00Z", End: "2026-09-29T08:45:00Z",
			Subject: "Mathe", Room: "R112", Teacher: "Müller", ModVer: 4},
	}
	prev := []store.PeriodRow{
		{PeriodID: 10, Kind: "UNCHANGED", Start: "2026-09-29T08:00:00Z", End: "2026-09-29T08:45:00Z",
			Subject: "Mathe", Room: "R204", Teacher: "Müller", ModVer: 3},
	}
	d := p.buildDigest("testschool", 5000, prev, rows)
	if d.Changed != 1 || d.Added != 0 || d.Removed != 0 {
		t.Errorf("counts = %+v, want one CHANGED", d)
	}
	if d.Title != "10b · 1 change" {
		t.Errorf("title = %q, want %q", d.Title, "10b · 1 change")
	}
	if !strings.Contains(d.Message(), "room R204 → R112") {
		t.Errorf("message does not name the room move: %q", d.Message())
	}
	for _, want := range []string{"Tue 29.09. 08:00–08:45", "Mathe", "Müller"} {
		if !strings.Contains(d.Message(), want) {
			t.Errorf("message %q missing %q", d.Message(), want)
		}
	}
	if strings.Contains(d.Message(), "room R112") {
		t.Errorf("an unchanged room should not be repeated: %q", d.Message())
	}
}

func TestDigestCoversEveryChangeKind(t *testing.T) {
	p, _ := newTestProxy(t)
	rows := []store.PeriodRow{
		{PeriodID: 1, Kind: "ADDED", Start: "2026-09-29T10:00:00Z", End: "2026-09-29T10:45:00Z",
			Subject: "Deutsch", Room: "R7", Description: "Substitution: Frau Kruse"},
		{PeriodID: 2, Kind: "REMOVED", Start: "2026-09-30T09:00:00Z", End: "2026-09-30T09:45:00Z",
			Subject: "Sport", Room: "Turnhalle"},
		{PeriodID: 3, Kind: "CHANGED", Start: "2026-09-30T11:00:00Z", End: "2026-09-30T11:45:00Z",
			Subject: "Englisch", Room: "R3", Teacher: "Smith"},
	}
	prev := []store.PeriodRow{
		{PeriodID: 3, Start: "2026-09-30T11:00:00Z", End: "2026-09-30T11:45:00Z",
			Subject: "Englisch", Room: "R9", Teacher: "Jones"},
	}
	d := p.buildDigest("testschool", 5000, prev, rows)
	if d.Added != 1 || d.Removed != 1 || d.Changed != 1 {
		t.Errorf("counts = %+v, want one of each kind", d)
	}
	// The header is transliterated to plain ASCII, so the separators are "-".
	if d.Summary != "class 5000 - 3 changes: 1 added - 1 removed - 1 changed" {
		t.Errorf("summary = %q", d.Summary)
	}
	msg := d.Message()
	for _, want := range []string{
		"new: Tue 29.09. 10:00–10:45 · Deutsch · R7 · Substitution: Frau Kruse",
		"removed: Wed 30.09. 09:00–09:45 · Sport · Turnhalle",
		"room R9 → R3", "teacher Jones → Smith",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
}

func TestDigestDetectsAMovedSlot(t *testing.T) {
	p, _ := newTestProxy(t)
	rows := []store.PeriodRow{{PeriodID: 5, Kind: "CHANGED",
		Start: "2026-10-01T14:00:00Z", End: "2026-10-01T14:45:00Z", Subject: "Mathe"}}
	prev := []store.PeriodRow{{PeriodID: 5,
		Start: "2026-09-30T14:00:00Z", End: "2026-09-30T14:45:00Z", Subject: "Mathe"}}
	d := p.buildDigest("testschool", 5000, prev, rows)
	if !strings.Contains(d.Message(), "moved from Wed 30.09. 14:00–14:45") {
		t.Errorf("message does not mention the old slot: %q", d.Message())
	}
}

// TestDigestWithoutPreviousSnapshot: the changes API has no old values, so the
// digest must still be useful instead of rendering empty fragments.
func TestDigestWithoutPreviousSnapshot(t *testing.T) {
	p, _ := newTestProxy(t)
	d := p.buildDigest("testschool", 5000, nil, []store.PeriodRow{
		{PeriodID: 1, Kind: "ADDED", Start: "2026-09-29T08:00:00Z", End: "2026-09-29T08:45:00Z", Subject: "Mathe", Room: "R1"},
	})
	if got := d.Message(); strings.Contains(got, "→") || strings.Contains(got, " · · ") {
		t.Errorf("diff-less digest should have no arrows or empty parts: %q", got)
	}
	if d.Summary == "" || d.Title == "" {
		t.Errorf("title/summary missing: %+v", d)
	}
}

// TestDigestTruncatesLongEvents: a mass cancellation must not produce a
// thousand-line push message.
func TestDigestTruncatesLongEvents(t *testing.T) {
	p, _ := newTestProxy(t)
	var rows []store.PeriodRow
	for i := 0; i < 30; i++ {
		rows = append(rows, store.PeriodRow{PeriodID: int64(i), Kind: "REMOVED",
			Start: "2026-09-29T08:00:00Z", End: "2026-09-29T08:45:00Z", Subject: "Mathe"})
	}
	d := p.buildDigest("testschool", 5000, nil, rows)
	msg := d.Message()
	// 8 spelled-out lessons plus the "... and N more" footer.
	if n := strings.Count(msg, "\n"); n != digestMaxLines {
		t.Errorf("message has %d line breaks, want %d", n, digestMaxLines)
	}
	if !strings.Contains(msg, "and 22 more") {
		t.Errorf("message does not report the remainder: %q", msg)
	}
	if !strings.Contains(d.Summary, "30 removed") {
		t.Errorf("summary = %q, want the full removal count", d.Summary)
	}
}

// TestDigestHeaderIsASCII: HTTP header values must not carry raw umlauts or
// newlines, or receivers see mojibake / a broken header.
func TestDigestHeaderIsASCII(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.SaveMasterNames("testschool", "CLASS", map[int64]string{5000: "Größe"}); err != nil {
		t.Fatalf("seed class name: %v", err)
	}
	d := p.buildDigest("testschool", 5000, nil, []store.PeriodRow{
		{PeriodID: 1, Kind: "ADDED", Start: "2026-09-29T08:00:00Z", End: "2026-09-29T08:45:00Z",
			Subject: "Deutsch", Room: "Räume 3", Teacher: "Müller", Description: "Info: Schulfest"},
	})
	for _, r := range []byte(d.Summary) {
		if r > 127 {
			t.Fatalf("summary header is not ASCII: %q", d.Summary)
		}
	}
	if strings.ContainsAny(d.Summary, "\r\n") {
		t.Errorf("summary header must be single-line: %q", d.Summary)
	}
	if !strings.Contains(d.Summary, "Groesse") {
		t.Errorf("umlauts were not transliterated: %q", d.Summary)
	}
	if got := asciiHeader("a\tb\nc\u00e4\u00d6\u00fc\u00df\u2192\u2013"); strings.ContainsAny(got, "\t\n\u00e4\u00d6\u00fc\u00df\u2192\u2013") {
		t.Errorf("asciiHeader left unsafe characters: %q", got)
	}
	// The message body keeps the readable text.
	if !strings.Contains(d.Message(), "Müller") {
		t.Errorf("message should keep the original spelling: %q", d.Message())
	}
}

// TestNtfyMessageCarriesTheDigest: end to end, a real poll must produce a push
// message that names the change rather than counting it.
func TestNtfyMessageCarriesTheDigest(t *testing.T) {
	ch := make(chan received, 8)
	srv := receiver(t, ch)

	tomorrow := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	day := func(hour string) string { return tomorrow + "T" + hour + ":00:00Z" }
	f := &fakeUpstream{userDataBody: `{"jsonrpc":"2.0","id":"upstream","result":{
		"sessionId":"upstreamsid","personId":7,"personType":5,"klasseId":5000,
		"masterData":{"timeStamp":1,
			"klassen":[{"id":5000,"name":"10b"}],
			"teachers":[{"id":1,"name":"A. Hartley"}],
			"rooms":[{"id":169,"name":"R204"},{"id":321,"name":"R112"}],
			"subjects":[{"id":7,"longName":"Mathe"}]}}}`}
	f.setTimetable(t, []map[string]any{
		{"id": 10, "startDateTime": day("08:00"), "endDateTime": day("08:45"),
			"elements": []any{map[string]any{"id": 7, "type": "SUBJECT"},
				map[string]any{"id": 1, "type": "TEACHER"},
				map[string]any{"id": 169, "type": "ROOM"}}},
	})
	p2, st2 := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st2, "owen", 100, 5000)
	if _, err := st2.AddNtfyTopic(&store.NtfyTopic{School: "testschool", Topic: "klass5000", BaseURL: srv.URL, Enabled: true}); err != nil {
		t.Fatalf("add topic: %v", err)
	}

	if !p2.checkClass("testschool", 5000) {
		t.Fatal("first poll found no change")
	}
	// Delivery is queued durably; the outbox worker is what sends it.
	p2.drainOutbox()
	// The initial snapshot is itself an ADDED change and pushes a notification;
	// the interesting one is the room move below.
	first := awaitRecv(t, ch)
	if !strings.Contains(string(first.Body), "R204") {
		t.Fatalf("first push should describe the new lesson, body=%s", first.Body)
	}
	// Move the lesson to another room.
	f.setTimetable(t, []map[string]any{
		{"id": 10, "startDateTime": day("08:00"), "endDateTime": day("08:45"),
			"elements": []any{map[string]any{"id": 7, "type": "SUBJECT"},
				map[string]any{"id": 1, "type": "TEACHER"},
				map[string]any{"id": 321, "type": "ROOM"}}},
	})
	if !p2.checkClass("testschool", 5000) {
		t.Fatal("room move was not detected as a change")
	}
	p2.drainOutbox()

	r := awaitRecv(t, ch)
	var payload struct {
		Title   string `json:"title"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(r.Body, &payload); err != nil {
		t.Fatalf("bad ntfy json: %v", err)
	}
	if !strings.Contains(payload.Message, "room") {
		t.Errorf("push message does not describe the move: %q", payload.Message)
	}
	if !strings.Contains(payload.Message, "R112") {
		t.Errorf("push message does not name the new room: %q", payload.Message)
	}
	if strings.Contains(payload.Message, "change(s)") {
		t.Errorf("push message still uses the old count-only text: %q", payload.Message)
	}
}

// TestChangesAPIIncludesDigest: the polling API carries the same digest so the
// app can render "what changed" without reimplementing the diff.
func TestChangesAPIIncludesDigest(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", School: "testschool",
		PersonType: 5, PersonID: 5005, ClassID: 5000, Password: "x"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := st.SaveMasterNames("testschool", "CLASS", map[int64]string{5000: "10b"}); err != nil {
		t.Fatalf("seed class name: %v", err)
	}
	if _, err := st.ReplaceClassSnapshot("testschool", 5000, []store.PeriodRow{
		{PeriodID: 10, Start: "2026-09-29T08:00:00Z", End: "2026-09-29T08:45:00Z", Subject: "Mathe", Room: "R1"},
	}, 1, ""); err != nil {
		t.Fatalf("ReplaceClassSnapshot: %v", err)
	}
	sess := p.sessions.New("dee", 0)
	req := httptest.NewRequest(http.MethodGet, "/api/timetable/changes?school=testschool&classId=5000&since=0", nil)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: sess.ID})
	rec := httptest.NewRecorder()
	p.handleTimetableChanges(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Summary string       `json:"summary"`
		Digest  changeDigest `json:"digest"`
		Changes []store.PeriodRow
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if out.Summary == "" {
		t.Error("summary missing from the changes answer")
	}
	if len(out.Digest.Lines) != len(out.Changes) {
		t.Errorf("digest has %d lines for %d changes", len(out.Digest.Lines), len(out.Changes))
	}
	if out.Digest.Title != "10b · 1 change" {
		t.Errorf("digest title = %q", out.Digest.Title)
	}
	if !strings.Contains(out.Summary, "10b") {
		t.Errorf("summary = %q, want it to name the class", out.Summary)
	}
}
