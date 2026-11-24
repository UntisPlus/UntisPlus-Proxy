package store

import (
	"errors"
	"testing"
	"time"
)

func TestLoginCredentialSchoolOwnership(t *testing.T) {
	st := openTestStore(t)
	if err := st.SetDefaultSchool("alpha"); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveLoginCredential("alpha", "SharedName", "key-a", "password-a"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertLoginUser(&User{Username: "sharedname", School: "alpha", Admin: true}); err != nil {
		t.Fatal(err)
	}
	u, _ := st.GetUser("sharedname")
	if u == nil || u.Admin {
		t.Fatal("upstream login must not grant server administrator")
	}
	if err := st.SetAdmin("sharedname", true); err != nil {
		t.Fatal(err)
	}
	for _, school := range []string{"beta", ""} {
		if err := st.SaveLoginCredential(school, "sharedname", "key-b", "password-b"); !errors.Is(err, ErrAccountSchool) {
			t.Fatalf("foreign credential write: %v", err)
		}
		key, password, err := st.SchoolCredential(school, "sharedname")
		if key != "" || password != "" {
			t.Fatal("foreign school received credentials")
		}
		_ = err
	}
	if err := st.UpsertLoginUser(&User{Username: "sharedname", School: "beta"}); !errors.Is(err, ErrAccountSchool) {
		t.Fatalf("foreign login profile: %v", err)
	}
	key, password, err := st.SchoolCredential("alpha", "SHAREDNAME")
	if err != nil || key != "key-a" || password != "password-a" {
		t.Fatal("owner credentials changed")
	}
	if err := st.UpsertLoginUser(&User{Username: "sharedname", School: "alpha"}); err != nil {
		t.Fatal(err)
	}
	u, _ = st.GetUser("sharedname")
	if !u.Admin {
		t.Fatal("login removed existing server admin")
	}
}

func TestDeletedAccountDoesNotRestoreSessionOrAdmin(t *testing.T) {
	st := openTestStore(t)
	if err := st.SaveLoginCredential("alpha", "same", "key", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertLoginUser(&User{Username: "same", School: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAdmin("same", true); err != nil {
		t.Fatal(err)
	}
	old, _ := st.GetUser("same")
	if err := st.SaveBoundSession("old", "same", 0, old.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteUser("SAME"); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveLoginCredential("alpha", "same", "key", ""); err != nil {
		t.Fatal(err)
	}
	// An in-flight login can carry stale administrative metadata.
	if err := st.UpsertLoginUser(old); err != nil {
		t.Fatal(err)
	}
	current, _ := st.GetUser("same")
	if current.ID == old.ID || current.Admin {
		t.Fatal("deleted account identity or admin was resurrected")
	}
	if err := st.SaveBoundSession("stale", "same", 0, old.ID, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("stale session generation accepted")
	}
	sessions, err := st.LoadSessions(time.Now())
	if err != nil || len(sessions) != 0 {
		t.Fatalf("revoked sessions restored: %v %v", sessions, err)
	}
}

func TestLegacyUnboundSessionsRequireLogin(t *testing.T) {
	st := openTestStore(t)
	if err := st.UpsertUser(&User{Username: "person", School: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO proxy_sessions (id,username,class_id,expires) VALUES ('legacy','person',0,?)`, time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	sessions, err := st.LoadSessions(time.Now())
	if err != nil || len(sessions) != 0 {
		t.Fatalf("unbound legacy sessions: %v %v", sessions, err)
	}
}

func TestDeleteUserClearsOwnedDataWithoutCrossSchoolTokenDeletion(t *testing.T) {
	st := openTestStore(t)
	if err := st.UpsertUser(&User{Username: "person", School: "alpha", PersonID: 7}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO homework_done VALUES ('alpha','person',1,1)`,
		`INSERT INTO ntfy_topics (school,topic,created_by) VALUES ('alpha','topic','person')`,
		`INSERT INTO class_tokens (token,school,person_id,created_at) VALUES ('own','alpha',7,1)`,
		`INSERT INTO class_tokens (token,school,person_id,created_at) VALUES ('foreign','beta',7,1)`,
	} {
		if _, err := st.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DeleteUser("person"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"homework_done", "ntfy_topics"} {
		var n int
		if err := st.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s retained owned data: %d %v", table, n, err)
		}
	}
	var token string
	if err := st.db.QueryRow(`SELECT token FROM class_tokens`).Scan(&token); err != nil || token != "foreign" {
		t.Fatalf("foreign-school token removed: %q %v", token, err)
	}
}
