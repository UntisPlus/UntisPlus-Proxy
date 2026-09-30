package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"
)

// healthPollStaleAfter is how long a school may go without a single successful
// class poll before /healthz reports it as degraded.
const healthPollStaleAfter = 10 * time.Minute

// schoolHealth is the per-school view behind /healthz and /metrics.
type schoolHealth struct {
	School      string    `json:"school"`
	Classes     int       `json:"classes"`
	ScanScanned int       `json:"scanScanned"`
	ScanStale   int       `json:"scanStale"`
	ScanTarget  string    `json:"scanTarget"`
	PollRuns    int64     `json:"pollRuns"`
	PollChanges int64     `json:"pollChanges"`
	PollFails   int64     `json:"pollFails"`
	LastPoll    int64     `json:"lastPollAgoSec"`
	LastPollOK  int64     `json:"lastSuccessfulPollAgoSec"`
	Degraded    bool      `json:"degraded"`
	Reasons     []string  `json:"reasons,omitempty"`
	LastPollAt  time.Time `json:"-"`
	// LastOK is the last poll that actually read a timetable. Staleness is
	// judged on this, not on the last attempt: a proxy whose polls all fail is
	// still serving stale data, and that is exactly the state /healthz exists
	// to expose.
	LastOK time.Time `json:"-"`
}

// health reports process liveness plus, per school, whether the change poller
// and the recon enumeration are actually keeping up. A school with a non-empty
// pool but no successful poll within healthPollStaleAfter is degraded, which is
// the failure mode that is otherwise invisible: the process is up, the app still
// works, but nobody notices the timetable went stale.
func (p *Proxy) health() (all []schoolHealth, ok bool) {
	ok = true
	now := time.Now()
	schools, err := p.store.ListSchools()
	if err != nil {
		ok = false
		return []schoolHealth{{School: p.opts.School, Degraded: true, Reasons: []string{"store unavailable: " + err.Error()}}}, false
	}
	if len(schools) == 0 {
		// Nothing registered yet: the process is still healthy.
		return []schoolHealth{}, true
	}
	seen := map[string]bool{}
	for _, sc := range schools {
		if sc.Name == "" || seen[sc.Name] {
			continue
		}
		seen[sc.Name] = true
		h := schoolHealth{School: sc.Name, ScanTarget: reconHorizon()}
		if pool, err := p.store.Pool(sc.Name); err == nil {
			h.Classes = len(pool)
		}
		progress := p.reconProgress(sc.Name)
		h.ScanScanned, _ = progress["scanned"].(int)
		h.ScanStale, _ = progress["stale"].(int)
		st := p.stateFor(sc.Name)
		st.pollMu.Lock()
		h.PollRuns, h.PollChanges, h.PollFails = st.pollRuns, st.pollChanges, st.pollFails
		h.LastPollAt = st.lastPoll
		h.LastOK = st.lastPollOK
		st.pollMu.Unlock()
		if !h.LastPollAt.IsZero() {
			h.LastPoll = int64(now.Sub(h.LastPollAt).Seconds())
		}
		if !h.LastOK.IsZero() {
			h.LastPollOK = int64(now.Sub(h.LastOK).Seconds())
		}
		if h.Classes > 0 {
			switch {
			case h.LastOK.IsZero():
				h.Reasons = append(h.Reasons, "no class poll has ever succeeded")
			case h.LastPollAt.After(h.LastOK):
				// A poller that succeeded recently and has been failing since is
				// already serving stale data; waiting out the staleness window
				// would hide a school server that went down ten minutes ago.
				h.Reasons = append(h.Reasons, "the most recent class poll failed")
			case now.Sub(h.LastOK) > healthPollStaleAfter:
				h.Reasons = append(h.Reasons, fmt.Sprintf("last successful class poll was %ds ago", h.LastPollOK))
			}
			if h.ScanStale > 0 {
				h.Reasons = append(h.Reasons, fmt.Sprintf("%d/%d classes not scanned through %s", h.ScanStale, h.Classes, h.ScanTarget))
			}
		}
		h.Degraded = len(h.Reasons) > 0
		if h.Degraded {
			ok = false
		}
		all = append(all, h)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].School < all[j].School })
	return all, ok
}

// handleHealthz answers 200 while the proxy is serving and every school with a
// non-empty pool is being polled, and 503 when a school has gone stale.
func (p *Proxy) handleHealthz(w http.ResponseWriter, r *http.Request) {
	schools, ok := p.health()
	code := http.StatusOK
	status := "ok"
	if !ok {
		code = http.StatusServiceUnavailable
		status = "degraded"
	}
	body, _ := json.Marshal(map[string]any{
		"status":     status,
		"uptime_sec": int64(time.Since(startedAt).Seconds()),
		"schools":    schools,
	})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// handleMetrics exposes the same health in Prometheus text format for
// container monitoring. No auth: it carries counts and names only, and a
// deployment that disagrees can simply not scrape it.
func (p *Proxy) handleMetrics(w http.ResponseWriter, r *http.Request) {
	schools, ok := p.health()
	var b []byte
	add := func(name, help, typ string, value any, labels []string) {
		b = append(b, ("#HELP " + name + " " + help + "\n#TYPE " + name + " " + typ + "\n")...)
		b = append(b, fmt.Sprintf("%s%s %v\n", name, labelsSuffix(labels), value)...)
	}
	b = append(b, "#HELP untis_up 1 while the proxy is serving and every school is polled.\n#TYPE untis_up gauge\n"...)
	b = append(b, fmt.Sprintf("untis_up %d\n", boolToInt(ok))...)
	b = append(b, "#HELP untis_uptime_seconds Seconds since process start.\n#TYPE untis_uptime_seconds gauge\n"...)
	b = append(b, fmt.Sprintf("untis_uptime_seconds %d\n", int64(time.Since(startedAt).Seconds()))...)

	for _, s := range schools {
		add("untis_school_degraded", "1 when the school's polling or recon scan is behind.",
			"gauge", boolToInt(s.Degraded), []string{"school", s.School})
		add("untis_pool_classes", "Pooled classes with a timetable owner.",
			"gauge", s.Classes, []string{"school", s.School})
		add("untis_recon_classes_scanned", "Pooled classes scanned through the target horizon.",
			"gauge", s.ScanScanned, []string{"school", s.School})
		add("untis_recon_classes_stale", "Pooled classes behind the target horizon.",
			"gauge", s.ScanStale, []string{"school", s.School})
		add("untis_poll_runs_total", "Class polls attempted.",
			"counter", s.PollRuns, []string{"school", s.School})
		add("untis_poll_changes_total", "Polls that detected a timetable change.",
			"counter", s.PollChanges, []string{"school", s.School})
		add("untis_poll_failures_total", "Polls that failed to reach the school server.",
			"counter", s.PollFails, []string{"school", s.School})
		add("untis_poll_last_age_seconds", "Seconds since the last class poll attempt.",
			"gauge", s.LastPoll, []string{"school", s.School})
		add("untis_poll_last_success_age_seconds", "Seconds since the last class poll that read a timetable.",
			"gauge", s.LastPollOK, []string{"school", s.School})
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

func labelsSuffix(labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	out := "{"
	for i := 0; i+1 < len(labels); i += 2 {
		if i > 0 {
			out += ","
		}
		out += labels[i] + `="` + labels[i+1] + `"`
	}
	return out + "}"
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
