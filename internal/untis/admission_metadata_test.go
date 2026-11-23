package untis

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPersonInfoCapturesOnlyExplicitUpstreamAdminFlag(t *testing.T) {
	for _, value := range []string{`true`, `false`, `null`} {
		t.Run(value, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "app/config") {
					w.Write([]byte(`{"data":{"loginServiceConfig":{"user":{"personId":7,"isAdmin":` + value + `,"persons":[{"id":7,"type":2}]}}}}`))
				} else {
					w.Write([]byte(`{"data":{}}`))
				}
			}))
			defer server.Close()
			client := New(Config{Server: strings.TrimPrefix(server.URL, "https://"), School: "school", HTTPClient: server.Client()})
			info, err := client.PersonInfo("school", "cookie")
			if err != nil {
				t.Fatal(err)
			}
			if value == "null" {
				if info.UpstreamAdmin != nil {
					t.Fatal("unknown admin flag inferred from teacher identity")
				}
			} else if info.UpstreamAdmin == nil || *info.UpstreamAdmin != (value == "true") {
				t.Fatal("explicit flag not retained")
			}
		})
	}
}
