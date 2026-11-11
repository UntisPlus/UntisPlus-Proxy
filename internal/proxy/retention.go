package proxy

// Opt-in retention pruning for the two tables that only ever grow as students
// use the app: finished-homework flags and private absence notes. Both belong to
// a single student and go stale with the term, but the proxy cannot know when an
// assignment or absence is no longer relevant, so pruning is disabled unless an
// operator picks a retention age with -retention-days.
//
// The sweep is deliberately separate from the outbox sweep: one is about bounded
// delivery state, the other deletes user-authored data, so the two cannot share
// a default or an on/off switch.

import (
	"log"
	"time"
)

// retentionInterval is how often the sweep runs once enabled. Daily is enough for
// data measured in terms, and keeps the delete cheap.
const retentionInterval = 24 * time.Hour

// retentionAge is how long homework flags and absence notes are kept. Zero (the
// default) disables pruning entirely.
var retentionAge time.Duration

// SetRetentionAge sets the retention age for homework flags and absence notes. A
// non-positive value disables pruning.
func SetRetentionAge(d time.Duration) { retentionAge = d }

// StartRetentionLoop prunes old rows immediately and then on a timer. It returns
// at once when retention is disabled, so a caller can start it unconditionally.
func (p *Proxy) StartRetentionLoop(interval time.Duration, done <-chan struct{}) {
	if retentionAge <= 0 {
		return
	}
	if interval <= 0 {
		interval = retentionInterval
	}
	p.pruneRetention(time.Now())
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			p.pruneRetention(time.Now())
		}
	}
}

// pruneRetention deletes homework flags and absence notes older than the
// configured age. Logs counts only — never the student or note text.
func (p *Proxy) pruneRetention(now time.Time) {
	if retentionAge <= 0 {
		return
	}
	cutoff := now.Add(-retentionAge)
	if n, err := p.store.PruneHomeworkDone(cutoff); err != nil {
		log.Printf("[retention] prune homework flags: %v", err)
	} else if n > 0 {
		log.Printf("[retention] pruned %d homework flags older than %s", n, retentionAge)
	}
	if n, err := p.store.PruneAbsenceNotes(cutoff); err != nil {
		log.Printf("[retention] prune absence notes: %v", err)
	} else if n > 0 {
		log.Printf("[retention] pruned %d absence notes older than %s", n, retentionAge)
	}
}
