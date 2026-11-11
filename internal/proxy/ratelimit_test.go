package proxy

// Login rate limiting tests. The limiter is the only thing standing between an
// online attacker and an unlimited credential-guessing oracle, so the guarantees
// worth asserting are: the threshold locks the account, a lock sets Retry-After,
// a success clears the account but not the source, and the feature can be turned
// off explicitly.

import (
	"net/http/httptest"
	"testing"
	"time"
)

// TestLoginLimiterLocksAccountAfterThreshold walks the limiter through its
// counting window without going through the proxy, so the boundary is explicit.
func TestLoginLimiterLocksAccountAfterThreshold(t *testing.T) {
	l := newAttemptLimiter(3, time.Minute)
	now := time.Now()
	key := loginLimitKey("testschool", "alice")
	for i := 1; i <= 2; i++ {
		if locked, _ := l.fail(key, now); locked {
			t.Fatalf("locked after %d failures, want the threshold at 3", i)
		}
		if locked, _ := l.locked(key, now); locked {
			t.Fatalf("locked() reports a lock after %d failures", i)
		}
	}
	locked, retry := l.fail(key, now)
	if !locked || retry <= 0 {
		t.Fatalf("third failure did not lock: locked=%v retry=%v", locked, retry)
	}
	if locked, _ := l.locked(key, now); !locked {
		t.Error("locked() does not report the lock")
	}
	// The lock expires with the window.
	if locked, _ := l.locked(key, now.Add(time.Minute+time.Millisecond)); locked {
		t.Error("limiter is still locked after the window elapsed")
	}
}

// TestLoginRateLimitedSetsRetryAfter: a locked login gets a JSON-RPC error at
// the caller, and the limiter must also advertise how long to wait.
func TestLoginRateLimitedSetsRetryAfter(t *testing.T) {
	p, _ := newTestProxy(t)
	p.loginAccountLimit = newAttemptLimiter(2, time.Minute)
	p.loginIPLimit = newAttemptLimiter(10, time.Minute)

	req := httptest.NewRequest("POST", "/", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	p.recordLoginFailure(req, "testschool", "alice")
	p.recordLoginFailure(req, "testschool", "alice")

	rec := httptest.NewRecorder()
	if !p.loginRateLimited(rec, req, "testschool", "alice") {
		t.Fatal("account is not rate limited after reaching the threshold")
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" || ra == "0" {
		t.Errorf("Retry-After = %q, want a positive number of seconds", ra)
	}
}

// TestLoginSuccessClearsAccountNotSource: one valid login must not whitewash a
// password spray from the same address.
func TestLoginSuccessClearsAccountNotSource(t *testing.T) {
	p, _ := newTestProxy(t)
	p.loginAccountLimit = newAttemptLimiter(2, time.Minute)
	p.loginIPLimit = newAttemptLimiter(2, time.Minute)

	req := httptest.NewRequest("POST", "/", nil)
	req.RemoteAddr = "203.0.113.10:4444"
	p.recordLoginFailure(req, "testschool", "alice")
	p.recordLoginSuccess("testschool", "alice")

	if p.loginRateLimited(httptest.NewRecorder(), req, "testschool", "alice") {
		t.Error("a successful login should reset the account counter")
	}
	// The address counter still has one failure; a second locks it.
	p.recordLoginFailure(req, "testschool", "bob")
	if !p.loginRateLimited(httptest.NewRecorder(), req, "testschool", "bob") {
		t.Error("the source counter should stay hot after an unrelated success")
	}
}

// TestLoginRateLimitCanBeDisabled: -login-max-attempts 0 is an explicit opt-out,
// not a request for the default.
func TestLoginRateLimitCanBeDisabled(t *testing.T) {
	p, _ := newTestProxy(t)
	p.loginAccountLimit = newAttemptLimiter(0, time.Minute)
	p.loginIPLimit = newAttemptLimiter(0, time.Minute)

	req := httptest.NewRequest("POST", "/", nil)
	req.RemoteAddr = "203.0.113.11:3333"
	for i := 0; i < 20; i++ {
		p.recordLoginFailure(req, "testschool", "alice")
	}
	if p.loginRateLimited(httptest.NewRecorder(), req, "testschool", "alice") {
		t.Error("limiter is active even though it was disabled")
	}
}

// TestClientIPIgnoresForwardedFor: trusting a client-written header would let an
// attacker reset the per-source counter on every attempt.
func TestClientIPIgnoresForwardedFor(t *testing.T) {
	req := httptest.NewRequest("POST", "/", nil)
	req.RemoteAddr = "198.51.100.7:1234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := clientIP(req.RemoteAddr); got != "198.51.100.7" {
		t.Errorf("clientIP = %q, want the socket address", got)
	}
}
