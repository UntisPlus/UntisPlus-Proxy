package proxy

// End-to-end coverage for homework enrichment on the jsonrpc_intern.do path: the
// decorator is only reached for the identity the proxy actually used, and never
// for a response that belongs to a different account.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"untis-proxy/internal/store"
)

const oneHomework = `{"jsonrpc":"2.0","id":"upstream","result":{"homeWorks":[
	{"id":1001,"completed":false,"text":"Mathe"}]}}`

// firstHomeworkFlag reads the proxy-added done field off the first assignment.
func firstHomeworkFlag(t *testing.T, recBody string) bool {
	t.Helper()
	result := decodeResult(t, []byte(recBody))
	hw := jsonArray(result["homeWorks"])
	if len(hw) == 0 {
		t.Fatalf("no homeWorks in response: %s", recBody)
	}
	done, _ := hw[0]["done"].(bool)
	return done
}

// TestHomeworkInternEnrichesWithSession proves the normal student path works:
// the flag set through the REST endpoint shows up in the proxied homework list.
func TestHomeworkInternEnrichesWithSession(t *testing.T) {
	f := &fakeUpstream{internReply: oneHomework}
	p, st := newFakeProxyUpstream(t, f)
	seedSessionUser(t, st, "dee", 5, 7, 5000)
	if _, err := st.SetHomeworkDone("testschool", "dee", 1001); err != nil {
		t.Fatalf("SetHomeworkDone: %v", err)
	}

	rec := internReq(t, p, "getHomeWork2017", "dee",
		internBody("getHomeWork2017", map[string]any{"startDate": "2026-10-01", "endDate": "2026-10-07"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !firstHomeworkFlag(t, rec.Body.String()) {
		t.Errorf("the student's own flag did not reach the homework response: %s", rec.Body.String())
	}
}

// TestHomeworkInternSkippedForBoostedTeacher is the privacy guard. When an
// editor's class-scoped request is rewritten to run as the boosted teacher, the
// response is the teacher's data. Attaching the editor's private flags to it
// would be both wrong data and a small leak about that student.
func TestHomeworkInternSkippedForBoostedTeacher(t *testing.T) {
	f := &fakeUpstream{internReply: oneHomework}
	p, st := newFakeProxyUpstream(t, f)
	seedTeacherSource(t, st, "mrTeacher", 5009)
	seedSessionUser(t, st, "dee", 5, 7, 5000)
	// Both conditions of classScopedOwner, asserted explicitly below: an editor
	// who is also boosted swaps to the teacher source for class-scoped methods.
	if err := st.SetBoostedFlag("dee", true); err != nil {
		t.Fatalf("boost dee: %v", err)
	}
	if err := st.SetPerm("dee", store.FeatureEditor, true); err != nil {
		t.Fatalf("grant editor: %v", err)
	}
	if _, err := st.SetHomeworkDone("testschool", "dee", 1001); err != nil {
		t.Fatalf("SetHomeworkDone: %v", err)
	}
	// Precondition: without a rewrite this test would pass for the wrong reason,
	// because a dee-scoped response has no homeWorks to decorate at all.
	if p.classScopedOwner("testschool", "dee", "getPeriodData2017") == nil {
		t.Fatal("precondition: getPeriodData2017 should resolve to the teacher source here")
	}

	rec := internReq(t, p, "getPeriodData2017", "dee",
		internBody("getPeriodData2017", map[string]any{"ttId": 5005, "ttType": 2}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	forwards := f.Forwards()
	if len(forwards) != 1 || !strings.Contains(forwards[0].Cookie, "JSESSIONID=") {
		t.Fatalf("expected the request to be forwarded with a session, got %+v", forwards)
	}
	if strings.Contains(rec.Body.String(), `"done"`) {
		t.Errorf("teacher data was decorated with the editor's private flags: %s", rec.Body.String())
	}
}

// TestHomeworkInternUnchangedForTimetable guards the scoping of the feature: a
// method that carries no homework must come back byte-for-byte as upstream sent
// it, so the app cannot tell the proxy did anything at all.
func TestHomeworkInternUnchangedForTimetable(t *testing.T) {
	f := &fakeUpstream{internReply: oneHomework}
	p, st := newFakeProxyUpstream(t, f)
	seedSessionUser(t, st, "dee", 5, 7, 5000)

	rec := internReq(t, p, "getTimetable2017", "dee",
		internBody("getTimetable2017", ttParam(5000, "CLASS", "2026-10-05", "2026-10-11", "")))
	if got := rec.Body.String(); got != oneHomework {
		t.Errorf("a method without homework was altered:\n got: %s\nwant: %s", got, oneHomework)
	}
}

// TestHomeworkInternPassesThroughUpstreamError: a failing upstream request must
// not be dressed up with a flag, and must keep its status.
func TestHomeworkInternPassesThroughUpstreamError(t *testing.T) {
	upErr := `{"jsonrpc":"2.0","id":"upstream","error":{"code":-32000,"message":"boom"},"result":{"homeWorks":[{"id":1001}]}}`
	f := &fakeUpstream{internReply: upErr}
	p, st := newFakeProxyUpstream(t, f)
	seedSessionUser(t, st, "dee", 5, 7, 5000)
	if _, err := st.SetHomeworkDone("testschool", "dee", 1001); err != nil {
		t.Fatalf("SetHomeworkDone: %v", err)
	}

	rec := internReq(t, p, "getHomeWork2017", "dee",
		internBody("getHomeWork2017", map[string]any{"startDate": "2026-10-01", "endDate": "2026-10-07"}))
	if got := rec.Body.String(); got != upErr {
		t.Errorf("an error response was decorated:\n got: %s\nwant: %s", got, upErr)
	}
}

// TestHomeworkInternUnauthenticated: no session, no personal data.
func TestHomeworkInternUnauthenticated(t *testing.T) {
	f := &fakeUpstream{internReply: oneHomework}
	p, st := newFakeProxyUpstream(t, f)
	seedSessionUser(t, st, "dee", 5, 7, 5000)
	if _, err := st.SetHomeworkDone("testschool", "dee", 1001); err != nil {
		t.Fatalf("SetHomeworkDone: %v", err)
	}

	rec := internReq(t, p, "getHomeWork2017", "", internBody("getHomeWork2017"))
	if code := errCode(t, rec); code != -8520 {
		t.Errorf("error code = %d, want -8520 (not logged in)", code)
	}
	if len(f.Forwards()) != 0 {
		t.Error("an unauthenticated request was forwarded upstream")
	}
}

// TestHomeworkFlagsRequireSession is the read-endpoint counterpart.
func TestHomeworkFlagsRequireSession(t *testing.T) {
	p, _ := homeworkProxy(t)
	req, err := http.NewRequest(http.MethodGet, "/api/homework/flags", nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated read returned %d, want 401", rec.Code)
	}
}

// TestHomeworkWriteSurvivesRestart: the flags live in SQLite precisely so they
// outlive the process holding the app's HTTP connection.
func TestHomeworkWriteSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/test.db"
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := st.SetHomeworkDone("testschool", "dee", 1001); err != nil {
		t.Fatalf("SetHomeworkDone: %v", err)
	}
	st.Close()

	reopened, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	done, err := reopened.HomeworkDone("testschool", "dee")
	if err != nil {
		t.Fatalf("HomeworkDone: %v", err)
	}
	if len(done) != 1 {
		t.Fatalf("after reopen there are %d flags, want 1", len(done))
	}
	if at, ok := done[1001]; !ok || at.IsZero() {
		t.Errorf("flag 1001 did not survive the restart: %v", done)
	}
}

// TestHomeworkDoneWritePersistsAcrossSessions: identity comes from the session,
// so a fresh session for the same user sees the same flags.
func TestHomeworkDoneWritePersistsAcrossSessions(t *testing.T) {
	p, st := homeworkProxy(t)
	rec := sessionedRequest(t, p, "dee", http.MethodPost, "/api/homework/done",
		`{"homeworkId":1001,"done":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("write failed: %s", rec.Body.String())
	}
	read := sessionedRequest(t, p, "dee", http.MethodGet, "/api/homework/flags", "")
	var out struct {
		Flags []struct {
			HomeworkID int64 `json:"homeworkId"`
		} `json:"flags"`
	}
	if err := json.Unmarshal(read.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if len(out.Flags) != 1 || out.Flags[0].HomeworkID != 1001 {
		t.Errorf("a later session did not see the earlier write: %s", read.Body.String())
	}
	if n, _ := st.HomeworkDoneCount("testschool", "dee"); n != 1 {
		t.Errorf("row count = %d, want 1", n)
	}
}

// TestHomeworkInternSkippedWhenAuthNamesNobody: an auth block with no user names
// nobody, so the proxy has no viewer to attribute. Personal fields stay off
// rather than being filled in with a meaningless false.
func TestHomeworkInternSkippedWhenAuthNamesNobody(t *testing.T) {
	f := &fakeUpstream{internReply: oneHomework}
	p, st := newFakeProxyUpstream(t, f)
	seedSessionUser(t, st, "dee", 5, 7, 5000)
	if _, err := st.SetHomeworkDone("testschool", "dee", 1001); err != nil {
		t.Fatalf("SetHomeworkDone: %v", err)
	}

	body := `{"jsonrpc":"2.0","id":"t","method":"getHomeWork2017","params":[{"auth":{"otp":""}}]}`
	req := httptest.NewRequest(http.MethodPost,
		"/WebUntis/jsonrpc_intern.do?m=getHomeWork2017&school=testschool", strings.NewReader(body))
	rec := httptest.NewRecorder()
	p.handleJSONRPCIntern(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"done"`) {
		t.Errorf("an unattributable response was decorated: %s", rec.Body.String())
	}
}
