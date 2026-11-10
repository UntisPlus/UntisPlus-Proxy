package store

import "testing"

// TestSchoolBlockingAndAllowNew covers the access controls behind the dashboard:
// a blocked flag that survives an upsert, an allow-new switch whose default
// preserves auto-registration, and the delete path.
func TestSchoolBlockingAndAllowNew(t *testing.T) {
	st := openTestStore(t)

	// Default is permissive: existing deployments auto-register new schools.
	if allow, err := st.AllowNewSchools(); err != nil || !allow {
		t.Fatalf("AllowNewSchools default = (%v, %v), want (true, nil)", allow, err)
	}
	if err := st.SetAllowNewSchools(false); err != nil {
		t.Fatalf("disable allow-new: %v", err)
	}
	if allow, err := st.AllowNewSchools(); err != nil || allow {
		t.Fatalf("AllowNewSchools after disable = (%v, %v), want (false, nil)", allow, err)
	}
	if err := st.SetAllowNewSchools(true); err != nil {
		t.Fatalf("enable allow-new: %v", err)
	}
	if allow, err := st.AllowNewSchools(); err != nil || !allow {
		t.Fatalf("AllowNewSchools after enable = (%v, %v), want (true, nil)", allow, err)
	}

	if err := st.UpsertSchool("other"); err != nil {
		t.Fatalf("upsert school: %v", err)
	}
	if err := st.SetSchoolBlocked("other", true); err != nil {
		t.Fatalf("block school: %v", err)
	}
	info, err := st.SchoolInfo("other")
	if err != nil || info == nil || !info.Blocked {
		t.Fatalf("SchoolInfo after block = (%+v, %v), want blocked", info, err)
	}

	// A blocked school that logs in again is upserted to bump last_seen; that
	// must not silently clear the block.
	if err := st.UpsertSchool("other"); err != nil {
		t.Fatalf("re-upsert blocked school: %v", err)
	}
	info, err = st.SchoolInfo("other")
	if err != nil || info == nil || !info.Blocked {
		t.Fatalf("blocked flag was cleared by upsert: (%+v, %v)", info, err)
	}
	schools, err := st.ListSchools()
	if err != nil {
		t.Fatalf("ListSchools: %v", err)
	}
	if len(schools) != 1 || !schools[0].Blocked {
		t.Fatalf("ListSchools = %+v, want one blocked school", schools)
	}

	if err := st.SetSchoolBlocked("other", false); err != nil {
		t.Fatalf("unblock school: %v", err)
	}
	info, err = st.SchoolInfo("other")
	if err != nil || info == nil || info.Blocked {
		t.Fatalf("SchoolInfo after unblock = (%+v, %v), want not blocked", info, err)
	}

	if err := st.DeleteSchool("other"); err != nil {
		t.Fatalf("delete school: %v", err)
	}
	if known, err := st.KnownSchool("other"); err != nil || known {
		t.Fatalf("KnownSchool after delete = (%v, %v), want false", known, err)
	}
}

// TestOutageNtfyCRUD: re-adding the same scope/topic/server is idempotent, and
// deletion only removes the requested row.
func TestOutageNtfyCRUD(t *testing.T) {
	st := openTestStore(t)
	all, err := st.AddOutageNtfy(&OutageNtfy{Topic: "ops", Enabled: true, CreatedBy: "admin"})
	if err != nil {
		t.Fatalf("add all-school subscription: %v", err)
	}
	school, err := st.AddOutageNtfy(&OutageNtfy{School: "testschool", Topic: "ops", BaseURL: "https://ntfy.example", Enabled: true})
	if err != nil {
		t.Fatalf("add school subscription: %v", err)
	}
	again, err := st.AddOutageNtfy(&OutageNtfy{Topic: "ops", Enabled: true, CreatedBy: "someone-else"})
	if err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if again != all {
		t.Errorf("re-add returned id %d, want the existing row %d", again, all)
	}

	subs, err := st.ListOutageNtfy()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(subs) != 2 {
		t.Fatalf("ListOutageNtfy = %d rows, want 2", len(subs))
	}
	if subs[0].Scope() != "all" {
		t.Errorf("empty school scope = %q, want all", subs[0].Scope())
	}
	if got := subs[1].Scope(); got != "testschool" {
		t.Errorf("school scope = %q, want testschool", got)
	}

	o, err := st.OutageNtfyByID(school)
	if err != nil || o == nil || o.Topic != "ops" || o.BaseURL != "https://ntfy.example" {
		t.Fatalf("OutageNtfyByID = (%+v, %v)", o, err)
	}
	if n, err := st.DeleteOutageNtfy(school); err != nil || n != 1 {
		t.Fatalf("delete = (%d, %v), want one row", n, err)
	}
	if o, err := st.OutageNtfyByID(school); err != nil || o != nil {
		t.Fatalf("deleted row still present: (%+v, %v)", o, err)
	}
}
