package proxy

import (
	"encoding/json"
	"net/http"
	"testing"

	"untis-proxy/internal/store"
)

func TestMappedCSVPreviewAndApply(t *testing.T) {
	p, st := adminEventProxy(t)
	req := mappedCSV{Text: "ignored\nTopic;Login;Day;Begins;Ends;Notes\n\"Show; practice\";dee;24/11/2026;9:00;10:00;\"first\nsecond\"\n",
		Delimiter: ";", Header: 2, First: 3, DateFormat: "2/1/2006", Fields: map[string]csvField{
			"title": {Column: 1}, "student": {Column: 2}, "date": {Column: 3}, "start": {Column: 4}, "end": {Column: 5}, "description": {Column: 6}, "room": {Value: "Aula"},
		}}
	call := func(user, dest, phase string, want int) []byte {
		t.Helper()
		b, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		out := doAdmin(t, p, user, http.MethodPost, "/api/admin/events/import?format=mapped-csv&school=testschool&destination="+dest+"&phase="+phase, string(b))
		if out.Code != want {
			t.Fatalf("%s status %d: %s", phase, out.Code, out.Body.String())
		}
		return out.Body.Bytes()
	}
	events := func() []store.StudentEvent {
		t.Helper()
		got, err := st.AllStudentEventsForRange("testschool", "2026-11-24", "2026-11-24")
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	call("dee", "events", "preview", http.StatusForbidden)
	call("adm", "events", "columns", 200)
	var preview struct {
		Rows   []mappedEventRow
		Errors int
	}
	if err := json.Unmarshal(call("adm", "events", "preview", 200), &preview); err != nil {
		t.Fatal(err)
	}
	if len(preview.Rows) != 1 || preview.Errors != 0 || preview.Rows[0].Title != "Show; practice" || preview.Rows[0].Line != 3 || len(events()) != 0 {
		t.Fatalf("unexpected preview or premature save: %+v", preview)
	}
	call("adm", "events", "apply", 200)
	call("adm", "events", "apply", 200)
	got := events()
	if len(got) != 1 || got[0].Room != "Aula" || got[0].Description != "first\nsecond" || got[0].StartTime != "09:00" {
		t.Fatalf("mapped fields were not saved: %+v", got)
	}
	delete(req.Fields, "start")
	delete(req.Fields, "end")
	call("dee", "absences", "apply", http.StatusForbidden)
	call("adm", "absences", "preview", 200)
	readAbsences := func() []store.EventAbsence {
		t.Helper()
		a, err := st.EventAbsences("testschool", sharedEventKey(got[0]))
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	if len(readAbsences()) != 0 {
		t.Fatal("preview marked a student absent")
	}
	req.Fields["student"] = csvField{Value: "sam"}
	call("adm", "absences", "apply", http.StatusBadRequest)
	req.Fields["student"] = csvField{Column: 2}
	call("adm", "absences", "apply", 200)
	absences := readAbsences()
	if len(absences) != 1 || absences[0].Username != "dee" || absences[0].StartTime != "T09:00" || absences[0].EndTime != "T10:00" {
		t.Fatalf("unexpected absence: %+v", absences)
	}
}

func TestMappedCSVRejectsBadQuotesAndAllowsHeaderless(t *testing.T) {
	records, err := readMappedCSV(mappedCSV{Text: "\ufeffdee\tAula\n", Delimiter: "\t"})
	if err != nil || len(records) != 1 || records[0].Values[0] != "dee" {
		t.Fatalf("headerless TSV: %+v, %v", records, err)
	}
	if _, err := readMappedCSV(mappedCSV{Text: "dee,\"unfinished", Delimiter: ","}); err == nil {
		t.Fatal("malformed CSV accepted")
	}
}
