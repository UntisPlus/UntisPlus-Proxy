package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"untis-proxy/internal/session"
	"untis-proxy/internal/store"
	"untis-proxy/internal/untis"
)

// fakeUntis implements the handful of upstream endpoints the keyLogin path
// touches so the login can be exercised without a real WebUntis server.
type fakeUntis struct {
	mu        sync.Mutex
	loginAuth map[string]any // decoded auth block sent to getUserData2017
	rejectKey string         // if set, logins presenting this replay key fail
}

func (f *fakeUntis) handler() http.Handler {
	host := ""
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/WebUntis/jsonrpc_intern.do"):
			var req struct {
				Method string `json:"method"`
				Params []struct {
					Auth map[string]any `json:"auth"`
				} `json:"params"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.mu.Lock()
			if len(req.Params) > 0 {
				f.loginAuth = req.Params[0].Auth
			}
			reject := ""
			if f.rejectKey != "" {
				reject = f.rejectKey
			}
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Set-Cookie", "JSESSIONID=fakesid; Path=/")
			if reject != "" {
				auth := map[string]any{}
				if len(req.Params) > 0 {
					auth = req.Params[0].Auth
				}
				if got, _ := auth["key"].(string); got == reject {
					_, _ = w.Write([]byte(`{"error":{"code":-8998,"message":"invalid secret"},"id":"upstream"}`))
					return
				}
			}
			_, _ = w.Write([]byte(`{"id":"upstream","jsonrpc":"2.0","result":{}}`))
		case strings.HasPrefix(r.URL.Path, "/WebUntis/jsonrpc.do"):
			w.WriteHeader(http.StatusOK)
		default:
			// /api/app/config and /api/daytimetable/config: leave info zeroed
			http.Error(w, "no", http.StatusNotFound)
		}
		_ = host
	})
}

func newFakeProxy(t *testing.T, f *fakeUntis) (*Proxy, *store.Store, string) {
	t.Helper()
	srv := httptest.NewTLSServer(f.handler())
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "https://")

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	_ = st.SetDefaultSchool("testschool")

	cl := untis.New(untis.Config{Server: host, School: "testschool", HTTPClient: srv.Client()})
	p := New(st, cl, session.NewManager(5*time.Minute), Options{School: "testschool"})
	return p, st, host
}

func loginBody(t *testing.T, user, otp string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"id": "t", "jsonrpc": "2.0", "method": "getUserData2017",
		"params": []any{map[string]any{
			"auth": map[string]any{
				"user": user, "otp": otp, "clientTime": time.Now().UnixMilli(),
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestKeyLoginPastedSecret exercises the password fallback: pasting the base32
// shared secret in the otp field must (a) derive a valid TOTP for the upstream
// login, (b) present the pasted secret as the replay key, and (c) persist it so
// the account is replayable afterwards — mirroring the getAppSharedSecret path.
func TestKeyLoginPastedSecret(t *testing.T) {
	f := &fakeUntis{}
	p, st, _ := newFakeProxy(t, f)
	secret := "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"

	// The login and the comparison below both read the clock. Frozen, so the two
	// cannot land on opposite sides of a 30-second TOTP window and make this test
	// fail once every few runs.
	restore := untis.SetTOTPClock(func() time.Time { return time.Unix(1790000000, 0).UTC() })
	defer restore()

	req := httptest.NewRequest(http.MethodPost, "/WebUntis/jsonrpc_intern.do?m=getUserData2017&school=testschool", strings.NewReader(loginBody(t, "newkid", secret)))
	rec := httptest.NewRecorder()
	p.handleJSONRPCIntern(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d, body %s", rec.Code, rec.Body.String())
	}
	sid := findCookie(rec, "JSESSIONID")
	if sid == "" {
		t.Fatal("expected a proxy session cookie (JSESSIONID) after login")
	}

	f.mu.Lock()
	auth := f.loginAuth
	f.mu.Unlock()
	if got := auth["otp"]; got != untis.TOTP(secret) {
		t.Errorf("upstream otp = %v, want derived TOTP %s (secret must be turned into a live code)", got, untis.TOTP(secret))
	}
	if got := auth["key"]; got != secret {
		t.Errorf("upstream key = %v, want pasted secret %s", got, secret)
	}

	stored, _ := st.GetSecret("newkid")
	if stored != secret {
		t.Errorf("stored secret = %q, want %q (account becomes replayable)", stored, secret)
	}
	u, _ := st.GetUser("newkid")
	if u == nil || u.Password != secret {
		t.Errorf("user row password = %v, want %q", u, secret)
	}
}

// TestKeyLoginPastedSecretRejectedDoesNotClobber proves a stale/foreign pasted
// secret that the real server rejects leaves a previously stored key intact.
func TestKeyLoginPastedSecretRejectedDoesNotClobber(t *testing.T) {
	f := &fakeUntis{rejectKey: "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"}
	p, st, _ := newFakeProxy(t, f)
	good := "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
	if err := st.UpsertSecret("evan", good); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/WebUntis/jsonrpc_intern.do?m=getUserData2017&school=testschool", strings.NewReader(loginBody(t, "evan", f.rejectKey)))
	rec := httptest.NewRecorder()
	p.handleJSONRPCIntern(rec, req)

	if rec.Code != http.StatusBadGateway && rec.Code != http.StatusOK {
		t.Fatalf("unexpected login status %d", rec.Code)
	}
	stored, _ := st.GetSecret("evan")
	if stored != good {
		t.Errorf("stored secret = %q, want the original %q (failed paste must not clobber)", stored, good)
	}
	u, _ := st.GetUser("evan")
	if u != nil && u.Password != good {
		t.Errorf("user row password = %q, want original %q", u.Password, good)
	}
}

// TestKeyLoginLiveOTPUnchanged verifies a real 6-digit OTP does not trigger the
// secret fallback (upstream receives it verbatim, no replay key forced).
func TestKeyLoginLiveOTPUnchanged(t *testing.T) {
	f := &fakeUntis{}
	p, _, _ := newFakeProxy(t, f)
	otp := "246810" // all chars in base32 range but length 6 → never a secret

	req := httptest.NewRequest(http.MethodPost, "/WebUntis/jsonrpc_intern.do?m=getUserData2017&school=testschool", strings.NewReader(loginBody(t, "evan", otp)))
	rec := httptest.NewRecorder()
	p.handleJSONRPCIntern(rec, req)

	f.mu.Lock()
	auth := f.loginAuth
	f.mu.Unlock()
	if got := auth["otp"]; got != otp {
		t.Errorf("upstream otp = %v, want verbatim %q", got, otp)
	}
	if got := auth["key"]; got != nil && got != "" {
		t.Errorf("upstream key = %v, want empty (no stored secret, no paste)", got)
	}
}

func findCookie(rec *httptest.ResponseRecorder, name string) string {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}
