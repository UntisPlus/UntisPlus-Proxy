package proxy

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"untis-proxy/internal/session"
	"untis-proxy/internal/store"
	"untis-proxy/internal/untis"
)

func newFreshSetupProxy(t *testing.T) (*Proxy, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	_ = st.SetDefaultSchool("testschool")
	p := New(st, untis.New(untis.Config{Server: "school.example.com", School: "testschool"}), session.NewManager(5*time.Minute), Options{School: "testschool", OutageVerifyPings: -1})
	return p, st
}

func TestHandleRootSetupAndReadyStates(t *testing.T) {
	t.Run("fresh setup", func(t *testing.T) {
		p, _ := newFreshSetupProxy(t)
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		p.handleRoot(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d want 200", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Fatalf("content-type=%q", ct)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
			t.Fatalf("cache-control=%q", cc)
		}
		body := rec.Body.String()
		for _, want := range []string{
			"Initial setup required",
			"Continue to guided setup & login",
			"Setup pending",
			"trusted proxy",
			"real client IP",
		} {
			if !strings.Contains(strings.ToLower(body), strings.ToLower(want)) {
				t.Errorf("root setup page missing %q", want)
			}
		}
	})

	t.Run("ready", func(t *testing.T) {
		p, _ := newTestProxy(t) // has admin bob, normalized/setup-complete behavior expected
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		p.handleRoot(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d want 200", rec.Code)
		}
		body := rec.Body.String()
		for _, want := range []string{"Open admin dashboard", "Ready", "Administration dashboard"} {
			if !strings.Contains(body, want) {
				t.Errorf("root ready page missing %q", want)
			}
		}
		if strings.Contains(body, "Initial setup required") {
			t.Error("ready root page should not show initial setup required")
		}
	})
}

func TestHandleSetupPublic(t *testing.T) {
	t.Run("method not allowed", func(t *testing.T) {
		p, _ := newFreshSetupProxy(t)
		req := httptest.NewRequest(http.MethodPost, "/setup", nil)
		rec := httptest.NewRecorder()
		p.handleSetupPublic(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status=%d want 405", rec.Code)
		}
	})

	t.Run("fresh setup diagnostics", func(t *testing.T) {
		p, _ := newFreshSetupProxy(t)
		req := httptest.NewRequest(http.MethodGet, "http://example.test/setup", nil)
		req.RemoteAddr = "198.51.100.20:4567"
		req.Header.Set("X-Forwarded-For", "203.0.113.9")
		req.Header.Set("X-Real-IP", "203.0.113.10")
		req.Header.Set("X-Forwarded-Proto", "https")
		rec := httptest.NewRecorder()
		p.handleSetupPublic(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var st setupStatus
		if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
			t.Fatalf("decode: %v body=%s", err, rec.Body.String())
		}
		if !st.SetupNeeded || st.SetupCompleted || st.HasAdmin || !st.CanClaimAdmin {
			t.Fatalf("unexpected setup flags: %+v", st)
		}
		if st.ClientIP.PeerIP != "198.51.100.20" {
			t.Errorf("peerIP=%q want 198.51.100.20", st.ClientIP.PeerIP)
		}
		if st.ClientIP.ResolvedIP != "198.51.100.20" {
			t.Errorf("resolvedIP=%q want peer ip when no trusted proxies", st.ClientIP.ResolvedIP)
		}
		if st.ClientIP.PeerTrusted || st.ClientIP.UsingForwarded {
			t.Errorf("unexpected trust flags: trusted=%v usingForwarded=%v", st.ClientIP.PeerTrusted, st.ClientIP.UsingForwarded)
		}
		if st.ClientIP.XForwardedFor == "" || st.ClientIP.XForwardedProto != "https" {
			t.Errorf("expected forwarded headers echoed: %+v", st.ClientIP)
		}
	})

	t.Run("ready instance minimal response", func(t *testing.T) {
		p, st := newTestProxy(t)
		// newTestProxy seeds admin bob; store normalization should treat as completed/ready.
		if done, _ := st.SetupCompleted(); !done {
			// ensure normalized if needed by querying state helpers
			_, _ = st.SetupNeeded()
		}
		req := httptest.NewRequest(http.MethodGet, "/setup", nil)
		rec := httptest.NewRecorder()
		p.handleSetupPublic(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if v, ok := m["setupNeeded"].(bool); !ok || v {
			t.Fatalf("expected setupNeeded=false in ready instance, got %+v", m)
		}
	})
}

func TestMaybeClaimSetupAdmin(t *testing.T) {
	t.Run("claims first non-anonymous user on fresh setup", func(t *testing.T) {
		p, st := newFreshSetupProxy(t)
		p.maybeClaimSetupAdmin("Alice")
		admin, _ := st.IsAdmin("alice")
		if !admin {
			t.Fatal("expected alice to be claimed as admin")
		}
		fresh, _ := st.SetupFreshClaim()
		if !fresh {
			t.Fatal("expected fresh claim flag to be set")
		}
		needed, _ := st.SetupNeeded()
		if needed {
			t.Fatal("claim should clear setup-needed state")
		}
		onboarding, _ := st.SetupOnboarding()
		if !onboarding {
			t.Fatal("fresh claim should keep onboarding open until completed")
		}
	})

	t.Run("does not claim anonymous or when admin exists", func(t *testing.T) {
		p, st := newFreshSetupProxy(t)
		p.maybeClaimSetupAdmin("#anonymous#")
		has, _ := st.HasAnyAdmin()
		if has {
			t.Fatal("anonymous must not claim admin")
		}
		if err := st.SetAdmin("bob", true); err != nil {
			t.Fatalf("seed admin: %v", err)
		}
		_ = st.SetSetupCompleted(false)
		_ = st.SetSetupFreshClaim(false)
		p.maybeClaimSetupAdmin("alice")
		admin, _ := st.IsAdmin("alice")
		if admin {
			t.Fatal("must not claim when an admin already exists")
		}
	})

	t.Run("does not claim when setup not needed", func(t *testing.T) {
		p, st := newFreshSetupProxy(t)
		if err := st.SetSetupCompleted(true); err != nil {
			t.Fatalf("complete: %v", err)
		}
		p.maybeClaimSetupAdmin("alice")
		has, _ := st.HasAnyAdmin()
		if has {
			t.Fatal("must not claim when setup already completed")
		}
	})
}

func TestTrustedProxyConfigAndRealClientIP(t *testing.T) {
	t.Run("parse and normalize", func(t *testing.T) {
		p, _ := newFreshSetupProxy(t)
		if err := p.SetTrustedProxies(" 10.0.0.1, 10.2.0.0/16  2001:db8::1 "); err != nil {
			t.Fatalf("SetTrustedProxies: %v", err)
		}
		got := p.TrustedProxies()
		if !strings.Contains(got, "10.0.0.1") || !strings.Contains(got, "10.2.0.0/16") || !strings.Contains(got, "2001:db8::1") {
			t.Fatalf("normalized=%q", got)
		}
		if err := p.SetTrustedProxies("not-an-ip"); err == nil {
			t.Fatal("expected invalid trusted proxy to fail")
		}
		if err := p.SetTrustedProxies(""); err != nil {
			t.Fatalf("clear: %v", err)
		}
		if got := p.TrustedProxies(); got != "" {
			t.Fatalf("cleared=%q want empty", got)
		}
	})

	t.Run("real client ip from forwarded chain only for trusted peer", func(t *testing.T) {
		p, _ := newFreshSetupProxy(t)
		if err := p.SetTrustedProxies("10.0.0.0/8"); err != nil {
			t.Fatalf("set trusted: %v", err)
		}
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.1.2.3:1234"
		req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.9.9.9")
		if got := p.realClientIP(req); got != "203.0.113.9" {
			t.Errorf("trusted peer realClientIP=%q want 203.0.113.9", got)
		}
		req.Header.Set("X-Forwarded-For", "")
		req.Header.Set("X-Real-IP", "203.0.113.10")
		if got := p.realClientIP(req); got != "203.0.113.10" {
			t.Errorf("trusted peer X-Real-IP realClientIP=%q want 203.0.113.10", got)
		}

		req2 := httptest.NewRequest(http.MethodGet, "/", nil)
		req2.RemoteAddr = "198.51.100.20:1234"
		req2.Header.Set("X-Forwarded-For", "203.0.113.9")
		if got := p.realClientIP(req2); got != "198.51.100.20" {
			t.Errorf("untrusted peer realClientIP=%q want peer ip", got)
		}
	})
}

func TestAdminSetupEndpoints(t *testing.T) {
	p, st := newTestProxy(t)

	// GET /admin/setup returns current setup/client-ip diagnostics for admins.
	req := httptest.NewRequest(http.MethodGet, "/admin/setup", nil)
	req.RemoteAddr = "10.0.0.5:1111"
	req.Header.Set("X-Forwarded-For", "203.0.113.77")
	s := p.sessions.New("bob", 0)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: s.ID})
	rec := httptest.NewRecorder()
	p.handleAdmin(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/setup status=%d body=%s", rec.Code, rec.Body.String())
	}
	var stResp setupStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &stResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if stResp.ClientIP.PeerIP != "10.0.0.5" || stResp.ClientIP.ResolvedIP == "" {
		t.Fatalf("unexpected client ip diagnostics: %+v", stResp.ClientIP)
	}

	// POST trusted-proxies validates input.
	rec = adminRequest(t, p, "bob", http.MethodPost, "/admin/setup/trusted-proxies", `{"trustedProxies":"bad value"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid trusted-proxies status=%d body=%s want 400", rec.Code, rec.Body.String())
	}

	// POST trusted-proxies persists normalized config and affects real client IP resolution.
	req = httptest.NewRequest(http.MethodPost, "/admin/setup/trusted-proxies", strings.NewReader(`{"trustedProxies":"10.0.0.0/8"}`))
	req.RemoteAddr = "10.0.0.5:2222"
	req.Header.Set("X-Forwarded-For", "203.0.113.77, 10.0.0.5")
	s = p.sessions.New("bob", 0)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: s.ID})
	rec = httptest.NewRecorder()
	p.handleAdmin(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("set trusted-proxies status=%d body=%s", rec.Code, rec.Body.String())
	}
	var setResp struct {
		OK             bool   `json:"ok"`
		TrustedProxies string `json:"trustedProxies"`
		ClientIP       struct {
			ResolvedIP     string `json:"resolvedIP"`
			PeerTrusted    bool   `json:"peerTrusted"`
			UsingForwarded bool   `json:"usingForwarded"`
		} `json:"clientIP"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &setResp); err != nil {
		t.Fatalf("decode set response: %v", err)
	}
	if !setResp.OK || setResp.Error != "" {
		t.Fatalf("unexpected set response: %+v body=%s", setResp, rec.Body.String())
	}
	if !strings.Contains(setResp.TrustedProxies, "10.0.0.0/8") {
		t.Fatalf("trustedProxies not normalized/persisted in response: %q", setResp.TrustedProxies)
	}
	if !setResp.ClientIP.PeerTrusted || !setResp.ClientIP.UsingForwarded || setResp.ClientIP.ResolvedIP != "203.0.113.77" {
		t.Fatalf("unexpected clientIP in response: %+v", setResp.ClientIP)
	}
	if got := p.TrustedProxies(); got != setResp.TrustedProxies {
		t.Fatalf("proxy trusted proxies=%q want %q", got, setResp.TrustedProxies)
	}
	if v, ok, err := st.GetSetting(store.SettingTrustedProxies); err != nil || !ok || v != setResp.TrustedProxies {
		t.Fatalf("stored trusted_proxies=%q ok=%v err=%v want %q", v, ok, err, setResp.TrustedProxies)
	}

	// GET trusted-proxies endpoint
	rec = adminRequest(t, p, "bob", http.MethodGet, "/admin/setup/trusted-proxies", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET trusted-proxies status=%d body=%s", rec.Code, rec.Body.String())
	}
	var gotTP map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &gotTP)
	if gotTP["trustedProxies"] != setResp.TrustedProxies {
		t.Fatalf("GET trusted-proxies=%v want %q", gotTP["trustedProxies"], setResp.TrustedProxies)
	}

	// Complete/reset onboarding flags.
	rec = adminRequest(t, p, "bob", http.MethodPost, "/admin/setup/complete", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("complete status=%d body=%s", rec.Code, rec.Body.String())
	}
	done, _ := st.SetupCompleted()
	if !done {
		t.Fatal("expected setup completed after /admin/setup/complete")
	}
	rec = adminRequest(t, p, "bob", http.MethodPost, "/admin/setup/reset", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("reset status=%d body=%s", rec.Code, rec.Body.String())
	}
	done, _ = st.SetupCompleted()
	if done {
		t.Fatal("expected setup reopened after /admin/setup/reset")
	}
}

func TestAdminLoginAndDashboardServeUpdatedUI(t *testing.T) {
	p, st := newFreshSetupProxy(t)

	// Login page served anonymously on fresh setup includes onboarding hinting.
	req := httptest.NewRequest(http.MethodGet, "/admin/login", nil)
	rec := httptest.NewRecorder()
	p.handleAdminLogin(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login page status=%d want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("login content-type=%q", ct)
	}
	login := rec.Body.String()
	for _, want := range []string{
		"UntisPlus Proxy",
		"Admin Console",
		"fetch('/setup'",
		"Initial setup detected",
		"/admin#onboarding",
		"6-digit code",
	} {
		if !strings.Contains(login, want) {
			t.Errorf("login page missing %q", want)
		}
	}

	// Dashboard HTML contains key new workflow sections and token fix.
	if err := st.SetAdmin("bob", true); err != nil {
		t.Fatalf("set admin: %v", err)
	}
	dreq := httptest.NewRequest(http.MethodGet, "/admin", nil)
	dreq.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: p.sessions.New("bob", 0).ID})
	drec := httptest.NewRecorder()
	p.handleAdminDashboard(drec, dreq)
	if drec.Code != http.StatusOK {
		t.Fatalf("dashboard status=%d body=%s", drec.Code, drec.Body.String())
	}
	html := drec.Body.String()
	for _, want := range []string{
		"UntisPlus Proxy Admin",
		`id="view-onboarding"`,
		`id="view-access"`,
		`id="view-notifications"`,
		`id="view-users"`,
		`id="view-tokens"`,
		`id="setupTrustedProxies"`,
		`saveSetupTrustedProxies`,
		`loadSetupStatus`,
		`value="STUDENT">student</option>`,
		`Admin-created calendar tokens are available for class, teacher, room, subject, and student elements.`,
		`Search and pick an element first.`,
		`async function testNtfy`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboard missing expected marker %q", want)
		}
	}
	// Token creator select must not offer PERSONAL/SCHOOL (admin calendar tokens API doesn't support them).
	tokenSelect := html
	if idx := strings.Index(html, `id="tokType"`); idx >= 0 {
		end := strings.Index(html[idx:], `</select>`)
		if end > 0 {
			tokenSelect = html[idx : idx+end]
		}
	}
	if strings.Contains(tokenSelect, `value="PERSONAL"`) || strings.Contains(tokenSelect, `value="SCHOOL"`) {
		t.Errorf("token creator select should not include PERSONAL/SCHOOL: %s", tokenSelect)
	}
}

func TestRequestIsHTTPS(t *testing.T) {
	t.Run("direct tls", func(t *testing.T) {
		p, _ := newFreshSetupProxy(t)
		req := httptest.NewRequest(http.MethodGet, "https://example.test/", nil)
		req.TLS = &tls.ConnectionState{}
		if !p.requestIsHTTPS(req) {
			t.Fatal("expected TLS request to be recognized as HTTPS")
		}
	})

	t.Run("no trusted proxies honors forwarded proto for backward compatibility", func(t *testing.T) {
		p, _ := newFreshSetupProxy(t)
		req := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
		req.RemoteAddr = "198.51.100.20:1234"
		req.Header.Set("X-Forwarded-Proto", "https")
		if !p.requestIsHTTPS(req) {
			t.Fatal("expected X-Forwarded-Proto=https to be honored when no trusted proxies configured")
		}
	})

	t.Run("trusted proxies require trusted peer for forwarded proto", func(t *testing.T) {
		p, _ := newFreshSetupProxy(t)
		if err := p.SetTrustedProxies("10.0.0.0/8"); err != nil {
			t.Fatalf("SetTrustedProxies: %v", err)
		}
		trusted := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
		trusted.RemoteAddr = "10.1.2.3:1234"
		trusted.Header.Set("X-Forwarded-Proto", "https")
		if !p.requestIsHTTPS(trusted) {
			t.Fatal("expected forwarded proto from trusted peer to be honored")
		}

		untrusted := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
		untrusted.RemoteAddr = "198.51.100.20:1234"
		untrusted.Header.Set("X-Forwarded-Proto", "https")
		if p.requestIsHTTPS(untrusted) {
			t.Fatal("forwarded proto from untrusted peer must be ignored when trusted proxies are configured")
		}
	})
}

func TestHandleSetupPublicFreshClaimedOnboardingState(t *testing.T) {
	p, st := newFreshSetupProxy(t)
	if err := st.SetAdmin("alice", true); err != nil {
		t.Fatalf("SetAdmin: %v", err)
	}
	if err := st.SetSetupFreshClaim(true); err != nil {
		t.Fatalf("SetSetupFreshClaim: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/setup", nil)
	rec := httptest.NewRecorder()
	p.handleSetupPublic(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var stResp setupStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &stResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !stResp.SetupNeeded {
		t.Fatalf("expected onboarding SetupNeeded=true after fresh claim, got %+v", stResp)
	}
	if stResp.SetupCompleted || !stResp.HasAdmin || stResp.CanClaimAdmin {
		t.Fatalf("unexpected fresh-claimed onboarding status: %+v", stResp)
	}
}

func TestNewLoadsTrustedProxiesFromSetting(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.SetSetting(store.SettingTrustedProxies, "10.9.0.0/16"); err != nil {
		t.Fatalf("set setting: %v", err)
	}
	p := New(st, untis.New(untis.Config{Server: "school.example.com", School: "testschool"}), session.NewManager(5*time.Minute), Options{
		School:            "testschool",
		TrustedProxies:    "10.1.0.0/16",
		OutageVerifyPings: -1,
	})
	if got := p.TrustedProxies(); got != "10.9.0.0/16" {
		t.Fatalf("trusted proxies=%q want setting override 10.9.0.0/16", got)
	}
}
