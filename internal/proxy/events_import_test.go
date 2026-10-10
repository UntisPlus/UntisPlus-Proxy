package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"untis-proxy/internal/store"
)

// importResult is the response shape the admin UI reads back.
type importResult struct {
	Created []struct {
		Username string `json:"username"`
		EventID  int64  `json:"eventId"`
	} `json:"created"`
	Skipped []map[string]any `json:"skipped"`
	Unknown []string         `json:"unknown"`
	Ignored []string         `json:"ignored"`
	NoTime  int              `json:"skippedTimes"`
}

func doImport(t *testing.T, p *Proxy, user, school, body string) (*importResult, int) {
	t.Helper()
	rec := doAdmin(t, p, user, http.MethodPost, "/api/admin/events/import?school="+school, body)
	if rec.Code != http.StatusOK {
		return nil, rec.Code
	}
	var out importResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("import response is not JSON: %v\n%s", err, rec.Body.String())
	}
	return &out, rec.Code
}

// TestAdminEventImportFansOutToTechnicians: one appointment naming several
// technicians becomes one private row per technician, a code that names no
// technician is ignored, and a code for somebody who has not signed up is filed
// under the folded code so it is there when they first log in.
func TestAdminEventImportFansOutToTechnicians(t *testing.T) {
	p, st := adminEventProxy(t)
	body := `{
	  "techniker":[
	    {"name":"Dee Student","code":"Dee"},
	    {"name":"Sam Student","code":"Sam"},
	    {"name":"New Person","code":"NewGuy"},
	    {"name":"Sve Cod","code":"SveCod"}
	  ],
	  "veranstaltungen":[
	    {"id":1,"datum":"07.08.2026","beginn":"10:00","ende":"12:00","titel":"GeKo",
	     "veranstalter":"Abc/Def","einsatz":["Dee","Sam"],"ort":"Aula","material":"Mappen"},
	    {"id":2,"datum":"08.08.2026","beginn":"10:00","ende":"12:00","titel":"Aufbau",
	     "veranstalter":"Abc","einsatz":["Dee","NewGuy","ShlM"],"ort":"","material":""},
	    {"id":3,"datum":"09.08.2026","beginn":"10:00","ende":"12:00","titel":"Abbau",
	     "veranstalter":"Abc","einsatz":["SveCod1 (nur A)"],"ort":"","material":""}
	  ]
	}`
	out, code := doImport(t, p, "adm", "testschool", body)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(out.Created) != 5 {
		t.Fatalf("created = %+v, want 5 (dee twice, sam, newguy, svecod)", out.Created)
	}
	if len(out.Skipped) != 0 {
		t.Errorf("skipped = %+v, want none on a first import", out.Skipped)
	}
	if len(out.Ignored) != 1 || out.Ignored[0] != "ShlM" {
		t.Errorf("ignored = %v, want [ShlM] (a code that names no technician)", out.Ignored)
	}
	if strings.Join(out.Unknown, ",") != "newguy,svecod" {
		t.Errorf("unknown = %v, want [newguy svecod] (the roster code, not the raw token)", out.Unknown)
	}

	// The known student's event carries the admin text, with the subject defaulted
	// because the appointment named none.
	dee, err := st.StudentEvents("testschool", "dee")
	if err != nil || len(dee) != 2 {
		t.Fatalf("dee events = %d (err %v), want 2", len(dee), err)
	}
	var geko *store.StudentEvent
	for i := range dee {
		if dee[i].Title == "GeKo" {
			geko = &dee[i]
		}
	}
	if geko == nil {
		t.Fatal("the GeKo event is missing for dee")
	}
	if geko.Subject != "Technik" || geko.Room != "Aula" || geko.Teacher != "Abc/Def" || geko.Description != "Mappen" {
		t.Errorf("GeKo event = %+v, want subject Technik, room Aula, teacher Abc/Def, material Mappen", *geko)
	}

	// The signed-up-later technicians are addressable by the folded roster code
	// now, not by the raw "SveCod1" token.
	for _, who := range []string{"newguy", "svecod"} {
		got, err := st.StudentEvents("testschool", who)
		if err != nil || len(got) != 1 {
			t.Fatalf("%s events = %d (err %v), want 1", who, len(got), err)
		}
	}

	// Re-importing the same export changes nothing: every slot is already filled.
	again, code := doImport(t, p, "adm", "testschool", body)
	if code != http.StatusOK {
		t.Fatalf("re-import status = %d", code)
	}
	if len(again.Created) != 0 || len(again.Skipped) != 5 {
		t.Errorf("re-import created %d, skipped %d; want 0 and 5", len(again.Created), len(again.Skipped))
	}
}

// TestAdminEventImportReadsLatin1CSV: the spreadsheet exports Windows-1252, so a
// raw file has to be decoded rather than read as UTF-8, and a code that resolves
// to an existing non-student account is dropped instead of filing an event on a
// teacher.
func TestAdminEventImportReadsLatin1CSV(t *testing.T) {
	p, st := adminEventProxy(t)
	csv := "Datum;Beginn;Ende;Dauer;Aufbau;Abbau;Nachmittag?;Ort;Titel;Material;Veranstalter;Interessierte;Einsatz;E\n" +
		"07.08.2026;10:00;12:00;2;;;;B\xfcro;GeKo;Mappen;Abc;Dee;Dee,Sam,adm;\n"
	out, code := doImport(t, p, "adm", "testschool", csv)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(out.Created) != 2 {
		t.Fatalf("created = %+v, want 2 (dee, sam)", out.Created)
	}
	if len(out.Ignored) != 1 || out.Ignored[0] != "adm" {
		t.Errorf("ignored = %v, want [adm] (an existing non-student)", out.Ignored)
	}
	dee, err := st.StudentEvents("testschool", "dee")
	if err != nil || len(dee) != 1 {
		t.Fatalf("dee events = %d (err %v), want 1", len(dee), err)
	}
	if dee[0].Room != "Büro" {
		t.Errorf("room = %q, want the Latin-1 decoded Büro", dee[0].Room)
	}
	if dee[0].Subject != "Technik" || dee[0].Title != "GeKo" {
		t.Errorf("event = %+v, want subject Technik and title GeKo", dee[0])
	}
}

func TestAdminEventImportRequiresAdminAndBody(t *testing.T) {
	p := adminEventProxy1(t)
	rec := doAdmin(t, p, "dee", http.MethodPost, "/api/admin/events/import?school=testschool", `{"veranstaltungen":[]}`)
	if rec.Code == http.StatusOK {
		t.Error("a student reached the import endpoint")
	}
	rec = doAdmin(t, p, "adm", http.MethodPost, "/api/admin/events/import?school=testschool", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty import status = %d, want 400", rec.Code)
	}
}

func TestFoldCodeAndImportDate(t *testing.T) {
	for in, want := range map[string]string{
		"AbcDef":          "abcdef",
		"AbcDef1 (nur A)": "abcdef1",
		"  A_b-c  ":       "abc",
		"Gr\xc3\xbcn":     "grun",
		"":                "",
	} {
		if got := foldCode(in); got != want {
			t.Errorf("foldCode(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"07.08.2026": "2026-08-07",
		"7.8.2026":   "2026-08-07",
		"2026-08-07": "2026-08-07",
		"":           "",
		"keine":      "",
	} {
		if got := normalizeImportDate(in); got != want {
			t.Errorf("normalizeImportDate(%q) = %q, want %q", in, got, want)
		}
	}
}
