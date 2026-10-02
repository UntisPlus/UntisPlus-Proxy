package proxy

// Tests for the admin event API and the per-student change signal.
//
// The two things worth guarding here are that a non-admin cannot reach the API at
// all, and that a change to one student's events is never delivered to another
// student — including through the class-wide webhook and ntfy fan-out.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"untis-proxy/internal/store"
)

// adminEventProxy sets up a proxy with an admin account, a student and a classmate.
func adminEventProxy(t *testing.T) (*Proxy, *store.Store) {
	t.Helper()
	p, st, _ := newFakeProxy(t, &fakeUntis{})
	for _, u := range []*store.User{
		{Username: "adm", School: "testschool", PersonID: 1, PersonType: 1, Admin: true},
		{Username: "dee", School: "testschool", PersonID: 9, PersonType: 5, ClassID: 5000},
		{Username: "sam", School: "testschool", PersonID: 10, PersonType: 5, ClassID: 5000},
	} {
		if err := st.UpsertUser(u); err != nil {
			t.Fatalf("seed %s: %v", u.Username, err)
		}
	}
	return p, st
}

// doAdmin sends an admin API request as the given user, through the real router so
// the registered path patterns and the id wildcard are exercised too.
func doAdmin(t *testing.T, p *Proxy, user, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	if user != "" {
		req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: p.sessions.New(user, 0).ID})
	}
	req.SetPathValue("id", idFromPath(target))
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)
	return rec
}

// idFromPath pulls the trailing path segment an event id would sit in.
func idFromPath(target string) string {
	path := target
	if i := strings.Index(path, "?"); i >= 0 {
		path = path[:i]
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	last := parts[len(parts)-1]
	if last == "events" || !strings.Contains(path, "/events/") {
		return ""
	}
	return last
}

func decodeEvent(t *testing.T, rec *httptest.ResponseRecorder) studentEventPayload {
	t.Helper()
	var out studentEventPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not an event: %v\n%s", err, rec.Body.String())
	}
	return out
}

func TestAdminCreatesStudentEvent(t *testing.T) {
	p, st := adminEventProxy(t)
	rec := doAdmin(t, p, "adm", http.MethodPost, "/api/admin/events",
		`{"username":"dee","date":"2026-10-01","startTime":"14:00","endTime":"15:00","title":"Technik","room":"R12"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	ev := decodeEvent(t, rec)
	if ev.Username != "dee" || ev.Title != "Technik" || ev.Revision != 1 || ev.CreatedBy != "adm" {
		t.Errorf("created event = %+v", ev)
	}
	stored, found, err := st.StudentEventByID("testschool", ev.EventID)
	if err != nil || !found {
		t.Fatalf("the event was not persisted: found %v, err %v", found, err)
	}
	if stored.Room != "R12" {
		t.Errorf("room = %q, want R12", stored.Room)
	}
}

func TestAdminEventAPIRejectsNonAdmins(t *testing.T) {
	p := adminEventProxy1(t)
	// Acting as the student, who holds no admin flag.
	for _, tc := range []struct{ method, target, body string }{
		{http.MethodGet, "/api/admin/events?username=dee", ""},
		{http.MethodPost, "/api/admin/events", `{"username":"dee","date":"2026-10-01","startTime":"14:00","endTime":"15:00","title":"x"}`},
		{http.MethodPatch, "/api/admin/events/1", `{"title":"x"}`},
		{http.MethodDelete, "/api/admin/events/1", ""},
	} {
		rec := doAdmin(t, p, "dee", tc.method, tc.target, tc.body)
		if rec.Code == http.StatusOK {
			t.Errorf("a student reached the admin API: %s", rec.Body.String())
		}
	}
	// And nothing was written.
	if _, found, _ := p.store.StudentEventByID("testschool", 1); found {
		t.Error("an event was created by a non-admin")
	}
}

func TestAdminEventAPIRejectsAnonymous(t *testing.T) {
	p := adminEventProxy1(t)
	rec := doAdmin(t, p, "", http.MethodGet, "/api/admin/events?username=dee", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestAdminEventCreateValidates(t *testing.T) {
	p, _ := adminEventProxy(t)
	for name, body := range map[string]string{
		"no username":      `{"date":"2026-10-01","startTime":"14:00","endTime":"15:00","title":"x"}`,
		"no title":         `{"username":"dee","date":"2026-10-01","startTime":"14:00","endTime":"15:00"}`,
		"bad date":         `{"username":"dee","date":"01.10.2026","startTime":"14:00","endTime":"15:00","title":"x"}`,
		"bad time":         `{"username":"dee","date":"2026-10-01","startTime":"2pm","endTime":"15:00","title":"x"}`,
		"end before start": `{"username":"dee","date":"2026-10-01","startTime":"15:00","endTime":"14:00","title":"x"}`,
		"end equals start": `{"username":"dee","date":"2026-10-01","startTime":"14:00","endTime":"14:00","title":"x"}`,
	} {
		rec := doAdmin(t, p, "adm", http.MethodPost, "/api/admin/events", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body %s)", name, rec.Code, rec.Body.String())
		}
	}
	// An unknown username is 404, not 400: the request is well formed, and the
	// distinction tells the admin a typo from a bad field.
	rec := doAdmin(t, p, "adm", http.MethodPost, "/api/admin/events",
		`{"username":"nobody","date":"2026-10-01","startTime":"14:00","endTime":"15:00","title":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown user: status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestAdminEventEditAndDelete(t *testing.T) {
	p, st := adminEventProxy(t)
	rec := doAdmin(t, p, "adm", http.MethodPost, "/api/admin/events",
		`{"username":"dee","date":"2026-10-01","startTime":"14:00","endTime":"15:00","title":"Technik"}`)
	id := decodeEvent(t, rec).EventID

	rec = doAdmin(t, p, "adm", http.MethodPatch, "/api/admin/events/"+strconv.FormatInt(id, 10), `{"title":"Technik (Raum 12)","room":"R12"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d, body %s", rec.Code, rec.Body.String())
	}
	ev := decodeEvent(t, rec)
	if ev.Title != "Technik (Raum 12)" || ev.Room != "R12" {
		t.Errorf("after patch = %+v", ev)
	}
	if ev.Revision != 2 {
		t.Errorf("revision = %d, want 2 so an .ics client sees the change", ev.Revision)
	}
	if ev.StartTime != "14:00" || ev.EndTime != "15:00" {
		t.Errorf("the patch overwrote untouched fields: %+v", ev)
	}

	rec = doAdmin(t, p, "adm", http.MethodDelete, "/api/admin/events/"+strconv.FormatInt(id, 10), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d", rec.Code)
	}
	if _, found, _ := st.StudentEventByID("testschool", id); found {
		t.Error("the event survived the delete")
	}
}

func TestAdminEventPatchValidatesTheMergedResult(t *testing.T) {
	p, _ := adminEventProxy(t)
	rec := doAdmin(t, p, "adm", http.MethodPost, "/api/admin/events",
		`{"username":"dee","date":"2026-10-01","startTime":"14:00","endTime":"15:00","title":"Technik"}`)
	id := decodeEvent(t, rec).EventID

	// Only the start time is sent; against the stored end it makes the event
	// nonsensical, and the stored row is what is checked before writing.
	rec = doAdmin(t, p, "adm", http.MethodPatch, "/api/admin/events/"+strconv.FormatInt(id, 10), `{"startTime":"16:00"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	still, _, _ := p.store.StudentEventByID("testschool", id)
	if still.StartTime != "14:00" {
		t.Errorf("the rejected patch was applied anyway: start = %q", still.StartTime)
	}
}

func TestAdminEventEditAndDeleteOfMissingEvent(t *testing.T) {
	p, _ := adminEventProxy(t)
	for _, m := range []string{http.MethodPatch, http.MethodDelete} {
		rec := doAdmin(t, p, "adm", m, "/api/admin/events/999999", `{"title":"x"}`)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", m, rec.Code)
		}
	}
}

func TestAdminEventListRequiresAUsername(t *testing.T) {
	p, _ := adminEventProxy(t)
	doAdmin(t, p, "adm", http.MethodPost, "/api/admin/events",
		`{"username":"dee","date":"2026-10-01","startTime":"14:00","endTime":"15:00","title":"Technik"}`)
	// A list with no subject would have to choose between dumping every student's
	// events or silently returning one student's, so it is refused.
	rec := doAdmin(t, p, "adm", http.MethodGet, "/api/admin/events", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestAdminEventListIsSortedByDateThenTime(t *testing.T) {
	p, _ := adminEventProxy(t)
	for _, b := range []string{
		`{"username":"dee","date":"2026-10-02","startTime":"09:00","endTime":"10:00","title":"later"}`,
		`{"username":"dee","date":"2026-10-01","startTime":"15:00","endTime":"16:00","title":"afternoon"}`,
		`{"username":"dee","date":"2026-10-01","startTime":"08:00","endTime":"09:00","title":"morning"}`,
	} {
		if rec := doAdmin(t, p, "adm", http.MethodPost, "/api/admin/events", b); rec.Code != http.StatusOK {
			t.Fatalf("create failed: %s", rec.Body.String())
		}
	}
	rec := doAdmin(t, p, "adm", http.MethodGet, "/api/admin/events?username=dee", "")
	var out struct {
		Events []studentEventPayload `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []string{"morning", "afternoon", "later"}
	if len(out.Events) != len(want) {
		t.Fatalf("got %d events, want %d", len(out.Events), len(want))
	}
	for i, title := range want {
		if out.Events[i].Title != title {
			t.Errorf("event %d = %q, want %q", i, out.Events[i].Title, title)
		}
	}
}

func TestAdminEventPatchCannotTouchAnotherSchool(t *testing.T) {
	p, _ := adminEventProxy(t)
	rec := doAdmin(t, p, "adm", http.MethodPost, "/api/admin/events",
		`{"username":"dee","date":"2026-10-01","startTime":"14:00","endTime":"15:00","title":"Technik"}`)
	id := decodeEvent(t, rec).EventID

	rec = doAdmin(t, p, "adm", http.MethodPatch, "/api/admin/events?school=otherschool/"+strconv.FormatInt(id, 10), "")
	if rec.Code == http.StatusOK {
		t.Errorf("an event was reachable through another school: %s", rec.Body.String())
	}
	got, found, _ := p.store.StudentEventByID("testschool", id)
	if !found || got.Title != "Technik" {
		t.Errorf("the event in the original school changed: %+v", got)
	}
}

// adminEventProxy1 is the no-return counterpart of adminEventProxy for tests that
// only need the proxy.
func adminEventProxy1(t *testing.T) *Proxy {
	t.Helper()
	p, _ := adminEventProxy(t)
	return p
}

// --- the change signal ---

func TestStudentEventChangeReachesOnlyTheStudent(t *testing.T) {
	p, _ := adminEventProxy(t)
	deeCh, unsubDee := p.hub.subscribeUser("testschool", "dee")
	defer unsubDee()
	samCh, unsubSam := p.hub.subscribeUser("testschool", "sam")
	defer unsubSam()

	if rec := doAdmin(t, p, "adm", http.MethodPost, "/api/admin/events",
		`{"username":"dee","date":"2026-10-01","startTime":"14:00","endTime":"15:00","title":"Technik"}`); rec.Code != http.StatusOK {
		t.Fatalf("create: %s", rec.Body.String())
	}

	select {
	case msg := <-deeCh:
		if msg.Username != "dee" || msg.Reason != "created" || msg.EventVersion == 0 {
			t.Errorf("dee got %+v", msg)
		}
		// The signal must not carry the event itself: it is the only thing that has
		// to be trusted not to travel, and content would travel with it.
		if strings.Contains(mustJSON(t, msg), "Technik") {
			t.Errorf("the signal carries the event content: %s", mustJSON(t, msg))
		}
	case <-time.After(time.Second):
		t.Error("the student was not told their events changed")
	}

	select {
	case msg := <-samCh:
		t.Errorf("a classmate received %+v", msg)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestStudentEventChangeIsAnnouncedOnEditAndDelete(t *testing.T) {
	p, _ := adminEventProxy(t)
	rec := doAdmin(t, p, "adm", http.MethodPost, "/api/admin/events",
		`{"username":"dee","date":"2026-10-01","startTime":"14:00","endTime":"15:00","title":"Technik"}`)
	id := decodeEvent(t, rec).EventID

	ch, unsub := p.hub.subscribeUser("testschool", "dee")
	defer unsub()

	if rec := doAdmin(t, p, "adm", http.MethodPatch, "/api/admin/events/"+strconv.FormatInt(id, 10), `{"room":"R12"}`); rec.Code != http.StatusOK {
		t.Fatalf("patch: %s", rec.Body.String())
	}
	if msg := recvUserMsg(t, ch); msg.Reason != "updated" {
		t.Errorf("after edit got %+v, want reason updated", msg)
	}

	if rec := doAdmin(t, p, "adm", http.MethodDelete, "/api/admin/events/"+strconv.FormatInt(id, 10), ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: %s", rec.Body.String())
	}
	// The delete is the case that matters most: the row is gone, so a client that
	// only watched the events table would still be showing it.
	if msg := recvUserMsg(t, ch); msg.Reason != "deleted" {
		t.Errorf("after delete got %+v, want reason deleted", msg)
	}
}

func recvUserMsg(t *testing.T, ch chan studentEventMsg) studentEventMsg {
	t.Helper()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(time.Second):
		t.Fatal("no change signal arrived")
		return studentEventMsg{}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestUserHubKeysCannotCollideWithClassKeys(t *testing.T) {
	// The two hubs share a struct, so the keys have to be provably disjoint. A
	// username crafted to look like a class key must not land on one.
	if userHubKey("testschool", "5000") == hubKey("testschool", 5000) {
		t.Errorf("a user key collides with a class key: %q", userHubKey("testschool", "5000"))
	}
	// Username normalization is part of the key, so case and padding cannot produce
	// two subscriptions for one student.
	if userHubKey("testschool", " DEE ") != userHubKey("testschool", "dee") {
		t.Error("the same student gets two different user keys")
	}
}

func TestAdminDashboardOffersEventManagement(t *testing.T) {
	// The dashboard is an embedded file, so nothing fails loudly if the events
	// section is dropped from it. The API would still work, leaving an admin with no
	// way to reach it from the browser.
	for _, want := range []string{"/api/admin/events", "loadEvents", "addEvent", "delEvent", "editEvent"} {
		if !strings.Contains(adminDashboardHTML, want) {
			t.Errorf("the admin dashboard does not reference %q", want)
		}
	}
}

// TestValidateEventTimesComparesTimesNotStrings pins the ordering rule for an
// event's times.
//
// time.Parse("15:04", ...) accepts a one-digit hour, so "9:00" is valid input and
// reaches this check. The check then compared the two strings, which put
// "10:00" below "9:00": a plain 09:00–10:00 Technik slot was refused with
// "endTime must be after startTime", while an inverted 10:00–09:00 one was
// accepted and stored.
func TestValidateEventTimesComparesTimesNotStrings(t *testing.T) {
	for _, tc := range []struct {
		start, end, want string
	}{
		{"09:00", "10:00", ""},
		{"9:00", "10:00", ""},
		{"9:00", "10:30", ""},
		{"9:00", "9:30", ""},
		{"14:00", "15:00", ""},
		{"10:00", "9:00", "endTime must be after startTime"},
		{"9:00", "9:00", "endTime must be after startTime"},
		{"9:00", "8:00", "endTime must be after startTime"},
		{"9:00", "10:0", "startTime and endTime must be HH:MM"},
		{"9:00", "noon", "startTime and endTime must be HH:MM"},
	} {
		if got := validateEventTimes(tc.start, tc.end); got != tc.want {
			t.Errorf("validateEventTimes(%q, %q) = %q, want %q", tc.start, tc.end, got, tc.want)
		}
	}
}

// TestAdminEventTimesAcceptUnpaddedHours walks the same case through the API, so
// the fix is pinned where an admin's client actually hits it.
func TestAdminEventTimesAcceptUnpaddedHours(t *testing.T) {
	p, _ := adminEventProxy(t)
	rec := doAdmin(t, p, "adm", http.MethodPost, "/api/admin/events",
		`{"username":"dee","date":"2026-10-01","startTime":"9:00","endTime":"10:00","title":"Technik"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("a 09:00–10:00 event was refused: %d %s", rec.Code, rec.Body.String())
	}
	rec = doAdmin(t, p, "adm", http.MethodPost, "/api/admin/events",
		`{"username":"dee","date":"2026-10-01","startTime":"10:00","endTime":"9:00","title":"Backwards"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("an event ending before it starts was accepted: %d %s", rec.Code, rec.Body.String())
	}
}

// TestAdminEventsRequireAStudentOfThatSchool covers the target check.
//
// The events table has no foreign key to users, so this check is the only thing
// between a typo and a row nobody will ever see. It originally accepted any
// known username: a teacher account, or a student of a *different* school with
// the same username, produced an event that no surface could ever serve.
func TestAdminEventsRequireAStudentOfThatSchool(t *testing.T) {
	p, st := adminEventProxy(t)
	if err := st.UpsertUser(&store.User{
		Username: "teacher", School: "testschool", PersonType: 4, PersonID: 42, ClassID: 0,
	}); err != nil {
		t.Fatalf("seed teacher: %v", err)
	}
	if err := st.UpsertUser(&store.User{
		Username: "edna", School: "otherschool", PersonType: 5, PersonID: 43, ClassID: 6000,
	}); err != nil {
		t.Fatalf("seed other-school student: %v", err)
	}

	for _, tc := range []struct{ user, why string }{
		{"teacher", "a teacher account is a known user but has no per-student timetable"},
		{"edna", "the username exists, but in another school"},
		{"nosuchuser", "the username does not exist"},
	} {
		rec := doAdmin(t, p, "adm", http.MethodPost, "/api/admin/events",
			fmt.Sprintf(`{"username":%q,"date":"2026-10-01","startTime":"14:00","endTime":"15:00","title":"Technik"}`, tc.user))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s (%s): got %d, want 404", tc.user, tc.why, rec.Code)
		}
	}
	// The ordinary case still works.
	rec := doAdmin(t, p, "adm", http.MethodPost, "/api/admin/events",
		`{"username":"dee","date":"2026-10-01","startTime":"14:00","endTime":"15:00","title":"Technik"}`)
	if rec.Code != http.StatusOK {
		t.Errorf("a real student was refused: %d %s", rec.Code, rec.Body.String())
	}
}
