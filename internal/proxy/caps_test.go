package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"untis-proxy/internal/store"
)

// A teacher account answers with write and delete capabilities on every
// period. Boosted users are forwarded through such an account for visibility,
// which would otherwise hand them the editing UI as well.
const teacherViewTimetable = `{"jsonrpc":"2.0","id":"upstream","result":{
	"timetable":{"displayableStartDate":"2026-09-21","displayableEndDate":"2026-09-27","periods":[
		{"id":1,"startDateTime":"2026-09-21T08:00:00Z","endDateTime":"2026-09-21T08:45:00Z",
		 "text":{"lesson":"Photosynthesis","substitution":"","info":"","attachments":[]},
		 "elements":[{"id":5009,"type":"TEACHER"},{"id":169,"type":"ROOM"},{"id":7,"type":"SUBJECT"},{"id":5000,"type":"CLASS"}],
		 "can":["READ_LESSONTOPIC","WRITE_LESSONTOPIC","READ_HOMEWORK","WRITE_HOMEWORK",
		       "READ_CLASSREGEVENT","WRITE_CLASSREGEVENT","DELETE_CLASSREGEVENT",
		       "READ_CLASSROLE","READ_PERIODINFO","WRITE_PERIODINFO",
		       "READ_STUD_ABSENCE","WRITE_STUD_ABSENCE"]}]}}}`

// A student key answering for a class it belongs to: no write capabilities, and
// no READ_LESSONTOPIC, which is what hides the topic row in the app.
const studentViewTimetable = `{"jsonrpc":"2.0","id":"upstream","result":{
	"timetable":{"displayableStartDate":"2026-09-21","displayableEndDate":"2026-09-27","periods":[
		{"id":1,"startDateTime":"2026-09-21T08:00:00Z","endDateTime":"2026-09-21T08:45:00Z",
		 "text":{"lesson":"Photosynthesis","substitution":"","info":"","attachments":[]},
		 "elements":[{"id":5009,"type":"TEACHER"},{"id":169,"type":"ROOM"},{"id":7,"type":"SUBJECT"},{"id":5000,"type":"CLASS"}],
		 "can":["READ_HOMEWORK","READ_PERIODINFO"]}]}}}`

func setRawTimetable(t *testing.T, f *fakeUpstream, body string) {
	t.Helper()
	f.mu.Lock()
	f.timetable = body
	f.mu.Unlock()
}

func periodCaps(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var resp struct {
		Result struct {
			Timetable struct {
				Periods []struct {
					Can []string `json:"can"`
				} `json:"periods"`
			} `json:"timetable"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v (body %s)", err, rec.Body.String())
	}
	if len(resp.Result.Timetable.Periods) != 1 {
		t.Fatalf("periods = %d, want 1 (body %s)", len(resp.Result.Timetable.Periods), rec.Body.String())
	}
	return resp.Result.Timetable.Periods[0].Can
}

func hasCap(caps []string, want string) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}

// boosted is visibility only: the teacher account's periods keep their read
// capabilities but every write/delete capability is stripped.
func TestBoostedWithoutEditorLosesWriteCaps(t *testing.T) {
	f := &fakeUpstream{}
	setRawTimetable(t, f, teacherViewTimetable)
	p, st := newFakeProxyUpstream(t, f)
	seedTeacherSource(t, st, "mrTeacher", 5009)
	seedSessionUser(t, st, "dee", 5, 7, 5000)
	if err := st.SetBoostedFlag("dee", true); err != nil {
		t.Fatalf("boost dee: %v", err)
	}

	rec := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(5009, "TEACHER", "2026-09-21", "2026-09-27", "dee")))
	caps := periodCaps(t, rec)
	for _, c := range caps {
		if len(c) > 5 && c[:5] != "READ_" {
			t.Errorf("boosted non-editor keeps capability %q: %v", c, caps)
		}
	}
	if !hasCap(caps, "READ_LESSONTOPIC") {
		t.Errorf("boosted user lost the lesson topic: %v", caps)
	}
}

// The editor flag is the only thing that may keep upstream's write caps.
func TestEditorKeepsUpstreamWriteCaps(t *testing.T) {
	f := &fakeUpstream{}
	setRawTimetable(t, f, teacherViewTimetable)
	p, st := newFakeProxyUpstream(t, f)
	seedTeacherSource(t, st, "mrTeacher", 5009)
	seedSessionUser(t, st, "dee", 5, 7, 5000)
	if err := st.SetPerm("dee", store.FeatureEditor, true); err != nil {
		t.Fatalf("grant editor: %v", err)
	}

	rec := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(5009, "TEACHER", "2026-09-21", "2026-09-27", "dee")))
	caps := periodCaps(t, rec)
	for _, want := range []string{"READ_LESSONTOPIC", "WRITE_LESSONTOPIC", "WRITE_HOMEWORK",
		"DELETE_CLASSREGEVENT", "WRITE_PERIODINFO", "WRITE_STUD_ABSENCE"} {
		if !hasCap(caps, want) {
			t.Errorf("editor lost capability %q: %v", want, caps)
		}
	}
}

// A recon user viewing a pooled class gets the class owner's student caps, which
// omit READ_LESSONTOPIC - the app then shows teacher, class and room but hides
// the lesson topic even though text.lesson is in the same period.
func TestPooledClassViewAddsLessonTopicCap(t *testing.T) {
	f := &fakeUpstream{userDataBody: fullMasterDataBody}
	setRawTimetable(t, f, studentViewTimetable)
	p, st := newFakeProxyUpstream(t, f)
	seedKeyUser(t, st) // owen owns pooled class 5000
	seedSessionUser(t, st, "sam", 5, 11, 4420)
	if err := st.SaveReconElements("testschool", map[string][]int64{
		"TEACHER": {5009}, "ROOM": {169}, "SUBJECT": {7},
	}); err != nil {
		t.Fatalf("save recon elements: %v", err)
	}
	if err := st.SetPerm("sam", store.FeatureRecon, true); err != nil {
		t.Fatalf("grant recon: %v", err)
	}
	mdLogin(t, p, "sam")

	rec := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(5000, "CLASS", "2026-09-21", "2026-09-27", "sam")))
	caps := periodCaps(t, rec)
	if !hasCap(caps, "READ_LESSONTOPIC") {
		t.Errorf("pooled class view hides the lesson topic: %v", caps)
	}
	for _, c := range caps {
		if len(c) > 5 && c[:5] != "READ_" {
			t.Errorf("pooled class view grants capability %q: %v", c, caps)
		}
	}
}

// A member viewing their own class is served the same upstream body, so the
// topic has to appear there too - without inventing any write access.
func TestOwnClassViewAddsLessonTopicCap(t *testing.T) {
	f := &fakeUpstream{}
	setRawTimetable(t, f, studentViewTimetable)
	p, st := newFakeProxyUpstream(t, f)
	seedSessionUser(t, st, "dee", 5, 7, 5000)

	rec := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(5000, "CLASS", "2026-09-21", "2026-09-27", "dee")))
	caps := periodCaps(t, rec)
	if !hasCap(caps, "READ_LESSONTOPIC") {
		t.Errorf("own class view hides the lesson topic: %v", caps)
	}
}

// Reconstructed teacher/room/subject views are built from stored upstream
// periods, so they carry the same caps and need the same rewrite.
func TestReconstructedViewAddsLessonTopicCap(t *testing.T) {
	f := &fakeUpstream{userDataBody: fullMasterDataBody}
	setRawTimetable(t, f, studentViewTimetable)
	p, st := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st, "owen", 7, 5000)
	seedSessionUser(t, st, "sam", 5, 11, 5000)
	if err := st.SaveReconElements("testschool", map[string][]int64{
		"TEACHER": {5009}, "ROOM": {169}, "SUBJECT": {7},
	}); err != nil {
		t.Fatalf("save recon elements: %v", err)
	}
	if err := st.SetPerm("sam", store.FeatureRecon, true); err != nil {
		t.Fatalf("grant recon: %v", err)
	}
	mdLogin(t, p, "sam")

	rec := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(5009, "TEACHER", "2026-09-21", "2026-09-27", "sam")))
	var resp struct {
		Result struct {
			Timetable struct {
				Periods []struct {
					Can []string `json:"can"`
				} `json:"periods"`
			} `json:"timetable"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v (body %s)", err, rec.Body.String())
	}
	periods := resp.Result.Timetable.Periods
	if len(periods) != 1 {
		t.Fatalf("periods = %d, want 1 (body %s)", len(periods), rec.Body.String())
	}
	if !hasCap(periods[0].Can, "READ_LESSONTOPIC") {
		t.Errorf("reconstructed view hides the lesson topic: %v", periods[0].Can)
	}
	for _, c := range periods[0].Can {
		if len(c) > 5 && c[:5] != "READ_" {
			t.Errorf("reconstructed view grants capability %q: %v", c, periods[0].Can)
		}
	}
}

// editor means boosted + editing: the flag alone must already unlock the raw
// teacher-account forwarding.
func TestEditorImpliesBoosted(t *testing.T) {
	f := &fakeUpstream{}
	setRawTimetable(t, f, teacherViewTimetable)
	p, st := newFakeProxyUpstream(t, f)
	seedTeacherSource(t, st, "mrTeacher", 5009)
	seedSessionUser(t, st, "dee", 5, 7, 5000)
	if err := st.SetPerm("dee", store.FeatureEditor, true); err != nil {
		t.Fatalf("grant editor: %v", err)
	}
	if !p.isBoosted("dee") {
		t.Error("isBoosted(editor) = false, want true: editor must imply boosted")
	}
	rec := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(5009, "TEACHER", "2026-09-21", "2026-09-27", "dee")))
	if caps := periodCaps(t, rec); !hasCap(caps, "WRITE_LESSONTOPIC") {
		t.Errorf("editor answer lost its write capabilities: %v", caps)
	}
	forwards := f.Forwards()
	if len(forwards) != 1 {
		t.Fatalf("forwards = %d, want 1 (raw via teacher source)", len(forwards))
	}

	// and /v1/me must report the combined level
	if lvl, code := meLevel(t, p, "dee"); code != 200 || lvl != "boosted+editor" {
		t.Errorf("/v1/me level = %q (code %d), want boosted+editor", lvl, code)
	}
}

// An admin of this proxy is not an editor of Untis: without the editor flag
// the write capabilities go away, otherwise the app shows editing that the
// write-method gate refuses anyway.
func TestAdminWithoutEditorLosesWriteCaps(t *testing.T) {
	f := &fakeUpstream{}
	setRawTimetable(t, f, teacherViewTimetable)
	p, st := newFakeProxyUpstream(t, f)
	seedTeacherSource(t, st, "mrTeacher", 5009)
	seedSessionUser(t, st, "root", 5, 7, 5000)
	if err := st.SetAdmin("root", true); err != nil {
		t.Fatalf("grant admin: %v", err)
	}
	if err := st.SetBoostedFlag("root", true); err != nil {
		t.Fatalf("boost root: %v", err)
	}
	if !p.isAdmin("root") {
		t.Fatal("isAdmin(root) = false, want true")
	}

	rec := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(5009, "TEACHER", "2026-09-21", "2026-09-27", "root")))
	caps := periodCaps(t, rec)
	for _, c := range caps {
		if len(c) > 5 && c[:5] != "READ_" {
			t.Errorf("admin without editor keeps capability %q: %v", c, caps)
		}
	}
	if !hasCap(caps, "READ_LESSONTOPIC") {
		t.Errorf("admin lost the lesson topic: %v", caps)
	}
}
