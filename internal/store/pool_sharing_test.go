package store

import (
	"testing"
	"untis-proxy/internal/access"
)

func TestPoolSharingExcludesSourceButPreservesIdentity(t *testing.T) {
	st := openTestStore(t)
	for _, name := range []string{"first", "second"} {
		if err := st.UpsertUser(&User{Username: name, School: "school", PersonType: 5, PersonID: 7, ClassID: 5000, Password: "fake", Method: "key"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetUserCapGrant("school", "first", string(access.CapPoolTimetableShare), -1); err != nil {
		t.Fatal(err)
	}
	owner, err := st.OwnerForClass("school", 5000)
	if err != nil || owner == nil || owner.Username != "second" {
		t.Fatalf("hidden source selected: %+v %v", owner, err)
	}
	if ok, _ := st.PoolContains("school", 5000); !ok {
		t.Fatal("other contributor should keep class shared")
	}
	if err := st.SetUserCapGrant("school", "second", string(access.CapPoolTimetableShare), -1); err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.PoolContains("school", 5000); ok {
		t.Fatal("class still pooled after all accounts opt out")
	}
	pool, err := st.Pool("school")
	if err != nil || len(pool) != 0 {
		t.Fatalf("pool still contains hidden class: %+v %v", pool, err)
	}
	if owner, _ := st.OwnerForClass("school", 5000); owner != nil {
		t.Fatal("hidden source returned")
	}
	u, _ := st.GetUser("first")
	if u == nil || u.ClassID != 5000 || !st.EffectiveHas("school", u, access.CapTimetableStudentPersonalUpstreamView) {
		t.Fatal("opt-out changed personal identity/access")
	}
}
func TestGuardianLinksAreSeparateFromChildAccounts(t *testing.T) {
	st := openTestStore(t)
	u := &User{Username: "guardian", School: "school", PersonType: 5, PersonID: 1, ClassID: -1}
	if err := st.UpsertUser(u); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveAccountLinks("school", u.Username, 1, []byte(`[{"id":10,"type":5,"displayName":"First"},{"id":20,"type":5},{"id":30,"type":5}]`)); err != nil {
		t.Fatal(err)
	}
	links, err := st.AccountLinks("school", u.Username)
	if err != nil || !links.Guardian || len(links.Persons) != 3 {
		t.Fatalf("links=%+v err=%v", links, err)
	}
	effective, err := st.ResolveEffective("school", u)
	if err != nil || effective.Role != "parent" || !effective.Decisions[access.CapTimetableStudentPersonalUpstreamView].Effective {
		t.Fatalf("guardian access=%+v err=%v", effective, err)
	}
	if _, err := st.AccountLinks("another-school", u.Username); err == nil {
		t.Fatal("linked children crossed school boundary")
	}
	if count, _ := st.UserCount(); count != 1 {
		t.Fatal("children incorrectly added as pool accounts")
	}
}

func TestDeletingGuardianRemovesLinkedIdentities(t *testing.T) {
	st := openTestStore(t)
	if err := st.UpsertUser(&User{Username: "guardian", School: "school"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveAccountLinks("school", "guardian", 1, []byte(`[{"id":10,"type":5}]`)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteUser("guardian"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AccountLinks("school", "guardian"); err == nil {
		t.Fatal("deleted account retained linked child identities")
	}
}

func TestPoolDeduplicationPreservesSharingAndNames(t *testing.T) {
	st := openTestStore(t)
	for _, u := range []User{
		{Username: "allowed", School: "school", PersonType: 5, ClassID: 5000, ClassName: "A"},
		{Username: "hidden", School: "school", PersonType: 5, ClassID: 5000, ClassName: "Z"},
		{Username: "other", School: "school", PersonType: 5, ClassID: 5000, ClassName: "B"},
	} {
		if err := st.UpsertUser(&u); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetUserCapGrant("school", "hidden", string(access.CapPoolTimetableShare), -1); err != nil {
		t.Fatal(err)
	}
	pool, err := st.Pool("school")
	if err != nil || len(pool) != 1 || pool[0].Name != "B" {
		t.Fatalf("pool=%v err=%v", pool, err)
	}
	// Revocations take effect on the next call; optimisation must not cache access.
	for _, user := range []string{"allowed", "other"} {
		if err := st.SetUserCapGrant("school", user, string(access.CapPoolTimetableShare), -1); err != nil {
			t.Fatal(err)
		}
	}
	pool, err = st.Pool("school")
	if err != nil || len(pool) != 0 {
		t.Fatalf("revoked class remained: %v %v", pool, err)
	}
}
