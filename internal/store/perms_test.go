package store

import (
	"strings"
	"testing"
	"time"
)

func TestPermReconGlobalFallback(t *testing.T) {
	st, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// default deny for any elType
	if v, _ := st.ReconAccess("fresh", "TEACHER"); v {
		t.Fatal("default recon should be denied")
	}
	if v, _ := st.ReconAccess("fresh", "ROOM"); v {
		t.Fatal("default recon should be denied")
	}

	// global on -> fresh user inherits (elType ignored; single recon flag)
	if err := st.SetReconType("TEACHER", true); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.ReconAccess("fresh", "ROOM"); !v {
		t.Fatal("fresh should inherit global recon")
	}

	// per-user deny override beats global
	if err := st.SetReconOverride("fresh", "TEACHER", false); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.ReconAccess("fresh", "TEACHER"); v {
		t.Fatal("per-user deny should beat global recon")
	}

	// clear overrides -> back to global
	if _, err := st.ClearReconOverrides("fresh"); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.ReconAccess("fresh", "SUBJECT"); !v {
		t.Fatal("after clear, fresh should inherit global recon again")
	}

	// per-user grant beats global off
	if err := st.SetReconType("TEACHER", false); err != nil {
		t.Fatal(err)
	}
	if err := st.SetReconOverride("fresh", "TEACHER", true); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.ReconAccess("fresh", "ROOM"); !v {
		t.Fatal("per-user grant should beat global off")
	}

	// RevokeAll -> everything off
	if _, err := st.RevokeAll(); err != nil {
		t.Fatal(err)
	}
	for _, ty := range []string{"TEACHER", "ROOM", "SUBJECT"} {
		if v, _ := st.ReconAccess("fresh", ty); v {
			t.Fatalf("after RevokeAll, recon should be denied")
		}
	}
}

func TestReconBoostedMutualExclusion(t *testing.T) {
	st, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Grant boosted -> recon revoked
	if err := st.SetBoostedFlag("alice", true); err != nil {
		t.Fatal(err)
	}
	if in, err := st.BoostedAccess("alice"); err != nil || !in {
		t.Fatal("boosted should grant boosted access")
	}
	if v, _ := st.ReconAccess("alice", "TEACHER"); v {
		t.Fatal("boosting should revoke recon")
	}

	// Grant recon -> boosted revoked
	if err := st.SetReconOverride("alice", "TEACHER", true); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.ReconAccess("alice", "TEACHER"); !v {
		t.Fatal("recon should be granted")
	}
	if in, err := st.BoostedAccess("alice"); err != nil || in {
		t.Fatal("recon should revoke boosted")
	}

	// Revoking boosted restores nothing automatically, but revoking recon
	// leaves the user free to be boosted again.
	if err := st.SetReconOverride("alice", "TEACHER", false); err != nil {
		t.Fatal(err)
	}
	if err := st.SetBoostedFlag("alice", true); err != nil {
		t.Fatal(err)
	}
	if in, err := st.BoostedAccess("alice"); err != nil || !in {
		t.Fatal("re-granting boosted after clearing recon should work")
	}
}

func TestUsernameCaseInsensitiveMerge(t *testing.T) {
	path := t.TempDir() + "/t.db"
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate legacy rows whose usernames differ only by case.
	now := time.Now().Unix()
	ins := func(name, pw string, ls int64) {
		if _, err := st.db.Exec(`INSERT INTO users
			(username,password,method,person_id,person_type,class_id,class_name,created_at,last_seen)
			VALUES (?,?,?,?,?,?,?,?,?)`, name, pw, "key", 1, 5, 5000, "10aR", now, ls); err != nil {
			t.Fatal(err)
		}
	}
	ins("Schneider", "oldpw", now-1000)
	ins("SCHNEIDER", "newpw", now)
	if _, err := st.db.Exec(`INSERT INTO secrets (username, secret, updated_at) VALUES (?,?,?)`,
		"Schneider", "OLDSECRET", now-1000); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO secrets (username, secret, updated_at) VALUES (?,?,?)`,
		"SCHNEIDER", "NEWSECRET", now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO perms (username, feature, allowed) VALUES (?,?,?)`,
		"Schneider", FeatureBoosted, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO perms (username, feature, allowed) VALUES (?,?,?)`,
		"SCHNEIDER", FeatureRecon, 1); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// Reopen runs the merge migration.
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	u, err := st.GetUser("Schneider")
	if err != nil || u == nil {
		t.Fatalf("GetUser(capital) err=%v u=%v", err, u)
	}
	if u.Password != "newpw" {
		t.Fatal("should keep the most recently active variant's credentials")
	}
	if u.Username != strings.ToLower(u.Username) {
		t.Fatal("canonical username should be lowercase")
	}
	if u2, _ := st.GetUser("SCHNEIDER"); u2 == nil || u2.ID != u.ID {
		t.Fatal("both casings must resolve to the same account")
	}
	if sec, _ := st.GetSecret("schneider"); sec != "NEWSECRET" {
		t.Fatalf("secret should be merged and case-insensitive, got %q", sec)
	}
	if in, err := st.BoostedAccess("SCHNEIDER"); err != nil || !in {
		t.Fatal("merged account should be boosted regardless of case")
	}
	if v, _ := st.ReconAccess("schneider", "TEACHER"); v {
		t.Fatal("boost/recon conflict should resolve in favour of boosted (recon off)")
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("duplicates must collapse to a single row, got %d", n)
	}
}

func TestUsernameCaseInsensitiveWrites(t *testing.T) {
	st, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.SetBoostedFlag("BoBbY", true); err != nil {
		t.Fatal(err)
	}
	if in, _ := st.BoostedAccess("bobby"); !in {
		t.Fatal("grant+lookup must be case-insensitive")
	}
	if in, _ := st.HasPerm("BOBBY", FeatureBoosted); !in {
		t.Fatal("HasPerm must be case-insensitive")
	}

	u := &User{Username: "EvAn", Password: "p", Method: "key", PersonType: 5, ClassID: 10}
	if err := st.UpsertUser(u); err != nil {
		t.Fatal(err)
	}
	if err := st.Touch("evan"); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetUser("evan")
	if err != nil || got == nil {
		t.Fatalf("GetUser err=%v user=%v", err, got)
	}
	got2, err := st.GetUser("EVAN")
	if err != nil || got2 == nil {
		t.Fatalf("GetUser(caps) err=%v user=%v", err, got2)
	}
	if got.ID != got2.ID {
		t.Fatal("upsert+lookup must hit the same account regardless of case")
	}
}

func TestUsernameCaseSelfHealOnWrite(t *testing.T) {
	path := t.TempDir() + "/t.db"
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	// A capital-cased account exists (as if seeded before normalization) with a
	// permission flag stored under the same capital casing.
	if _, err := st.db.Exec(`INSERT INTO users (username,password,method,last_seen) VALUES (?,?,?,?)`,
		"Jdoe", "pw", "key", 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO perms (username, feature, allowed) VALUES (?,?,?)`,
		"Jdoe", FeatureBoosted, 1); err != nil {
		t.Fatal(err)
	}

	// The app now logs in with the lowercase casing: this must collapse the
	// variants at write time (not wait for a restart) and keep the flag.
	u := &User{Username: "jdoe", Password: "pw", Method: "key", PersonType: 5, ClassID: 5000}
	if err := st.UpsertUser(u); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetUser("Jdoe")
	if err != nil || got == nil {
		t.Fatalf("GetUser err=%v user=%v", err, got)
	}
	got2, _ := st.GetUser("jdoe")
	if got2 == nil || got2.ID != got.ID {
		t.Fatal("case variants must collapse to one account")
	}
	if in, _ := st.BoostedAccess("jdoe"); !in {
		t.Fatal("flag must survive the write-time merge")
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected a single merged row, got %d", n)
	}
}

func TestEditorImpliesBoosted(t *testing.T) {
	st, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.SetPerm("ed", FeatureEditor, true); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.EditorAccess("ed"); !v {
		t.Fatal("editor should be allowed")
	}
	// editor is a boosted user who may also edit
	if v, _ := st.BoostedAccess("ed"); !v {
		t.Fatal("editor must imply boosted")
	}
	if v, _ := st.BoostedAccess("nobody"); v {
		t.Fatal("a user without flags must not be boosted")
	}
	// and boosted/recon XOR ignores editor
	if err := st.SetBoostedFlag("ed", true); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.EditorAccess("ed"); !v {
		t.Fatal("granting boosted must not revoke editor")
	}
	if v, _ := st.ReconAccess("ed", "TEACHER"); v {
		t.Fatal("boosted should revoke recon")
	}
	if err := st.SetReconOverride("ed", "TEACHER", true); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.EditorAccess("ed"); !v {
		t.Fatal("granting recon must not revoke editor")
	}
	if v, _ := st.EditorAccess("ED"); !v {
		t.Fatal("editor access must be case-insensitive")
	}
}

func TestLookupElement_NumericID(t *testing.T) {
	st, _ := Open(t.TempDir() + "/t.db")
	// Pure numeric input should be returned as-is (no name needed).
	id, err := st.LookupElement("testschool", "TEACHER", "123")
	if err != nil {
		t.Fatal(err)
	}
	if id != 123 {
		t.Fatalf("expected 123, got %d", id)
	}
}

func TestLookupElement_ExactMatch(t *testing.T) {
	st, _ := Open(t.TempDir() + "/t.db")
	st.SaveMasterNames("testschool", "TEACHER", map[int64]string{10: "Müller", 20: "Schmidt"})

	id, err := st.LookupElement("testschool", "TEACHER", "Müller")
	if err != nil {
		t.Fatal(err)
	}
	if id != 10 {
		t.Fatalf("expected 10, got %d", id)
	}
	// Case insensitive
	id, err = st.LookupElement("testschool", "TEACHER", "müller")
	if err != nil {
		t.Fatal(err)
	}
	if id != 10 {
		t.Fatalf("expected 10, got %d", id)
	}
}

func TestLookupElement_SubstringMatch(t *testing.T) {
	st, _ := Open(t.TempDir() + "/t.db")
	st.SaveMasterNames("testschool", "ROOM", map[int64]string{5: "Aula", 6: "Aula EG", 7: "Bibliothek"})

	// Unique substring
	id, err := st.LookupElement("testschool", "ROOM", "Bib")
	if err != nil {
		t.Fatal(err)
	}
	if id != 7 {
		t.Fatalf("expected 7, got %d", id)
	}
}

func TestLookupElement_Ambiguous(t *testing.T) {
	st, _ := Open(t.TempDir() + "/t.db")
	st.SaveMasterNames("testschool", "ROOM", map[int64]string{5: "Aula", 6: "Aula EG", 7: "Bibliothek"})

	// "aul" is substring of both "Aula" and "Aula EG" — ambiguous
	_, err := st.LookupElement("testschool", "ROOM", "aul")
	if err == nil {
		t.Fatal("expected error for ambiguous match")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected 'ambiguous' in error, got: %v", err)
	}
}

func TestLookupElement_NoMatch(t *testing.T) {
	st, _ := Open(t.TempDir() + "/t.db")
	st.SaveMasterNames("testschool", "SUBJECT", map[int64]string{1: "BIO", 2: "MAT", 3: "DEU"})

	_, err := st.LookupElement("testschool", "SUBJECT", "XYZ")
	if err == nil {
		t.Fatal("expected error for no match")
	}
	// Should suggest close matches
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected 'not found' in error, got: %v", err)
	}
}

func TestLookupElement_EmptyType(t *testing.T) {
	st, _ := Open(t.TempDir() + "/t.db")
	_, err := st.LookupElement("testschool", "TEACHER", "foo")
	if err == nil {
		t.Fatal("expected error for empty DB")
	}
	if !strings.Contains(err.Error(), "no TEACHER elements") {
		t.Fatalf("expected 'no TEACHER elements' in error, got: %v", err)
	}
}

func TestSaveMasterNames_Upsert(t *testing.T) {
	st, _ := Open(t.TempDir() + "/t.db")
	st.SaveMasterNames("testschool", "TEACHER", map[int64]string{1: "Müller"})
	st.SaveMasterNames("testschool", "TEACHER", map[int64]string{1: "Müller (neu)", 2: "Schmidt"})

	id, err := st.LookupElement("testschool", "TEACHER", "Müller (neu)")
	if err != nil {
		t.Fatal(err)
	}
	if id != 1 {
		t.Fatalf("expected 1, got %d", id)
	}
	id, err = st.LookupElement("testschool", "TEACHER", "Schmidt")
	if err != nil {
		t.Fatal(err)
	}
	if id != 2 {
		t.Fatalf("expected 2, got %d", id)
	}
}
