package proxy

// Shared fake upstream WebUntis server for handler-level tests that need the
// proxy to reach the real API: keyLogin flows, raw/stale-session forwarding,
// REST calls. Every request is recorded so tests can assert what the proxy
// actually forwarded (cookies, rewritten auth bodies, method names).

import (
	"encoding/json"
	"io"
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

// forwardedReq is one request the fake server received from the proxy.
type forwardedReq struct {
	Method  string
	Cookie  string
	AuthHdr string
	Body    []byte
	Path    string
}

// fakeUpstream stands in for the real WebUntis server.
type fakeUpstream struct {
	mu sync.Mutex
	// forwards receives every jsonrpc_intern.do request except getUserData2017.
	forwards []forwardedReq
	// rest receives every /WebUntis/api/* request.
	rest []forwardedReq
	// public receives every /WebUntis/jsonrpc.do request.
	public []forwardedReq
	// timetableCalls counts getTimetable2017 forwards, so tests can assert how
	// much upstream traffic a scan actually caused.
	timetableCalls int

	// restBody / restStatus are the canned answer served for /WebUntis/api/*.
	restBody   string
	restStatus int
	// timetable, when set, is the full upstream body served for any
	// getTimetable2017 forward (e.g. canned periods for ICS tests).
	timetable string
	// timetableStatus, when non-zero, makes every getTimetable2017 forward fail
	// with that HTTP status, so tests can exercise upstream outages.
	timetableStatus int
	// authenticateBody, when set, overrides the canned jsonrpc.do authenticate
	// answer (used to drive password-login failure paths). By default the fake
	// accepts every password login and returns real-ish auth data.
	authenticateBody string
	// userDataBody, when set, is served for the intern getUserData2017 (key
	// login) request, e.g. a login answer carrying a full masterData block.
	userDataBody string
}

func (f *fakeUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasPrefix(r.URL.Path, "/WebUntis/jsonrpc_intern.do"):
			var req struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(body, &req)
			w.Header().Set("Content-Type", "application/json")
			if req.Method == "getUserData2017" {
				w.Header().Set("Set-Cookie", "JSESSIONID=fakesid; Path=/")
				f.mu.Lock()
				udb := f.userDataBody
				f.mu.Unlock()
				if udb != "" {
					_, _ = w.Write([]byte(udb))
					return
				}
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"upstream","result":{}}`))
				return
			}
			f.mu.Lock()
			f.forwards = append(f.forwards, forwardedReq{Method: req.Method, Cookie: r.Header.Get("Cookie"), Body: body, Path: r.URL.String()})
			if req.Method == "getTimetable2017" {
				f.timetableCalls++
			}
			tm, tstat := f.timetable, f.timetableStatus
			f.mu.Unlock()
			if req.Method == "getTimetable2017" && tstat != 0 {
				w.WriteHeader(tstat)
				_, _ = w.Write([]byte(`{"error":{"message":"upstream unavailable"}}`))
				return
			}
			if req.Method == "getTimetable2017" && tm != "" {
				_, _ = w.Write([]byte(tm))
				return
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"upstream","result":{}}`))
		case strings.HasPrefix(r.URL.Path, "/WebUntis/api/"):
			f.mu.Lock()
			f.rest = append(f.rest, forwardedReq{Method: r.Method, Cookie: r.Header.Get("Cookie"), AuthHdr: r.Header.Get("Authorization"), Body: body, Path: r.URL.String()})
			b, s := f.restBody, f.restStatus
			f.mu.Unlock()
			if b == "" {
				b = `{}`
			}
			if s == 0 {
				s = http.StatusOK
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(s)
			_, _ = w.Write([]byte(b))
		case strings.HasPrefix(r.URL.Path, "/WebUntis/jsonrpc.do"):
			f.mu.Lock()
			f.public = append(f.public, forwardedReq{Cookie: r.Header.Get("Cookie"), Body: body, Path: r.URL.String()})
			var req struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(body, &req)
			authBody := f.authenticateBody
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if req.Method == "authenticate" && authBody != "" {
				_, _ = w.Write([]byte(authBody))
				return
			}
			if req.Method == "authenticate" {
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"upstream","result":{"sessionId":"authsid","personType":5,"personId":5005,"klasseId":5000}}`))
				return
			}
			if req.Method == "logout" {
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"upstream","result":{}}`))
				return
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"upstream","error":{"code":-32601,"message":"method not found"}}`))
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	})
}

// TimetableCalls returns how many getTimetable2017 requests the fake served.
func (f *fakeUpstream) TimetableCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.timetableCalls
}

func (f *fakeUpstream) Forwards() []forwardedReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]forwardedReq(nil), f.forwards...)
}

func (f *fakeUpstream) Rests() []forwardedReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]forwardedReq(nil), f.rest...)
}

func (f *fakeUpstream) Public() []forwardedReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]forwardedReq(nil), f.public...)
}

// setTimetable configures canned getTimetable2017 periods (list of maps).
func (f *fakeUpstream) setTimetable(t *testing.T, periods []map[string]any) {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": "upstream",
		"result": map[string]any{"timetable": map[string]any{"periods": periods}},
	})
	if err != nil {
		t.Fatalf("marshal timetable: %v", err)
	}
	f.mu.Lock()
	f.timetable = string(b)
	f.mu.Unlock()
}

// setTimetableStatus makes every getTimetable2017 forward fail with code.
func (f *fakeUpstream) setTimetableStatus(code int) {
	f.mu.Lock()
	f.timetableStatus = code
	f.mu.Unlock()
}

// restoreTimetable clears a forced failure status, so the canned body is served
// again (status 0 is the fake's "no forced failure" value).
func (f *fakeUpstream) restoreTimetable() { f.setTimetableStatus(0) }

// newFakeProxyUpstream wires a Proxy to a fakeUpstream server.
func newFakeProxyUpstream(t *testing.T, f *fakeUpstream) (*Proxy, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	_ = st.SetDefaultSchool("testschool")
	return newFakeProxyUpstreamWithStore(t, f, st), st
}

// newFakeProxyUpstreamWithStore wires a second Proxy (as a restart would) to the
// same fake upstream and the same database file, with fresh in-memory state.
func newFakeProxyUpstreamWithStore(t *testing.T, f *fakeUpstream, st *store.Store) *Proxy {
	t.Helper()
	srv := httptest.NewTLSServer(f.handler())
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "https://")
	cl := untis.New(untis.Config{Server: host, School: "testschool", HTTPClient: srv.Client()})
	return New(st, cl, session.NewManager(5*time.Minute), Options{School: "testschool"})
}
