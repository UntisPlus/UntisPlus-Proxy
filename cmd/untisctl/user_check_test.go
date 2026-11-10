package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"untis-proxy/internal/store"
	"untis-proxy/internal/untis"
)

// fakeUserCheckAPI records which school each call used, so a test can assert
// that `users check` routes every account to its own school instead of the
// globally configured one.
type fakeUserCheckAPI struct {
	mu sync.Mutex

	sessions     []string // "school|username"
	klassenCalls []string // "school"
	personCalls  []string // "school|cookie"
	logouts      []string // "school|cookie"

	classes   map[string]map[int64]string
	classFail map[string]bool
	loginFail map[string]bool
	people    map[string]*untis.PersonInfo
}

func (f *fakeUserCheckAPI) Session(school, username, secret, method string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions = append(f.sessions, school+"|"+username)
	if f.loginFail[username] {
		return "", fmt.Errorf("bad credentials")
	}
	return "cookie:" + username, nil
}

func (f *fakeUserCheckAPI) GetKlassen(school, cookie string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.klassenCalls = append(f.klassenCalls, school)
	if f.classFail[school] {
		return nil, fmt.Errorf("class list unavailable")
	}
	var res struct {
		Result []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"result"`
	}
	for id, name := range f.classes[school] {
		res.Result = append(res.Result, struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		}{ID: id, Name: name})
	}
	return json.Marshal(res)
}

func (f *fakeUserCheckAPI) PersonInfo(school, cookie string) (*untis.PersonInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.personCalls = append(f.personCalls, school+"|"+cookie)
	username := strings.TrimPrefix(cookie, "cookie:")
	if info, ok := f.people[username]; ok {
		copy := *info
		return &copy, nil
	}
	return &untis.PersonInfo{}, nil
}

func (f *fakeUserCheckAPI) Logout(school, cookie string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logouts = append(f.logouts, school+"|"+cookie)
}

func checkStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/check.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func addCheckUser(t *testing.T, st *store.Store, username, school string, info untis.PersonInfo) {
	t.Helper()
	u := &store.User{
		Username:   username,
		Password:   "pw-" + username,
		Method:     "password",
		School:     school,
		PersonType: info.PersonType,
		ClassID:    info.ClassID,
	}
	if err := st.UpsertUser(u); err != nil {
		t.Fatalf("add user %q: %v", username, err)
	}
}

func sessionsByUser(calls []string) map[string]string {
	out := map[string]string{}
	for _, c := range calls {
		parts := strings.SplitN(c, "|", 2)
		if len(parts) == 2 {
			out[parts[1]] = parts[0]
		}
	}
	return out
}

// personSchoolByUser unwraps the cookie prefix the fake uses to carry the
// username, since PersonInfo only receives the school and cookie.
func personSchoolByUser(calls []string) map[string]string {
	out := map[string]string{}
	for _, c := range calls {
		parts := strings.SplitN(c, "|", 2)
		if len(parts) == 2 {
			out[strings.TrimPrefix(parts[1], "cookie:")] = parts[0]
		}
	}
	return out
}

func countByValue(values []string) map[string]int {
	out := map[string]int{}
	for _, v := range values {
		out[v]++
	}
	return out
}

// TestUserCheckUsesEachUsersSchool is the regression test for the bug where
// every account was validated against the globally configured school, so
// multi-school deployments checked the wrong credentials at the wrong server.
// The two users share class ID 1 on purpose: names must come from each
// account's own school, not one merged id -> name map.
func TestUserCheckUsesEachUsersSchool(t *testing.T) {
	st := checkStore(t)
	addCheckUser(t, st, "alice", "alpha", untis.PersonInfo{PersonType: 5, ClassID: 1})
	addCheckUser(t, st, "bob", "beta", untis.PersonInfo{PersonType: 5, ClassID: 1})
	addCheckUser(t, st, "legacy", "", untis.PersonInfo{PersonType: 5, ClassID: 1})

	fake := &fakeUserCheckAPI{
		classes: map[string]map[int64]string{
			"alpha": {1: "alpha-class"},
			"beta":  {1: "beta-class"},
		},
		people: map[string]*untis.PersonInfo{
			"alice":  {PersonType: 5, ClassID: 1},
			"bob":    {PersonType: 5, ClassID: 1},
			"legacy": {PersonType: 5, ClassID: 1},
		},
	}
	var out bytes.Buffer
	checkUsers(st, fake, "alpha", false, &out)

	if got := sessionsByUser(fake.sessions); got["alice"] != "alpha" || got["bob"] != "beta" || got["legacy"] != "alpha" {
		t.Fatalf("sessions used the wrong school: %v", got)
	}
	if got := personSchoolByUser(fake.personCalls); got["alice"] != "alpha" || got["bob"] != "beta" || got["legacy"] != "alpha" {
		t.Fatalf("PersonInfo used the wrong school: %v", got)
	}
	if got := countByValue(fake.klassenCalls); got["alpha"] != 1 || got["beta"] != 1 {
		t.Fatalf("class lists should be fetched once per school, got %v", fake.klassenCalls)
	}
	for _, want := range []string{"SCHOOL", "alpha-class", "beta-class", "legacy"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
	if !strings.Contains(out.String(), "keep") {
		t.Errorf("expected valid accounts to be kept:\n%s", out.String())
	}
}

// TestUserCheckLegacyRowsUseFallback pins the compatibility path: a row with no
// recorded school (created before schools were stored per user) is checked
// against the explicit global -school fallback.
func TestUserCheckLegacyRowsUseFallback(t *testing.T) {
	st := checkStore(t)
	addCheckUser(t, st, "legacy", "", untis.PersonInfo{PersonType: 5, ClassID: 3})

	fake := &fakeUserCheckAPI{
		classes: map[string]map[int64]string{"fallback": {3: "fallback-class"}},
		people:  map[string]*untis.PersonInfo{"legacy": {PersonType: 5, ClassID: 3}},
	}
	var out bytes.Buffer
	checkUsers(st, fake, "fallback", false, &out)

	if got := sessionsByUser(fake.sessions)["legacy"]; got != "fallback" {
		t.Fatalf("legacy row used school %q, want fallback", got)
	}
	if !strings.Contains(out.String(), "fallback-class") {
		t.Errorf("fallback class name missing:\n%s", out.String())
	}
}

// TestUserCheckSkipsRowsWithNoResolvableSchool makes sure the tool reports the
// row instead of guessing a school and querying the wrong upstream.
func TestUserCheckSkipsRowsWithNoResolvableSchool(t *testing.T) {
	st := checkStore(t)
	addCheckUser(t, st, "homeless", "", untis.PersonInfo{PersonType: 5, ClassID: 1})

	fake := &fakeUserCheckAPI{}
	var out bytes.Buffer
	checkUsers(st, fake, "", false, &out)

	if len(fake.sessions) != 0 {
		t.Fatalf("skipped row still contacted upstream: %v", fake.sessions)
	}
	if !strings.Contains(out.String(), "no school on the account") {
		t.Errorf("missing skip explanation:\n%s", out.String())
	}
}

// TestUserCheckRemoveClasslessUsesAccountSchool ensures deletion is also based
// on the account's own school, and still applies --remove-classless.
func TestUserCheckRemoveClasslessUsesAccountSchool(t *testing.T) {
	st := checkStore(t)
	addCheckUser(t, st, "alice", "alpha", untis.PersonInfo{PersonType: 5, ClassID: 1})
	addCheckUser(t, st, "bob", "beta", untis.PersonInfo{PersonType: 5, ClassID: 0})

	fake := &fakeUserCheckAPI{
		classes: map[string]map[int64]string{"alpha": {1: "alpha-class"}},
		people: map[string]*untis.PersonInfo{
			"alice": {PersonType: 5, ClassID: 1},
			"bob":   {PersonType: 5, ClassID: 0},
		},
	}
	var out bytes.Buffer
	checkUsers(st, fake, "alpha", true, &out)

	if got := sessionsByUser(fake.sessions)["bob"]; got != "beta" {
		t.Fatalf("bob checked against %q, want beta", got)
	}
	if u, _ := st.GetUser("bob"); u != nil {
		t.Error("classless bob should have been removed")
	}
	if u, err := st.GetUser("alice"); err != nil || u == nil {
		t.Errorf("alice should have been kept: user=%v err=%v", u, err)
	}
	if !strings.Contains(out.String(), "REMOVED") {
		t.Errorf("missing REMOVED verdict:\n%s", out.String())
	}
}
