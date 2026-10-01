package proxy

// Change-notification layer: the SSE hub, row conversion, summary rendering,
// topic matching, and the two /api/timetable endpoints (poll diff + SSE stream).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"untis-proxy/internal/store"
)

func TestHubKeyAndSubscribePublish(t *testing.T) {
	if got := hubKey("testschool", 5000); got != "testschool|5000" {
		t.Errorf("hubKey = %q", got)
	}
	h := newNotifyHub()
	ch, unsub := h.subscribe("testschool", 5000)
	defer unsub()

	want := notifyMsg{School: "testschool", ClassID: 5000, Version: 7, Changes: []store.PeriodRow{{PeriodID: 1}}}
	h.publish("testschool", 5000, want)
	select {
	case got := <-ch:
		if got.Version != 7 || len(got.Changes) != 1 {
			t.Errorf("published msg = %+v, want v7 with 1 change", got)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber never received the published change")
	}

	// publishers are keyed per (school,class): a different class does not receive it
	h.publish("testschool", 9999, want)
	select {
	case got := <-ch:
		t.Errorf("received mis-keyed msg %+v", got)
	case <-time.After(30 * time.Millisecond):
	}

	// unsubscribing stops delivery and closes the channel
	unsub()
	h.publish("testschool", 5000, want)
	time.Sleep(20 * time.Millisecond)
	if _, ok := <-ch; ok {
		t.Error("channel not closed after unsubscribe")
	}
}

func TestHubFanoutToMultipleSubscribers(t *testing.T) {
	h := newNotifyHub()
	ch1, unA := h.subscribe("testschool", 1)
	ch2, unB := h.subscribe("testschool", 1)
	defer unA()
	defer unB()
	h.publish("testschool", 1, notifyMsg{Version: 3})
	for i, ch := range []chan notifyMsg{ch1, ch2} {
		select {
		case m := <-ch:
			if m.Version != 3 {
				t.Errorf("subscriber %d got version %d", i, m.Version)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d missed the fanout", i)
		}
	}
}

func TestPeriodRowBuildsSnapshotRow(t *testing.T) {
	md := &masterDataCache{
		teachers: map[int64]string{5009: "A. Hartley"},
		rooms:    map[int64]string{169: "B.112"},
		subjects: map[int64]string{1: "Englisch"},
		klassen:  map[int64]string{},
	}
	pd := map[string]any{
		"id":            float64(42),
		"startDateTime": "2026-09-21T08:00:00+02:00",
		"endDateTime":   "2026-09-21T08:45:00+02:00",
		"elements": []any{
			map[string]any{"type": "SUBJECT", "id": float64(1)},
			map[string]any{"type": "ROOM", "id": float64(169)},
			map[string]any{"type": "TEACHER", "id": float64(5009)},
		},
	}
	row := periodRow(pd, md)
	if row.PeriodID != 42 || row.Start != "2026-09-21T08:00:00+02:00" || row.Subject != "Englisch" ||
		row.Room != "B.112" || row.Teacher != "A. Hartley" {
		t.Errorf("row = %+v, want resolved names", row)
	}
}

func TestPeriodRowTextFallbacks(t *testing.T) {
	md := &masterDataCache{teachers: map[int64]string{}, rooms: map[int64]string{}, subjects: map[int64]string{}, klassen: map[int64]string{}}
	pd := map[string]any{
		"id":            float64(1),
		"startDateTime": "2026-09-21T08:00:00+02:00",
		"endDateTime":   "2026-09-21T08:45:00+02:00",
		"elements":      []any{},
		"text": map[string]any{
			"subject":      "Sport",
			"substitution": "Vertretung Erasmus",
			"info":         "frei",
		},
	}
	row := periodRow(pd, md)
	if row.Subject != "Sport" {
		t.Errorf("subject = %q, want fallback from text", row.Subject)
	}
	if !strings.Contains(row.Description, "Vertretung Erasmus") || !strings.Contains(row.Description, "frei") {
		t.Errorf("description = %q, want substitution + info", row.Description)
	}
}

func TestSummarize(t *testing.T) {
	cases := []struct {
		name string
		rows []store.PeriodRow
		want string
	}{
		{"empty", nil, "0 timetable change(s)"},
		{"added", []store.PeriodRow{{Kind: "ADDED"}}, "1 change(s): 1 added"},
		{"removed", []store.PeriodRow{{Kind: "REMOVED"}}, "1 change(s): 1 removed"},
		{"changed", []store.PeriodRow{{Kind: "CHANGED"}, {Kind: "NEW"}}, "2 change(s): 2 changed"},
		{"mixed", []store.PeriodRow{{Kind: "ADDED"}, {Kind: "REMOVED"}, {Kind: "CHANGED"}}, "3 change(s): 1 added · 1 removed · 1 changed"},
	}
	for _, c := range cases {
		if got := summarize(c.rows); got != c.want {
			t.Errorf("summarize(%s) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestNtfyBaseURL(t *testing.T) {
	if got := ntfyBaseURL(&store.NtfyTopic{BaseURL: "https://n.example.org/"}); got != "https://n.example.org" {
		t.Errorf("topic override = %q", got)
	}
	if got := ntfyBaseURL(&store.NtfyTopic{}); got != ntfyBase {
		t.Errorf("default base = %q, want %q", got, ntfyBase)
	}
	if got := ntfyBaseURL(nil); got != ntfyBase {
		t.Errorf("nil topic base = %q", got)
	}
}

func seedNames(t *testing.T, st *store.Store) {
	t.Helper()
	if err := st.SaveMasterNames("testschool", "TEACHER", map[int64]string{5009: "A. Hartley"}); err != nil {
		t.Fatalf("seed teacher name: %v", err)
	}
	if err := st.SaveMasterNames("testschool", "ROOM", map[int64]string{169: "B.112"}); err != nil {
		t.Fatalf("seed room name: %v", err)
	}
	if err := st.SaveMasterNames("testschool", "SUBJECT", map[int64]string{1: "Englisch"}); err != nil {
		t.Fatalf("seed subject name: %v", err)
	}
}

func TestElementMatches(t *testing.T) {
	p, st := newTestProxy(t)
	seedNames(t, st)
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", PersonType: 5, PersonID: 7, ClassID: 5000}); err != nil {
		t.Fatalf("seed student: %v", err)
	}
	rows := []store.PeriodRow{{Teacher: "A. Hartley", Room: "B.112", Subject: "Englisch"}}

	// Matching is driven by identities resolved up front, because it has to run
	// inside the snapshot transaction where the store cannot be queried. Resolve
	// them the way the real caller does, from actual subscriptions.
	hooks := []*store.Webhook{
		{School: "testschool", ElementType: "TEACHER", ElementID: 5009},
		{School: "testschool", ElementType: "TEACHER", ElementID: 112}, // unknown teacher
		{School: "testschool", ElementType: "ROOM", ElementID: 169},
		{School: "testschool", ElementType: "SUBJECT", ElementID: 1},
		{School: "testschool", ElementType: "STUDENT", ElementID: 7},
		{School: "testschool", ElementType: "STUDENT", ElementID: 99}, // unknown person
	}
	targets := p.resolveTargets("testschool", hooks, nil)

	if !p.elementMatches("testschool", "", 0, 5000, rows, targets) {
		t.Error("school-wide topic must match everything")
	}
	if !p.elementMatches("testschool", "ALL", 0, 5000, rows, targets) {
		t.Error("ALL topic must match")
	}
	if !p.elementMatches("testschool", "CLASS", 5000, 5000, rows, targets) {
		t.Error("CLASS topic should match the changed class id")
	}
	if p.elementMatches("testschool", "CLASS", 9999, 5000, rows, targets) {
		t.Error("CLASS topic must not match a different class")
	}
	if !p.elementMatches("testschool", "TEACHER", 5009, 5000, rows, targets) {
		t.Error("TEACHER topic should match A. Hartley appearing in a row")
	}
	if p.elementMatches("testschool", "TEACHER", 112, 5000, rows, targets) {
		t.Error("TEACHER topic must not match an unrelated teacher")
	}
	if !p.elementMatches("testschool", "ROOM", 169, 5000, rows, targets) {
		t.Error("ROOM topic should match B.112")
	}
	if !p.elementMatches("testschool", "SUBJECT", 1, 5000, rows, targets) {
		t.Error("SUBJECT topic should match Englisch")
	}
	if !p.elementMatches("testschool", "STUDENT", 7, 5000, rows, targets) {
		t.Error("STUDENT topic should match a student in the changed class")
	}
	if p.elementMatches("testschool", "STUDENT", 99, 5000, rows, targets) {
		t.Error("STUDENT topic must not match an unrelated person")
	}
}

func TestHandleTimetableChangesGates(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", PersonType: 5, PersonID: 7, ClassID: 5000}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	// no session -> 401
	req := httptest.NewRequest(http.MethodGet, "/api/timetable/changes?school=testschool&classId=5000", nil)
	rec := httptest.NewRecorder()
	p.handleTimetableChanges(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/timetable/changes?school=testschool&classId=9999", nil)
	sess := p.sessions.New("dee", 0)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: sess.ID})
	rec = httptest.NewRecorder()
	p.handleTimetableChanges(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign class status = %d, want 403", rec.Code)
	}

	// own class with nothing pending -> 304
	req = httptest.NewRequest(http.MethodGet, "/api/timetable/changes?school=testschool", nil)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: sess.ID})
	rec = httptest.NewRecorder()
	p.handleTimetableChanges(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("no-change status = %d, want 304 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestHandleTimetableChangesPending(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", PersonType: 5, PersonID: 7, ClassID: 5000}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	// simulate one poll: new rows at version 1
	_, err := st.ReplaceClassSnapshot("testschool", 5000, []store.PeriodRow{{PeriodID: 10, Start: "2026-09-21", Subject: "Englisch"}}, 1, "2026-09-21")
	if err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/timetable/changes?school=testschool", nil)
	sess := p.sessions.New("dee", 0)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: sess.ID})
	rec := httptest.NewRecorder()
	p.handleTimetableChanges(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Current int64             `json:"current"`
		Changes []store.PeriodRow `json:"changes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if out.Current != 1 || len(out.Changes) != 1 || out.Changes[0].PeriodID != 10 {
		t.Errorf("changes response = %+v, want v1 with period 10", out)
	}
}

// lockedWriter is a concurrency-safe ResponseWriter for exercising the SSE
// handler (which writes from a goroutine while the test polls the stream).
type lockedWriter struct {
	mu   sync.Mutex
	code int
	hdr  http.Header
	body bytes.Buffer
}

func newLockedWriter() *lockedWriter {
	return &lockedWriter{hdr: http.Header{}, code: http.StatusOK}
}

func (l *lockedWriter) Header() http.Header { return l.hdr }
func (l *lockedWriter) WriteHeader(code int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.code = code
}
func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.body.Write(p)
}
func (l *lockedWriter) Flush() {} // http.Flusher
func (l *lockedWriter) Bytes() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	cp := make([]byte, l.body.Len())
	copy(cp, l.body.Bytes())
	return cp
}

func waitForBytes(t *testing.T, lw *lockedWriter, needle []byte) []byte {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		b := lw.Bytes()
		if bytes.Contains(b, needle) || time.Now().After(deadline) {
			return b
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestHandleTimetableStreamUnauthorized(t *testing.T) {
	p, _ := newTestProxy(t)
	req := httptest.NewRequest(http.MethodGet, "/api/timetable/stream", nil)
	rec := httptest.NewRecorder()
	p.handleTimetableStream(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHandleTimetableStreamSnapshotThenChange(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "dee", Method: "key", PersonType: 5, PersonID: 7, ClassID: 5000}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/timetable/stream?school=testschool", nil).WithContext(ctx)
	sess := p.sessions.New("dee", 0)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: sess.ID})
	rec := newLockedWriter()

	done := make(chan struct{})
	go func() {
		p.handleTimetableStream(rec, req)
		close(done)
	}()

	// the handler must emit the initial snapshot before blocking on the hub
	if got := waitForBytes(t, rec, []byte("event: snapshot")); !bytes.Contains(got, []byte("event: snapshot")) {
		t.Fatalf("no snapshot event after 3s; body = %s", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}

	// publish a change for this class -> snapshot should be followed by a change event
	p.hub.publish("testschool", 5000, notifyMsg{School: "testschool", ClassID: 5000, Version: 2,
		Changes: []store.PeriodRow{{PeriodID: 10, Kind: "ADDED"}}})
	if got := waitForBytes(t, rec, []byte("event: change")); !bytes.Contains(got, []byte("event: change")) {
		t.Fatalf("no change event after publish; body = %s", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream handler did not return after context cancel")
	}
}

// TestUpstreamOutageDoesNotWipeSnapshot: a school-server error must never be
// read as "this class has no periods". Treating it as an empty timetable marks
// every cached period REMOVED, bumps the version and fires a change storm at
// every subscriber — the proxy used to do exactly that on any non-2xx answer.
func TestUpstreamOutageDoesNotWipeSnapshot(t *testing.T) {
	f := &fakeUpstream{}
	periods := []map[string]any{reconPeriod(10, "2026-09-28", "08:00", "08:45", 5009, 169, 7)}
	f.setTimetable(t, periods)
	p, st := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st, "owen", 7, 5000)

	if !p.checkClass("testschool", 5000) {
		t.Fatal("first poll found no change, expected the initial snapshot")
	}
	snap, err := st.LoadClassSnapshot("testschool", 5000)
	if err != nil {
		t.Fatalf("LoadClassSnapshot: %v", err)
	}
	if len(snap) != 1 || snap[0].Kind != "ADDED" {
		t.Fatalf("snapshot after first poll = %+v, want one ADDED period", snap)
	}
	ver := st.ClassVersion("testschool", 5000)

	// The school server starts failing.
	f.setTimetableStatus(http.StatusBadGateway)
	if changed, ferr := p.checkClassErr("testschool", 5000); changed || ferr == nil {
		t.Errorf("outage poll = (changed %v, err %v), want (false, non-nil)", changed, ferr)
	}
	snap, err = st.LoadClassSnapshot("testschool", 5000)
	if err != nil {
		t.Fatalf("LoadClassSnapshot after outage: %v", err)
	}
	if len(snap) != 1 || snap[0].Kind != "ADDED" {
		t.Errorf("outage changed the snapshot to %+v, want the cached period untouched", snap)
	}
	if got := st.ClassVersion("testschool", 5000); got != ver {
		t.Errorf("class version = %d, want %d (unchanged by an outage)", got, ver)
	}

	// Recovery: the same data must not be reported as a change either.
	f.setTimetableStatus(0)
	if changed := p.checkClass("testschool", 5000); changed {
		snap, _ = st.LoadClassSnapshot("testschool", 5000)
		t.Errorf("recovery reported a change: %+v", snap)
	}
}

// TestUpstreamJSONRPCErrorDoesNotWipeSnapshot: a 200 answer carrying a JSON-RPC
// error is equally unusable data and must not be parsed as an empty timetable.
func TestUpstreamJSONRPCErrorDoesNotWipeSnapshot(t *testing.T) {
	f := &fakeUpstream{}
	f.setTimetable(t, []map[string]any{reconPeriod(10, "2026-09-28", "08:00", "08:45", 5009, 169, 7)})
	p, st := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st, "owen", 7, 5000)
	if !p.checkClass("testschool", 5000) {
		t.Fatal("first poll found no change")
	}

	f.setTimetable(t, nil) // no canned answer: the fake serves an empty result
	f.mu.Lock()
	f.timetable = `{"jsonrpc":"2.0","id":"upstream","error":{"code":-32603,"message":"internal error"}}`
	f.mu.Unlock()

	if changed, err := p.checkClassErr("testschool", 5000); changed || err == nil {
		t.Errorf("jsonrpc error poll = (changed %v, err %v), want (false, non-nil)", changed, err)
	}
	snap, _ := st.LoadClassSnapshot("testschool", 5000)
	if len(snap) != 1 || snap[0].Kind != "ADDED" {
		t.Errorf("jsonrpc error changed the snapshot to %+v", snap)
	}
}
