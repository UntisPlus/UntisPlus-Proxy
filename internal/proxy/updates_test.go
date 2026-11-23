package proxy

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestReleaseVersionStates(t *testing.T) {
	for _, tc := range []struct{ current, latest, status string }{{"v1.2.3", "1.2.3", "current"}, {"1.2.9", "v1.2.10", "update_available"}, {"2.0.0", "v1.9.9", "ahead"}, {"2.0.0-dev-analytics", "v2.0.0", "development"}, {"1.2.3-rc1", "v1.2.3", "development"}, {"dev", "v1.2.3", "development"}, {"nonsense", "v1.2.3", "unknown"}, {"v1.2.3+local", "v1.2.3", "current"}} {
		status, _, _ := versionState(tc.current, tc.latest)
		if status != tc.status {
			t.Errorf("%s vs %s = %s", tc.current, tc.latest, status)
		}
	}
}
func TestReleaseCacheCoalescingAndStaleFailure(t *testing.T) {
	var calls atomic.Int64
	var failed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("User-Agent") == "" {
			t.Error("missing user agent")
		}
		if failed.Load() {
			w.WriteHeader(500)
			return
		}
		w.Write([]byte(`{"tag_name":"v2.0.0","html_url":"https://github.com/UntisPlus/UntisPlus-Proxy/releases/tag/v2.0.0","published_at":"2026-11-23T00:00:00Z"}`))
	}))
	defer server.Close()
	c := newReleaseChecker()
	c.endpoint = server.URL
	c.client = server.Client()
	now := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := c.check("1.0.0", now)
			if result.Status != "update_available" {
				t.Error("unexpected release state")
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("release checks not coalesced")
	}
	failed.Store(true)
	result := c.check("1.0.0", now.Add(2*time.Hour))
	if result.Status != "unknown" || !result.Stale || result.LatestRelease != "v2.0.0" || result.UpdateAvailable != nil {
		t.Fatal("failure reported stale result as current")
	}
}
func TestReleaseRejectsBadMetadataAndHandlesRateLimit(t *testing.T) {
	for _, tc := range []struct {
		status     int
		body, kind string
	}{{200, `{"tag_name":"v1.0.0","html_url":"javascript:alert(1)"}`, "invalid_release"}, {200, `{"tag_name":"v1.0.0","html_url":"https://github.com/UntisPlus/UntisPlus-Proxy/releases/tag/v1.0.0","prerelease":true}`, "invalid_release"}, {403, ``, "rate_limited"}, {404, ``, "no_release"}, {200, `broken`, "invalid_release"}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); w.Write([]byte(tc.body)) }))
		c := newReleaseChecker()
		c.endpoint = server.URL
		c.client = server.Client()
		r := c.check("1.0.0", time.Now())
		server.Close()
		if r.Error != tc.kind || r.Status != "unknown" || r.MatchesLatest != nil {
			t.Fatalf("bad metadata treated as release: %+v", r)
		}
	}
}
func TestReleaseMasterOffDoesNotFetch(t *testing.T) {
	p, st := newTestProxy(t)
	seedSessionUser(t, st, "nobody", 5, 7, 5000)
	st.SetFeatureEnabled("", "update_check", false)
	p.releaseChecker.endpoint = "http://127.0.0.1:1"
	w := adminRequest(t, p, "bob", "GET", "/admin/updates", "")
	if w.Code != 200 || w.Body.String() == "" {
		t.Fatal("disabled release status unavailable")
	}
	if !p.releaseChecker.cached.CheckedAt.IsZero() {
		t.Fatal("disabled checker fetched network")
	}
	if denied := adminRequest(t, p, "nobody", "GET", "/admin/updates", ""); denied.Code != 403 {
		t.Fatal("non-admin could access release check")
	}
}
