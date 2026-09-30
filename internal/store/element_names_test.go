package store

import (
	"path/filepath"
	"testing"
)

// The name catalog and the reconstructed-element set used to be the same table,
// so persisting the school's names made every teacher/room/subject count as
// reconstructed - and therefore selectable in the app. Names go to
// element_names; recon_elements stays limited to scanned ids.
func TestMasterNamesDoNotPolluteReconSet(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "names.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.UpsertElementNames("testschool", "TEACHER", map[int64]string{
		5009: "A. Hartley", 225: "Turing A",
	}); err != nil {
		t.Fatalf("UpsertElementNames: %v", err)
	}
	if err := st.UpsertElementNames("testschool", "ROOM", map[int64]string{170: "R102"}); err != nil {
		t.Fatalf("UpsertElementNames: %v", err)
	}
	if err := st.SaveMasterNames("testschool", "SUBJECT", map[int64]string{8: "Sport"}); err != nil {
		t.Fatalf("SaveMasterNames: %v", err)
	}

	elems, err := st.LoadReconElements("testschool")
	if err != nil {
		t.Fatalf("LoadReconElements: %v", err)
	}
	for typ, ids := range elems {
		t.Errorf("names leaked %d %s rows into the reconstructed set: %v", len(ids), typ, ids)
	}

	// the names are all still resolvable
	for _, tc := range []struct {
		typ, want string
		id        int64
	}{
		{"TEACHER", "A. Hartley", 5009},
		{"TEACHER", "Turing A", 225},
		{"ROOM", "R102", 170},
		{"SUBJECT", "Sport", 8},
	} {
		if got := st.ElementName("testschool", tc.typ, tc.id); got != tc.want {
			t.Errorf("ElementName(%s,%d) = %q, want %q", tc.typ, tc.id, got, tc.want)
		}
	}
	if id, err := st.LookupElement("testschool", "TEACHER", "Turing"); err != nil || id != 225 {
		t.Errorf("LookupElement(Turing) = %d, %v; want 225, nil", id, err)
	}
	all, err := st.ListElementsWithNames("testschool")
	if err != nil {
		t.Fatalf("ListElementsWithNames: %v", err)
	}
	if got := len(all["TEACHER"]); got != 2 {
		t.Errorf("admin picker sees %d teachers, want 2: %v", got, all["TEACHER"])
	}

	// a scan persists the real set, and it is the only thing that does
	if err := st.SaveReconElements("testschool", map[string][]int64{"TEACHER": {5009}}); err != nil {
		t.Fatalf("SaveReconElements: %v", err)
	}
	elems, _ = st.LoadReconElements("testschool")
	if len(elems["TEACHER"]) != 1 || elems["TEACHER"][0] != 5009 {
		t.Errorf("reconstructed teacher set = %v, want [5009]", elems["TEACHER"])
	}
	if got := st.ElementName("testschool", "TEACHER", 225); got != "Turing A" {
		t.Errorf("ElementName lost the catalog entry: %q", got)
	}
}

// Upgrading an existing database must move the names across and clear the
// polluted set, so the next sweep rebuilds it from real class periods.
func TestMigrationMovesNamesOutOfReconSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// simulate a pre-upgrade database: the migration has not run yet, and names
	// and ids share recon_elements
	if _, err := st.db.Exec(`DELETE FROM settings WHERE key=?`, elementNamesMigrated); err != nil {
		t.Fatalf("clear migration flag: %v", err)
	}
	if _, err := st.db.Exec(`INSERT INTO recon_elements (el_type, el_id, name, school)
		VALUES ('TEACHER', 5009, 'A. Hartley', 'testschool'), ('TEACHER', 999, 'Stale', 'testschool')`); err != nil {
		t.Fatalf("seed legacy rows: %v", err)
	}
	if _, err := st.db.Exec(`INSERT INTO recon_scan (school, class_id, scan_until)
		VALUES ('testschool', 5000, '2026-10-19')`); err != nil {
		t.Fatalf("seed scan progress: %v", err)
	}
	if err := st.db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := st2.ElementName("testschool", "TEACHER", 5009); got != "A. Hartley" {
		t.Errorf("ElementName after migration = %q, want %q", got, "A. Hartley")
	}
	elems, _ := st2.LoadReconElements("testschool")
	if len(elems["TEACHER"]) != 0 {
		t.Errorf("polluted reconstructed set survived the migration: %v", elems["TEACHER"])
	}
	// the class scan progress is dropped too, otherwise the sweep would resume
	// and leave the set empty
	scan, err := st2.ReconScan("testschool")
	if err != nil {
		t.Fatalf("ReconScan: %v", err)
	}
	if len(scan) != 0 {
		t.Errorf("scan progress survived the migration, the set would stay empty: %v", scan)
	}
}
