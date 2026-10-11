package proxy

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"untis-proxy/internal/store"
)

func TestValidateTargetURLRejectsUnsafe(t *testing.T) {
	cases := []string{
		"",
		"ftp://example.com/h",
		"file:///etc/passwd",
		"gopher://127.0.0.1:70/",
		"http://localhost/h",
		"http://localhost:9999/h",
		"http://api.localhost/h",
		"http://127.0.0.1/h",
		"http://127.0.0.1:9999/h",
		"http://[::1]/h",
		"http://0.0.0.0/h",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.5/h",
		"http://192.168.1.10/h",
		"http://172.16.5.5/h",
	}
	for _, u := range cases {
		if err := validateTargetURL(u, false); err == nil {
			t.Errorf("validateTargetURL(%q) = nil, want error", u)
		}
	}
}

func TestValidateTargetURLAllowsPublic(t *testing.T) {
	for _, u := range []string{
		"https://example.com/hook",
		"http://1.1.1.1/h",
		"https://[2606:4700:4700::1111]/h",
	} {
		if err := validateTargetURL(u, false); err != nil {
			t.Errorf("validateTargetURL(%q) = %v, want nil", u, err)
		}
	}
}

func TestValidateTargetURLAllowPrivateOverride(t *testing.T) {
	if err := validateTargetURL("http://127.0.0.1:9000/h", true); err != nil {
		t.Errorf("private target rejected even with allowPrivate: %v", err)
	}
	if err := validateTargetURL("ftp://127.0.0.1/h", true); err == nil {
		t.Error("a non-http scheme was accepted even with allowPrivate")
	}
}

func TestOutboundClientBlocksPrivateRedirect(t *testing.T) {
	prev := allowPrivateTargets
	allowPrivateTargets = false
	t.Cleanup(func() { allowPrivateTargets = prev })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1/internal", http.StatusFound)
	}))
	defer srv.Close()

	_, err := outboundHTTPClient().Get(srv.URL)
	if err == nil {
		t.Fatal("expected a redirect to a private host to be refused")
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSubsRejectsSSRFTargets(t *testing.T) {
	p, st := newTestProxy(t)
	// newTestProxy allows private targets for the loopback delivery tests; turn
	// the guard back on for this one.
	allowPrivateTargets = false
	if err := st.UpsertUser(&store.User{Username: "carla", Method: "key", ClassID: 5000}); err != nil {
		t.Fatalf("add carla: %v", err)
	}
	for _, url := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://127.0.0.1:6379/",
		"file:///etc/passwd",
	} {
		rec := subsRequest(t, p, "carla", http.MethodPost, "/api/webhooks", `{"classId":5000,"url":"`+url+`"}`)
		if !strings.Contains(rec.Body.String(), "not allowed") && !strings.Contains(rec.Body.String(), "http") {
			t.Fatalf("unsafe webhook %q not refused: %s", url, rec.Body.String())
		}
	}
	hooks, err := p.store.ListWebhooks("testschool")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(hooks) != 0 {
		t.Fatalf("an unsafe webhook was stored: %+v", hooks)
	}
}

func TestSessionCookieSecureOnlyOverTLS(t *testing.T) {
	p := &Proxy{}

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/WebUntis/jsonrpc.do", nil)
	r.TLS = &tls.ConnectionState{}
	p.setSessionCookies(rec, r, "sid123", "testschool")
	ck := namedCookie(rec.Result().Cookies(), "JSESSIONID")
	if ck == nil {
		t.Fatal("no JSESSIONID cookie set")
	}
	if !ck.Secure {
		t.Error("cookie is not Secure over TLS")
	}
	if !ck.HttpOnly {
		t.Error("cookie is not HttpOnly")
	}
	if ck.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", ck.SameSite)
	}

	// A plaintext local run must still receive a usable (non-Secure) cookie.
	rec = httptest.NewRecorder()
	p.setSessionCookies(rec, httptest.NewRequest(http.MethodPost, "/", nil), "sid", "testschool")
	if ck := namedCookie(rec.Result().Cookies(), "JSESSIONID"); ck == nil || ck.Secure {
		t.Error("cookie marked Secure over plain HTTP; the local dashboard would lose its session")
	}
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	p, _ := newTestProxy(t)
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	h := rec.Header()
	for _, k := range []string{"X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy", "Content-Security-Policy"} {
		if h.Get(k) == "" {
			t.Errorf("missing security header %s", k)
		}
	}
	if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := h.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}

	// HSTS is only meaningful (and only sent) over TLS.
	rec = httptest.NewRecorder()
	rt := httptest.NewRequest(http.MethodGet, "/nope", nil)
	rt.TLS = &tls.ConnectionState{}
	p.Handler().ServeHTTP(rec, rt)
	if rec.Header().Get("Strict-Transport-Security") == "" {
		t.Error("no HSTS header over TLS")
	}
}

func TestCalendarTokenExpiry(t *testing.T) {
	prev := calendarTokenMaxAge
	defer func() { calendarTokenMaxAge = prev }()

	calendarTokenMaxAge = 24 * time.Hour
	if !tokenExpired(&store.ClassToken{CreatedAt: time.Now().Add(-48 * time.Hour).Unix()}) {
		t.Error("a 48h-old token is not expired with a 24h max age")
	}
	if tokenExpired(&store.ClassToken{CreatedAt: time.Now().Add(-1 * time.Hour).Unix()}) {
		t.Error("a 1h-old token is expired with a 24h max age")
	}

	calendarTokenMaxAge = 0
	if tokenExpired(&store.ClassToken{CreatedAt: 1}) {
		t.Error("expiry disabled, but an old token is reported expired")
	}
}

func namedCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}
