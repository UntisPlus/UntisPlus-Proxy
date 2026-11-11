package proxy

// Login rate limiting.
//
// A login proxy is an oracle for whether a username/OTP pair is valid, so an
// unthrottled endpoint lets an attacker try codes as fast as the upstream will
// answer. The limiter is deliberately in-process: the proxy is deployed as a
// single process (the outbox worker already assumes that), and a per-process
// counter is enough to blunt online guessing without adding a database write to
// every successful login.
//
// Failures are counted per account and per client address. A successful login
// clears the account counter, but not the address counter: one valid account
// must not be able to whitewash a spray from the same host.

import (
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultLoginMaxAttempts is the number of rejected logins allowed per key
// inside one window before the key is locked out. Operators can change it with
// -login-max-attempts; 0 disables limiting.
const defaultLoginMaxAttempts = 5

// defaultLoginWindow is both the counting window and the lockout duration.
const defaultLoginWindow = 15 * time.Minute

// attemptLimiter is a fixed-window failure counter. It stores only keys that
// have failed at least once, so the map is bounded by active sources rather than
// by every username that has ever logged in.
type attemptLimiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	entries map[string]attemptEntry
}

type attemptEntry struct {
	failures int
	first    time.Time
	until    time.Time
}

func newAttemptLimiter(max int, window time.Duration) *attemptLimiter {
	if window <= 0 {
		window = defaultLoginWindow
	}
	return &attemptLimiter{
		max:     max,
		window:  window,
		entries: map[string]attemptEntry{},
	}
}

// enabled reports whether this limiter is doing anything.
func (l *attemptLimiter) enabled() bool {
	return l != nil && l.max > 0
}

// locked reports whether the key is currently locked out and, if so, how long
// remains. An expired lock and an expired counting window are both dropped.
func (l *attemptLimiter) locked(key string, now time.Time) (bool, time.Duration) {
	if !l.enabled() {
		return false, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok {
		return false, 0
	}
	if e.until.IsZero() {
		// Still inside the counting window: failures have not yet crossed the
		// threshold, so keep counting rather than discarding the entry.
		if now.Sub(e.first) > l.window {
			delete(l.entries, key)
		}
		return false, 0
	}
	if now.Before(e.until) {
		return true, e.until.Sub(now)
	}
	// The lock or window has elapsed; start clean on the next failure.
	delete(l.entries, key)
	return false, 0
}

// fail records one rejected login attempt and returns whether that attempt
// crossed the threshold. The transition is reported once so the caller can log
// a lockout without logging every rejected code.
func (l *attemptLimiter) fail(key string, now time.Time) (locked bool, retry time.Duration) {
	if !l.enabled() {
		return false, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if ok && now.Before(e.until) {
		// Already locked: additional attempts neither extend the lock nor count.
		return true, e.until.Sub(now)
	}
	if !ok || now.Sub(e.first) > l.window {
		e = attemptEntry{first: now}
	}
	e.failures++
	if e.failures >= l.max {
		e.until = now.Add(l.window)
		l.entries[key] = e
		return true, l.window
	}
	l.entries[key] = e
	return false, 0
}

// reset clears a key after a successful login.
func (l *attemptLimiter) reset(key string) {
	if !l.enabled() {
		return
	}
	l.mu.Lock()
	delete(l.entries, key)
	l.mu.Unlock()
}

// loginLimitKey namespaces an account by school so the same username at two
// schools is not one counter.
func loginLimitKey(school, username string) string {
	return strings.ToLower(strings.TrimSpace(school)) + "\x00" + strings.ToLower(strings.TrimSpace(username))
}

// loginLimitIPKey namespaces a client address. The account limiter has its own
// map, so a prefix here is only for logging clarity.
func loginLimitIPKey(ip string) string {
	return "ip\x00" + ip
}

// clientIP extracts the address used for the per-source limit. It deliberately
// ignores X-Forwarded-For: trusting a header the client can write would let an
// attacker bypass the limit by inventing a new address each attempt.
func clientIP(remoteAddr string) string {
	if remoteAddr == "" {
		return "unknown"
	}
	if i := strings.LastIndexByte(remoteAddr, ':'); i > 0 {
		return remoteAddr[:i]
	}
	return remoteAddr
}

// loginRateLimited reports whether either the account or the source address is
// locked out, and sets Retry-After on the response when it is.
func (p *Proxy) loginRateLimited(w http.ResponseWriter, r *http.Request, school, username string) bool {
	now := time.Now()
	for _, pair := range []struct {
		l   *attemptLimiter
		key string
	}{
		{p.loginAccountLimit, loginLimitKey(school, username)},
		{p.loginIPLimit, loginLimitIPKey(clientIP(r.RemoteAddr))},
	} {
		if locked, retry := pair.l.locked(pair.key, now); locked {
			secs := int(retry.Seconds())
			if secs < 1 {
				secs = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			return true
		}
	}
	return false
}

// recordLoginFailure counts a rejected login against both keys.
func (p *Proxy) recordLoginFailure(r *http.Request, school, username string) {
	now := time.Now()
	accountLocked, _ := p.loginAccountLimit.fail(loginLimitKey(school, username), now)
	ipLocked, _ := p.loginIPLimit.fail(loginLimitIPKey(clientIP(r.RemoteAddr)), now)
	if accountLocked || ipLocked {
		// Log the fact of a lockout, never the credential.
		logLoginLockout(school, username, accountLocked, ipLocked)
	}
}

// recordLoginSuccess clears the account counter for a valid login.
func (p *Proxy) recordLoginSuccess(school, username string) {
	p.loginAccountLimit.reset(loginLimitKey(school, username))
}

func logLoginLockout(school, username string, accountLocked, ipLocked bool) {
	scope := "account"
	if ipLocked && !accountLocked {
		scope = "client address"
	} else if ipLocked {
		scope = "account and client address"
	}
	log.Printf("[auth] locked out %s for %s/%s after repeated failed logins", scope, school, username)
}
