package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"untis-proxy/internal/store"
)

// A boosted user is forwarded through a teacher account for *reading*. Any
// write method must be refused for him, otherwise the visibility boost turns
// into write access to classes he does not even attend.
func TestBoostedNonEditorCannotWrite(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	seedTeacherSource(t, st, "mrTeacher", 5009)
	seedKeyUser(t, st) // owen, a normal student
	if err := st.SetBoostedFlag("owen", true); err != nil {
		t.Fatalf("boost owen: %v", err)
	}

	for _, method := range []string{
		"setAbsencesList", "addAbsence", "updateAbsence", "deleteAbsence",
		"putLessonInfo", "removeAbsence", "saveLesson", "changeClassRegEvent",
	} {
		rec := publicReq(t, p, "owen", method)
		if rec.Code != 200 {
			t.Errorf("%s: HTTP %d, want 200 json-rpc", method, rec.Code)
		}
		var resp struct {
			Error *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s: bad json: %v (body %s)", method, err, rec.Body.String())
		}
		if resp.Error == nil || resp.Error.Message != "method not allowed" {
			t.Errorf("boosted non-editor: %s was not refused locally (body %s)", method, rec.Body.String())
		}
	}
	// and nothing was forwarded: the refusal happens before any upstream call
	if got := len(f.Public()); got != 0 {
		t.Errorf("refused writes still reached upstream: %d forwards, want 0", got)
	}
}

// The same call goes through for an editor.
func TestEditorMayWrite(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	seedKeyUser(t, st)
	if err := st.SetPerm("owen", store.FeatureEditor, true); err != nil {
		t.Fatalf("grant editor: %v", err)
	}
	rec := publicReq(t, p, "owen", "putLessonInfo")
	var resp struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v (body %s)", err, rec.Body.String())
	}
	// The fake upstream answers unknown methods with -32601; what matters is
	// that the request reached it instead of being refused locally.
	if len(f.Public()) != 1 {
		t.Errorf("editor write did not reach upstream: %d forwards, want 1", len(f.Public()))
	}
	if resp.Error != nil && resp.Error.Code == -32601 && len(f.Public()) == 0 {
		t.Errorf("editor write was refused locally: %s", rec.Body.String())
	}
}

// Read methods must not be mistaken for writes, or every read would need the
// editor flag.
func TestReadMethodsAreNotWrites(t *testing.T) {
	for _, method := range []string{
		"getTimetable2017", "getUserData2017", "getKlassen", "getRooms",
		"getTeachers", "getSubjects", "getAbsence", "getOwnData",
		"getAppSharedSecret", "getAppName", "getTimetable", "getLatestImportTime",
		"listEvents", "findRooms", "queryTimetable", "fetchChanges",
		"existsUntis", "checkTimetable", "isLoggedIn", "logout", "authenticate",
	} {
		if isWriteMethod(method) {
			t.Errorf("isWriteMethod(%q) = true, want false", method)
		}
	}
	for _, method := range []string{
		"setAbsencesList", "addAbsence", "updateAbsence", "deleteAbsence",
		"putLessonInfo", "removeLesson", "saveLesson", "changeClassRegEvent",
		"createEvent", "modifyNote", "insertEntry", "replaceTopic",
		"assignTeacher", "unassignTeacher", "moveLesson", "copyLesson",
		"renameRoom", "importData", "uploadFile", "clearAbsence", "resetPassword",
		"cancelEvent", "SetAbsence", "PUTLessonInfo",
	} {
		if !isWriteMethod(method) {
			t.Errorf("isWriteMethod(%q) = false, want true", method)
		}
	}
}

// publicReq calls the public jsonrpc.do endpoint as a logged-in user.
func publicReq(t *testing.T, p *Proxy, username, method string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": "x", "method": method, "params": []any{},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/WebUntis/jsonrpc.do?school=testschool", bytes.NewReader(body))
	sess := p.sessions.New(username, 0)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: sess.ID})
	rec := httptest.NewRecorder()
	p.handleJSONRPC(rec, req)
	return rec
}

// The intern endpoint has a second, session-authenticated write path (no auth
// block in the body). It must refuse a boosted non-editor just as firmly as the
// self-authenticating one, and forward for an editor.
func TestInternSessionWriteGatedByEditorFlag(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	seedKeyUser(t, st) // owen, boosted below
	if err := st.SetBoostedFlag("owen", true); err != nil {
		t.Fatalf("boost owen: %v", err)
	}

	for _, method := range []string{"putLessonInfo", "removeAbsence", "saveLesson"} {
		rec := internReq(t, p, method, "owen", internBody(method, map[string]any{"userId": 7}))
		if code := errCode(t, rec); code != -32601 {
			t.Errorf("%s: error code = %d, want -32601 (method not allowed)", method, code)
		}
	}
	if got := len(f.Forwards()); got != 0 {
		t.Errorf("refused intern writes still reached upstream: %d forwards, want 0", got)
	}

	if err := st.SetPerm("owen", store.FeatureEditor, true); err != nil {
		t.Fatalf("grant editor: %v", err)
	}
	rec := internReq(t, p, "putLessonInfo", "owen", internBody("putLessonInfo", map[string]any{"userId": 7}))
	if code := errCode(t, rec); code == -32601 {
		t.Errorf("editor write was refused: %s", rec.Body.String())
	}
	if got := len(f.Forwards()); got != 1 {
		t.Errorf("editor intern write forwards = %d, want 1", got)
	}
}
