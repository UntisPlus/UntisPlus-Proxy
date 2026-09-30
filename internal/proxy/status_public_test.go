package proxy

// Public status, /me and full-mux route smoke tests. These cover the routing
// table (Handler) end-to-end: unknown paths 404, public endpoints answer
// without a session, and admin endpoints stay behind the session gate.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicStatus(t *testing.T) {
	p, _ := newTestProxy(t)
	rec := httptest.NewRecorder()
	p.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if out["status"] != "ok" {
		t.Errorf("status field = %v, want ok", out["status"])
	}
	mode, _ := out["mode"].(string)
	version, _ := out["version"].(string)
	if mode != "dev" {
		t.Errorf("mode = %q, want dev default", mode)
	}
	if version != "dev" {
		t.Errorf("version = %q, want dev default", version)
	}
	if _, ok := out["uptime_sec"].(float64); !ok {
		t.Errorf("uptime_sec missing: %v", out)
	}
}

func TestPublicStatusEnvOverrides(t *testing.T) {
	p, _ := newTestProxy(t)
	t.Setenv("UNTIS_ENV", "prod")
	t.Setenv("UNTIS_VERSION", "v9.9")
	rec := httptest.NewRecorder()
	p.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if out["mode"] != "prod" || out["version"] != "v9.9" {
		t.Errorf("unexpected status with env overrides: %v", out)
	}
}

func TestHandleMeUnauthorized(t *testing.T) {
	p, _ := newTestProxy(t)
	rec := httptest.NewRecorder()
	p.handleMe(rec, httptest.NewRequest(http.MethodGet, "/me", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if out["error"] != "not logged in" {
		t.Errorf("error body = %v, want not logged in", out)
	}
}

func TestHandlerRoutingSmoke(t *testing.T) {
	p, _ := newTestProxy(t)
	h := p.Handler()

	// unknown paths fall through to the mux default (404)
	req := httptest.NewRequest(http.MethodGet, "/does-not-exist", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown path status = %d, want 404", rec.Code)
	}

	// public /status answers anonymously through the real mux
	req = httptest.NewRequest(http.MethodGet, "/status", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET /status = %d, want 200", rec.Code)
	}

	// /me without a session is 401 through the mux
	req = httptest.NewRequest(http.MethodGet, "/me", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /me = %d, want 401", rec.Code)
	}

	// admin API stays gated even when the page itself would redirect
	req = httptest.NewRequest(http.MethodGet, "/admin/status", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /admin/status anonymous = %d, want 401", rec.Code)
	}
}

func TestHandlerRoutingPublicJSONRPCNoSession(t *testing.T) {
	p, _ := newTestProxy(t)
	h := p.Handler()
	req := httptest.NewRequest(http.MethodPost, "/WebUntis/jsonrpc.do",
		strings.NewReader(`{"jsonrpc":"2.0","id":"r","method":"getAppName","params":{}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 error carrier", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "-8520") {
		t.Errorf("body = %s, want -8520 not logged in", body)
	}
}
