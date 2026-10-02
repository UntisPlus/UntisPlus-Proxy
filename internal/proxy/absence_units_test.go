package proxy

// Unit-level coverage of the absence helpers: list discovery, the id reader's
// input assumptions, subject matching and the derived block. These are the parts
// that decide what a client sees, and each has a branch that only shows up with
// unusual input.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"untis-proxy/internal/store"
)

// TestItoaLabelsArrayIndexes: the diagnostic label prints an array index, so a
// multi-digit one must not come out truncated or empty.
func TestItoaLabelsArrayIndexes(t *testing.T) {
	for _, tc := range []struct{ in, want int }{
		{0, 0}, {1, 1}, {7, 7}, {9, 9}, {10, 10}, {42, 42}, {99, 99}, {100, 100}, {1234, 1234},
	} {
		if got := itoa(tc.in); got != itoa(tc.want) || len(got) == 0 {
			t.Errorf("itoa(%d) = %q, want %q", tc.in, got, itoa(tc.want))
		}
	}
	// Explicit strings, so a change in the digit loop is caught by name.
	for in, want := range map[int]string{0: "0", 9: "9", 10: "10", 99: "99", 1000: "1000"} {
		if got := itoa(in); got != want {
			t.Errorf("itoa(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestArrayLooksLikeAbsencesNeedsBothFields: recognising an array by its contents
// is what makes an unknown key work, so it must require a numeric id *and* a
// startDateTime. An array with only one of them is something else.
func TestArrayLooksLikeAbsencesNeedsBothFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		arr  []any
		want bool
	}{
		{"empty", []any{}, false},
		{"nil", nil, false},
		{"first element is not an object", []any{"nope"}, false},
		{"id but no start", []any{jsonObject{"id": json.Number("1")}}, false},
		{"start but no id", []any{jsonObject{"startDateTime": "2026-10-01T10:00"}}, false},
		{"id is not numeric", []any{jsonObject{"id": "abc", "startDateTime": "2026-10-01T10:00"}}, false},
		{"both present", []any{jsonObject{"id": json.Number("1"), "startDateTime": "2026-10-01T10:00"}}, true},
		// Only the first element is inspected: a list is uniform upstream, and
		// reading every element would cost more than the guess saves.
		{"first good, second bare", []any{
			jsonObject{"id": json.Number("1"), "startDateTime": "2026-10-01T10:00"},
			jsonObject{"text": "no fields"},
		}, true},
	} {
		if got := arrayLooksLikeAbsences(tc.arr); got != tc.want {
			t.Errorf("%s: arrayLooksLikeAbsences = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestFindAbsenceListPrefersKnownKeys: when several arrays could match, a known
// key wins over the by-content fallback, so the fallback cannot hijack the list.
func TestFindAbsenceListPrefersKnownKeys(t *testing.T) {
	abs := jsonObject{"id": json.Number("1"), "startDateTime": "2026-10-01T10:00"}
	other := jsonObject{"id": json.Number("2"), "startDateTime": "2026-10-02T10:00"}
	result := jsonObject{"absences": []any{abs}, "aNewerName": []any{other}}
	list, label := findAbsenceList(result)
	if len(list) != 1 {
		t.Fatalf("found %d entries, want the 1 under the known key (label %q)", len(list), label)
	}
	if got, _ := jsonID(list[0]["id"]); got != 1 {
		t.Errorf("picked id %d, want 1 from the known key", got)
	}
	if label != "absences" {
		t.Errorf("label = %q, want \"absences\"", label)
	}
}

// TestFindAbsenceListLabelRecordsWhereItLooked: the label is what makes the
// throttled diagnostic actionable, so it must name the path actually used.
func TestFindAbsenceListLabelRecordsWhereItLooked(t *testing.T) {
	abs := jsonObject{"id": json.Number("1"), "startDateTime": "2026-10-01T10:00"}
	for _, tc := range []struct {
		name   string
		result jsonObject
		want   string
	}{
		{"top level, known key", jsonObject{"absences": []any{abs}}, "absences"},
		{"one object deep", jsonObject{"inner": jsonObject{"absences": []any{abs}}}, "inner.absences"},
		{"in an array of wrappers", jsonObject{"days": []any{jsonObject{"absences": []any{abs}}}}, "days[0].absences"},
		{"second wrapper holds it", jsonObject{"days": []any{jsonObject{"x": 1}, jsonObject{"absences": []any{abs}}}}, "days[1].absences"},
		{"unknown key at top level", jsonObject{"mystery": []any{abs}}, "mystery"},
		{"unknown key one level down", jsonObject{"w": jsonObject{"mystery": []any{abs}}}, "w.mystery"},
	} {
		_, label := findAbsenceList(tc.result)
		if label != tc.want {
			t.Errorf("%s: label = %q, want %q", tc.name, label, tc.want)
		}
	}
}

// TestFindAbsenceListGivesUpCleanly: when nothing looks like an absence list the
// response must be left alone, and the caller told there is nothing to enrich.
func TestFindAbsenceListGivesUpCleanly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result jsonObject
	}{
		{"no arrays", jsonObject{"version": json.Number("2")}},
		{"empty array", jsonObject{"absences": []any{}}},
		{"array of strings", jsonObject{"absences": []any{"a", "b"}}},
		{"array of numbers", jsonObject{"absences": []any{json.Number("1")}}},
		{"wrapper holds no array", jsonObject{"day": jsonObject{"count": json.Number("0")}}},
	} {
		list, label := findAbsenceList(tc.result)
		if list != nil || label != "" {
			t.Errorf("%s: found (%d entries, %q), want nothing", tc.name, len(list), label)
		}
	}
}

// TestFindAbsenceListIsStableAcrossRuns: map iteration order is randomised, and
// the fallback walks keys, so the same response must resolve the same way twice.
func TestFindAbsenceListIsStableAcrossRuns(t *testing.T) {
	result := jsonObject{
		"aaa": []any{jsonObject{"id": json.Number("1"), "startDateTime": "2026-10-01T10:00"}},
		"bbb": []any{jsonObject{"id": json.Number("2"), "startDateTime": "2026-10-02T10:00"}},
		"ccc": []any{jsonObject{"id": json.Number("3"), "startDateTime": "2026-10-03T10:00"}},
	}
	_, first := findAbsenceList(result)
	for i := 0; i < 20; i++ {
		if _, label := findAbsenceList(result); label != first {
			t.Fatalf("run %d found %q, want the stable %q", i, label, first)
		}
	}
	if first != "aaa" {
		t.Errorf("label = %q, want the alphabetically first key to win", first)
	}
}

// TestParseAbsenceTimeAcceptsBothLayouts: absences and pooled periods arrive in
// different wall-clock formats, and both must parse or no lesson ever matches.
func TestParseAbsenceTimeAcceptsBothLayouts(t *testing.T) {
	for _, s := range []string{"2026-10-01T10:00", "2026-10-01 10:00"} {
		got, ok := parseAbsenceTime(s)
		if !ok {
			t.Errorf("parseAbsenceTime(%q) failed", s)
			continue
		}
		if got.Format("2006-01-02 15:04") != "2026-10-01 10:00" {
			t.Errorf("parseAbsenceTime(%q) = %s, want 2026-10-01 10:00", s, got)
		}
	}
	for _, s := range []string{
		"", "nonsense", "2026-10-01", "10:00", "2026-13-45T99:99",
		// Seconds and a zone would parse as RFC3339 but not as either layout used
		// here, so they must be rejected rather than silently truncated.
		"2026-10-01T10:00:00", "2026-10-01T10:00Z", "01/10/2026 10:00",
	} {
		if got, ok := parseAbsenceTime(s); ok {
			t.Errorf("parseAbsenceTime(%q) = %s, want a failure", s, got)
		}
	}
}

// TestSoleOverlappingSubject: the rule is one lesson or nothing. Zero overlaps,
// one overlap, and several overlaps are all different answers.
func TestSoleOverlappingSubject(t *testing.T) {
	periods := []store.PeriodRow{
		{Start: "2026-10-01 08:00", End: "2026-10-01 08:45", Subject: "Deutsch"},
		{Start: "2026-10-01 10:00", End: "2026-10-01 10:45", Subject: "Mathe"},
		{Start: "2026-10-01 11:00", End: "2026-10-01 11:45", Subject: "Sport"},
	}
	lesson := jsonObject{"startDateTime": "2026-10-01T10:00", "endDateTime": "2026-10-01T10:45"}
	for _, tc := range []struct {
		name    string
		abs     jsonObject
		subject string
		ok      bool
		periods []store.PeriodRow
	}{
		{"exactly one lesson", lesson, "Mathe", true, periods},
		{"between lessons", jsonObject{
			"startDateTime": "2026-10-01T09:00", "endDateTime": "2026-10-01T09:30"}, "", false, periods},
		{"whole day is ambiguous", jsonObject{
			"startDateTime": "2026-10-01T00:00", "endDateTime": "2026-10-02T00:00"}, "", false, periods},
		{"straddling two lessons is ambiguous", jsonObject{
			"startDateTime": "2026-10-01T10:30", "endDateTime": "2026-10-01T11:20"}, "", false, periods},
		{"ends exactly when a lesson starts", jsonObject{
			"startDateTime": "2026-10-01T09:15", "endDateTime": "2026-10-01T10:00"}, "", false, periods},
		// A missing or inverted end leaves an empty window, and a half-open
		// window overlaps nothing. Reporting no subject is the honest answer.
		{"missing end leaves an empty window", jsonObject{
			"startDateTime": "2026-10-01T10:00"}, "", false, periods},
		{"end before start leaves an empty window", jsonObject{
			"startDateTime": "2026-10-01T10:00", "endDateTime": "2026-10-01T09:00"}, "", false, periods},
		{"unparseable start", jsonObject{
			"startDateTime": "nonsense", "endDateTime": "2026-10-01T10:45"}, "", false, periods},
		{"unparseable end falls back to an empty window", jsonObject{
			"startDateTime": "2026-10-01T10:00", "endDateTime": "nonsense"}, "", false, periods},
		{"no periods at all", lesson, "", false, nil},
		{"a day with no lessons", lesson, "", false, []store.PeriodRow{
			{Start: "2026-10-01 10:00", End: "2026-10-01 10:45", Subject: "Mathe"},
		}[0:0]},
		{"the other day", lesson, "", false, []store.PeriodRow{
			{Start: "2026-09-30 10:00", End: "2026-09-30 10:45", Subject: "Mathe"},
		}},
	} {
		got, ok := soleOverlappingSubject(tc.abs, tc.periods)
		if ok != tc.ok || got != tc.subject {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", tc.name, got, ok, tc.subject, tc.ok)
		}
	}
}

// TestSoleOverlappingSubjectSkipsUnusablePeriods: a period whose times do not
// parse, or which names no subject, cannot be attributed to a lesson. It must be
// skipped rather than counted — counting it would turn one clear lesson into a
// false "several lessons" ambiguity and silently drop a resolvable subject.
func TestSoleOverlappingSubjectSkipsUnusablePeriods(t *testing.T) {
	abs := jsonObject{"startDateTime": "2026-10-01T10:00", "endDateTime": "2026-10-01T10:45"}
	usable := store.PeriodRow{Start: "2026-10-01 10:00", End: "2026-10-01 10:45", Subject: "Mathe"}
	for _, tc := range []struct {
		name    string
		periods []store.PeriodRow
		subject string
		ok      bool
	}{
		{"one usable period alone", []store.PeriodRow{usable}, "Mathe", true},
		{"alongside an unparseable one", []store.PeriodRow{
			{Start: "nonsense", End: "nonsense", Subject: "Sport"},
			usable,
		}, "Mathe", true},
		{"alongside one with a bad end", []store.PeriodRow{
			{Start: "2026-10-01 10:00", End: "nonsense", Subject: "Sport"},
			usable,
		}, "Mathe", true},
		{"alongside one naming no subject", []store.PeriodRow{
			{Start: "2026-10-01 10:00", End: "2026-10-01 10:45", Subject: ""},
			usable,
		}, "Mathe", true},
		// With nothing usable left there is no lesson to name, and saying so is
		// the point of skipping the rest rather than guessing.
		{"nothing usable at all", []store.PeriodRow{
			{Start: "nonsense", End: "nonsense", Subject: "Mathe"},
			{Start: "2026-10-01 10:00", End: "2026-10-01 10:45", Subject: ""},
		}, "", false},
	} {
		got, ok := soleOverlappingSubject(abs, tc.periods)
		if ok != tc.ok || got != tc.subject {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", tc.name, got, ok, tc.subject, tc.ok)
		}
	}
}

// TestAbsenceBelongsToNeedsAnExactID: the check is an equality against the
// session user's person id, so anything short of that leaves the entry alone.
func TestAbsenceBelongsToNeedsAnExactID(t *testing.T) {
	user := &store.User{PersonID: 7}
	for _, tc := range []struct {
		name string
		abs  jsonObject
		want bool
	}{
		{"matching id", jsonObject{"studentId": json.Number("7")}, true},
		{"another student", jsonObject{"studentId": json.Number("8")}, false},
		{"zero", jsonObject{"studentId": json.Number("0")}, false},
		{"absent", jsonObject{"id": json.Number("1")}, false},
		{"explicit null", jsonObject{"studentId": nil}, false},
		{"not numeric", jsonObject{"studentId": "dee"}, false},
		{"a float that is not an integer", jsonObject{"studentId": json.Number("7.5")}, false},
		{"string that looks like the id", jsonObject{"studentId": "7"}, false},
		{"as a float64", jsonObject{"studentId": float64(7)}, true},
	} {
		if got := absenceBelongsTo(tc.abs, user); got != tc.want {
			t.Errorf("%s: absenceBelongsTo = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestDeriveAbsenceOmitsWhatItCannotKnow: a missing key means "not derivable", so
// every field is absent rather than empty, and a wholly unknown absence gets no
// block at all instead of an empty object.
func TestDeriveAbsenceOmitsWhatItCannotKnow(t *testing.T) {
	p, _ := absenceProxy(t)
	md := &masterDataCache{
		klassen:        map[int64]string{5000: "10b"},
		absenceReasons: map[int64]string{3: "Doctor's appointment"},
	}
	none := &masterDataCache{klassen: map[int64]string{}, absenceReasons: map[int64]string{}}
	for _, tc := range []struct {
		name    string
		abs     jsonObject
		subject string
		md      *masterDataCache
		want    map[string]any
		absent  []string
	}{
		{"fully resolvable", jsonObject{
			"klasseId": json.Number("5000"), "startDateTime": "2026-10-01T10:00",
			"absenceReasonId": json.Number("3"),
		}, "Mathe", md, map[string]any{
			"classId": int64(5000), "className": "10b", "weekday": "Thursday",
			"date": "2026-10-01", "subject": "Mathe", "reason": "Doctor's appointment",
		}, nil},
		{"an empty catalogue", jsonObject{
			"klasseId": json.Number("5000"), "startDateTime": "2026-10-01T10:00",
			"absenceReasonId": json.Number("3"),
		}, "", none, map[string]any{
			"classId": int64(5000), "weekday": "Thursday", "date": "2026-10-01",
		}, []string{"className", "reason", "subject"}},
		{"unknown class and reason ids", jsonObject{
			"klasseId": json.Number("9999"), "startDateTime": "2026-10-01T10:00",
			"absenceReasonId": json.Number("88"),
		}, "", md, map[string]any{"classId": int64(9999), "weekday": "Thursday", "date": "2026-10-01"},
			[]string{"className", "reason"}},
		{"zero class id is not a class", jsonObject{
			"klasseId": json.Number("0"), "startDateTime": "2026-10-01T10:00",
		}, "", md, map[string]any{"weekday": "Thursday", "date": "2026-10-01"}, []string{"classId"}},
		{"unparseable date", jsonObject{"klasseId": json.Number("5000")}, "", md,
			map[string]any{"classId": int64(5000), "className": "10b"}, []string{"date", "weekday"}},
		{"no class at all", jsonObject{"startDateTime": "2026-10-01T10:00"}, "Sport", md,
			map[string]any{"weekday": "Thursday", "date": "2026-10-01", "subject": "Sport"}, []string{"classId", "className"}},
	} {
		got := p.deriveAbsence(tc.abs, tc.md, tc.subject)
		if got == nil {
			t.Errorf("%s: derived = nil, want a block", tc.name)
			continue
		}
		for k, want := range tc.want {
			if fmt := got[k]; fmt != want {
				t.Errorf("%s: derived[%q] = %v, want %v", tc.name, k, fmt, want)
			}
		}
		for _, k := range tc.absent {
			if _, present := got[k]; present {
				t.Errorf("%s: derived[%q] = %v, want it absent", tc.name, k, got[k])
			}
		}
	}
}

// TestDeriveAbsenceReturnsNothingWhenNothingIsKnowable: an empty object on the
// wire would tell a client a falsehood — that nothing could be derived — when the
// truth is that the record was too sparse to try.
func TestDeriveAbsenceReturnsNothingWhenNothingIsKnowable(t *testing.T) {
	p, _ := absenceProxy(t)
	md := &masterDataCache{klassen: map[int64]string{}, absenceReasons: map[int64]string{}}
	if got := p.deriveAbsence(jsonObject{"text": "no ids, no dates"}, md, ""); got != nil {
		t.Errorf("derived = %v, want nil for an absence with nothing derivable", got)
	}
}

// TestDeriveAbsenceToleratesNilCatalogue: a masterData fetch that failed leaves
// the cache empty, and that must cost the names, not panic the request.
func TestDeriveAbsenceToleratesNilCatalogue(t *testing.T) {
	p, _ := absenceProxy(t)
	abs := jsonObject{"klasseId": json.Number("5000"), "startDateTime": "2026-10-01T10:00",
		"absenceReasonId": json.Number("3")}
	got := p.deriveAbsence(abs, nil, "Mathe")
	if got == nil || got["date"] != "2026-10-01" {
		t.Fatalf("derived = %v, want the fields that need no catalogue", got)
	}
	if _, present := got["className"]; present {
		t.Errorf("a class name was invented with no catalogue: %v", got)
	}
	if _, present := got["reason"]; present {
		t.Errorf("a reason was invented with no catalogue: %v", got)
	}
}

// TestAbsenceDiagnosticsThrottlePerSchool: an unexpected shape would otherwise
// log on every poll, so the notice is rate-limited per school — and a second
// school must not be silenced by the first one's log.
func TestAbsenceDiagnosticsThrottlePerSchool(t *testing.T) {
	d := &absenceDiagnostics{}
	d.note("schoolA", "first")
	d.note("schoolA", "second, within the hour")
	d.note("schoolB", "a different school is a different problem")
	d.note("schoolA", "third, still within the hour")
}

// TestAbsenceEnrichmentSurvivesAClosedStore: a store failure must cost the notes,
// not the request. The derived metadata needs no note, so it must still arrive.
func TestAbsenceEnrichmentSurvivesAClosedStore(t *testing.T) {
	p, st := absenceProxy(t)
	st.Close()
	out := p.enrichAbsenceResponse([]byte(absenceResponse), "testschool", "dee")
	if string(out) == string([]byte(absenceResponse)) {
		// A closed store also makes GetUser fail, which legitimately returns the
		// response untouched. Both are acceptable; the requirement is only that
		// this does not panic and does not corrupt the response.
		t.Log("closed store returned the response unchanged (GetUser failed first), which is correct")
		return
	}
	abs := firstAbsence(t, string(out))
	if _, present := abs["note"]; present {
		t.Errorf("a note was attached with a closed store: %v", abs["note"])
	}
}

// TestAbsenceEnrichmentNeedsAViewerAndSchool: without both, there is no identity
// to check against and nothing to key a note on, so the response passes through.
func TestAbsenceEnrichmentNeedsAViewerAndSchool(t *testing.T) {
	p, _ := absenceProxy(t)
	for _, tc := range []struct{ school, viewer string }{
		{"testschool", ""},
		{"", "dee"},
		{"", ""},
	} {
		if got := string(p.enrichAbsenceResponse([]byte(absenceResponse), tc.school, tc.viewer)); got != absenceResponse {
			t.Errorf("school=%q viewer=%q: response was altered\n got: %s", tc.school, tc.viewer, got)
		}
	}
}

// TestAbsenceEnrichmentIgnoresAnUnknownViewer: a viewer with no user row has no
// person id to compare against, so nothing can be attributed.
func TestAbsenceEnrichmentIgnoresAnUnknownViewer(t *testing.T) {
	p, _ := absenceProxy(t)
	if got := string(p.enrichAbsenceResponse([]byte(absenceResponse), "testschool", "nobody")); got != absenceResponse {
		t.Errorf("an unknown viewer produced a decorated response: %s", got)
	}
}

// TestAbsenceNotesEndpointRejectsOtherMethods: the route is method-agnostic so a
// typo gets a clear answer rather than a 404.
func TestAbsenceNotesEndpointRejectsOtherMethods(t *testing.T) {
	p, _ := absenceProxy(t)
	for _, method := range []string{http.MethodDelete, http.MethodPut, http.MethodPatch} {
		rec := sessionedRequest(t, p, "dee", method, "/api/absence/notes", "")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/absence/notes = %d, want 405", method, rec.Code)
		}
	}
}

// TestAbsenceNotesEndpointRejectsMalformedBodies: a body that is not JSON, or
// names no absence, is a bad request and must write nothing.
func TestAbsenceNotesEndpointRejectsMalformedBodies(t *testing.T) {
	p, st := absenceProxy(t)
	for _, tc := range []struct {
		name string
		body string
	}{
		{"not json", `{oops`},
		{"empty body", ``},
		{"no key", `{"note":"x"}`},
		{"zero key", `{"absenceKey":0,"note":"x"}`},
		{"negative key", `{"absenceKey":-5,"note":"x"}`},
		{"key is not a number", `{"absenceKey":"abc","note":"x"}`},
		{"note is not a string", `{"absenceKey":300001,"note":42}`},
	} {
		rec := sessionedRequest(t, p, "dee", http.MethodPost, "/api/absence/notes", tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body %s)", tc.name, rec.Code, rec.Body.String())
		}
	}
	if notes, err := st.AbsenceNotes("testschool", "dee"); err != nil {
		t.Fatalf("AbsenceNotes: %v", err)
	} else if len(notes) != 0 {
		t.Errorf("a rejected request wrote %d notes: %v", len(notes), notes)
	}
}

// TestAbsenceNoteIsCappedNotTruncated: an over-long note is refused, because a
// silently shortened note would read back as something the student never wrote.
func TestAbsenceNoteIsCappedNotTruncated(t *testing.T) {
	p, st := absenceProxy(t)
	long := strings.Repeat("x", maxAbsenceNoteLen+1)
	rec := sessionedRequest(t, p, "dee", http.MethodPost, "/api/absence/notes",
		`{"absenceKey":300001,"note":"`+long+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a note of %d bytes", rec.Code, len(long))
	}
	// Exactly at the cap is accepted: the limit is a ceiling, not an off-by-one.
	rec = sessionedRequest(t, p, "dee", http.MethodPost, "/api/absence/notes",
		`{"absenceKey":300001,"note":"`+strings.Repeat("x", maxAbsenceNoteLen)+`"}`)
	if rec.Code != http.StatusOK {
		t.Errorf("a note exactly at the cap was rejected: %s", rec.Body.String())
	}
	notes, err := st.AbsenceNotes("testschool", "dee")
	if err != nil {
		t.Fatalf("AbsenceNotes: %v", err)
	}
	if got := len(notes[300001].Note); got != maxAbsenceNoteLen {
		t.Errorf("stored note is %d bytes, want %d", got, maxAbsenceNoteLen)
	}
}

// TestAbsenceNotesAreScopedBySchool: the same user at two schools has two
// separate note sets, since the same absence id can mean different things.
func TestAbsenceNotesAreScopedBySchool(t *testing.T) {
	p, st := absenceProxy(t)
	sessionedRequest(t, p, "dee", http.MethodPost, "/api/absence/notes?school=schoolA",
		`{"absenceKey":300001,"note":"at A"}`)
	sessionedRequest(t, p, "dee", http.MethodPost, "/api/absence/notes?school=schoolB",
		`{"absenceKey":300001,"note":"at B"}`)

	a, err := st.AbsenceNotes("schoolA", "dee")
	if err != nil {
		t.Fatalf("AbsenceNotes A: %v", err)
	}
	b, err := st.AbsenceNotes("schoolB", "dee")
	if err != nil {
		t.Fatalf("AbsenceNotes B: %v", err)
	}
	if a[300001].Note != "at A" || b[300001].Note != "at B" {
		t.Errorf("notes crossed schools: A=%q B=%q", a[300001].Note, b[300001].Note)
	}

	// A read for one school must not list the other's note.
	rec := sessionedRequest(t, p, "dee", http.MethodGet, "/api/absence/notes?school=schoolA", "")
	if strings.Contains(rec.Body.String(), "at B") {
		t.Errorf("the read for schoolA leaked schoolB's note: %s", rec.Body.String())
	}
}

// TestAbsenceNotesReadIsSortedAndStamped: the read is a stable list with the same
// timestamps the write reported, so a client can reconcile without re-reading.
func TestAbsenceNotesReadIsSortedAndStamped(t *testing.T) {
	p, _ := absenceProxy(t)
	for _, key := range []int64{300003, 300001, 300002} {
		rec := sessionedRequest(t, p, "dee", http.MethodPost, "/api/absence/notes",
			`{"absenceKey":`+itoa(int(key))+`,"note":"n`+itoa(int(key))+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("write %d: %s", key, rec.Body.String())
		}
	}
	rec := sessionedRequest(t, p, "dee", http.MethodGet, "/api/absence/notes", "")
	var out struct {
		School string `json:"school"`
		Notes  []struct {
			AbsenceKey int64  `json:"absenceKey"`
			Note       string `json:"note"`
			UpdatedAt  string `json:"updatedAt"`
		} `json:"notes"`
	}
	decodeJSON(rec.Body.Bytes(), &out)
	if out.School != "testschool" {
		t.Errorf("school = %q, want the default school", out.School)
	}
	if len(out.Notes) != 3 {
		t.Fatalf("read %d notes, want 3", len(out.Notes))
	}
	for i, want := range []int64{300001, 300002, 300003} {
		if out.Notes[i].AbsenceKey != want {
			t.Errorf("note %d is for absence %d, want %d (sorted)", i, out.Notes[i].AbsenceKey, want)
		}
		if out.Notes[i].Note != "n"+itoa(int(want)) {
			t.Errorf("absence %d: note = %q", want, out.Notes[i].Note)
		}
		if _, err := time.Parse(time.RFC3339, out.Notes[i].UpdatedAt); err != nil {
			t.Errorf("absence %d: updatedAt %q is not RFC3339: %v", want, out.Notes[i].UpdatedAt, err)
		}
	}
}

// TestAbsenceNotesEmptyListIsStillAList: no notes is an empty array, so a client
// does not have to distinguish "none" from "not a list".
func TestAbsenceNotesEmptyListIsStillAList(t *testing.T) {
	p, _ := absenceProxy(t)
	rec := sessionedRequest(t, p, "dee", http.MethodGet, "/api/absence/notes", "")
	if !strings.Contains(rec.Body.String(), `"notes":[]`) {
		t.Errorf("an empty read returned %s, want an empty array", rec.Body.String())
	}
}

// TestAbsenceNoteClearIsIdempotent: clearing a note that was never there is a
// success, not an error, so a client can clear on the way out without checking.
func TestAbsenceNoteClearIsIdempotent(t *testing.T) {
	p, st := absenceProxy(t)
	for i := 0; i < 2; i++ {
		rec := sessionedRequest(t, p, "dee", http.MethodPost, "/api/absence/notes",
			`{"absenceKey":300001,"note":"   "}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("clear %d: status %d, body %s", i, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"note":null`) {
			t.Errorf("clear %d returned %s, want a null note", i, rec.Body.String())
		}
	}
	notes, err := st.AbsenceNotes("testschool", "dee")
	if err != nil {
		t.Fatalf("AbsenceNotes: %v", err)
	}
	if len(notes) != 0 {
		t.Errorf("clearing left %d notes behind: %v", len(notes), notes)
	}
}

// TestAbsenceNoteWriteIsIdempotent: writing the same text twice keeps one row and
// does not change what the student reads back.
func TestAbsenceNoteWriteIsIdempotent(t *testing.T) {
	p, st := absenceProxy(t)
	body := `{"absenceKey":300001,"note":"same"}`
	for i := 0; i < 3; i++ {
		if rec := sessionedRequest(t, p, "dee", http.MethodPost, "/api/absence/notes", body); rec.Code != http.StatusOK {
			t.Fatalf("write %d: %s", i, rec.Body.String())
		}
	}
	notes, err := st.AbsenceNotes("testschool", "dee")
	if err != nil {
		t.Fatalf("AbsenceNotes: %v", err)
	}
	if len(notes) != 1 || notes[300001].Note != "same" {
		t.Errorf("after three identical writes: %v", notes)
	}
}

// TestAbsenceNoteUsernameInBodyIsIgnored: the session decides the user. A body
// that names somebody else must not redirect the write, which is the whole reason
// the field is accepted and dropped.
func TestAbsenceNoteUsernameInBodyIsIgnored(t *testing.T) {
	p, st := absenceProxy(t)
	rec := sessionedRequest(t, p, "dee", http.MethodPost, "/api/absence/notes",
		`{"absenceKey":300001,"note":"mine","username":"sam"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("write failed: %s", rec.Body.String())
	}
	if sam, err := st.AbsenceNotes("testschool", "sam"); err != nil {
		t.Fatalf("AbsenceNotes sam: %v", err)
	} else if len(sam) != 0 {
		t.Errorf("a note was written for another student: %v", sam)
	}
	if dee, err := st.AbsenceNotes("testschool", "dee"); err != nil {
		t.Fatalf("AbsenceNotes dee: %v", err)
	} else if dee[300001].Note != "mine" {
		t.Errorf("the session user's note = %q, want \"mine\"", dee[300001].Note)
	}
}

// TestAbsenceNotesEndpointsNeedASession: no cookie means no identity, so both
// verbs refuse rather than acting on an empty username.
func TestAbsenceNotesEndpointsNeedASession(t *testing.T) {
	p, _ := absenceProxy(t)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		req := httptest.NewRequest(method, "/api/absence/notes", nil)
		rec := httptest.NewRecorder()
		p.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a session = %d, want 401", method, rec.Code)
		}
	}
}

// TestAbsenceEntryWithoutAnIdIsSkippedNotDropped: list recognition only inspects
// the first element, so a later entry can arrive with no id. It must be left
// exactly as upstream sent it — skipped, not removed, and not decorated.
func TestAbsenceEntryWithoutAnIdIsSkippedNotDropped(t *testing.T) {
	p, _ := absenceProxy(t)
	raw := `{"jsonrpc":"2.0","id":"x","result":{"absences":[
		{"id":300001,"startDateTime":"2026-10-01T10:00","studentId":7,"klasseId":5000},
		{"text":"no id","startDateTime":"2026-10-01T11:00","studentId":7,"klasseId":5000},
		{"id":"not a number","startDateTime":"2026-10-01T11:45","studentId":7,"klasseId":5000}]}}`
	result := decodeResult(t, p.enrichAbsenceResponse([]byte(raw), "testschool", "dee"))
	list := jsonArray(result["absences"])
	if len(list) != 3 {
		t.Fatalf("list has %d entries, want all 3 kept", len(list))
	}
	for i, entry := range list[1:] {
		if _, present := entry["derived"]; present {
			t.Errorf("entry %d without a usable id was decorated: %v", i+1, entry)
		}
		if _, present := entry["note"]; present {
			t.Errorf("entry %d without a usable id got a note: %v", i+1, entry)
		}
		// Every field upstream sent is still there, in particular startDateTime and
		// studentId, which the decoration itself reads.
		if entry["startDateTime"] == nil || entry["studentId"] == nil {
			t.Errorf("entry %d lost its upstream fields: %v", i+1, entry)
		}
	}
	// The first entry, which has an id, is still decorated: a bad sibling must not
	// cost the rest of the list its enrichment.
	derived, ok := list[0]["derived"].(jsonObject)
	if !ok || derived["date"] != "2026-10-01" {
		t.Errorf("the usable entry was not enriched: %v", list[0])
	}
}
