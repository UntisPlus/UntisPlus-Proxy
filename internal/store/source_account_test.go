package store

import "testing"

func TestTeacherSourcesExcludeUnknownAndOtherAccountTypes(t *testing.T) {
	st := openTestStore(t)
	for _, u := range []*User{{Username: "teacher", School: "school", PersonType: 2, Password: "fake-test-secret", Method: "key"}, {Username: "unknown", School: "school", PersonType: 0, Password: "fake-test-secret", Method: "key"}, {Username: "other", School: "school", PersonType: 6, Password: "fake-test-secret", Method: "key"}, {Username: "student", School: "school", PersonType: 5, Password: "fake-test-secret", Method: "key"}} {
		if err := st.UpsertUser(u); err != nil {
			t.Fatal(err)
		}
	}
	sources, err := st.BoostedSourceAccounts("school")
	if err != nil || len(sources) != 1 || sources[0].Username != "teacher" {
		t.Fatalf("unexpected source accounts: %+v %v", sources, err)
	}
}
