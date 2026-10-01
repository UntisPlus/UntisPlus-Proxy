package proxy

// Delivery outbox: durable, retrying fan-out of timetable changes.
//
// The version bump that records a change and the notification announcing it must
// not be able to disagree. Before this, the version committed first and the send
// was fired from a goroutine afterwards, so a failed post — or a restart in the
// gap — lost the notification permanently: the next poll saw no change and
// re-sent nothing. Every destination is now queued inside the same transaction
// that stamps the version, and this worker is what actually makes the HTTP call.
//
// One queue row per destination, not one per change. A change fans out to every
// matching webhook and ntfy topic; if the whole fan-out were retried as a unit,
// one permanently unreachable destination would replay the healthy ones forever,
// which is the same duplicate-notification failure as the v1.4.4 re-notification
// flood.

import (
	"encoding/json"
	"log"
	"time"

	"untis-proxy/internal/store"
)

const (
	// outboxMaxAttempts is how many times one delivery is tried before it is
	// parked as dead for an operator to look at. With the backoff below the
	// last attempt is roughly 25 minutes after the first.
	outboxMaxAttempts = 8
	// outboxBaseBackoff is the first retry delay; each attempt doubles it,
	// capped so a long outage does not push the final attempt days out.
	outboxBaseBackoff = 30 * time.Second
	outboxMaxBackoff  = 15 * time.Minute
	// outboxBatch bounds one drain pass, so a large backlog is worked through
	// over several ticks instead of in one long loop.
	outboxBatch = 50
)

// outboxBackoff returns the delay before the given attempt number is retried.
func outboxBackoff(attempts int) time.Duration {
	d := outboxBaseBackoff
	for i := 1; i < attempts; i++ {
		d *= 2
		if d >= outboxMaxBackoff {
			return outboxMaxBackoff
		}
	}
	if d > outboxMaxBackoff {
		return outboxMaxBackoff
	}
	return d
}

// StartOutboxWorker drains the delivery queue until done closes. On startup it
// reclaims rows abandoned in 'sending' by a process that died mid-flight, so a
// crash during a delivery is retried rather than silently lost.
func (p *Proxy) StartOutboxWorker(interval time.Duration, done <-chan struct{}) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if err := p.store.ReleaseOutboxStale(); err != nil {
		log.Printf("[outbox] release stale: %v", err)
	}
	p.drainOutbox()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			p.drainOutbox()
		}
	}
}

// drainOutbox attempts every delivery that is due as of now.
func (p *Proxy) drainOutbox() {
	p.drainOutboxDue(time.Now())
}

// drainOutboxDue attempts every delivery due as of now, whether or not a
// retry backoff has actually elapsed. The worker only ever calls it with the
// current time; tests use it to step past a backoff without sleeping.
func (p *Proxy) drainOutboxDue(now time.Time) {
	rows, err := p.store.DueOutbox(now, outboxBatch)
	if err != nil {
		log.Printf("[outbox] due: %v", err)
		return
	}
	for _, row := range rows {
		claimed, err := p.store.ClaimOutbox(row.ID)
		if err != nil {
			log.Printf("[outbox] claim %d: %v", row.ID, err)
			continue
		}
		if !claimed {
			continue
		}
		if err := p.sendOutboxRow(row); err != nil {
			attempts := row.Attempts + 1
			if ferr := p.store.FailOutbox(row.ID, attempts, err.Error(),
				time.Now().Add(outboxBackoff(attempts)), outboxMaxAttempts); ferr != nil {
				log.Printf("[outbox] fail %d: %v", row.ID, ferr)
				continue
			}
			if attempts >= outboxMaxAttempts {
				log.Printf("[outbox] delivery %d to %s %d gave up after %d attempts: %v",
					row.ID, row.Dest, row.DestID, attempts, err)
			} else {
				log.Printf("[outbox] delivery %d to %s %d failed (attempt %d), retrying in %s: %v",
					row.ID, row.Dest, row.DestID, attempts, outboxBackoff(attempts), err)
			}
			continue
		}
		if err := p.store.FinishOutbox(row.ID); err != nil {
			log.Printf("[outbox] finish %d: %v", row.ID, err)
		}
	}
}

// sendOutboxRow performs one delivery attempt for one queued row.
func (p *Proxy) sendOutboxRow(row store.OutboxRow) error {
	switch row.Dest {
	case "webhook":
		h := p.findWebhook(row.School, row.DestID)
		if h == nil {
			// The subscription was deleted while the delivery was queued. Not an
			// error worth retrying: the destination no longer exists by intent.
			log.Printf("[outbox] webhook %d for %s no longer exists, dropping", row.DestID, row.School)
			return nil
		}
		if !h.Enabled {
			return nil
		}
		return p.postWebhookOnce(h, row.Payload, outboxSummary(row.Payload))

	case "ntfy":
		t := p.findNtfyTopic(row.School, row.DestID)
		if t == nil {
			log.Printf("[outbox] ntfy topic %d for %s no longer exists, dropping", row.DestID, row.School)
			return nil
		}
		if !t.Enabled {
			return nil
		}
		var payload struct {
			Digest changeDigest `json:"digest"`
		}
		if err := json.Unmarshal(row.Payload, &payload); err != nil {
			// Undecodable bytes will never become decodable; retrying forever
			// would only hide it.
			return err
		}
		return p.publishNtfyOnce(t, row.School, row.ClassID, payload.Digest)
	}
	return nil
}

// outboxSummary pulls the single-line summary back out of a stored payload for
// the X-Untis-Summary header, falling back to a generic line if the stored bytes
// are not what we expect.
func outboxSummary(payload []byte) string {
	var p struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(payload, &p); err != nil || p.Summary == "" {
		return "timetable change"
	}
	return p.Summary
}

// findWebhook returns a webhook by id within a school, or nil.
func (p *Proxy) findWebhook(school string, id int64) *store.Webhook {
	hooks, err := p.store.ListWebhooks(school)
	if err != nil {
		return nil
	}
	for _, h := range hooks {
		if h.ID == id {
			return h
		}
	}
	return nil
}

// findNtfyTopic returns an ntfy topic by id within a school, or nil.
func (p *Proxy) findNtfyTopic(school string, id int64) *store.NtfyTopic {
	topics, err := p.store.ListNtfyTopics(school)
	if err != nil {
		return nil
	}
	for _, t := range topics {
		if t.ID == id {
			return t
		}
	}
	return nil
}
