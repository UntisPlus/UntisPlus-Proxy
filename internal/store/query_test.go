package store

// Query-layer tests for the SQL surface the proxy, untisctl and the admin
// dashboard depend on: school registration, admin flags, pool/owner
// selection, recon element bookkeeping, webhook/topic CRUD, calendar tokens
// and account deletion cascades.

import (
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestSchoolRegistrationAndListing(t *testing.T) {
	st := openTestStore(t)
	if known, err := st.KnownSchool("testschool"); err != nil || known {
		t.Errorf("KnownSchool on an empty store = (%v, %v), want (false, nil)", known, err)
	}
	if err := st.UpsertSchool("testschool"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := st.UpsertSchool("zweit"); err != nil {
		t.Fatalf("upsert second: %v", err)
	}
	// re-registering must not duplicate or fail
	if err := st.UpsertSchool("testschool"); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}

	if known, err := st.KnownSchool("testschool"); err != nil || !known {
		t.Errorf("KnownSchool after upsert = (%v, %v), want (true, nil)", known, err)
	}
	schools, err := st.ListSchools()
	if err != nil {
		t.Fatalf("ListSchools: %v", err)
	}
	if len(schools) != 2 {
		t.Errorf("ListSchools = %v, want exactly 2 schools (upsert must dedupe)", schools)
	}
	names := map[string]bool{}
	for _, s := range schools {
		names[s.Name] = true
		if s.AddedAt.IsZero() {
			t.Errorf("school %s has no added_at", s.Name)
		}
	}
	if !names["testschool"] || !names["zweit"] {
		t.Errorf("school names = %v, want testschool + zweit", names)
	}
}

func TestSetDefaultSchoolBackfillsUnrecordedUsers(t *testing.T) {
	st := openTestStore(t)
	// pre-multi-school rows have no school recorded; a row for another school
	// must never be rewritten
	if err := st.UpsertUser(&User{Username: "legacy", Method: "password"}); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	if err := st.UpsertUser(&User{Username: "anders", School: "zweit", Method: "password"}); err != nil {
		t.Fatalf("seed other: %v", err)
	}

	if err := st.SetDefaultSchool("testschool"); err != nil {
		t.Fatalf("SetDefaultSchool: %v", err)
	}
	legacy, err := st.GetUser("legacy")
	if err != nil || legacy == nil {
		t.Fatalf("GetUser(legacy) = (%v, %v)", legacy, err)
	}
	if legacy.School != "testschool" {
		t.Errorf("legacy user school = %q, want the backfilled default", legacy.School)
	}
	other, err := st.GetUser("anders")
	if err != nil || other == nil {
		t.Fatalf("GetUser(anders) = (%v, %v)", other, err)
	}
	if other.School != "zweit" {
		t.Errorf("other school = %q, want zweit (backfill must not touch it)", other.School)
	}
}

func TestAdminFlagAndBootstrap(t *testing.T) {
	st := openTestStore(t)
	if err := st.UpsertUser(&User{Username: "bob", Method: "password"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if is, err := st.IsAdmin("bob"); err != nil || is {
		t.Errorf("IsAdmin before grant = (%v, %v), want (false, nil)", is, err)
	}
	// the flag must be visible case-insensitively
	if err := st.SetAdmin("BOB", true); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if is, err := st.IsAdmin("bob"); err != nil || !is {
		t.Errorf("IsAdmin after grant = (%v, %v), want (true, nil)", is, err)
	}
	if err := st.SetAdmin("bob", false); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if is, _ := st.IsAdmin("bob"); is {
		t.Error("IsAdmin after revoke = true, want false")
	}
	// an unknown user is simply not an admin
	if is, err := st.IsAdmin("nobody"); err != nil || is {
		t.Errorf("IsAdmin(unknown) = (%v, %v), want (false, nil)", is, err)
	}

	if seeded, err := st.AdminBootstrapSeeded(); err != nil || seeded {
		t.Errorf("AdminBootstrapSeeded = (%v, %v), want (false, nil)", seeded, err)
	}
	if err := st.MarkAdminBootstrapSeeded(); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if seeded, err := st.AdminBootstrapSeeded(); err != nil || !seeded {
		t.Errorf("AdminBootstrapSeeded after mark = (%v, %v), want (true, nil)", seeded, err)
	}
}

func TestGlobalPermUserConstant(t *testing.T) {
	if GlobalPermUser() != "*" {
		t.Errorf("GlobalPermUser() = %q, want *", GlobalPermUser())
	}
}

func TestSecretsRoundTripAndCaseInsensitivity(t *testing.T) {
	st := openTestStore(t)
	if sec, err := st.GetSecret("owen"); err != nil || sec != "" {
		t.Errorf("GetSecret for unknown user = (%q, %v), want empty", sec, err)
	}
	if err := st.UpsertSecret("Owen", "JBSWY3DPEHPK3PXP"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if sec, err := st.GetSecret("owen"); err != nil || sec != "JBSWY3DPEHPK3PXP" {
		t.Errorf("GetSecret(owen) = (%q, %v), want the stored secret (case-insensitive)", sec, err)
	}
	// re-upsert replaces the value
	if err := st.UpsertSecret("owen", "OTHER"); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if sec, _ := st.GetSecret("owen"); sec != "OTHER" {
		t.Errorf("GetSecret after replace = %q, want OTHER", sec)
	}
}

func TestPoolOwnerAndSourceSelection(t *testing.T) {
	st := openTestStore(t)
	seed := []struct {
		u          *User
		wantInPool bool
	}{
		{&User{Username: "dee", School: "testschool", Password: "pw", PersonType: 5, PersonID: 7, ClassID: 5000, ClassName: "10b"}, true},
		{&User{Username: "sam", School: "testschool", Password: "pw", PersonType: 5, PersonID: 8, ClassID: 5000, ClassName: "10b"}, true},
		{&User{Username: "eve", School: "testschool", Method: "session", PersonType: 5, PersonID: 9, ClassID: 4420, ClassName: "10c"}, true},
		{&User{Username: "noClass", School: "testschool", Password: "pw", PersonType: 5, PersonID: 10}, false},
	}
	for _, s := range seed {
		if err := st.UpsertUser(s.u); err != nil {
			t.Fatalf("seed %s: %v", s.u.Username, err)
		}
	}
	// a second school must not leak into the first school's pool
	if err := st.UpsertUser(&User{Username: "anders", School: "zweit", Password: "pw", PersonType: 5, PersonID: 11, ClassID: 4430, ClassName: "7a"}); err != nil {
		t.Fatalf("seed other school: %v", err)
	}

	pool, err := st.Pool("testschool")
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	if len(pool) != 2 {
		t.Errorf("Pool = %+v, want 2 classes (5000 pooled, 4420 session-only)", pool)
	}
	byID := map[int64]Class{}
	for _, c := range pool {
		byID[c.ID] = c
	}
	if c, ok := byID[5000]; !ok || c.Name != "10b" {
		t.Errorf("class 5000 = %+v (ok=%v), want name 10b", c, ok)
	}
	if _, ok := byID[4430]; ok {
		t.Error("class 4430 from another school leaked into testschool's pool")
	}

	if ok, err := st.PoolContains("testschool", 5000); err != nil || !ok {
		t.Errorf("PoolContains(5000) = (%v, %v), want true", ok, err)
	}
	if ok, err := st.PoolContains("zweit", 5000); err != nil || ok {
		t.Errorf("PoolContains for a foreign school = (%v, %v), want false", ok, err)
	}

	// the owner must be replayable (password<>''), otherwise upstream would
	// reject the rewritten auth block
	owner, err := st.OwnerForClass("testschool", 5000)
	if err != nil || owner == nil {
		t.Fatalf("OwnerForClass(5000) = (%v, %v), want a replayable owner", owner, err)
	}
	if owner.Password == "" {
		t.Errorf("owner = %+v, must have a password (password<>'' filter)", owner)
	}
	if none, err := st.OwnerForClass("testschool", 4420); err != nil || none != nil {
		t.Errorf("OwnerForClass(session-only class) = (%v, %v), want nil", none, err)
	}
	if none, err := st.OwnerForClass("testschool", 9999); err != nil || none != nil {
		t.Errorf("OwnerForClass(unknown class) = (%v, %v), want nil", none, err)
	}
}

func TestBoostedSourceAccountsSelectsNonStudents(t *testing.T) {
	st := openTestStore(t)
	// seeded mixed-case on purpose: the point of this test is that the store
	// lowercases, so seeding and expecting the same literal would assert nothing
	if err := st.UpsertUser(&User{Username: "MrTeacher", School: "testschool", Password: "secret", PersonType: 2, PersonID: 5009}); err != nil {
		t.Fatalf("seed teacher: %v", err)
	}
	if err := st.UpsertUser(&User{Username: "dee", School: "testschool", Password: "pw", PersonType: 5, PersonID: 7}); err != nil {
		t.Fatalf("seed student: %v", err)
	}
	if err := st.UpsertUser(&User{Username: "sessiononly", School: "testschool", Method: "session", PersonType: 2, PersonID: 224}); err != nil {
		t.Fatalf("seed session-only teacher: %v", err)
	}

	sources, err := st.BoostedSourceAccounts("testschool")
	if err != nil {
		t.Fatalf("BoostedSourceAccounts: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("sources = %+v, want only the replayable teacher account (no students, no session-only accounts)", sources)
	}
	// usernames are stored lowercased
	if sources[0].Username != "mrteacher" {
		t.Errorf("username = %q, want %q", sources[0].Username, "mrteacher")
	}
}

func TestAnyUserAndUserLookups(t *testing.T) {
	st := openTestStore(t)
	if u, err := st.AnyUser("testschool"); err != nil || u != nil {
		t.Errorf("AnyUser on an empty store = (%v, %v), want nil", u, err)
	}
	if err := st.UpsertUser(&User{Username: "dee", School: "testschool", Password: "pw", PersonType: 5, PersonID: 7, ClassID: 5000}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	u, err := st.AnyUser("testschool")
	if err != nil || u == nil || u.Username != "dee" {
		t.Fatalf("AnyUser = (%v, %v), want dee", u, err)
	}

	if byPerson, err := st.UserByPersonID(7); err != nil || byPerson == nil || byPerson.Username != "dee" {
		t.Errorf("UserByPersonID(7) = (%v, %v), want dee", byPerson, err)
	}
	if none, err := st.UserByPersonID(999); err != nil || none != nil {
		t.Errorf("UserByPersonID(unknown) = (%v, %v), want nil", none, err)
	}
	if cid, err := st.ClassForPerson("testschool", 7); err != nil || cid != 5000 {
		t.Errorf("ClassForPerson(7) = (%d, %v), want 5000", cid, err)
	}
	if n, err := st.UserCount(); err != nil || n != 1 {
		t.Errorf("UserCount = (%d, %v), want 1", n, err)
	}
}

func TestListUsersAndListStudents(t *testing.T) {
	st := openTestStore(t)
	seed := []*User{
		{Username: "dee", School: "testschool", PersonType: 5, PersonID: 7, ClassName: "10b", DisplayName: "Dee"},
		{Username: "sam", School: "testschool", PersonType: 5, PersonID: 8, ClassName: "10a", DisplayName: "Sam"},
		{Username: "mrTeacher", School: "testschool", PersonType: 2, PersonID: 5009},
		{Username: "anon", School: "testschool", PersonType: 0},
		{Username: "anders", School: "zweit", PersonType: 5, PersonID: 11},
	}
	for _, u := range seed {
		if err := st.UpsertUser(u); err != nil {
			t.Fatalf("seed %s: %v", u.Username, err)
		}
	}

	all, err := st.ListUsers()
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(all) != 5 {
		t.Errorf("ListUsers = %d accounts, want 5 (every school)", len(all))
	}

	students, err := st.ListStudents("testschool")
	if err != nil {
		t.Fatalf("ListStudents: %v", err)
	}
	if len(students) != 2 {
		t.Fatalf("ListStudents = %+v, want only the two person_type=5 pupils of testschool", students)
	}
	// ordered by class_name then display_name
	if students[0].Username != "sam" || students[1].Username != "dee" {
		t.Errorf("ListStudents order = %s,%s, want sam (10a) then dee (10b)",
			students[0].Username, students[1].Username)
	}
}

func TestDeleteUserCascades(t *testing.T) {
	st := openTestStore(t)
	if err := st.UpsertUser(&User{Username: "dee", School: "testschool", Password: "pw", PersonType: 5, PersonID: 7, ClassID: 5000}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := st.UpsertSecret("dee", "SECRET"); err != nil {
		t.Fatalf("secret: %v", err)
	}
	if err := st.SetPerm("dee", "editor", true); err != nil {
		t.Fatalf("perm: %v", err)
	}
	if err := st.SetPerm(GlobalPermUser(), "editor", true); err != nil {
		t.Fatalf("global perm: %v", err)
	}
	if err := st.CreateClassToken(&ClassToken{Token: "tok", School: "testschool", PersonID: 7, Days: 30}); err != nil {
		t.Fatalf("token: %v", err)
	}
	if err := st.CreateClassToken(&ClassToken{Token: "cls", School: "testschool", ClassID: 5000, Days: 30}); err != nil {
		t.Fatalf("class token: %v", err)
	}

	if _, err := st.DeleteUser("dee"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if u, _ := st.GetUser("dee"); u != nil {
		t.Error("user row survived DeleteUser")
	}
	if sec, _ := st.GetSecret("dee"); sec != "" {
		t.Error("secret survived DeleteUser")
	}
	if ok, _ := st.HasPerm("dee", "editor"); ok {
		t.Error("per-user perm survived DeleteUser")
	}
	if ok, _ := st.HasPerm(GlobalPermUser(), "editor"); !ok {
		t.Error("DeleteUser must not touch the global permission switch")
	}
	if tok, _ := st.ClassTokenByToken("tok"); tok != nil {
		t.Error("the user's personal calendar token survived DeleteUser")
	}
	if tok, _ := st.ClassTokenByToken("cls"); tok == nil {
		t.Error("a class token is not the user's personal token and must survive")
	}
}

func TestReconElementBookkeeping(t *testing.T) {
	st := openTestStore(t)
	if err := st.SaveReconElements("testschool", map[string][]int64{
		"TEACHER": {5009, 224}, "ROOM": {169}, "SUBJECT": {7},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	// saving again must not duplicate
	if err := st.SaveReconElements("testschool", map[string][]int64{"TEACHER": {5009}}); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	elems, err := st.LoadReconElements("testschool")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(elems["TEACHER"]) != 2 {
		t.Errorf("TEACHER set = %v, want 5009+224 without duplicates", elems["TEACHER"])
	}
	if len(elems["ROOM"]) != 1 || elems["ROOM"][0] != 169 {
		t.Errorf("ROOM set = %v, want [169]", elems["ROOM"])
	}
	// a school with nothing scanned reports nothing
	if other, err := st.LoadReconElements("zweit"); err != nil || len(other) != 0 {
		t.Errorf("LoadReconElements(zweit) = (%v, %v), want empty", other, err)
	}
}

func TestElementNamesAndListing(t *testing.T) {
	st := openTestStore(t)
	if got := st.ElementName("testschool", "TEACHER", 5009); got != "" {
		t.Errorf("ElementName for an unknown element = %q, want empty", got)
	}
	// ids <= 0 are never looked up
	if got := st.ElementName("testschool", "TEACHER", 0); got != "" {
		t.Errorf("ElementName(id=0) = %q, want empty", got)
	}
	if err := st.UpsertElementNames("testschool", "TEACHER", map[int64]string{5009: "A. Hartley", 224: ""}); err != nil {
		t.Fatalf("upsert names: %v", err)
	}
	if got := st.ElementName("testschool", "TEACHER", 5009); got != "A. Hartley" {
		t.Errorf("ElementName(5009) = %q, want A. Hartley", got)
	}
	if got := st.ElementName("testschool", "TEACHER", 224); got != "" {
		t.Errorf("ElementName(224) = %q, want empty (empty names are not stored)", got)
	}
	// names must be refreshed in place on re-login
	if err := st.UpsertElementNames("testschool", "TEACHER", map[int64]string{5009: "A. Hartley."}); err != nil {
		t.Fatalf("refresh names: %v", err)
	}
	if got := st.ElementName("testschool", "TEACHER", 5009); got != "A. Hartley." {
		t.Errorf("ElementName after refresh = %q, want the new name", got)
	}

	list, err := st.ListElementsWithNames("testschool")
	if err != nil {
		t.Fatalf("ListElementsWithNames: %v", err)
	}
	if list["TEACHER"][5009] != "A. Hartley." {
		t.Errorf("ListElementsWithNames = %v, want the teacher name", list)
	}
	// the same ids are also reachable as recon elements
	if err := st.SaveReconElements("testschool", map[string][]int64{"TEACHER": {5009}}); err != nil {
		t.Fatalf("save recon: %v", err)
	}
	if elems, _ := st.LoadReconElements("testschool"); len(elems["TEACHER"]) != 1 {
		t.Errorf("element ids and names must share one table, got %v", elems)
	}
}

func TestReconScanProgress(t *testing.T) {
	st := openTestStore(t)
	if err := st.SaveReconScanAt("testschool", 5000, "2026-12-20"); err != nil {
		t.Fatalf("save: %v", err)
	}
	var until string
	if err := st.db.QueryRow(`SELECT scan_until FROM recon_scan WHERE school=? AND class_id=?`,
		"testschool", int64(5000)).Scan(&until); err != nil {
		t.Fatalf("read scan marker: %v", err)
	}
	if until != "2026-12-20" {
		t.Errorf("scan_until = %q, want 2026-12-20", until)
	}

	// re-saving the same class advances the marker instead of adding a row
	if err := st.SaveReconScanAt("testschool", 5000, "2027-01-10"); err != nil {
		t.Fatalf("advance: %v", err)
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM recon_scan WHERE school='testschool' AND class_id=5000`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("recon_scan rows = %d, want 1 (upsert, not insert)", n)
	}
	if err := st.db.QueryRow(`SELECT scan_until FROM recon_scan WHERE school=? AND class_id=?`,
		"testschool", int64(5000)).Scan(&until); err != nil {
		t.Fatalf("read after advance: %v", err)
	}
	if until != "2027-01-10" {
		t.Errorf("scan_until after advance = %q, want 2027-01-10", until)
	}

	// per-school isolation
	if err := st.SaveReconScanAt("zweit", 5000, "2026-11-01"); err != nil {
		t.Fatalf("other school: %v", err)
	}
	var first string
	if err := st.db.QueryRow(`SELECT scan_until FROM recon_scan WHERE school='testschool' AND class_id=5000`).Scan(&first); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if first != "2027-01-10" {
		t.Errorf("testschool scan_until = %q after a zweit write, want 2027-01-10", first)
	}
}

func TestWebhookCRUD(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.AddWebhook(&Webhook{School: "testschool", ElementType: "TEACHER", ElementID: 5009,
		URL: "https://hooks.example/abc", Secret: "s3cret", Enabled: true, CreatedBy: "bob"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	hooks, err := st.ListWebhooks("testschool")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(hooks) != 1 {
		t.Fatalf("ListWebhooks = %d, want 1", len(hooks))
	}
	h := hooks[0]
	if h.URL != "https://hooks.example/abc" || h.Secret != "s3cret" || !h.Enabled || h.CreatedBy != "bob" {
		t.Errorf("webhook = %+v, want the stored url/secret/enabled/creator", h)
	}
	if et, eid := h.Target(); et != "TEACHER" || eid != 5009 {
		t.Errorf("Target() = (%q, %d), want (TEACHER, 5009)", et, eid)
	}
	if h.CreatedAt.IsZero() {
		t.Error("CreatedAt not stamped")
	}

	// another school must not see it
	if others, err := st.ListWebhooks("zweit"); err != nil || len(others) != 0 {
		t.Errorf("ListWebhooks(zweit) = (%v, %v), want empty", others, err)
	}

	n, err := st.DeleteWebhook(h.ID)
	if err != nil || n != 1 {
		t.Fatalf("DeleteWebhook = (%d, %v), want (1, nil)", n, err)
	}
	if left, _ := st.ListWebhooks("testschool"); len(left) != 0 {
		t.Errorf("webhooks after delete = %v, want none", left)
	}
	if n, _ := st.DeleteWebhook(h.ID); n != 0 {
		t.Errorf("second DeleteWebhook affected %d rows, want 0", n)
	}
}

func TestNtfyTopicCRUD(t *testing.T) {
	st := openTestStore(t)
	id, err := st.AddNtfyTopic(&NtfyTopic{School: "testschool", Topic: "klass5000",
		BaseURL: "https://ntfy.example.org/", Enabled: true, CreatedBy: "bob"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	topics, err := st.ListNtfyTopics("testschool")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(topics) != 1 {
		t.Fatalf("ListNtfyTopics = %d, want 1", len(topics))
	}
	tp := topics[0]
	if tp.Topic != "klass5000" || tp.BaseURL != "https://ntfy.example.org/" || !tp.Enabled {
		t.Errorf("topic = %+v, want the stored topic/base/enabled", tp)
	}
	if et, eid := tp.Target(); et != "" || eid != 0 {
		t.Errorf("Target() = (%q, %d), want the school-wide target", et, eid)
	}
	if n, err := st.DeleteNtfyTopic(id); err != nil || n != 1 {
		t.Fatalf("DeleteNtfyTopic = (%d, %v), want (1, nil)", n, err)
	}
	if left, _ := st.ListNtfyTopics("testschool"); len(left) != 0 {
		t.Errorf("topics after delete = %v, want none", left)
	}
}

func TestAllPermsListsGlobalsFirst(t *testing.T) {
	st := openTestStore(t)
	if err := st.SetPerm("dee", "boosted", true); err != nil {
		t.Fatalf("user perm: %v", err)
	}
	if err := st.SetPerm(GlobalPermUser(), "recon", true); err != nil {
		t.Fatalf("global perm: %v", err)
	}
	perms, err := st.AllPerms()
	if err != nil {
		t.Fatalf("AllPerms: %v", err)
	}
	// granting boosted to a real user also writes the explicit mutual-exclusion
	// row (recon=false), so there are three rows: the global switch, dee's
	// boosted grant and the exclusion it forced.
	if len(perms) != 3 {
		t.Fatalf("AllPerms = %+v, want 3 rows (global + boosted + forced recon exclusion)", perms)
	}
	if perms[0].Username != GlobalPermUser() {
		t.Errorf("AllPerms[0] = %+v, want the global switch first", perms[0])
	}
	if !perms[0].Allowed || perms[0].Feature != "recon" {
		t.Errorf("global perm = %+v, want recon allowed", perms[0])
	}
	if perms[1].Username != "dee" || perms[1].Feature != "boosted" || !perms[1].Allowed {
		t.Errorf("perms[1] = %+v, want dee/boosted allowed", perms[1])
	}
	if perms[2].Username != "dee" || perms[2].Feature != "recon" || perms[2].Allowed {
		t.Errorf("perms[2] = %+v, want the forced dee/recon exclusion", perms[2])
	}
}

func TestClassTokenLookupsAndUpdates(t *testing.T) {
	st := openTestStore(t)
	seed := []*ClassToken{
		{Token: "clstok", School: "testschool", ClassID: 5000, Days: 30, CreatedBy: "dee", Timezone: "Europe/Berlin"},
		{Token: "pers", School: "testschool", PersonID: 7, Days: 30, CreatedBy: "dee"},
		{Token: "elem", School: "testschool", ElementType: "TEACHER", ElementID: 5009, Days: 30, CreatedBy: "dee"},
	}
	for _, tok := range seed {
		if err := st.CreateClassToken(tok); err != nil {
			t.Fatalf("create %s: %v", tok.Token, err)
		}
	}

	if tok, err := st.ClassTokenForClass("testschool", 5000); err != nil || tok == nil || tok.Token != "clstok" {
		t.Errorf("ClassTokenForClass(5000) = (%v, %v), want clstok", tok, err)
	}
	if tok, err := st.ClassTokenForPerson("testschool", 7); err != nil || tok == nil || tok.Token != "pers" {
		t.Errorf("ClassTokenForPerson(7) = (%v, %v), want pers", tok, err)
	}
	if tok, err := st.ClassTokenForElement("testschool", "TEACHER", 5009); err != nil || tok == nil || tok.Token != "elem" {
		t.Errorf("ClassTokenForElement(TEACHER,5009) = (%v, %v), want elem", tok, err)
	}
	// tokens of another school must not resolve
	if tok, err := st.ClassTokenForClass("zweit", 5000); err != nil || tok != nil {
		t.Errorf("ClassTokenForClass for a foreign school = (%v, %v), want nil", tok, err)
	}
	if none, _ := st.ClassTokenByToken("nope"); none != nil {
		t.Errorf("ClassTokenByToken(unknown) = %v, want nil", none)
	}

	full, err := st.ClassTokenByToken("clstok")
	if err != nil || full == nil {
		t.Fatalf("ClassTokenByToken(clstok) = (%v, %v)", full, err)
	}
	if full.School != "testschool" || full.ClassID != 5000 || full.Timezone != "Europe/Berlin" || full.CreatedBy != "dee" {
		t.Errorf("token = %+v, want the full stored row", full)
	}
	if full.Days != 30 {
		t.Errorf("Days = %d, want 30", full.Days)
	}

	// extending the window and recording access
	if n, err := st.UpdateClassTokenDays("clstok", 90); err != nil || n != 1 {
		t.Fatalf("UpdateClassTokenDays = (%d, %v), want (1, nil)", n, err)
	}
	if tok, _ := st.ClassTokenByToken("clstok"); tok == nil || tok.Days != 90 {
		t.Errorf("Days after update = %+v, want 90", tok)
	}
	if err := st.TouchClassToken("clstok", 1700000000); err != nil {
		t.Fatalf("TouchClassToken: %v", err)
	}
	if tok, _ := st.ClassTokenByToken("clstok"); tok == nil || tok.LastAccess != 1700000000 {
		t.Errorf("LastAccess = %+v, want 1700000000", tok)
	}
	if n, _ := st.DeleteClassToken("clstok"); n != 1 {
		t.Errorf("DeleteClassToken affected %d rows, want 1", n)
	}
	if tok, _ := st.ClassTokenByToken("clstok"); tok != nil {
		t.Error("token survived deletion")
	}
}

func TestClassAndSchoolVersionCounters(t *testing.T) {
	st := openTestStore(t)
	if got := st.SchoolVersion("testschool"); got != 0 {
		t.Errorf("SchoolVersion on an empty store = %d, want 0", got)
	}
	rows := func(subject string) []PeriodRow {
		return []PeriodRow{{PeriodID: 1, Start: "2026-01-05T08:00:00", End: "2026-01-05T08:45:00", Subject: subject}}
	}
	// ClassVersion only advances when the snapshot actually changed, and the
	// school counter is the maximum across that school's classes.
	if _, err := st.ReplaceClassSnapshot("testschool", 5000, rows("Mathe"), 1, ""); err != nil {
		t.Fatalf("snapshot 5000 v1: %v", err)
	}
	if _, err := st.ReplaceClassSnapshot("testschool", 5000, rows("Mathe"), 2, ""); err != nil {
		t.Fatalf("snapshot 5000 v2: %v", err)
	}
	if got := st.ClassVersion("testschool", 5000); got != 1 {
		t.Errorf("ClassVersion after an unchanged snapshot = %d, want 1", got)
	}
	if _, err := st.ReplaceClassSnapshot("testschool", 777, rows("Deutsch"), 5, ""); err != nil {
		t.Fatalf("snapshot 777 v5: %v", err)
	}
	if got := st.SchoolVersion("testschool"); got != 5 {
		t.Errorf("SchoolVersion = %d, want 5 (max over classes)", got)
	}
	// Another school must not leak into the counter.
	if _, err := st.ReplaceClassSnapshot("zweit", 42, rows("Englisch"), 9, ""); err != nil {
		t.Fatalf("snapshot zweit: %v", err)
	}
	if got := st.SchoolVersion("testschool"); got != 5 {
		t.Errorf("SchoolVersion after another school changed = %d, want 5", got)
	}
	if got := st.SchoolVersion("zweit"); got != 9 {
		t.Errorf("SchoolVersion(zweit) = %d, want 9", got)
	}
}

// TestReconScanProgressRoundTrip: the progress table is a plain upsert/read/clear.
func TestStoreReconScanRoundTrip(t *testing.T) {
	st := openTestStore(t)
	if got, err := st.ReconScan("testschool"); err != nil || len(got) != 0 {
		t.Fatalf("ReconScan on empty = (%v, %v), want empty", got, err)
	}
	if err := st.SaveReconScanAt("testschool", 5000, "2026-10-18"); err != nil {
		t.Fatalf("SaveReconScanAt: %v", err)
	}
	if err := st.SaveReconScanAt("testschool", 777, "2026-10-19"); err != nil {
		t.Fatalf("SaveReconScanAt second class: %v", err)
	}
	if err := st.SaveReconScanAt("testschool", 5000, "2026-10-25"); err != nil {
		t.Fatalf("SaveReconScanAt update: %v", err)
	}
	got, err := st.ReconScan("testschool")
	if err != nil {
		t.Fatalf("ReconScan: %v", err)
	}
	if got[5000] != "2026-10-25" || got[777] != "2026-10-19" || len(got) != 2 {
		t.Errorf("ReconScan = %v, want 5000=2026-10-25 and 777=2026-10-19", got)
	}
	// another school must stay separate
	if err := st.SaveReconScanAt("zweit", 5000, "2026-01-01"); err != nil {
		t.Fatalf("SaveReconScanAt zweit: %v", err)
	}
	other, _ := st.ReconScan("zweit")
	if len(other) != 1 || other[5000] != "2026-01-01" {
		t.Errorf("ReconScan(zweit) = %v, want only its own row", other)
	}
	if err := st.ClearReconScan("testschool"); err != nil {
		t.Fatalf("ClearReconScan: %v", err)
	}
	if got, _ := st.ReconScan("testschool"); len(got) != 0 {
		t.Errorf("ReconScan after clear = %v, want empty", got)
	}
	if other, _ := st.ReconScan("zweit"); len(other) != 1 {
		t.Errorf("ClearReconScan leaked into another school: %v", other)
	}
}
