package proxy

// Operational endpoints: /healthz (liveness + per-school staleness),
// /metrics (Prometheus text) and the recon scan progress they report.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func healthz(t *testing.T, p *Proxy) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	p.handleHealthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	return rec
}

func metricsGet(t *testing.T, p *Proxy) string {
	t.Helper()
	rec := httptest.NewRecorder()
	p.handleMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("metrics content-type = %q, want text/plain", ct)
	}
	return rec.Body.String()
}

// TestHealthz_EmptyDeploymentIsHealthy: with no school registered there is
// nothing to poll, so the proxy must not report itself as broken.
func TestHealthz_EmptyDeploymentIsHealthy(t *testing.T) {
	p, _ := newTestProxy(t)
	rec := healthz(t, p)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Status  string `json:"status"`
		Schools []struct {
			School   string `json:"school"`
			Degraded bool   `json:"degraded"`
		} `json:"schools"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if out.Status != "ok" || len(out.Schools) != 0 {
		t.Errorf("status=%q schools=%d, want ok and none", out.Status, len(out.Schools))
	}
}

// TestHealthz_PoolWithoutPollIsDegraded: a pooled class that has never been
// polled means the change detector is not running — the exact failure that is
// otherwise invisible, because the app keeps working on a stale timetable.
func TestHealthz_PoolWithoutPollIsDegraded(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertSchool("testschool"); err != nil {
		t.Fatalf("seed school: %v", err)
	}
	seedClassOwner(t, st, "owen", 7, 5000)

	rec := healthz(t, p)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503 (body %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`"status":"degraded"`, "no class poll has ever succeeded"} {
		if !strings.Contains(body, want) {
			t.Errorf("healthz body missing %q: %s", want, body)
		}
	}
}

// TestHealthz_PollResetsDegraded: once the poller has run successfully the
// school is healthy again, and the poll counters are reported.
func TestHealthz_PollResetsDegraded(t *testing.T) {
	f := &fakeUpstream{}
	f.setTimetable(t, []map[string]any{reconPeriod(10, "2026-09-21", "08:00", "08:45", 5009, 169, 7)})
	p, st := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st, "owen", 7, 5000)
	// A class scanned through the horizon keeps the recon side of the check quiet.
	if err := st.SaveReconScanAt("testschool", 5000, reconHorizon()); err != nil {
		t.Fatalf("SaveReconScanAt: %v", err)
	}

	p.pollOnce("testschool")
	rec := healthz(t, p)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 after a successful poll (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"pollRuns":1`) {
		t.Errorf("healthz body missing pollRuns=1: %s", rec.Body.String())
	}
}

// TestHealthz_StalePollDegrades: a poll that happened long ago is degraded even
// though the process itself is fine.
func TestHealthz_StalePollDegrades(t *testing.T) {
	p, st := newTestProxy(t)
	if err := st.UpsertSchool("testschool"); err != nil {
		t.Fatalf("seed school: %v", err)
	}
	seedClassOwner(t, st, "owen", 7, 5000)
	if err := st.SaveReconScanAt("testschool", 5000, reconHorizon()); err != nil {
		t.Fatalf("SaveReconScanAt: %v", err)
	}
	s := p.stateFor("testschool")
	s.pollMu.Lock()
	s.pollRuns = 5
	s.lastPoll = time.Now().Add(-time.Hour)
	s.lastPollOK = time.Now().Add(-time.Hour)
	s.pollMu.Unlock()

	rec := healthz(t, p)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503 for a 1h old poll (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "last successful class poll was") {
		t.Errorf("healthz body should explain the stale poll: %s", rec.Body.String())
	}
}

// TestHealthz_PollFailureCounted: an upstream failure is counted separately so
// a flapping school server is visible in the metrics.
func TestHealthz_PollFailureCounted(t *testing.T) {
	f := &fakeUpstream{}
	f.setTimetableStatus(http.StatusBadGateway)
	p, st := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st, "owen", 7, 5000)
	if err := st.SaveReconScanAt("testschool", 5000, reconHorizon()); err != nil {
		t.Fatalf("SaveReconScanAt: %v", err)
	}
	p.pollOnce("testschool")

	s := p.stateFor("testschool")
	s.pollMu.Lock()
	runs, fails := s.pollRuns, s.pollFails
	s.pollMu.Unlock()
	if runs != 1 {
		t.Errorf("pollRuns = %d, want 1", runs)
	}
	if fails != 1 {
		t.Errorf("pollFails = %d, want 1 (upstream returned 502)", fails)
	}
	if body := metricsGet(t, p); !strings.Contains(body, `untis_poll_failures_total{school="testschool"} 1`) {
		t.Errorf("metrics missing failure counter:\n%s", body)
	}
}

// TestMetrics_ExposesPrometheusText: the scrape format carries the process
// gauge and one labelled series set per school.
func TestMetrics_ExposesPrometheusText(t *testing.T) {
	f := &fakeUpstream{}
	f.setTimetable(t, []map[string]any{reconPeriod(10, "2026-09-21", "08:00", "08:45", 5009, 169, 7)})
	p, st := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st, "owen", 7, 5000)
	if err := st.SaveReconScanAt("testschool", 5000, reconHorizon()); err != nil {
		t.Fatalf("SaveReconScanAt: %v", err)
	}
	p.pollOnce("testschool")

	body := metricsGet(t, p)
	for _, want := range []string{
		"untis_up 1",
		"untis_uptime_seconds ",
		`untis_pool_classes{school="testschool"} 1`,
		`untis_recon_classes_scanned{school="testschool"} 1`,
		`untis_recon_classes_stale{school="testschool"} 0`,
		`untis_poll_runs_total{school="testschool"} 1`,
		`untis_school_degraded{school="testschool"} 0`,
		"#TYPE untis_poll_runs_total counter",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q:\n%s", want, body)
		}
	}
}

// TestAdminReconScanProgressAndRescan: the admin recon card reports per-class
// progress and the rescan action clears the recorded horizons.
func TestAdminReconScanProgressAndRescan(t *testing.T) {
	f := &fakeUpstream{}
	f.setTimetable(t, []map[string]any{reconPeriod(10, "2026-09-21", "08:00", "08:45", 5009, 169, 7)})
	p, st := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st, "owen", 7, 5000)
	if err := st.SaveReconScanAt("testschool", 5000, "2020-01-01"); err != nil {
		t.Fatalf("SaveReconScanAt: %v", err)
	}

	get := httptest.NewRequest(http.MethodGet, "/admin/recon", nil)
	rec := httptest.NewRecorder()
	p.adminRecon(rec, get, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET recon: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Scan struct {
			Target   string `json:"target"`
			Classes  int    `json:"classes"`
			Scanned  int    `json:"scanned"`
			Stale    int    `json:"stale"`
			PerClass []struct {
				ClassID   int64  `json:"classId"`
				ScanUntil string `json:"scanUntil"`
			} `json:"perClass"`
		} `json:"scan"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if out.Scan.Classes != 1 || out.Scan.Stale != 1 || out.Scan.Scanned != 0 {
		t.Errorf("scan progress = %+v, want 1 class stale and none scanned", out.Scan)
	}
	if out.Scan.Target == "" {
		t.Error("scan target missing")
	}
	if len(out.Scan.PerClass) != 1 || out.Scan.PerClass[0].ClassID != 5000 ||
		out.Scan.PerClass[0].ScanUntil != "2020-01-01" {
		t.Errorf("per-class progress = %+v, want class 5000 at 2020-01-01", out.Scan.PerClass)
	}

	post := httptest.NewRequest(http.MethodPost, "/admin/recon/rescan", nil)
	rec2 := httptest.NewRecorder()
	p.adminRecon(rec2, post, []string{"rescan"})
	if rec2.Code != http.StatusOK {
		t.Fatalf("POST rescan: code=%d body=%s", rec2.Code, rec2.Body.String())
	}
	waitFor(t, 10*time.Second, func() bool {
		scan, _ := st.ReconScan("testschool")
		return scan[5000] != "" && scan[5000] != "2020-01-01"
	}, "rescan to record a fresh horizon")
	elems, _ := st.LoadReconElements("testschool")
	if len(elems["TEACHER"]) != 1 {
		t.Errorf("rescan found %v teachers, want 1", elems["TEACHER"])
	}
}

// TestHealthz_FailingPollsDegrade: a proxy that keeps polling a broken school
// server has recent poll *attempts* but no successful read, so it must still be
// reported as degraded. Treating an attempt as success is the failure this test
// exists to prevent.
func TestHealthz_FailingPollsDegrade(t *testing.T) {
	f := &fakeUpstream{}
	f.setTimetableStatus(http.StatusBadGateway)
	p, st := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st, "owen", 7, 5000)

	p.pollOnce("testschool")

	s := p.stateFor("testschool")
	s.pollMu.Lock()
	runs, fails, ok := s.pollRuns, s.pollFails, s.lastPollOK
	s.pollMu.Unlock()
	if runs != 1 || fails != 1 {
		t.Fatalf("pollRuns=%d pollFails=%d, want 1/1", runs, fails)
	}
	if !ok.IsZero() {
		t.Errorf("lastPollOK = %v, want zero after a failed poll", ok)
	}
	rec := healthz(t, p)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503 when every poll fails (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no class poll has ever succeeded") {
		t.Errorf("healthz should report the missing successful poll: %s", rec.Body.String())
	}
}

// TestMetrics_ExposeSuccessfulPollAge: the successful-poll age is scraped, so an
// operator can alert on it directly.
func TestMetrics_ExposeSuccessfulPollAge(t *testing.T) {
	p, st := newFakeProxyUpstream(t, &fakeUpstream{})
	seedClassOwner(t, st, "owen", 7, 5000)
	s := p.stateFor("testschool")
	s.pollMu.Lock()
	s.lastPoll = time.Now()
	s.lastPollOK = time.Now()
	s.pollMu.Unlock()
	body := metricsGet(t, p)
	if !strings.Contains(body, `untis_poll_last_success_age_seconds{school="testschool"}`) {
		t.Errorf("metrics missing the successful-poll age:\n%s", body)
	}
}

// TestHealthz_RecoveredThenFailingIsDegraded: once a poll has succeeded, a
// school server that goes down is visible immediately — waiting for the
// staleness window would mean ten more minutes of serving stale data while
// /healthz still answers 200.
func TestHealthz_RecoveredThenFailingIsDegraded(t *testing.T) {
	f := &fakeUpstream{}
	f.setTimetable(t, []map[string]any{{"id": 1, "startDateTime": "2026-01-05T08:00:00Z", "endDateTime": "2026-01-05T08:45:00Z"}})
	p, st := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st, "owen", 7, 5000)
	p.pollOnce("testschool")
	if code := healthz(t, p).Code; code != http.StatusOK {
		t.Fatalf("code = %d after a successful poll, want 200", code)
	}

	f.setTimetableStatus(http.StatusBadGateway)
	p.pollOnce("testschool")
	rec := healthz(t, p)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503 once polls start failing (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "the most recent class poll failed") {
		t.Errorf("healthz should name the failing poll: %s", rec.Body.String())
	}

	// Recovery clears the reason again.
	f.restoreTimetable()
	p.pollOnce("testschool")
	if code := healthz(t, p).Code; code != http.StatusOK {
		t.Errorf("code = %d after recovery, want 200", code)
	}
}
