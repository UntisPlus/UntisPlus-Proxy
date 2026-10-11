package proxy

// Regression tests for the security-hardening pass: the Technik aggregate is no
// longer served on a client-asserted identity, credential-bearing bodies are
// redacted before logging, the secrets map is read under its lock, request
// bodies are bounded, the synthetic Technik class resolves for every viewer that
// sees an event, and the timetable cache respects its cap.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"untis-proxy/internal/store"
	"untis-proxy/internal/untis"
)

// TestTechnikAggregateRefusesBareBodyAuth is the C1 guard: a username in the
// request body is not a credential. Without either a real session or an OTP that
// matches the stored secret, the aggregate must be refused — anyone can type a
// techniker's name.
func TestTechnikAggregateRefusesBareBodyAuth(t *testing.T) {
	p, st := eventProxy(t)
	if err := st.SetPerm("dee", store.FeatureTechniker, true); err != nil {
		t.Fatalf("grant techniker: %v", err)
	}
	seedEvent(t, st, "dee", testEvent())

	// No session cookie at all, and an OTP that matches no stored secret.
	rec := internReq(t, p, "getTimetable2017", "",
		internBody("getTimetable2017", ttParam(technikClassID, "CLASS", "2026-10-01", "2026-10-07", "dee")))
	if code := errCode(t, rec); code != -8520 {
		t.Errorf("body-only aggregate request: code = %d, want -8520 (not logged in)", code)
	}
}

// TestTechnikAggregateAcceptsVerifiedOTP is the reason the gate is not simply
// "session required": the mobile app self-authenticates every request with an
// auth block and never resends the session cookie, so it is let in by proving
// possession of the user's TOTP secret.
func TestTechnikAggregateAcceptsVerifiedOTP(t *testing.T) {
	p, st := eventProxy(t)
	if err := st.SetPerm("dee", store.FeatureTechniker, true); err != nil {
		t.Fatalf("grant techniker: %v", err)
	}
	if err := st.UpsertSecret("dee", "JBSWY3DPEHPK3PXP"); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	seedEvent(t, st, "dee", testEvent())

	now := time.Now()
	params := map[string]any{
		"id": technikClassID, "type": "CLASS",
		"startDate": "2026-10-01", "endDate": "2026-10-07",
		"auth": map[string]any{
			"user":       "dee",
			"otp":        untis.TOTPAt("JBSWY3DPEHPK3PXP", now),
			"clientTime": now.UnixMilli(),
		},
	}
	rec := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017", params))
	body := rec.Body.String()
	if strings.Contains(body, "\"error\"") {
		t.Fatalf("valid OTP was refused: %s", body)
	}
	if !strings.Contains(body, "\"result\"") {
		t.Errorf("valid OTP did not produce a result: %s", body)
	}
}

// TestTechnikAggregateRejectsWrongOTP: possession is what matters, so a code that
// does not match the stored secret is refused even for the named user.
func TestTechnikAggregateRejectsWrongOTP(t *testing.T) {
	p, st := eventProxy(t)
	if err := st.SetPerm("dee", store.FeatureTechniker, true); err != nil {
		t.Fatalf("grant techniker: %v", err)
	}
	if err := st.UpsertSecret("dee", "JBSWY3DPEHPK3PXP"); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	seedEvent(t, st, "dee", testEvent())

	bad := "000000"
	if bad == untis.TOTPAt("JBSWY3DPEHPK3PXP", time.Now()) {
		bad = "111111"
	}
	params := map[string]any{
		"id": technikClassID, "type": "CLASS",
		"startDate": "2026-10-01", "endDate": "2026-10-07",
		"auth": map[string]any{"user": "dee", "otp": bad, "clientTime": time.Now().UnixMilli()},
	}
	rec := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017", params))
	if code := errCode(t, rec); code != -8520 {
		t.Errorf("wrong OTP: code = %d, want -8520", code)
	}
}

// TestTechnikAggregateIgnoresBodyAuthImpersonation: a logged-in non-techniker
// naming a techniker in the body must still be refused. The session decides.
func TestTechnikAggregateIgnoresBodyAuthImpersonation(t *testing.T) {
	p, st := eventProxy(t)
	if err := st.SetPerm("dee", store.FeatureTechniker, true); err != nil {
		t.Fatalf("grant techniker: %v", err)
	}
	seedEvent(t, st, "dee", testEvent())

	// sam has a session but no techniker permission, and claims to be dee.
	rec := internReq(t, p, "getTimetable2017", "sam",
		internBody("getTimetable2017", ttParam(technikClassID, "CLASS", "2026-10-01", "2026-10-07", "dee")))
	if code := errCode(t, rec); code != -8509 {
		t.Errorf("impersonated aggregate request: code = %d, want -8509 (no right)", code)
	}
}

func TestRedactBodyBlanksCredentials(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","method":"keyLogin","params":[{"auth":{` +
		`"user":"dee","otp":"114360","key":"JBSWY3DPEHPK3PXP"},` +
		`"password":"hunter2","secret":"S3CR3T"}]}`)
	out := redactBody(body)
	for _, secret := range []string{"114360", "JBSWY3DPEHPK3PXP", "hunter2", "JBSWY3DPEHPK3PXP"} {
		if secret != "" && strings.Contains(out, secret) {
			t.Errorf("redacted body still leaks %q: %s", secret, out)
		}
	}
	// The method and username are not secrets and stay, so the log still says
	// which request it was.
	if !strings.Contains(out, "keyLogin") || !strings.Contains(out, "dee") {
		t.Errorf("redaction dropped the method/user: %s", out)
	}
	if !strings.Contains(out, "***") {
		t.Errorf("no redaction marker in %s", out)
	}
}

// TestRedactBodyHidesNonJSON: a malformed body could still embed a secret in a
// shape the walker cannot see, so it must not be printed at all.
func TestRedactBodyHidesMalformed(t *testing.T) {
	out := redactBody([]byte(`otp=114360&password=hunter2`))
	if strings.Contains(out, "114360") || strings.Contains(out, "hunter2") {
		t.Errorf("malformed body leaked a credential: %q", out)
	}
	if !strings.Contains(out, "non-json") {
		t.Errorf("malformed body was not labelled: %q", out)
	}
}

// TestReplayKeyIsReadUnderLock exercises the map from many goroutines so
// `go test -race` catches an unsynchronised read (the C3 crash).
func TestReplayKeyIsReadUnderLock(t *testing.T) {
	p, _ := newTestProxy(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("user%d", i%2)
			for j := 0; j < 500; j++ {
				p.setReplayKey(name, "secret")
				if got := p.replayKey(name); got != "secret" {
					t.Errorf("replayKey(%q) = %q, want secret", name, got)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestHandlerRejectsOversizedBody(t *testing.T) {
	p, _ := newTestProxy(t)
	big := strings.NewReader(strings.Repeat("a", maxRequestBodyBytes+1))
	req := httptest.NewRequest(http.MethodPost, "/WebUntis/jsonrpc.do", big)
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("oversized body: status = %d, want 400", rec.Code)
	}
}

// TestPersonalEventAddsResolvableTechnikClass: a personal event references the
// synthetic Technik class, so the same response's masterData must carry it even
// for a viewer who does not hold the techniker permission, or the app drops the
// row (M1).
func TestPersonalEventAddsResolvableTechnikClass(t *testing.T) {
	p, st := eventProxy(t)
	seedEvent(t, st, "dee", testEvent())
	if p.hasTechniker("dee") {
		t.Fatal("test assumes dee is not a techniker")
	}
	raw, _ := json.Marshal(map[string]any{
		"result": map[string]any{
			"masterData": map[string]any{
				"timeStamp": 1000,
				"klassen":   []any{map[string]any{"id": 5000, "name": "10aR", "active": true}},
			},
			"timetable": map[string]any{"periods": []any{}},
		},
	})
	got := p.decorateStudentEvents(raw, "testschool", "dee", "2026-10-01", "2026-10-01")

	var out struct {
		Result struct {
			MasterData struct {
				Klassen []map[string]any `json:"klassen"`
			} `json:"masterData"`
		} `json:"result"`
	}
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatalf("bad response: %v (%s)", err, got)
	}
	found := false
	for _, k := range out.Result.MasterData.Klassen {
		if id, ok := jsonID(k["id"]); ok && id == technikClassID {
			found = true
		}
	}
	if !found {
		t.Errorf("personal event references the Technik class but masterData lacks it: %s", got)
	}
}

func TestEnsureTechnikClassDoesNotDuplicate(t *testing.T) {
	md := map[string]any{"klassen": []any{map[string]any{"id": 5000, "name": "10aR"}}}
	if !ensureTechnikClass(md) {
		t.Fatal("first call must add the class")
	}
	if ensureTechnikClass(md) {
		t.Error("second call must report no change")
	}
	klassen, _ := md["klassen"].([]any)
	n := 0
	for _, k := range klassen {
		if km, ok := k.(map[string]any); ok {
			if id, _ := jsonID(km["id"]); id == technikClassID {
				n++
			}
		}
	}
	if n != 1 {
		t.Errorf("Technik class appears %d times, want 1", n)
	}
}

func TestTTCacheEnforcesCap(t *testing.T) {
	c := newTTCache(time.Hour)
	for i := 0; i < maxTTCacheEntries+250; i++ {
		c.Put(fmt.Sprintf("k%d", i), []byte("x"))
	}
	c.mu.Lock()
	n := len(c.m)
	c.mu.Unlock()
	if n > maxTTCacheEntries {
		t.Errorf("cache holds %d entries, want at most %d", n, maxTTCacheEntries)
	}
}
