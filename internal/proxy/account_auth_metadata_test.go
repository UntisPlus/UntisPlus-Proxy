package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"untis-proxy/internal/store"
)

func TestAdminUsersReportsAuthenticationKindsWithoutCredentials(t *testing.T) {
	p, st := newTestProxy(t)
	for _, u := range []*store.User{{Username: "token", Method: "key", Password: "private-token-test"}, {Username: "password", Method: "password", Password: "private-password-test"}, {Username: "both", Method: "key", Password: "private-both-key"}, {Username: "empty", Method: "key"}} {
		if err := st.UpsertUser(u); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpsertCredential("both", "private-both-secret", "private-both-password"); err != nil {
		t.Fatal(err)
	}
	rec := adminRequest(t, p, "bob", http.MethodGet, "/admin/users", "")
	for _, secret := range []string{"private-token-test", "private-password-test", "private-both-key", "private-both-secret", "private-both-password"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatal("credential leaked into admin response")
		}
	}
	var result struct {
		Users []struct {
			Username string `json:"username"`
			Known    bool   `json:"credentialStatusKnown"`
			Token    bool   `json:"hasSharedSecret"`
			Password bool   `json:"hasPassword"`
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	expected := map[string][2]bool{"token": {true, false}, "password": {false, true}, "both": {true, true}, "empty": {false, false}}
	found := 0
	for _, u := range result.Users {
		want, ok := expected[u.Username]
		if !ok {
			continue
		}
		found++
		if !u.Known || u.Token != want[0] || u.Password != want[1] {
			t.Fatalf("wrong metadata for %s: %+v", u.Username, u)
		}
	}
	if found != 4 {
		t.Fatalf("found %d test accounts, want 4", found)
	}
}
