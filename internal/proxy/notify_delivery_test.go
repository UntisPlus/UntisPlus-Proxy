package proxy

// Outbound delivery (webhook + ntfy) and the change-detection poll loop.
// The receivers here are httptest servers, so the whole fan-out path —
// element matching, headers, HMAC signature, ntfy payload — is asserted
// end-to-end without touching the network.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"untis-proxy/internal/store"
)

type received struct {
	Path   string
	Header http.Header
	Body   []byte
}

func receiver(t *testing.T, ch chan received) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 0, 512)
		buf := make([]byte, 512)
		for {
			n, err := r.Body.Read(buf)
			body = append(body, buf[:n]...)
			if err != nil {
				break
			}
		}
		select {
		case ch <- received{Path: r.URL.Path, Header: r.Header.Clone(), Body: body}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func awaitRecv(t *testing.T, ch chan received) received {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("no delivery within 3s")
		return received{}
	}
}

func expectNoRecv(t *testing.T, ch chan received, what string) {
	t.Helper()
	select {
	case r := <-ch:
		t.Errorf("%s received a delivery it should not match: path=%s body=%s", what, r.Path, r.Body)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestPostWebhookSetsHeadersAndSignature(t *testing.T) {
	ch := make(chan received, 4)
	srv := receiver(t, ch)
	p, _ := newTestProxy(t)

	body := []byte(`{"event":"change","school":"testschool","classId":5000,"version":4}`)
	p.postWebhook(&store.Webhook{URL: srv.URL, Secret: "s3cret"}, body, "1 change(s): 1 added")

	r := awaitRecv(t, ch)
	if r.Path != "/" {
		t.Errorf("path = %q, want /", r.Path)
	}
	if got := r.Header.Get("X-Untis-Event"); got != "timetable-change" {
		t.Errorf("X-Untis-Event = %q, want timetable-change", got)
	}
	if got := r.Header.Get("X-Untis-Summary"); got != "1 change(s): 1 added" {
		t.Errorf("X-Untis-Summary = %q", got)
	}
	if got := r.Header.Get("X-Untis-Signature"); got == "" {
		t.Error("X-Untis-Signature missing for a secret-bearing webhook")
	} else {
		mac := hmac.New(sha256.New, []byte("s3cret"))
		mac.Write(body)
		if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); got != want {
			t.Errorf("signature = %q, want %q", got, want)
		}
	}
	if string(r.Body) != string(body) {
		t.Errorf("body = %s, want the change payload %s", r.Body, body)
	}
}

func TestPostWebhookWithoutSecretOmitstSignature(t *testing.T) {
	ch := make(chan received, 4)
	srv := receiver(t, ch)
	p, _ := newTestProxy(t)
	p.postWebhook(&store.Webhook{URL: srv.URL}, []byte(`{"event":"change"}`), "1 change(s)")
	if got := awaitRecv(t, ch).Header.Get("X-Untis-Signature"); got != "" {
		t.Errorf("X-Untis-Signature = %q, want empty without a shared secret", got)
	}
}

func TestPublishNtfyPayload(t *testing.T) {
	ch := make(chan received, 4)
	srv := receiver(t, ch)
	p, _ := newTestProxy(t)

	p.publishNtfy(&store.NtfyTopic{Topic: "klass5000", BaseURL: srv.URL + "/"}, "testschool", 5000,
		changeDigest{Title: "class 5000 · 2 changes", Summary: "2 changes", Lines: []string{"new: Tue 29.09. 08:00–08:45 · Mathe · R204"}})

	r := awaitRecv(t, ch)
	// The JSON envelope goes to the server ROOT; the topic travels in the body.
	// Posting it to /<topic> is accepted with 200 but stored as plain text, so
	// the notification would show the raw JSON instead of the digest.
	if r.Path != "/" {
		t.Errorf("path = %q, want the server root \"/\" with the topic in the body", r.Path)
	}
	var payload struct {
		Topic   string `json:"topic"`
		Title   string `json:"title"`
		Message string `json:"message"`
		Click   string `json:"click"`
		Tags    []string
	}
	if err := json.Unmarshal(r.Body, &payload); err != nil {
		t.Fatalf("bad ntfy json: %v (body=%s)", err, r.Body)
	}
	if payload.Topic != "klass5000" {
		t.Errorf("topic = %q", payload.Topic)
	}
	if payload.Title != "Timetable change — class 5000 · 2 changes" {
		t.Errorf("title = %q, want the digest title", payload.Title)
	}
	if payload.Message != "new: Tue 29.09. 08:00–08:45 · Mathe · R204" {
		t.Errorf("message = %q, want the digest body", payload.Message)
	}
	if payload.Click == "" {
		t.Error("click (deep link back into the app) missing")
	}
}

func TestPublishNtfyTitleNamesTheElement(t *testing.T) {
	ch := make(chan received, 4)
	srv := receiver(t, ch)
	p, st := newTestProxy(t)
	if err := st.SaveMasterNames("testschool", "TEACHER", map[int64]string{5009: "A. Hartley"}); err != nil {
		t.Fatalf("seed teacher name: %v", err)
	}
	p.publishNtfy(&store.NtfyTopic{Topic: "teacher1", BaseURL: srv.URL, ElementType: "TEACHER", ElementID: 5009},
		"testschool", 5000, changeDigest{Title: "class 5000 · 1 change", Summary: "1 change", Lines: []string{"new: Mathe"}})

	var payload struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(awaitRecv(t, ch).Body, &payload); err != nil {
		t.Fatalf("bad ntfy json: %v", err)
	}
	if payload.Title != "Timetable change — teacher A. Hartley" {
		t.Errorf("title = %q, want the teacher-named title", payload.Title)
	}
}

func TestTestWebhookAndTestNtfyReportReceiverStatus(t *testing.T) {
	ch := make(chan received, 4)
	srv := receiver(t, ch)
	p, _ := newTestProxy(t)

	code, err := p.testWebhook(&store.Webhook{URL: srv.URL, Secret: "x"})
	if err != nil || code != 200 {
		t.Fatalf("testWebhook = (%d, %v), want (200, nil)", code, err)
	}
	if got := awaitRecv(t, ch).Header.Get("X-Untis-Event"); got != "test" {
		t.Errorf("test delivery event header = %q, want test", got)
	}

	code, err = p.testNtfy(&store.NtfyTopic{Topic: "probe", BaseURL: srv.URL})
	if err != nil || code != 200 {
		t.Fatalf("testNtfy = (%d, %v), want (200, nil)", code, err)
	}
	if got := awaitRecv(t, ch).Path; got != "/" {
		t.Errorf("test ntfy path = %q, want the server root \"/\" with the topic in the body", got)
	}
}

func TestDeliverChangeFansOutOnlyToMatchingTargets(t *testing.T) {
	hookCh := make(chan received, 4)
	roomCh := make(chan received, 4)
	offCh := make(chan received, 4)
	ntfyCh := make(chan received, 4)
	hookSrv := receiver(t, hookCh)
	roomSrv := receiver(t, roomCh)
	offSrv := receiver(t, offCh)
	ntfySrv := receiver(t, ntfyCh)

	p, st := newTestProxy(t)
	if err := st.SaveMasterNames("testschool", "TEACHER", map[int64]string{5009: "A. Hartley"}); err != nil {
		t.Fatalf("seed teacher name: %v", err)
	}
	if _, err := st.AddWebhook(&store.Webhook{School: "testschool", ElementType: "TEACHER", ElementID: 5009,
		URL: hookSrv.URL, Enabled: true}); err != nil {
		t.Fatalf("add teacher webhook: %v", err)
	}
	if _, err := st.AddWebhook(&store.Webhook{School: "testschool", ElementType: "ROOM", ElementID: 169,
		URL: roomSrv.URL, Enabled: true}); err != nil {
		t.Fatalf("add room webhook: %v", err)
	}
	// a disabled subscription must never fire, even though it matches
	if _, err := st.AddWebhook(&store.Webhook{School: "testschool", ElementType: "TEACHER", ElementID: 5009,
		URL: offSrv.URL, Enabled: false}); err != nil {
		t.Fatalf("add disabled webhook: %v", err)
	}
	if _, err := st.AddNtfyTopic(&store.NtfyTopic{School: "testschool", Topic: "changes",
		BaseURL: ntfySrv.URL, Enabled: true}); err != nil {
		t.Fatalf("add ntfy topic: %v", err)
	}

	p.deliverChange("testschool", 5000, 7, []store.PeriodRow{
		{PeriodID: 10, Kind: "REMOVED", Teacher: "A. Hartley", Subject: "MATH"},
		{PeriodID: 11, Kind: "ADDED", Teacher: "A. Hartley", Subject: "MATH"},
	}, []store.PeriodRow{
		{PeriodID: 10, Teacher: "A. Hartley", Subject: "MATH"},
	})

	hook := awaitRecv(t, hookCh)
	var payload struct {
		Event   string `json:"event"`
		School  string `json:"school"`
		ClassID int64  `json:"classId"`
		Version int64  `json:"version"`
		Changes []store.PeriodRow
	}
	if err := json.Unmarshal(hook.Body, &payload); err != nil {
		t.Fatalf("bad webhook json: %v (body=%s)", err, hook.Body)
	}
	if payload.Event != "change" || payload.School != "testschool" || payload.ClassID != 5000 || payload.Version != 7 {
		t.Errorf("payload header = %+v, want change/testschool/5000/7", payload)
	}
	if len(payload.Changes) != 2 {
		t.Errorf("changes = %d, want 2", len(payload.Changes))
	}
	hdr := hook.Header.Get("X-Untis-Summary")
	if !strings.Contains(hdr, "1 added") || !strings.Contains(hdr, "1 removed") {
		t.Errorf("summary header = %q, want it to name both kinds of change", hdr)
	}
	if strings.ContainsAny(hdr, "\r\n") {
		t.Errorf("summary header must stay single-line, got %q", hdr)
	}

	// The ntfy envelope is posted to the receiver's root (see ntfyPost); the
	// target matching is asserted by the channels that must stay silent below.
	if r := awaitRecv(t, ntfyCh); r.Path != "/" {
		t.Errorf("ntfy path = %q, want the server root \"/\"", r.Path)
	}
	expectNoRecv(t, roomCh, "room 169 webhook (the changed lessons had no room 169)")
	expectNoRecv(t, offCh, "disabled teacher webhook")
}

func TestPollDiscoversClassChangeAndBumpsVersion(t *testing.T) {
	f := &fakeUpstream{}
	f.setTimetable(t, []map[string]any{{
		"id": 10, "startDateTime": "2026-09-27 08:00:00", "endDateTime": "2026-09-27 08:45:00",
		"subject": "MATH", "elements": []any{map[string]any{"id": 5009, "type": "TEACHER"}},
	}})
	p, st := newFakeProxyUpstream(t, f)
	if err := st.UpsertUser(&store.User{Username: "owen", School: "testschool", Method: "key",
		PersonType: 5, PersonID: 7, ClassID: 5000, ClassName: "10b", Password: "replayable"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	p.pollOnce("testschool")

	if v := st.ClassVersion("testschool", 5000); v != 1 {
		t.Errorf("ClassVersion = %d, want 1 after the first snapshot", v)
	}
	rows, ver, err := st.PendingChanges("testschool", 5000, 0)
	if err != nil {
		t.Fatalf("PendingChanges: %v", err)
	}
	if ver != 1 {
		t.Errorf("PendingChanges version = %d, want 1", ver)
	}
	if len(rows) != 1 || rows[0].Kind != "ADDED" || rows[0].PeriodID != 10 {
		t.Errorf("snapshot rows = %+v, want one ADDED row for period 10", rows)
	}
	// the poll must actually reach upstream (fresh fetch, not a cached answer)
	found := false
	for _, fw := range f.Forwards() {
		if fw.Method == "getTimetable2017" {
			found = true
		}
	}
	if !found {
		t.Errorf("no getTimetable2017 forward recorded, forwards = %d", len(f.Forwards()))
	}

	// polling again with unchanged data must not create a new version
	p.pollOnce("testschool")
	if v := st.ClassVersion("testschool", 5000); v != 1 {
		t.Errorf("ClassVersion = %d after an unchanged poll, want 1", v)
	}
}

func TestStartPollLoopRunsImmediatePassAndStops(t *testing.T) {
	f := &fakeUpstream{}
	f.setTimetable(t, []map[string]any{{
		"id": 11, "startDateTime": "2026-09-28 09:00:00", "endDateTime": "2026-09-28 09:45:00",
		"subject": "BIO",
	}})
	p, st := newFakeProxyUpstream(t, f)
	if err := st.UpsertUser(&store.User{Username: "owen", School: "testschool", Method: "key",
		PersonType: 5, PersonID: 7, ClassID: 5000, Password: "replayable"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		p.StartPollLoop("testschool", time.Hour, done)
		close(stopped)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for st.ClassVersion("testschool", 5000) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("StartPollLoop did not run its immediate pass")
		}
		time.Sleep(5 * time.Millisecond)
	}

	close(done)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("StartPollLoop did not return after done was closed")
	}
}

func TestCheckClassWithoutOwnerSkips(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	// a session-only account (no replayable credential) can never own a class
	if err := st.UpsertUser(&store.User{Username: "evan", School: "testschool", Method: "session",
		PersonType: 5, PersonID: 9, ClassID: 4421}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if p.checkClass("testschool", 4421) {
		t.Error("checkClass reported a change for a class with no replayable owner")
	}
	if v := st.ClassVersion("testschool", 4421); v != 0 {
		t.Errorf("ClassVersion = %d, want 0 (nothing was stored)", v)
	}
	if n := len(f.Forwards()); n != 0 {
		t.Errorf("upstream forwards = %d, want 0 (no owner -> no fetch)", n)
	}
}

func TestSetNtfyBaseOverridesDefaultServer(t *testing.T) {
	old := ntfyBase
	defer SetNtfyBase(old)
	SetNtfyBase("https://ntfy.example.org/")
	if ntfyBase != "https://ntfy.example.org" {
		t.Errorf("ntfyBase = %q, want the trimmed override", ntfyBase)
	}
	if got := ntfyBaseURL(&store.NtfyTopic{Topic: "t"}); got != "https://ntfy.example.org" {
		t.Errorf("default topic base = %q, want the global override", got)
	}
	if got := ntfyBaseURL(&store.NtfyTopic{Topic: "t", BaseURL: "https://own.example.org/"}); got != "https://own.example.org" {
		t.Errorf("per-topic base = %q, want the topic override (trailing slash trimmed)", got)
	}
}

func TestKlassesExpired(t *testing.T) {
	p, _ := newTestProxy(t)
	if p.klassesExpired(time.Now()) {
		t.Error("klasses fetched just now must not be expired")
	}
	if !p.klassesExpired(time.Now().Add(-2 * time.Hour)) {
		t.Error("klasses fetched 2h ago must be expired (TTL is one hour)")
	}
	if !p.klassesExpired(time.Now().Add(-61 * time.Minute)) {
		t.Error("klasses fetched 61min ago must be expired")
	}
	if p.klassesExpired(time.Now().Add(-59 * time.Minute)) {
		t.Error("klasses fetched 59min ago must still be fresh")
	}
}

func TestEnsureReconScanIsNoopWithoutPool(t *testing.T) {
	p, st := newTestProxy(t)
	p.ensureReconScan("zweitschule") // empty pool: nothing to reconstruct
	p.schoolsMu.Lock()
	active := p.reconActive
	p.schoolsMu.Unlock()
	if len(active) != 0 {
		t.Errorf("reconActive = %v, want no scan for an empty school", active)
	}
	// the default school's recon is owned by StartRecon at boot
	if err := st.UpsertUser(&store.User{Username: "dee", ClassID: 5000, Password: "pw"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	p.ensureReconScan(p.opts.School)
	p.schoolsMu.Lock()
	active = p.reconActive
	p.schoolsMu.Unlock()
	if len(active) != 0 {
		t.Errorf("reconActive = %v, want the default school left to StartRecon", active)
	}
}

func TestHandleAdminLoginRedirectsAdmins(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertUser(&store.User{Username: "bob", Admin: true}); err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	// anonymous -> the OTP login page
	req := httptest.NewRequest(http.MethodGet, "/admin/login", nil)
	rec := httptest.NewRecorder()
	p.handleAdminLogin(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("content-type = %q, want the login page html", ct)
	}
	if len(rec.Body.Bytes()) == 0 {
		t.Error("admin login page is empty")
	}

	// logged-in admin -> straight to the dashboard
	req = httptest.NewRequest(http.MethodGet, "/admin/login", nil)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: p.sessions.New("bob", 0).ID})
	rec = httptest.NewRecorder()
	p.handleAdminLogin(rec, req)
	if rec.Code != http.StatusFound {
		t.Errorf("admin status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/admin" {
		t.Errorf("Location = %q, want /admin", loc)
	}

	// logged-in non-admin -> the page again (the JSON-RPC login decides)
	if err := st.UpsertUser(&store.User{Username: "dee"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	req = httptest.NewRequest(http.MethodGet, "/admin/login", nil)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: p.sessions.New("dee", 0).ID})
	rec = httptest.NewRecorder()
	p.handleAdminLogin(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("non-admin status = %d, want 200 (login page, not a redirect)", rec.Code)
	}
}
