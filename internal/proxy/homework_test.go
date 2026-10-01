package proxy

// Proxy-local homework completion: the enrichment rules, the identity rules,
// and the scoping of the write endpoint.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"untis-proxy/internal/store"
)

// hwResponse builds an upstream-shaped getHomeWork2017 result.
const hwResponse = `{"jsonrpc":"2.0","id":"hw1","result":{"homeWorks":[
	{"id":1001,"completed":false,"startDate":"2026-10-01","endDate":"2026-10-02",
	 "text":"Mathe S.42","remark":"","lessonId":5001,"attachments":[]},
	{"id":1002,"completed":true,"startDate":"2026-10-02","endDate":"2026-10-03",
	 "text":"Deutsch","lessonId":5002,"futureField":{"nested":1}}],
	"lessonsById":{"5001":{"ttId":77}}}}`

func homeworkProxy(t *testing.T) (*Proxy, *store.Store) {
	t.Helper()
	p, st := newTestProxy(t)
	for _, u := range []struct {
		name string
		id   int64
	}{{"dee", 7}, {"sam", 8}} {
		if err := st.UpsertUser(&store.User{Username: u.name, School: "testschool",
			Method: "key", Password: "x", PersonType: 5, PersonID: u.id, ClassID: 5000}); err != nil {
			t.Fatalf("seed %s: %v", u.name, err)
		}
	}
	return p, st
}

// decodeResult decodes with UseNumber so the test sees the same number fidelity
// the proxy does. A plain Unmarshal would round ids through float64 and hide
// exactly the corruption the large-id test exists to catch.
func decodeResult(t *testing.T, raw []byte) jsonObject {
	t.Helper()
	var env struct {
		Result jsonObject `json:"result"`
	}
	if !decodeJSON(raw, &env) {
		t.Fatalf("bad json (body=%s)", raw)
	}
	return env.Result
}

// TestHomeworkEnrichmentAddsFlagsWithoutDisturbingUpstream is the contract: the
// proxy adds two fields and changes nothing else. The teacher's `completed` must
// survive untouched, and a field this proxy has never heard of must not be
// dropped, or the app loses data it may depend on.
func TestHomeworkEnrichmentAddsFlagsWithoutDisturbingUpstream(t *testing.T) {
	p, st := homeworkProxy(t)
	if _, err := st.SetHomeworkDone("testschool", "dee", 1001); err != nil {
		t.Fatalf("SetHomeworkDone: %v", err)
	}

	out := p.enrichHomeWorkResponse([]byte(hwResponse), "testschool", "dee")
	result := decodeResult(t, out)
	list, _ := result["homeWorks"].([]any)
	if len(list) != 2 {
		t.Fatalf("homeWorks has %d entries, want 2", len(list))
	}

	first := list[0].(map[string]any)
	if first["done"] != true {
		t.Errorf("first assignment done = %v, want true", first["done"])
	}
	if first["doneAt"] == nil {
		t.Error("first assignment is missing doneAt")
	}
	// The teacher's decision is not the student's answer and must be passed on
	// exactly as received.
	if first["completed"] != false {
		t.Errorf("upstream completed was altered: %v, want false", first["completed"])
	}

	second := list[1].(map[string]any)
	if second["done"] != false {
		t.Errorf("second assignment done = %v, want false", second["done"])
	}
	if second["doneAt"] != nil {
		t.Errorf("second assignment doneAt = %v, want null", second["doneAt"])
	}
	if second["completed"] != true {
		t.Errorf("upstream completed was altered on the second assignment: %v", second["completed"])
	}
	// Unknown upstream field survived.
	if _, ok := second["futureField"]; !ok {
		t.Error("an unknown upstream field was dropped")
	}
	// Sibling structure untouched.
	if _, ok := result["lessonsById"]; !ok {
		t.Error("lessonsById was dropped")
	}
}

// TestHomeworkEnrichmentIsPerViewer is the isolation guarantee: two students
// sharing a class must never see each other's answers.
func TestHomeworkEnrichmentIsPerViewer(t *testing.T) {
	p, st := homeworkProxy(t)
	if _, err := st.SetHomeworkDone("testschool", "dee", 1001); err != nil {
		t.Fatalf("SetHomeworkDone: %v", err)
	}

	dee := decodeResult(t, p.enrichHomeWorkResponse([]byte(hwResponse), "testschool", "dee"))
	sam := decodeResult(t, p.enrichHomeWorkResponse([]byte(hwResponse), "testschool", "sam"))

	deeFirst := dee["homeWorks"].([]any)[0].(map[string]any)
	samFirst := sam["homeWorks"].([]any)[0].(map[string]any)
	if deeFirst["done"] != true {
		t.Errorf("dee should see their own flag set, got %v", deeFirst["done"])
	}
	if samFirst["done"] != false {
		t.Errorf("sam must not inherit dee's flag, got %v", samFirst["done"])
	}
}

// TestHomeworkEnrichmentPassesThroughOnBadInput: an unreadable response must
// reach the app exactly as upstream sent it. Decorating half of it would be worse
// than not decorating it.
func TestHomeworkEnrichmentPassesThroughOnBadInput(t *testing.T) {
	p, _ := homeworkProxy(t)
	for _, raw := range []string{
		`not json at all`,
		``,
		`{"jsonrpc":"2.0","id":"x"}`, // no result
		`{"jsonrpc":"2.0","id":"x","result":null}`,                             // null result
		`{"jsonrpc":"2.0","id":"x","result":{"homeWorks":"nope"}}`,             // wrong type
		`{"jsonrpc":"2.0","id":"x","result":{"homeWorks":[1,2,3]}}`,            // not objects
		`{"jsonrpc":"2.0","id":"x","result":{"homeWorks":[{"text":"no id"}]}}`, // unkeyable
	} {
		got := p.enrichHomeWorkResponse([]byte(raw), "testschool", "dee")
		if string(got) != raw {
			t.Errorf("response was altered for input %s\n got: %s\nwant: %s", raw, got, raw)
		}
	}
}

// TestHomeworkEnrichmentSkipsErrorResponses: a failure must keep looking like a
// failure even if its result field happens to be an object.
func TestHomeworkEnrichmentSkipsErrorResponses(t *testing.T) {
	p, st := homeworkProxy(t)
	if _, err := st.SetHomeworkDone("testschool", "dee", 1001); err != nil {
		t.Fatalf("SetHomeworkDone: %v", err)
	}
	raw := `{"jsonrpc":"2.0","id":"x","error":{"code":-32000,"message":"boom"},"result":{"homeWorks":[{"id":1001}]}}`
	got := p.enrichHomeWorkResponse([]byte(raw), "testschool", "dee")
	if string(got) != raw {
		t.Errorf("error response was decorated:\n got: %s\nwant: %s", got, raw)
	}
}

// TestHomeworkEnrichmentPreservesLargeIDs: ids are the storage key, so a round
// trip through float64 would corrupt them. json.Number is what prevents that.
func TestHomeworkEnrichmentPreservesLargeIDs(t *testing.T) {
	p, st := homeworkProxy(t)
	const bigID = int64(9007199254740993) // float64 cannot represent this exactly
	if _, err := st.SetHomeworkDone("testschool", "dee", bigID); err != nil {
		t.Fatalf("SetHomeworkDone: %v", err)
	}
	raw := `{"jsonrpc":"2.0","id":"x","result":{"homeWorks":[{"id":9007199254740993,"text":"big"}]}}`
	result := decodeResult(t, p.enrichHomeWorkResponse([]byte(raw), "testschool", "dee"))
	hw := result["homeWorks"].([]any)[0].(map[string]any)
	if id, ok := jsonID(hw["id"]); !ok || id != bigID {
		t.Errorf("large id was corrupted to %v (%d), want %d", hw["id"], id, bigID)
	}
	if hw["done"] != true {
		t.Errorf("large id did not match its flag, done = %v", hw["done"])
	}
}

// TestHomeworkFlagsReadEndpoint lists only the caller's own flags.
func TestHomeworkFlagsReadEndpoint(t *testing.T) {
	p, st := homeworkProxy(t)
	if _, err := st.SetHomeworkDone("testschool", "dee", 1002); err != nil {
		t.Fatalf("SetHomeworkDone: %v", err)
	}
	if _, err := st.SetHomeworkDone("testschool", "dee", 1001); err != nil {
		t.Fatalf("SetHomeworkDone: %v", err)
	}
	if _, err := st.SetHomeworkDone("testschool", "sam", 1001); err != nil {
		t.Fatalf("SetHomeworkDone: %v", err)
	}

	rec := sessionedRequest(t, p, "dee", http.MethodGet, "/api/homework/flags", "")
	var out struct {
		Flags []struct {
			HomeworkID int64  `json:"homeworkId"`
			Done       bool   `json:"done"`
			DoneAt     string `json:"doneAt"`
		} `json:"flags"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v (body %s)", err, rec.Body.String())
	}
	if len(out.Flags) != 2 {
		t.Fatalf("dee sees %d flags, want 2 (sam's must not leak)", len(out.Flags))
	}
	if out.Flags[0].HomeworkID != 1001 || out.Flags[1].HomeworkID != 1002 {
		t.Errorf("flags not sorted by id: %+v", out.Flags)
	}
	for _, f := range out.Flags {
		if !f.Done || f.DoneAt == "" {
			t.Errorf("flag is not marked done with a timestamp: %+v", f)
		}
	}
}

// TestHomeworkDoneWriteIsSessionScoped is the write-scoping guarantee. Supplying
// another username in the body must not move the write: the session decides.
func TestHomeworkDoneWriteIsSessionScoped(t *testing.T) {
	p, st := homeworkProxy(t)

	rec := sessionedRequest(t, p, "dee", http.MethodPost, "/api/homework/done",
		`{"homeworkId":1001,"done":true,"username":"sam"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("write failed: %d %s", rec.Code, rec.Body.String())
	}

	dee, _ := st.HomeworkDone("testschool", "dee")
	if len(dee) != 1 {
		t.Errorf("dee has %d flags, want 1 (the write should be hers)", len(dee))
	}
	sam, _ := st.HomeworkDone("testschool", "sam")
	if len(sam) != 0 {
		t.Errorf("sam has %d flags, want 0 — a body-supplied username must be ignored", len(sam))
	}
}

// TestHomeworkDoneRequiresSession: no session, no write.
func TestHomeworkDoneRequiresSession(t *testing.T) {
	p, _ := homeworkProxy(t)
	req := httptest.NewRequest(http.MethodPost, "/api/homework/done",
		strings.NewReader(`{"homeworkId":1001,"done":true}`))
	rec := httptest.NewRecorder()
	p.handleHomeworkDone(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated write returned %d, want 401", rec.Code)
	}
}

// TestHomeworkDoneValidatesInput: a missing id or a missing flag is rejected
// rather than silently clearing something.
func TestHomeworkDoneValidatesInput(t *testing.T) {
	p, _ := homeworkProxy(t)
	for _, body := range []string{
		`{"done":true}`,                 // no id
		`{"homeworkId":0,"done":true}`,  // id zero
		`{"homeworkId":-5,"done":true}`, // negative id
		`{"homeworkId":1001}`,           // no done flag
		`not json`,
	} {
		rec := sessionedRequest(t, p, "dee", http.MethodPost, "/api/homework/done", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s returned %d, want 400", body, rec.Code)
		}
	}
}

// TestHomeworkDoneIsIdempotent: clearing an unset flag, and setting twice, must
// both converge, so a client retrying after a dropped response is safe.
func TestHomeworkDoneIsIdempotent(t *testing.T) {
	p, st := homeworkProxy(t)
	for i := 0; i < 3; i++ {
		rec := sessionedRequest(t, p, "dee", http.MethodPost, "/api/homework/done",
			`{"homeworkId":1001,"done":true}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("set %d failed: %s", i, rec.Body.String())
		}
	}
	if n, _ := st.HomeworkDoneCount("testschool", "dee"); n != 1 {
		t.Errorf("setting three times produced %d rows, want 1", n)
	}
	for i := 0; i < 3; i++ {
		rec := sessionedRequest(t, p, "dee", http.MethodPost, "/api/homework/done",
			`{"homeworkId":1001,"done":false}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("clear %d failed: %s", i, rec.Body.String())
		}
	}
	if n, _ := st.HomeworkDoneCount("testschool", "dee"); n != 0 {
		t.Errorf("clearing left %d rows, want 0", n)
	}
}

// TestHomeworkFlagsAreScopedPerSchool: the same homework id in two schools must
// not share a flag.
func TestHomeworkFlagsAreScopedPerSchool(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if _, err := st.SetHomeworkDone("a", "dee", 1001); err != nil {
		t.Fatal(err)
	}
	other, _ := st.HomeworkDone("b", "dee")
	if len(other) != 0 {
		t.Errorf("a flag set in school a leaked into school b: %v", other)
	}
}

// sessionedRequest builds a request carrying a valid session cookie.
func sessionedRequest(t *testing.T, p *Proxy, user, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	s := p.sessions.New(user, 0)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: s.ID})
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)
	return rec
}
