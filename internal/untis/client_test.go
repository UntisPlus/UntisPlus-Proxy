package untis

// Direct unit tests for the auth-failure recognition and session-cookie
// building. The proxy-level suites exercise these only through happy-path
// fakes, so a regression here (e.g. the flat REST body that motivated the
// fresh-session retry, or a dropped schoolname cookie) must be pinned down
// at the client layer itself.

import (
	"strings"
	"testing"
)

// TestIsAuthFailureJSONRPC: the JSON-RPC error shapes that must trigger a
// fresh-session retry (-8520 not logged in, -8504 bad credentials, -8998
// invalid secret) are flagged; benign and unrelated errors are not.
func TestIsAuthFailureJSONRPC(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"not logged in", `{"jsonrpc":"2.0","error":{"code":-8520,"message":"not logged in"}}`, true},
		{"bad credentials", `{"jsonrpc":"2.0","error":{"code":-8504,"message":"bad credentials"}}`, true},
		{"invalid secret", `{"jsonrpc":"2.0","error":{"code":-8998,"message":"invalid secret"}}`, true},
		{"not authenticated text", `{"jsonrpc":"2.0","error":{"code":-1,"message":"not authenticated"}}`, true},
		{"ok result", `{"jsonrpc":"2.0","id":"1","result":{}}`, false},
		{"other upstream error", `{"jsonrpc":"2.0","error":{"code":-1,"message":"internal server error"}}`, false},
		{"garbage", `not json at all`, false},
	}
	for _, tc := range cases {
		if got := IsAuthFailure([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: IsAuthFailure(%q) = %v, want %v", tc.name, tc.body, got, tc.want)
		}
	}
}

// TestIsAuthFailureFlatREST: an upstream session the server has disowned
// surfaces as a flat REST error body, not JSON-RPC. The proxy's escalatedREST
// retry depends on recognising exactly this shape (the anonymous-user 403).
func TestIsAuthFailureFlatREST(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"anonymous user", `{"errorCode":"FORBIDDEN","errorMessage":"no right for anonymous user"}`, true},
		{"not authorized", `{"errorCode":"FORBIDDEN","errorMessage":"not authorized"}`, true},
		{"not authenticated", `{"errorCode":"UNAUTHORIZED","errorMessage":"not authenticated"}`, true},
		{"weekly data ok", `{"data":{"result":{"data":{"elementPeriods":{"5000":[]}}}}}`, false},
		{"other flat error", `{"errorCode":"NOT_FOUND","errorMessage":"no timetable for class"}}`, false},
	}
	for _, tc := range cases {
		if got := IsAuthFailure([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: IsAuthFailure(%q) = %v, want %v", tc.name, tc.body, got, tc.want)
		}
	}
}

// TestSchoolCookieEncoding: the schoolname cookie must be the base64-encoded
// school name prefixed with "_" so the real server recognises the tenant.
func TestSchoolCookieEncoding(t *testing.T) {
	c := &Client{cfg: Config{School: "testschool"}}
	if got := c.schoolCookie("testschool"); got != "_dGVzdHNjaG9vbA==" {
		t.Errorf("schoolCookie(testschool) = %q, want _dGVzdHNjaG9vbA==", got)
	}
	if got := c.schoolCookie(""); got != "_dGVzdHNjaG9vbA==" {
		t.Errorf("schoolCookie('') should fall back to cfg.School, got %q", got)
	}
}

// TestBuildCookieIncludesSchoolname: every cookie handed to the upstream must
// carry both the JSESSIONID and the tenant so RawIntern/REST forwards are not
// mistaken for a different school (Bug A regression: dropping schoolname made
// info-center methods answer -8500 invalid schoolname).
func TestBuildCookieIncludesSchoolname(t *testing.T) {
	c := &Client{}
	cookie := c.buildCookie("abc-deadbeef", "testschool")
	if !strings.HasPrefix(cookie, "JSESSIONID=abc-deadbeef; ") {
		t.Errorf("cookie = %q, want JSESSIONID prefix", cookie)
	}
	if !strings.Contains(cookie, "schoolname=_dGVzdHNjaG9vbA==") {
		t.Errorf("cookie = %q, want schoolname=_dGVzdHNjaG9vbA==", cookie)
	}
}
