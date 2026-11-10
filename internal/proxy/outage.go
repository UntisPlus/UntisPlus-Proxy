package proxy

import (
	"fmt"
	"log"
	"strings"
	"time"

	"untis-proxy/internal/store"
)

// outageAlertDebounce is how long a poll failure has to persist before an ntfy
// alert is sent. A single failed poll is often a timeout or an upstream restart;
// waiting a couple of ticks keeps a transient blip from paging anyone.
const outageAlertDebounce = 3 * time.Minute

// outageAlertTracker remembers, per subscription, when the outage started and
// whether the down alert has already been delivered. It lives in the alert loop,
// so it is only touched by that goroutine.
type outageAlertTracker struct {
	since map[int64]time.Time
	down  map[int64]bool
}

func newOutageAlertTracker() *outageAlertTracker {
	return &outageAlertTracker{since: map[int64]time.Time{}, down: map[int64]bool{}}
}

// schoolPollOutage reports whether a school's change poller is currently down,
// using only poll success/failure. Recon lag is deliberately ignored: a slow
// scan is not an outage, and including it would alert on a busy but reachable
// school.
func (p *Proxy) schoolPollOutage(school string, now time.Time) (bool, string) {
	info, err := p.store.SchoolInfo(school)
	if err != nil || info == nil || info.Blocked {
		return false, ""
	}
	pool, err := p.store.Pool(school)
	if err == nil && len(pool) == 0 {
		return false, ""
	}
	p.schoolsMu.Lock()
	st := p.schools[school]
	p.schoolsMu.Unlock()
	if st == nil {
		// The poller has not touched this school yet in this process. It will get
		// an immediate pass at startup, so there is no evidence of an outage yet.
		return false, ""
	}
	st.pollMu.Lock()
	lastPoll, lastOK := st.lastPoll, st.lastPollOK
	st.pollMu.Unlock()
	switch {
	case lastOK.IsZero():
		return true, "no class poll has ever succeeded"
	case lastPoll.After(lastOK):
		return true, "the most recent class poll failed"
	case now.Sub(lastOK) > healthPollStaleAfter:
		return true, fmt.Sprintf("last successful class poll was %s ago", now.Sub(lastOK).Round(time.Second))
	}
	return false, ""
}

// outageCondition evaluates one subscription scope. An empty school means all
// registered schools; any one of them being down makes the overall alert fire,
// and the detail names each affected school and reason.
func (p *Proxy) outageCondition(scope string, now time.Time) (bool, string) {
	if scope != "" {
		down, reason := p.schoolPollOutage(scope, now)
		if !down {
			return false, ""
		}
		return true, reason
	}
	schools, err := p.store.ListSchools()
	if err != nil {
		return false, ""
	}
	var affected []string
	for _, sc := range schools {
		if sc.Name == "" || sc.Blocked {
			continue
		}
		if down, reason := p.schoolPollOutage(sc.Name, now); down {
			affected = append(affected, sc.Name+" ("+reason+")")
		}
	}
	if len(affected) == 0 {
		return false, ""
	}
	return true, strings.Join(affected, "; ")
}

// runOutageAlerts evaluates every subscription once. A down alert is sent after
// the condition has held for outageAlertDebounce; a recovery alert is sent once
// when the condition clears. Failed posts are retried on the next tick.
func (p *Proxy) runOutageAlerts(now time.Time, tr *outageAlertTracker) {
	subs, err := p.store.ListOutageNtfy()
	if err != nil {
		log.Printf("[outage] list subscriptions: %v", err)
		return
	}
	active := make(map[int64]bool, len(subs))
	for _, o := range subs {
		if !o.Enabled {
			continue
		}
		active[o.ID] = true
		down, detail := p.outageCondition(o.School, now)
		if down {
			if _, ok := tr.since[o.ID]; !ok {
				tr.since[o.ID] = now
			}
			if !tr.down[o.ID] && now.Sub(tr.since[o.ID]) >= outageAlertDebounce {
				if err := p.sendOutageNtfy(o, true, detail); err != nil {
					log.Printf("[outage] alert %d: %v", o.ID, err)
					continue
				}
				tr.down[o.ID] = true
				log.Printf("[outage] alerted %q (scope %q): %s", o.Topic, o.Scope(), detail)
			}
			continue
		}
		if tr.down[o.ID] {
			if err := p.sendOutageNtfy(o, false, ""); err != nil {
				log.Printf("[outage] recovery %d: %v", o.ID, err)
				continue
			}
			delete(tr.down, o.ID)
			delete(tr.since, o.ID)
			log.Printf("[outage] recovery sent to %q (scope %q)", o.Topic, o.Scope())
			continue
		}
		delete(tr.since, o.ID)
	}
	for id := range tr.since {
		if !active[id] {
			delete(tr.since, id)
		}
	}
	for id := range tr.down {
		if !active[id] {
			delete(tr.down, id)
		}
	}
}

// StartOutageAlertLoop watches poll health and delivers outage/recovery alerts
// until done. The first evaluation runs immediately; the debounce keeps that
// initial pass from alerting before a school has had a chance to poll.
func (p *Proxy) StartOutageAlertLoop(interval time.Duration, done <-chan struct{}) {
	if interval <= 0 {
		interval = time.Minute
	}
	tr := newOutageAlertTracker()
	p.runOutageAlerts(time.Now(), tr)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case now := <-t.C:
			p.runOutageAlerts(now, tr)
		}
	}
}

// sendOutageNtfy posts one outage or recovery notification to the subscription's
// topic. It uses the same root-URL JSON envelope as timetable notifications, so
// a self-hosted ntfy server override behaves identically.
func (p *Proxy) sendOutageNtfy(o *store.OutageNtfy, down bool, detail string) error {
	scope := o.School
	if scope == "" {
		scope = "all schools"
	}
	title := "Untis outage — " + scope
	tags := []string{"rotating_light"}
	priority := 4
	var message string
	if o.School == "" {
		message = "Untis is down for: " + detail
	} else {
		message = "Untis is down for " + o.School + ": " + detail
	}
	if !down {
		title = "Untis recovered — " + scope
		tags = []string{"white_check_mark"}
		priority = 3
		if o.School == "" {
			message = "Untis is reachable again for all monitored schools."
		} else {
			message = "Untis is reachable again for " + o.School + "."
		}
	}
	status, err := ntfyPost(&store.NtfyTopic{Topic: o.Topic, BaseURL: o.BaseURL}, map[string]any{
		"topic":    o.Topic,
		"title":    title,
		"message":  message,
		"tags":     tags,
		"priority": priority,
		"click":    publicURL("/healthz"),
	})
	if err != nil {
		return fmt.Errorf("ntfy %s: %w", o.Topic, err)
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("ntfy %s returned %d", o.Topic, status)
	}
	return nil
}

// testOutageNtfy sends a sample outage alert so an admin can verify the topic
// and server without waiting for a real failure.
func (p *Proxy) testOutageNtfy(o *store.OutageNtfy) (int, error) {
	scope := o.School
	if scope == "" {
		scope = "all schools"
	}
	return ntfyPost(&store.NtfyTopic{Topic: o.Topic, BaseURL: o.BaseURL}, map[string]any{
		"topic":    o.Topic,
		"title":    "untis-proxy outage alert test — " + scope,
		"message":  "This is a test outage notification from the untis-proxy admin dashboard. Real alerts use this topic when Untis polling fails.",
		"tags":     []string{"white_check_mark", "rotating_light"},
		"priority": 3,
		"click":    publicURL("/healthz"),
	})
}
