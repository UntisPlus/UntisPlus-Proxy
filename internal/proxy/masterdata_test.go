package proxy

// masterData rewriting on key login: the pooled class becomes displayable,
// recon/boosted permissions decide the displayAllowed flags on
// teachers/rooms/subjects, and every resolved name is persisted so
// untisctl can fuzzy-lookup elements by name.

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"untis-proxy/internal/store"
)

// masterDataLoginBody is a key-login answer carrying a full masterData block
// with one entry per name variant persistElementNames has to cope with
// (name, firstName+lastName, longName) plus a zero-id entry it must skip.
const masterDataLoginBody = `{"jsonrpc":"2.0","id":"upstream","result":{
	"sessionId":"upstreamsid","personId":7,"personType":5,"klasseId":5000,
	"masterData":{
		"timeStamp":1,
		"klassen":[{"id":5000,"name":"10b"},{"id":4420,"name":"10c"}],
		"teachers":[
			{"id":5009,"name":"A. Hartley"},
			{"id":224,"firstName":"Ada","lastName":"Lovelace"},
			{"id":0,"name":"Ghost"}
		],
		"rooms":[{"id":169,"name":"R101"}],
		"subjects":[{"id":7,"longName":"Mathematik 3"}]
	}}}`

type mdFlags struct {
	Klassen  map[int64]bool
	Teachers map[int64]bool
	Rooms    map[int64]bool
	Subjects map[int64]bool
}

func decodeMDFlags(t *testing.T, rec *httptest.ResponseRecorder) mdFlags {
	t.Helper()
	return decodeMDFlagsFrom(t, rec.Body.Bytes())
}

func decodeMDFlagsFrom(t *testing.T, body []byte) mdFlags {
	t.Helper()
	rec := httptest.NewRecorder()
	rec.Body = bytes.NewBuffer(body)
	var resp struct {
		Result struct {
			MasterData struct {
				Klassen []struct {
					ID          int64 `json:"id"`
					Displayable bool  `json:"displayable"`
				} `json:"klassen"`
				Teachers []struct {
					ID             int64 `json:"id"`
					DisplayAllowed bool  `json:"displayAllowed"`
				} `json:"teachers"`
				Rooms []struct {
					ID             int64 `json:"id"`
					DisplayAllowed bool  `json:"displayAllowed"`
				} `json:"rooms"`
				Subjects []struct {
					ID             int64 `json:"id"`
					DisplayAllowed bool  `json:"displayAllowed"`
				} `json:"subjects"`
			} `json:"masterData"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v (body=%s)", err, rec.Body.String())
	}
	out := mdFlags{
		Klassen:  map[int64]bool{},
		Teachers: map[int64]bool{},
		Rooms:    map[int64]bool{},
		Subjects: map[int64]bool{},
	}
	for _, k := range resp.Result.MasterData.Klassen {
		out.Klassen[k.ID] = k.Displayable
	}
	for _, x := range resp.Result.MasterData.Teachers {
		out.Teachers[x.ID] = x.DisplayAllowed
	}
	for _, x := range resp.Result.MasterData.Rooms {
		out.Rooms[x.ID] = x.DisplayAllowed
	}
	for _, x := range resp.Result.MasterData.Subjects {
		out.Subjects[x.ID] = x.DisplayAllowed
	}
	return out
}

func mdLogin(t *testing.T, p *Proxy, user ...string) *httptest.ResponseRecorder {
	t.Helper()
	name := "owen"
	if len(user) > 0 {
		name = user[0]
	}
	body := internBody("getUserData2017", map[string]any{
		"auth": map[string]any{"user": name, "otp": "123456", "clientTime": 0},
	})
	return internReq(t, p, "m=getUserData2017", "", body)
}

func seedKeyUser(t *testing.T, st *store.Store) {
	t.Helper()
	if err := st.UpsertUser(&store.User{
		Username: "owen", Method: "key", School: "testschool", Password: "replayable",
		PersonType: 5, PersonID: 7, ClassID: 5000, ClassName: "10b",
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
}

func TestKeyLoginRewritesMasterDataAndPersistsNames(t *testing.T) {
	f := &fakeUpstream{userDataBody: masterDataLoginBody}
	p, st := newFakeProxyUpstream(t, f)
	seedKeyUser(t, st)
	if err := st.SetPerm("owen", store.FeatureRecon, true); err != nil {
		t.Fatalf("grant recon: %v", err)
	}
	if err := st.SaveReconElements("testschool", map[string][]int64{
		"TEACHER": {5009},
		"SUBJECT": {7},
	}); err != nil {
		t.Fatalf("save recon elements: %v", err)
	}

	rec := mdLogin(t, p)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	flags := decodeMDFlags(t, rec)

	// the pooled class is displayable for everyone, the other class is not
	if !flags.Klassen[5000] {
		t.Errorf("class 5000 (pooled) displayable = false, want true")
	}
	if flags.Klassen[4420] {
		t.Errorf("class 4420 (not pooled) displayable = true, want false")
	}
	// recon perm + reconstructed element -> selectable
	if !flags.Teachers[5009] {
		t.Errorf("teacher 5009 displayAllowed = false, want true (recon + reconstructed)")
	}
	if !flags.Subjects[7] {
		t.Errorf("subject 7 displayAllowed = false, want true (recon + reconstructed)")
	}
	// recon perm but not reconstructed -> hidden
	if flags.Teachers[224] {
		t.Errorf("teacher 224 displayAllowed = true, want false (not reconstructed)")
	}
	// reconstructed type not granted
	if flags.Rooms[169] {
		t.Errorf("room 169 displayAllowed = true, want false (no recon room elements)")
	}

	// names are persisted for CLI fuzzy lookup
	for _, tc := range []struct {
		typ, want string
		id        int64
	}{
		{"TEACHER", "A. Hartley", 5009},
		{"TEACHER", "Ada Lovelace", 224},
		{"ROOM", "R101", 169},
		{"SUBJECT", "Mathematik 3", 7},
	} {
		if got := st.ElementName("testschool", tc.typ, tc.id); got != tc.want {
			t.Errorf("ElementName(%s,%d) = %q, want %q", tc.typ, tc.id, got, tc.want)
		}
	}
	// the zero-id entry is never persisted (recon_elements PK would collide)
	if n := st.ElementName("testschool", "TEACHER", 0); n != "" {
		t.Errorf("ElementName(TEACHER,0) = %q, want empty (zero ids are skipped)", n)
	}

	// the raw masterData is cached and re-stamped per request for
	// getTimetable2017 responses
	md := p.rewrittenMasterData("testschool", "owen")
	if md == nil {
		t.Fatal("rewrittenMasterData() = nil, want the cached login masterData")
	}
	ts, _ := md["timeStamp"].(int64)
	if ts <= 1 {
		t.Errorf("cached masterData timeStamp = %v, want a bumped unix-milli stamp > 1", md["timeStamp"])
	}
	if _, ok := md["teachers"].([]any); !ok {
		t.Errorf("cached masterData has no teachers list: %v", md)
	}

	// the login itself must have succeeded upstream and minted our session
	if !strings.Contains(rec.Body.String(), "upstreamsid") {
		t.Errorf("login response lost the upstream session id: %s", rec.Body.String())
	}
	if len(rec.Result().Header["Set-Cookie"]) == 0 {
		t.Error("no Set-Cookie on successful key login")
	}
}

func TestKeyLoginMasterDataBoostedSeesEveryElement(t *testing.T) {
	f := &fakeUpstream{userDataBody: masterDataLoginBody}
	p, st := newFakeProxyUpstream(t, f)
	seedKeyUser(t, st)
	if err := st.SetPerm("owen", store.FeatureBoosted, true); err != nil {
		t.Fatalf("grant boosted: %v", err)
	}

	flags := decodeMDFlags(t, mdLogin(t, p))
	for id, v := range flags.Teachers {
		if !v {
			t.Errorf("boosted: teacher %d displayAllowed = false, want true", id)
		}
	}
	for id, v := range flags.Rooms {
		if !v {
			t.Errorf("boosted: room %d displayAllowed = false, want true", id)
		}
	}
	for id, v := range flags.Subjects {
		if !v {
			t.Errorf("boosted: subject %d displayAllowed = false, want true", id)
		}
	}
	if !flags.Klassen[5000] {
		t.Error("boosted: pooled class 5000 displayable = false, want true")
	}
}

func TestKeyLoginMasterDataWithoutPermsHidesElements(t *testing.T) {
	f := &fakeUpstream{userDataBody: masterDataLoginBody}
	p, st := newFakeProxyUpstream(t, f)
	seedKeyUser(t, st)

	flags := decodeMDFlags(t, mdLogin(t, p))
	if flags.Teachers[5009] || flags.Rooms[169] || flags.Subjects[7] {
		t.Errorf("ungranted user sees elements: %+v", flags)
	}
	if !flags.Klassen[5000] {
		t.Error("pooled class must stay displayable even without any feature flag")
	}
}

func TestKeyLoginMasterDataUnpooledSchoolLeftUntouched(t *testing.T) {
	// A school whose classes are not in the pool has no klassen to rewrite, so
	// the upstream body must pass through byte-for-byte.
	f := &fakeUpstream{userDataBody: masterDataLoginBody}
	p, _ := newFakeProxyUpstream(t, f)
	// no users seeded -> pool empty
	rec := mdLogin(t, p)
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	result, _ := resp["result"].(map[string]any)
	md, _ := result["masterData"].(map[string]any)
	klassen, _ := md["klassen"].([]any)
	if len(klassen) == 0 {
		t.Fatalf("masterData missing/emptied: %s", rec.Body.String())
	}
	if k, _ := klassen[0].(map[string]any); k["displayable"] == true {
		t.Errorf("unpooled class 5000 marked displayable: %v", k)
	}
}

func TestRewrittenMasterDataEmptyWithoutLogin(t *testing.T) {
	p, _ := newTestProxy(t)
	if md := p.rewrittenMasterData("testschool", "owen"); md != nil {
		t.Errorf("rewrittenMasterData() = %v, want nil before any login", md)
	}
}

// TestMasterDataPrefersSubjectLongName: the school server sends a short code in
// "name" and the readable title in "longName". Every user-facing surface (iCal
// summary, /week page, admin element list) must show the long one.
func TestMasterDataPrefersSubjectLongName(t *testing.T) {
	f := &fakeUpstream{userDataBody: `{"jsonrpc":"2.0","id":"upstream","result":{
		"sessionId":"upstreamsid","personId":7,"personType":5,"klasseId":5000,
		"masterData":{"timeStamp":1,
			"klassen":[{"id":5000,"name":"10b"}],
			"rooms":[{"id":1,"name":"R1"}],
			"subjects":[
				{"id":7,"name":"M3","longName":"Mathematik 3"},
				{"id":8,"name":"Deutsch"},
				{"id":9,"name":"","longName":"Sport"}]}}}`}
	p, st := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st, "owen", 100, 5000)
	md := p.masterData("testschool")
	for id, want := range map[int64]string{7: "Mathematik 3", 8: "Deutsch", 9: "Sport"} {
		if got := elementName(md, "SUBJECT", id); got != want {
			t.Errorf("subject %d = %q, want %q", id, got, want)
		}
	}
	// The long name must also reach the persisted element registry, which the
	// admin UI and the displayable-element gating read from.
	byType, err := st.ListElementsWithNames("testschool")
	if err != nil {
		t.Fatalf("read master names: %v", err)
	}
	names := byType["SUBJECT"]
	if names[7] != "Mathematik 3" {
		t.Errorf("persisted subject 7 = %q, want %q", names[7], "Mathematik 3")
	}
}

// mustSetPerm grants (or revokes) the recon permission and fails the test if
// the store rejects it, so table cases can stay one-liners.
func mustSetPerm(t *testing.T, st *store.Store, user string, allowed bool) {
	t.Helper()
	if err := st.SetPerm(user, store.FeatureRecon, allowed); err != nil {
		t.Fatalf("set recon perm for %s=%v: %v", user, allowed, err)
	}
}

// fullMasterDataBody is a key-login answer with a realistic masterData block:
// several teachers, rooms and subjects so a test can tell "reconstructed"
// apart from "merely served by upstream", and a class outside the pool.
const fullMasterDataBody = `{"jsonrpc":"2.0","id":"upstream","result":{
	"sessionId":"upstreamsid","personId":7,"personType":5,"klasseId":5000,
	"masterData":{
		"timeStamp":1,
		"klassen":[{"id":5000,"name":"10b"},{"id":4420,"name":"10c"}],
		"teachers":[
			{"id":5009,"name":"A. Hartley"},
			{"id":224,"firstName":"Ada","lastName":"Lovelace"},
			{"id":225,"name":"Turing A"}
		],
		"rooms":[{"id":169,"name":"R101"},{"id":170,"name":"R102"},{"id":171,"name":"R103"}],
		"subjects":[{"id":7,"longName":"Mathematik 3"},{"id":8,"name":"Sport"}]
	}}}`

// A student who holds only the recon permission must be able to select every
// reconstructed room, teacher and subject - and nothing else. The app renders
// its teacher/room/subject pickers straight from these flags, so a false here
// hides a teacher's timetable in the UI even though the data exists.
func TestReconPermMakesEveryReconstructedElementSelectable(t *testing.T) {
	// Reconstructed: 2 of 3 teachers, 2 of 3 rooms, 1 of 2 subjects. The
	// leftovers (225, 171, 8) stand in for elements we never scanned.
	reconed := map[string][]int64{"TEACHER": {5009, 224}, "ROOM": {169, 170}, "SUBJECT": {7}}

	for _, tc := range []struct {
		name          string
		grant         func(t *testing.T, st *store.Store)
		wantTeachers  map[int64]bool
		wantRooms     map[int64]bool
		wantSubjects  map[int64]bool
		wantClass5000 bool
		wantClass4420 bool
	}{
		{
			name:          "no perm hides every teacher, room and subject",
			grant:         func(*testing.T, *store.Store) {},
			wantTeachers:  map[int64]bool{5009: false, 224: false, 225: false},
			wantRooms:     map[int64]bool{169: false, 170: false, 171: false},
			wantSubjects:  map[int64]bool{7: false, 8: false},
			wantClass5000: true,
			wantClass4420: false,
		},
		{
			name:          "recon perm exposes every reconstructed element",
			grant:         func(t *testing.T, st *store.Store) { mustSetPerm(t, st, "owen", true) },
			wantTeachers:  map[int64]bool{5009: true, 224: true, 225: false},
			wantRooms:     map[int64]bool{169: true, 170: true, 171: false},
			wantSubjects:  map[int64]bool{7: true, 8: false},
			wantClass5000: true,
			wantClass4420: false,
		},
		{
			name:          "the global recon perm covers users without their own row",
			grant:         func(t *testing.T, st *store.Store) { mustSetPerm(t, st, store.GlobalPermUser(), true) },
			wantTeachers:  map[int64]bool{5009: true, 224: true, 225: false},
			wantRooms:     map[int64]bool{169: true, 170: true, 171: false},
			wantSubjects:  map[int64]bool{7: true, 8: false},
			wantClass5000: true,
			wantClass4420: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeUpstream{userDataBody: fullMasterDataBody}
			p, st := newFakeProxyUpstream(t, f)
			seedKeyUser(t, st)
			if err := st.SaveReconElements("testschool", reconed); err != nil {
				t.Fatalf("save recon elements: %v", err)
			}
			tc.grant(t, st)

			flags := decodeMDFlags(t, mdLogin(t, p))
			for id, want := range tc.wantTeachers {
				if got := flags.Teachers[id]; got != want {
					t.Errorf("teacher %d displayAllowed = %v, want %v", id, got, want)
				}
			}
			for id, want := range tc.wantRooms {
				if got := flags.Rooms[id]; got != want {
					t.Errorf("room %d displayAllowed = %v, want %v", id, got, want)
				}
			}
			for id, want := range tc.wantSubjects {
				if got := flags.Subjects[id]; got != want {
					t.Errorf("subject %d displayAllowed = %v, want %v", id, got, want)
				}
			}
			if got := flags.Klassen[5000]; got != tc.wantClass5000 {
				t.Errorf("pooled class 5000 displayable = %v, want %v", got, tc.wantClass5000)
			}
			if got := flags.Klassen[4420]; got != tc.wantClass4420 {
				t.Errorf("unpooled class 4420 displayable = %v, want %v", got, tc.wantClass4420)
			}
		})
	}
}

// Selectability has to follow the permission, not the reconstruction: the same
// rooms and teachers must appear and disappear as the perm is granted and
// revoked, otherwise a revoked user keeps browsing other people's timetables.
func TestReconPermToggleFlipsRoomAndTeacherSelectability(t *testing.T) {
	f := &fakeUpstream{userDataBody: fullMasterDataBody}
	p, st := newFakeProxyUpstream(t, f)
	seedKeyUser(t, st)
	if err := st.SaveReconElements("testschool", map[string][]int64{
		"TEACHER": {5009}, "ROOM": {169, 170},
	}); err != nil {
		t.Fatalf("save recon elements: %v", err)
	}

	// selectable := the reconstructed rooms/teachers, and only those
	wantHidden := map[int64]bool{169: false, 170: false, 5009: false}
	wantShown := map[int64]bool{169: true, 170: true, 5009: true}
	check := func(step string, want map[int64]bool) {
		t.Helper()
		flags := decodeMDFlags(t, mdLogin(t, p))
		for id, wantSel := range want {
			got := flags.Rooms[id]
			if flags.Teachers[id] {
				got = flags.Teachers[id]
			}
			if got != wantSel {
				t.Errorf("%s: element %d selectable = %v, want %v", step, id, got, wantSel)
			}
		}
	}

	check("before the perm", wantHidden)
	if err := st.SetPerm("owen", store.FeatureRecon, true); err != nil {
		t.Fatalf("grant recon: %v", err)
	}
	check("with the recon perm", wantShown)
	if err := st.SetPerm("owen", store.FeatureRecon, false); err != nil {
		t.Fatalf("revoke recon: %v", err)
	}
	check("after revoking", wantHidden)
}

// Selectable must also mean servable: a recon user who can pick a room or a
// teacher must actually get that timetable back, and lose it on revoke.
// Opening a pooled class you do not attend must still ship *your* masterData.
// The forward runs under the class owner's auth, so upstream answers with the
// owner's view, and the app caches that block - which would wipe the recon
// user's working teacher/room/subject pickers after any pooled class view.
func TestPooledClassResponseCarriesRequesterMasterData(t *testing.T) {
	// What a student account gets back from upstream inside a timetable
	// answer: its class displayable, every element displayAllowed=false.
	const ownerView = `{"jsonrpc":"2.0","id":"upstream","result":{
		"timetable":{"displayableStartDate":"2026-09-21","displayableEndDate":"2026-09-27","periods":[]},
		"masterData":{"timeStamp":1,
			"klassen":[{"id":5000,"name":"10b","displayable":true},{"id":4420,"name":"10c","displayable":true}],
			"teachers":[{"id":5009,"displayAllowed":false},{"id":225,"displayAllowed":false}],
			"rooms":[{"id":169,"displayAllowed":false},{"id":171,"displayAllowed":false}],
			"subjects":[{"id":7,"displayAllowed":false}]}}}`
	f := &fakeUpstream{userDataBody: fullMasterDataBody, timetable: ownerView}
	p, st := newFakeProxyUpstream(t, f)
	seedKeyUser(t, st) // owen: class 5000, replayable, so the forward works
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

	tt := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(5000, "CLASS", "2026-09-21", "2026-09-27", "sam")))
	flags := decodeTimetableMDFlags(t, tt)
	if !flags.Teachers[5009] || !flags.Rooms[169] || !flags.Subjects[7] {
		t.Errorf("recon user got the owner's empty masterData: %+v", flags)
	}
	if flags.Teachers[225] || flags.Rooms[171] {
		t.Errorf("recon user sees elements that were never reconstructed: %+v", flags)
	}
}

func TestReconPermSelectableElementsAreServable(t *testing.T) {
	f := &fakeUpstream{}
	f.setTimetable(t, []map[string]any{reconPeriod(50, "2026-09-21", "08:00", "08:45", 5009, 169, 7)})
	p, st := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st, "owen", 7, 5000)
	seedSessionUser(t, st, "sam", 5, 11, 5000)
	if err := st.SaveReconElements("testschool", map[string][]int64{
		"TEACHER": {5009}, "ROOM": {169}, "SUBJECT": {7},
	}); err != nil {
		t.Fatalf("save recon elements: %v", err)
	}

	// 2 = teacher, 3 = subject, 4 = room
	get := func(user, elType, elID string) int {
		rec := restReq(t, p, user, "GET",
			"/WebUntis/api/public/timetable/weekly/data?elementType="+elType+
				"&elementId="+elID+"&date=2026-09-21&school=testschool", "", "")
		return rec.Code
	}

	if code := get("sam", "2", "5009"); code != 403 {
		t.Errorf("recon user, reconstructed teacher, no perm: status = %d, want 403", code)
	}
	if err := st.SetPerm("sam", store.FeatureRecon, true); err != nil {
		t.Fatalf("grant recon: %v", err)
	}
	for _, tc := range []struct{ elType, elID, what string }{
		{"2", "5009", "teacher"},
		{"4", "169", "room"},
		{"3", "7", "subject"},
	} {
		if code := get("sam", tc.elType, tc.elID); code != 200 {
			t.Errorf("recon user, reconstructed %s: status = %d, want 200", tc.what, code)
		}
	}
	// a room we never reconstructed stays out of reach even with the perm
	if code := get("sam", "4", "171"); code != 403 {
		t.Errorf("recon user, non-reconstructed room 171: status = %d, want 403", code)
	}
	if err := st.SetPerm("sam", store.FeatureRecon, false); err != nil {
		t.Fatalf("revoke recon: %v", err)
	}
	if code := get("sam", "4", "169"); code != 403 {
		t.Errorf("after revoking recon, room 169: status = %d, want 403", code)
	}
}

// decodeTimetableMDFlags reads the masterData the app gets with a
// getTimetable2017 answer - the payload its teacher/room pickers are built
// from after login.
func decodeTimetableMDFlags(t *testing.T, rec *httptest.ResponseRecorder) mdFlags {
	t.Helper()
	return decodeMDFlagsFrom(t, rec.Body.Bytes())
}

// A recon permission granted (or revoked) after login must take effect on the
// next request. Caching the rewritten masterData baked the flags in at login
// time, so the menus kept showing every element the school serves.
func TestReconPermAppliesWithoutRelogin(t *testing.T) {
	f := &fakeUpstream{userDataBody: fullMasterDataBody}
	p, st := newFakeProxyUpstream(t, f)
	seedKeyUser(t, st)
	if err := st.SaveReconElements("testschool", map[string][]int64{
		"TEACHER": {5009}, "ROOM": {169},
	}); err != nil {
		t.Fatalf("save recon elements: %v", err)
	}
	// log in *before* the permission exists
	mdLogin(t, p)

	// a reconstructed teacher/room timetable is what the app asks for next
	tt := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(5009, "TEACHER", "2026-09-21", "2026-09-27", "owen")))

	flags := decodeTimetableMDFlags(t, tt)
	if flags.Rooms[169] || flags.Teachers[5009] {
		t.Errorf("before the perm, elements already selectable: %+v", flags)
	}

	if err := st.SetPerm("owen", store.FeatureRecon, true); err != nil {
		t.Fatalf("grant recon: %v", err)
	}
	tt = internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(5009, "TEACHER", "2026-09-21", "2026-09-27", "owen")))
	flags = decodeTimetableMDFlags(t, tt)
	if !flags.Rooms[169] || !flags.Teachers[5009] {
		t.Errorf("after granting recon, rooms/teachers not selectable: %+v", flags)
	}
	if flags.Rooms[171] || flags.Teachers[225] {
		t.Errorf("non-reconstructed elements became selectable: %+v", flags)
	}

	if err := st.SetPerm("owen", store.FeatureRecon, false); err != nil {
		t.Fatalf("revoke recon: %v", err)
	}
	tt = internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(5009, "TEACHER", "2026-09-21", "2026-09-27", "owen")))
	flags = decodeTimetableMDFlags(t, tt)
	if flags.Rooms[169] || flags.Teachers[5009] {
		t.Errorf("after revoking recon, elements still selectable: %+v", flags)
	}
}

// The cached masterData used to be global, so the flags of whoever logged in
// last were served to everyone: a boosted colleague logging in turned every
// teacher and room selectable for everybody. Each user must only ever see
// their own permissions.
func TestMasterDataCacheIsNotSharedBetweenUsers(t *testing.T) {
	f := &fakeUpstream{userDataBody: fullMasterDataBody}
	p, st := newFakeProxyUpstream(t, f)
	seedKeyUser(t, st)
	if err := st.SaveReconElements("testschool", map[string][]int64{
		"TEACHER": {5009, 224}, "ROOM": {169, 170},
	}); err != nil {
		t.Fatalf("save recon elements: %v", err)
	}
	if err := st.SetPerm("owen", store.FeatureRecon, true); err != nil {
		t.Fatalf("grant recon to owen: %v", err)
	}
	if err := st.UpsertUser(&store.User{
		Username: "boosty", Method: "key", School: "testschool",
		PersonType: 5, PersonID: 8, ClassID: 5000, ClassName: "10b",
	}); err != nil {
		t.Fatalf("seed boosted user: %v", err)
	}
	if err := st.SetPerm("boosty", store.FeatureBoosted, true); err != nil {
		t.Fatalf("grant boosted: %v", err)
	}

	// boosted colleague logs in last: its flags are all-true
	mdLogin(t, p, "boosty")

	// owen must still only see what recon reconstructs
	tt := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(5009, "TEACHER", "2026-09-21", "2026-09-27", "owen")))
	flags := decodeTimetableMDFlags(t, tt)
	if !flags.Rooms[169] || !flags.Teachers[5009] {
		t.Errorf("owen lost his reconstructed elements: %+v", flags)
	}
	if flags.Rooms[171] || flags.Teachers[225] {
		t.Errorf("owen sees the boosted user's flags: %+v", flags)
	}

	// and the boosted user still sees everything
	tt = internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017",
		ttParam(5009, "TEACHER", "2026-09-21", "2026-09-27", "boosty")))
	flags = decodeTimetableMDFlags(t, tt)
	for id, v := range flags.Rooms {
		if !v {
			t.Errorf("boosted user: room %d not selectable", id)
		}
	}
	for id, v := range flags.Teachers {
		if !v {
			t.Errorf("boosted user: teacher %d not selectable", id)
		}
	}
}
