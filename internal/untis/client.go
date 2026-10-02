package untis

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	ErrNoRight       = errors.New("no right for timetable")
	ErrNotFound      = errors.New("method not found")
	ErrNotLoggedIn   = errors.New("not logged in")
	ErrBadCreds      = errors.New("bad credentials")
	ErrUnknownMethod = errors.New("unknown method")
)

// UpstreamError carries the real error code/message so the proxy can mirror it.
type UpstreamError struct {
	Code    int
	Message string
	Raw     []byte
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("upstream error %d: %s", e.Code, e.Message)
}

type Config struct {
	Server     string // e.g. testschool.webuntis.com
	School     string
	HTTPClient *http.Client
}

type cachedSession struct {
	cookie  string
	expires time.Time
}

type Client struct {
	cfg      Config
	hc       *http.Client
	mu       sync.Mutex
	sessions map[string]cachedSession // username -> real session cookie

	serverMu sync.Mutex
	servers  map[string]string // school -> resolved upstream host
}

func New(cfg Config) *Client {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 25 * time.Second}
	}
	servers := map[string]string{}
	if cfg.School != "" && cfg.Server != "" {
		servers[cfg.School] = cfg.Server
	}
	return &Client{cfg: cfg, hc: cfg.HTTPClient, sessions: make(map[string]cachedSession), servers: servers}
}

// totpNow supplies the current time for TOTP. It is a variable so a test can hold
// the clock still: the counter is a 30-second window, so a caller that compares a
// code produced during a request against one produced afterwards would otherwise
// disagree whenever the window rolls between the two.
var totpNow = time.Now

// TOTP computes a RFC6238 time-based one-time password from a base32 secret.
func TOTP(secret string) string {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(secret))
	if err != nil {
		key, err = base32.StdEncoding.DecodeString(strings.ToUpper(secret))
		if err != nil {
			return "000000"
		}
	}
	counter := uint64(totpNow().Unix() / 30)
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", code%1000000)
}

// SetTOTPClock overrides the clock that TOTP reads and returns a function that
// restores the previous one. It exists for tests outside this package that compare
// a code produced during a request with one produced afterwards: with the counter
// window held still, the two agree by construction instead of by luck, and a
// rollover mid-test shows up as a real failure rather than a flake.
func SetTOTPClock(now func() time.Time) (restore func()) {
	prev := totpNow
	if now != nil {
		totpNow = now
	}
	return func() { totpNow = prev }
}

func (c *Client) base(school string) string {
	if school == "" {
		school = c.cfg.School
	}
	return "https://" + c.serverFor(school)
}

// serverFor returns the canonical upstream host for a school. The WebUntis
// mobile apps identify a school by name and contact <school>.webuntis.com, so
// the server host is resolved from the school name via the official school
// query API on first use and cached thereafter. When resolution is unavailable
// (network error, unknown school, or a default has been configured directly)
// it falls back to the configured default server.
func (c *Client) serverFor(school string) string {
	if school == "" {
		school = c.cfg.School
	}
	c.serverMu.Lock()
	defer c.serverMu.Unlock()
	if h, ok := c.servers[school]; ok {
		return h
	}
	h := c.cfg.Server
	if resolved := c.resolveServerHost(school); resolved != "" {
		h = resolved
	}
	c.servers[school] = h
	return h
}

// resolveServerHost looks up the canonical server hostname for a school using
// the official WebUntis school search endpoint. Returns "" when the school is
// not found or the lookup fails.
func (c *Client) resolveServerHost(school string) string {
	body, _ := json.Marshal(map[string]any{
		"id": "untis-mobile-android", "jsonrpc": "2.0", "method": "searchSchool",
		"params": []any{map[string]any{"schoolid": 0, "search": school}},
	})
	req, err := http.NewRequest("POST", "https://schoolsearch.webuntis.com/schoolquery2?v=i2.2", bytes.NewReader(body))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) untis-proxy/1.0")
	resp, err := c.hc.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}
	var res struct {
		Result struct {
			Schools []struct {
				LoginName string `json:"loginName"`
				Server    string `json:"server"`
			} `json:"schools"`
		} `json:"result"`
	}
	if json.Unmarshal(b, &res) != nil {
		return ""
	}
	for _, s := range res.Result.Schools {
		if strings.EqualFold(s.LoginName, school) && s.Server != "" {
			return s.Server
		}
	}
	// fall back to the sole match if there is exactly one (avoids guessing on
	// ambiguous search strings that could return several schools)
	if len(res.Result.Schools) == 1 && res.Result.Schools[0].Server != "" {
		return res.Result.Schools[0].Server
	}
	return ""
}

// SchoolCookie returns the upstream schoolname cookie value for a school.
// Upstream uses it to resolve the tenant for self-auth requests; raw forwards
// that carry no session should still send it (otherwise info-center methods
// fail with "-8500 invalid schoolname").
func (c *Client) SchoolCookie(school string) string {
	return "schoolname=" + c.schoolCookie(school)
}

func (c *Client) schoolCookie(school string) string {
	if school == "" {
		school = c.cfg.School
	}
	return "_" + base64.StdEncoding.EncodeToString([]byte(school))
}

func cookieHeader(cookie string) http.Header {
	h := http.Header{}
	if cookie != "" {
		h.Set("Cookie", cookie)
	}
	return h
}

func (c *Client) post(url string, hdr http.Header, body []byte) ([]byte, int, http.Header, error) {
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) untis-proxy/1.0")
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return b, resp.StatusCode, resp.Header, err
}

func (c *Client) get(url string, hdr http.Header) ([]byte, int, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) untis-proxy/1.0")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return b, resp.StatusCode, err
}

// JSONRPC forwards a raw JSON-RPC request to the real server and returns the raw body.
func (c *Client) JSONRPC(school, cookie string, body []byte) ([]byte, error) {
	b, code, _, err := c.post(c.base(school)+"/WebUntis/jsonrpc.do?school="+school, cookieHeader(cookie), body)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("upstream status %d", code)
	}
	return b, nil
}

// PasswordLogin authenticates against the real server, returning the real
// session cookie and the parsed authenticate result.
func (c *Client) PasswordLogin(school, username, password, client string) (string, map[string]any, error) {
	body, _ := json.Marshal(map[string]any{
		"id": "upstream", "jsonrpc": "2.0", "method": "authenticate",
		"params": map[string]any{"user": username, "password": password, "client": client},
	})
	b, _, _, err := c.post(c.base(school)+"/WebUntis/jsonrpc.do?school="+school, nil, body)
	if err != nil {
		return "", nil, err
	}
	var res struct {
		Result map[string]any `json:"result"`
		Error  *UpstreamError `json:"error"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return "", nil, fmt.Errorf("bad upstream response: %w", err)
	}
	if res.Error != nil {
		res.Error.Raw = b
		return "", nil, res.Error
	}
	sid, _ := res.Result["sessionId"].(string)
	if sid == "" {
		return "", nil, &UpstreamError{Message: "bad credentials", Code: -8504, Raw: b}
	}
	return c.buildCookie(sid, school), res.Result, nil
}

// KeyLogin performs OTP-based login against the real server. extraAuth may carry
// non-standard fields (e.g. "key": secret) to allow replaying later.
func (c *Client) KeyLogin(school, username, otp string, clientTime int64, extraAuth map[string]any) (string, []byte, error) {
	auth := map[string]any{"clientTime": clientTime, "user": username, "otp": otp}
	for k, v := range extraAuth {
		auth[k] = v
	}
	body, _ := json.Marshal(map[string]any{
		"id": "upstream", "jsonrpc": "2.0", "method": "getUserData2017",
		"params": []any{map[string]any{"auth": auth}},
	})
	url := c.base(school) + "/WebUntis/jsonrpc_intern.do?m=getUserData2017&school=" + school + "&v=i2.2"
	b, _, hdr, err := c.post(url, nil, body)
	if err != nil {
		return "", nil, err
	}
	cookie := extractJSESSIONID(hdr)
	var e struct {
		Error *UpstreamError `json:"error"`
	}
	_ = json.Unmarshal(b, &e)
	if e.Error != nil {
		e.Error.Raw = b
		return "", nil, e.Error
	}
	if cookie == "" {
		var e struct {
			Error *UpstreamError `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		if e.Error != nil {
			e.Error.Raw = b
			return "", nil, e.Error
		}
		return "", nil, &UpstreamError{Message: "bad credentials", Code: -8504, Raw: b}
	}
	return cookie + "; schoolname=" + c.schoolCookie(school), b, nil
}

// Logout terminates a real session and evicts it from the session cache so a
// subsequent Session() does not reuse an invalidated cookie.
func (c *Client) Logout(school, cookie string) {
	body := []byte(`{"id":"upstream","jsonrpc":"2.0","method":"logout","params":{}}`)
	_, _, _, _ = c.post(c.base(school)+"/WebUntis/jsonrpc.do?school="+school, cookieHeader(cookie), body)
	c.mu.Lock()
	for k, cs := range c.sessions {
		if cs.cookie == cookie {
			delete(c.sessions, k)
		}
	}
	c.mu.Unlock()
}

// Session returns a valid real session cookie for a user, caching it briefly.
func (c *Client) Session(school, username, secret, method string) (string, error) {
	ck := username + "|" + method
	c.mu.Lock()
	if cs, ok := c.sessions[ck]; ok && time.Now().Before(cs.expires) {
		c.mu.Unlock()
		return cs.cookie, nil
	}
	c.mu.Unlock()

	var cookie string
	var err error
	switch method {
	case "key":
		if secret == "" {
			return "", fmt.Errorf("user %s has no replayable key", username)
		}
		cookie, _, err = c.KeyLogin(school, username, TOTP(secret), time.Now().UnixMilli(), nil)
	default:
		cookie, _, err = c.PasswordLogin(school, username, secret, "untis-proxy")
	}
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.sessions[ck] = cachedSession{cookie: cookie, expires: time.Now().Add(8 * time.Minute)}
	c.mu.Unlock()
	return cookie, nil
}

// FreshSession forces a brand-new login for a user regardless of the session
// cache. Use it to recover from an upstream session that expired server-side
// while still cached.
func (c *Client) FreshSession(school, username, secret, method string) (string, error) {
	c.mu.Lock()
	delete(c.sessions, username+"|"+method)
	c.mu.Unlock()
	return c.Session(school, username, secret, method)
}

// IsAuthFailure reports whether a raw response body signals an upstream
// authentication problem (expired/invalid session), so callers can retry with
// a fresh session instead of surfacing a stale-cookie error. Accepts both the
// JSON-RPC error shape and the flat REST error body REST endpoints use when a
// cached session has been disowned server-side.
func IsAuthFailure(b []byte) bool {
	var resp struct {
		Error *UpstreamError `json:"error"`
	}
	if json.Unmarshal(b, &resp) != nil {
		return false
	}
	if resp.Error != nil {
		switch resp.Error.Code {
		case -8504, -8520, -8998:
			return true
		}
		m := strings.ToLower(resp.Error.Message)
		if strings.Contains(m, "not authenticated") ||
			strings.Contains(m, "not logged in") ||
			strings.Contains(m, "bad credentials") {
			return true
		}
	}
	// REST endpoints report session expiry as a flat body, e.g.
	// {"errorCode":"FORBIDDEN","errorMessage":"no right for anonymous user"}.
	return isFlatAuthFailure(b)
}

// isFlatAuthFailure reports whether a body carries the REST-style
// authentication/authorization failure shape used when a session that is no
// longer valid server-side reaches a REST endpoint.
func isFlatAuthFailure(b []byte) bool {
	var flat struct {
		ErrorCode    string `json:"errorCode"`
		ErrorMessage string `json:"errorMessage"`
	}
	if json.Unmarshal(b, &flat) != nil {
		return false
	}
	m := strings.ToLower(flat.ErrorCode + " " + flat.ErrorMessage)
	return strings.Contains(m, "anonymous") ||
		strings.Contains(m, "not authorized") ||
		strings.Contains(m, "not authenticated")
}

type PersonInfo struct {
	PersonID    int64
	PersonType  int64
	ClassID     int64
	ClassName   string
	Email       string
	DisplayName string
}

// PersonInfo fetches identity/class data for a real session.
func (c *Client) PersonInfo(school, cookie string) (*PersonInfo, error) {
	info := &PersonInfo{}
	// /api/app/config
	b, status, err := c.get(c.base(school)+"/WebUntis/api/app/config", cookieHeader(cookie))
	if err == nil && status == http.StatusOK {
		var cfg struct {
			Data struct {
				LoginServiceConfig struct {
					User struct {
						PersonID int64 `json:"personId"`
						Persons  []struct {
							ID          int64  `json:"id"`
							Type        int64  `json:"type"`
							DisplayName string `json:"displayName"`
						} `json:"persons"`
						Email string `json:"email"`
					} `json:"user"`
				} `json:"loginServiceConfig"`
			} `json:"data"`
		}
		if json.Unmarshal(b, &cfg) == nil {
			info.PersonID = cfg.Data.LoginServiceConfig.User.PersonID
			info.Email = cfg.Data.LoginServiceConfig.User.Email
			if len(cfg.Data.LoginServiceConfig.User.Persons) > 0 {
				p := cfg.Data.LoginServiceConfig.User.Persons[0]
				info.PersonType = p.Type
				if info.PersonID == 0 {
					info.PersonID = p.ID
				}
				info.DisplayName = p.DisplayName
			}
		}
	}
	// /api/daytimetable/config
	b, status, err = c.get(c.base(school)+"/WebUntis/api/daytimetable/config", cookieHeader(cookie))
	if err == nil && status == http.StatusOK {
		var cfg struct {
			Data struct {
				KlasseID int64 `json:"klasseId"`
			} `json:"data"`
		}
		if json.Unmarshal(b, &cfg) == nil {
			info.ClassID = cfg.Data.KlasseID
		}
	}
	return info, nil
}

// GetKlassen returns the raw getKlassen result list.
func (c *Client) GetKlassen(school, cookie string) ([]byte, error) {
	body := []byte(`{"id":"upstream","jsonrpc":"2.0","method":"getKlassen","params":{}}`)
	return c.JSONRPC(school, cookie, body)
}

// RESTGet forwards a GET request to the real REST API.
func (c *Client) RESTGet(school, cookie, path, rawQuery string) ([]byte, int, error) {
	u := c.base(school) + path
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return c.get(u, cookieHeader(cookie))
}

// RESTGetToken fetches a REST endpoint using a Bearer token instead of a
// session cookie.
func (c *Client) RESTGetToken(school, token, path, rawQuery string) ([]byte, int, error) {
	u := c.base(school) + path
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	hdr := make(http.Header)
	hdr.Set("Authorization", token)
	return c.get(u, hdr)
}

// RawIntern forwards a raw body to jsonrpc_intern.do.
func (c *Client) RawIntern(school, cookie, method string, body []byte) ([]byte, int, http.Header, error) {
	u := c.base(school) + "/WebUntis/jsonrpc_intern.do?m=" + method + "&school=" + school + "&v=i2.2"
	return c.post(u, cookieHeader(cookie), body)
}

func (c *Client) buildCookie(sid, school string) string {
	return "JSESSIONID=" + sid + "; schoolname=" + c.schoolCookie(school)
}

func extractJSESSIONID(hdr http.Header) string {
	for _, ck := range hdr.Values("Set-Cookie") {
		if m := regexp.MustCompile(`JSESSIONID=([^;]+)`).FindStringSubmatch(ck); m != nil {
			return "JSESSIONID=" + m[1]
		}
	}
	return ""
}
