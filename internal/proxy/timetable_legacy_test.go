package proxy

// Legacy getTimetable (/WebUntis/jsonrpc.do, the non-2017 shape): session
// gate, non-class element forwarding, pooled-class owner rewrite plus the
// timestamp-range cache, and the pool gate itself.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"untis-proxy/internal/store"
)

func legacyBody(id int64, elType int, start, end string) string {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": "legacy", "method": "getTimetable",
		"params": map[string]any{"options": map[string]any{
			"element":   map[string]any{"id": id, "type": elType},
			"startDate": start, "endDate": end,
		}},
	})
	return string(b)
}

func legacyReq(t *testing.T, p *Proxy, user, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/WebUntis/jsonrpc.do?school=testschool", strings.NewReader(body))
	if user != "" {
		sess := p.sessions.New(user, 0)
		req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: sess.ID})
	}
	rec := httptest.NewRecorder()
	p.handleJSONRPC(rec, req)
	return rec
}

func seedSessionUser(t *testing.T, st *store.Store, username string, personType, personID, classID int64) {
	t.Helper()
	if err := st.UpsertUser(&store.User{Username: username, Method: "key", PersonType: personType, PersonID: personID, ClassID: classID, Password: "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"}); err != nil {
		t.Fatalf("seed %s: %v", username, err)
	}
}

func TestGetTimetableLegacyNoSessionIs8520(t *testing.T) {
	f := &fakeUpstream{}
	p, _ := newFakeProxyUpstream(t, f)
	rec := legacyReq(t, p, "", legacyBody(5000, 1, "2026-09-21", "2026-09-27"))
	if code := errCode(t, rec); code != -8520 {
		t.Errorf("error code = %d, want -8520", code)
	}
	if got := len(f.Public()); got != 0 {
		t.Errorf("upstream public calls = %d, want 0", got)
	}
}

func TestGetTimetableLegacyNonClassForwardedWithSession(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	seedSessionUser(t, st, "dee", 5, 7, 5000)
	rec := legacyReq(t, p, "dee", legacyBody(5009, 2, "2026-09-21", "2026-09-27"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	// forwarding goes to jsonrpc.do with the requesting user's real session
	pubs := f.Public()
	if len(pubs) != 1 {
		t.Fatalf("public calls = %d, want 1", len(pubs))
	}
	if !strings.Contains(pubs[0].Cookie, "JSESSIONID=") {
		t.Errorf("forwarded cookie = %q, want a real session cookie", pubs[0].Cookie)
	}
	if !strings.Contains(string(pubs[0].Body), `"method":"getTimetable"`) {
		t.Errorf("forwarded body = %s, want getTimetable", pubs[0].Body)
	}
}

func TestGetTimetableLegacyClassNotPooledIs8509(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	seedSessionUser(t, st, "dee", 5, 7, 5000)
	rec := legacyReq(t, p, "dee", legacyBody(9999, 1, "2026-09-21", "2026-09-27"))
	if code := errCode(t, rec); code != -8509 {
		t.Errorf("error code = %d, want -8509", code)
	}
	if got := len(f.Public()); got != 0 {
		t.Errorf("upstream public calls = %d, want 0 (gated)", got)
	}
}

func TestGetTimetableLegacyPooledClassServedViaOwnerAndCached(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	seedSessionUser(t, st, "owen", 5, 100, 5000)
	seedSessionUser(t, st, "dee", 5, 7, 5000)

	// requester does not attend 5000; the proxy must speak for the pool owner
	body := legacyBody(5000, 1, "2026-09-21", "2026-09-27")
	rec := legacyReq(t, p, "dee", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := len(f.Public()); got != 1 {
		t.Fatalf("public calls = %d, want 1 (owner forward)", got)
	}

	// identical request must be served from the range cache: no new upstream call
	rec2 := legacyReq(t, p, "dee", body)
	if rec2.Code != http.StatusOK {
		t.Fatalf("cached status = %d, want 200", rec2.Code)
	}
	if rec2.Body.String() != rec.Body.String() {
		t.Errorf("cached body differs from first response")
	}
	if got := len(f.Public()); got != 1 {
		t.Errorf("public calls after cache = %d, want still 1", got)
	}
}

func TestTTCacheExpires(t *testing.T) {
	c := newTTCache(time.Minute)
	k := "s|1|a|b"
	if _, ok := c.Get(k); ok {
		t.Fatal("cache get on empty map must miss")
	}
	c.Put(k, []byte("data"))
	if b, ok := c.Get(k); !ok || string(b) != "data" {
		t.Fatalf("cache get = %q/%v, want data/true", b, ok)
	}
	// force staleness by clock abuse via the stored entry time
	c.mu.Lock()
	c.m[k] = ttEntry{data: c.m[k].data, at: time.Now().Add(-2 * time.Minute)}
	c.mu.Unlock()
	if _, ok := c.Get(k); ok {
		t.Fatal("stale cache entry must miss")
	}
}
