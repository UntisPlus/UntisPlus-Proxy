package proxy

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"untis-proxy/internal/store"
)

// ntfyBase is the publish base URL for push notifications; overridden in tests
// to point at a local httptest server.
var ntfyBase = "https://ntfy.sh"

// publicBase is the externally reachable base URL of this proxy. It is used to
// build the click-through link on a notification and in untisctl's generated
// .ics links. Deployments differ, so it is configuration, never a hardcoded
// hostname.
var publicBase = ""

// SetPublicBase sets the externally reachable base URL (scheme + host) used for
// click-through links. Trailing slashes are trimmed; an empty value is ignored
// so the flag can be left unset.
func SetPublicBase(base string) {
	if base != "" {
		publicBase = strings.TrimRight(base, "/")
	}
}

// publicURL joins publicBase with a path, falling back to a relative path when
// no public base is configured. A relative click target is still useful to ntfy
// clients that resolve it, and beats embedding a hostname that belongs to
// somebody else's deployment.
func publicURL(path string) string {
	if publicBase == "" {
		return path
	}
	return publicBase + path
}

// SetNtfyBase overrides the publish base URL (e.g. a self-hosted ntfy server).
func SetNtfyBase(base string) {
	if base != "" {
		ntfyBase = strings.TrimRight(base, "/")
	}
}

// notifyHub fans out timetable-change events to SSE subscribers, keyed by
// school|classID.
type notifyHub struct {
	mu   sync.RWMutex
	subs map[string]map[chan notifyMsg]struct{}
}

type notifyMsg struct {
	School  string            `json:"school"`
	ClassID int64             `json:"classId"`
	Version int64             `json:"version"`
	Changes []store.PeriodRow `json:"changes"`
}

func newNotifyHub() *notifyHub {
	return &notifyHub{subs: map[string]map[chan notifyMsg]struct{}{}}
}

func hubKey(school string, classID int64) string {
	return fmt.Sprintf("%s|%d", school, classID)
}

func (h *notifyHub) subscribe(school string, classID int64) (chan notifyMsg, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan notifyMsg, 8)
	k := hubKey(school, classID)
	if h.subs[k] == nil {
		h.subs[k] = map[chan notifyMsg]struct{}{}
	}
	h.subs[k][ch] = struct{}{}
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if m, ok := h.subs[k]; ok {
			delete(m, ch)
			close(ch)
			if len(m) == 0 {
				delete(h.subs, k)
			}
		}
	}
}

func (h *notifyHub) publish(school string, classID int64, msg notifyMsg) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.subs[hubKey(school, classID)] {
		select {
		case ch <- msg:
		default:
		}
	}
}

// periodRow converts a fetched period into a change snapshot row.
func periodRow(pd map[string]any, md *masterDataCache) store.PeriodRow {
	st, _ := pd["startDateTime"].(string)
	en, _ := pd["endDateTime"].(string)
	pid, _ := pd["id"].(float64)
	var subject, room, teacher string
	for _, el := range periodElementIDs(pd) {
		switch el.Type {
		case "SUBJECT":
			if subject == "" {
				subject = elementName(md, "SUBJECT", el.ID)
			}
		case "ROOM":
			if room == "" {
				room = elementName(md, "ROOM", el.ID)
			}
		case "TEACHER":
			if teacher == "" {
				teacher = elementName(md, "TEACHER", el.ID)
			}
		}
	}
	if subject == "" {
		subject = textField(pd, "subject")
	}
	if subject == "" {
		subject = textField(pd, "lesson")
	}
	desc := ""
	if t := textField(pd, "substitution"); t != "" {
		desc = "Substitution: " + t
	}
	if t := textField(pd, "info"); t != "" {
		if desc != "" {
			desc += "\n"
		}
		desc += "Info: " + t
	}
	return store.PeriodRow{
		PeriodID:    int64(pid),
		Start:       st,
		End:         en,
		Subject:     subject,
		Room:        room,
		Teacher:     teacher,
		Description: desc,
	}
}

// checkClass polls one class for timetable changes, persisting any diff. It
// returns true if a change was detected. Upstream errors are logged inside.
func (p *Proxy) checkClass(school string, classID int64) bool {
	changed, _ := p.checkClassErr(school, classID)
	return changed
}

// checkClassErr is checkClass but also reports why a poll produced no usable
// data, so the poll loop can surface a stale upstream in /healthz and /metrics.
func (p *Proxy) checkClassErr(school string, classID int64) (bool, error) {
	now := time.Now()
	start := now.AddDate(0, 0, -1).Format("2006-01-02")
	end := now.AddDate(0, 0, 4).Format("2006-01-02")
	periods, err := p.classPeriodsFresh(school, classID, start, end)
	if err != nil {
		log.Printf("[notify] poll class %d: %v", classID, err)
		return false, err
	}
	md := p.masterData(school)
	next := make([]store.PeriodRow, 0, len(periods))
	for _, pd := range periods {
		if row := periodRow(pd, md); row.Start != "" {
			next = append(next, row)
		}
	}
	newVer := p.store.ClassVersion(school, classID) + 1
	// Keep the pre-change snapshot: it is the only place the old room/teacher/
	// time still exists, so it is what lets notifications say "R204 -> R112"
	// instead of "1 changed".
	prevSnapshot, _ := p.store.LoadClassSnapshot(school, classID)
	dropBefore := time.Now().Format("2006-01-02")

	// Read the subscription lists AND resolve the class display name BEFORE the
	// snapshot transaction opens. The store pool is a single connection, so the
	// in-transaction enqueue cannot query the store without deadlocking on the
	// connection it already holds.
	hooks, err := p.store.ListWebhooks(school)
	if err != nil {
		log.Printf("[notify] webhooks %s: %v", school, err)
	}
	topics, err := p.store.ListNtfyTopics(school)
	if err != nil {
		log.Printf("[notify] ntfy %s: %v", school, err)
	}
	className := p.classDisplayName(school, classID)
	targets := p.resolveTargets(school, hooks, topics)

	// The queued deliveries are written by the same transaction that stamps
	// newVer, so a committed change always has a delivery waiting for it.
	var queued []store.PeriodRow
	changed, err := p.store.ApplyClassSnapshot(school, classID, next, newVer, dropBefore,
		func(rows []store.PeriodRow, put func(string, int64, []byte) error) error {
			queued = rows
			return p.enqueueChange(school, className, classID, newVer, rows, prevSnapshot, hooks, topics, targets, put)
		})
	if err != nil {
		log.Printf("[notify] store class %d: %v", classID, err)
		return false, err
	}
	if changed == 0 {
		return false, nil
	}
	sort.Slice(queued, func(i, j int) bool { return queued[i].PeriodID < queued[j].PeriodID })
	p.hub.publish(school, classID, notifyMsg{
		School:  school,
		ClassID: classID,
		Version: newVer,
		Changes: queued,
	})
	log.Printf("[notify] class %d changed (%d updates) -> version %d", classID, changed, newVer)
	return true, nil
}

// elementTargetName resolves the current display name for a target element,
// for notification titles and change-row matching.
func (p *Proxy) elementTargetName(school, et string, eid int64) string {
	if eid <= 0 {
		return ""
	}
	if et == "STUDENT" {
		if u, _ := p.store.UserByPersonID(eid); u != nil && u.DisplayName != "" {
			return u.DisplayName
		}
		return ""
	}
	if n := elementName(p.masterData(school), et, eid); n != "" {
		return n
	}
	return p.store.ElementName(school, et, eid)
}

// resolvedTarget is the identity a subscription is matched on, resolved ahead of
// time so matching itself never has to touch the database.
type resolvedTarget struct {
	name    string // display name for TEACHER/ROOM/SUBJECT
	classID int64  // class id for CLASS/STUDENT
}

// targetKey identifies a subscription target within one resolve pass.
func targetKey(et string, eid int64) string {
	return et + ":" + strconv.FormatInt(eid, 10)
}

// resolveTargets pre-resolves the identity of every subscription target, so that
// elementMatches can decide purely from memory.
//
// It must run before the snapshot transaction opens: resolving a STUDENT needs
// the class of a person, and resolving a TEACHER/ROOM/SUBJECT needs its display
// name — both store reads, and the store pool is a single connection that the
// transaction already holds. Targets that cannot be resolved are left out, and
// a missing entry means "no match" rather than a fallback query, because a
// fallback query is exactly the deadlock this avoids.
func (p *Proxy) resolveTargets(school string, hooks []*store.Webhook, topics []*store.NtfyTopic) map[string]resolvedTarget {
	out := make(map[string]resolvedTarget, len(hooks)+len(topics))
	add := func(et string, eid int64) {
		key := targetKey(et, eid)
		if _, done := out[key]; done {
			return
		}
		switch et {
		case "CLASS":
			out[key] = resolvedTarget{classID: eid}
		case "STUDENT":
			cid, err := p.store.ClassForPerson(school, eid)
			if err != nil {
				return
			}
			out[key] = resolvedTarget{classID: cid}
		default:
			name := p.elementTargetName(school, et, eid)
			if name == "" {
				return
			}
			out[key] = resolvedTarget{name: name}
		}
	}
	for _, h := range hooks {
		et, eid := h.Target()
		add(et, eid)
	}
	for _, t := range topics {
		et, eid := t.Target()
		add(et, eid)
	}
	return out
}

// elementMatches reports whether a change event for classID (with the given
// changed rows) targets the element (et,eid). School-wide topics (empty et)
// match everything; legacy class-targeted rows match when eid is the changed
// class; STUDENT matches when that person belongs to the changed class; and
// TEACHER/ROOM/SUBJECT match when their resolved name appears in a changed
// row.
//
// targets comes from resolveTargets and must be supplied by the caller so that
// no store read happens here; inside the snapshot transaction there is no spare
// connection to perform one.
func (p *Proxy) elementMatches(school, et string, eid, classID int64, rows []store.PeriodRow, targets map[string]resolvedTarget) bool {
	switch et {
	case "", "ALL", "SCHOOL":
		return true
	case "CLASS":
		return eid == classID
	}
	rt, ok := targets[targetKey(et, eid)]
	if !ok {
		return false
	}
	if et == "STUDENT" {
		return rt.classID == classID
	}
	name := rt.name
	for _, r := range rows {
		switch et {
		case "TEACHER":
			if r.Teacher == name {
				return true
			}
		case "ROOM":
			if r.Room == name {
				return true
			}
		case "SUBJECT":
			if r.Subject == name {
				return true
			}
		}
	}
	return false
}

// enqueueChange renders one change and queues it for every destination that
// matches it — one outbox row per destination, so a destination that keeps
// failing cannot cause healthy ones to be replayed.
//
// hooks, topics, targets and className must be supplied by the caller, all
// resolved before the snapshot transaction opened: the store pool is a single
// connection, so reading from inside that transaction would deadlock on the
// connection it already holds. A destination created in the gap between that
// read and the commit does not receive this change, which is the correct trade
// against a deadlock.
func (p *Proxy) enqueueChange(school, className string, classID, version int64, rows []store.PeriodRow,
	prev []store.PeriodRow, hooks []*store.Webhook, topics []*store.NtfyTopic,
	targets map[string]resolvedTarget,
	put func(dest string, destID int64, payload []byte) error) error {

	digest := p.buildDigestNamed(school, className, classID, prev, rows)
	body, err := json.Marshal(map[string]any{
		"event":   "change",
		"school":  school,
		"classId": classID,
		"version": version,
		"summary": digest.Summary,
		"digest":  digest,
		"changes": rows,
	})
	if err != nil {
		return err
	}
	for _, h := range hooks {
		if !h.Enabled {
			continue
		}
		et, eid := h.Target()
		if !p.elementMatches(school, et, eid, classID, rows, targets) {
			continue
		}
		if err := put("webhook", h.ID, body); err != nil {
			return err
		}
	}
	for _, t := range topics {
		if !t.Enabled {
			continue
		}
		et, eid := t.Target()
		if !p.elementMatches(school, et, eid, classID, rows, targets) {
			continue
		}
		if err := put("ntfy", t.ID, body); err != nil {
			return err
		}
	}
	return nil
}

// deliverChange queues one change for delivery and returns once it is durable.
// The actual HTTP happens later, in the outbox worker.
func (p *Proxy) deliverChange(school string, classID, version int64, rows, prev []store.PeriodRow) error {
	hooks, err := p.store.ListWebhooks(school)
	if err != nil {
		return err
	}
	topics, err := p.store.ListNtfyTopics(school)
	if err != nil {
		return err
	}
	targets := p.resolveTargets(school, hooks, topics)
	return p.enqueueChange(school, p.classDisplayName(school, classID), classID, version, rows, prev, hooks, topics, targets,
		func(dest string, destID int64, payload []byte) error {
			return p.store.EnqueueOutbox(school, classID, version, dest, destID, payload)
		})
}

// postWebhookOnce performs a single delivery attempt and reports the outcome, so
// the outbox worker can decide whether to retry. Retrying is the worker's job
// now: an in-process retry loop cannot survive a restart and cannot tell a
// temporary failure from a dead receiver.
func (p *Proxy) postWebhookOnce(h *store.Webhook, body []byte, summary string) error {
	req, err := http.NewRequest(http.MethodPost, h.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "untis-proxy/1.0")
	req.Header.Set("X-Untis-Event", "timetable-change")
	req.Header.Set("X-Untis-Summary", summary)
	if h.Secret != "" {
		mac := hmac.New(sha256.New, []byte(h.Secret))
		mac.Write(body)
		req.Header.Set("X-Untis-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook %s returned %d", h.URL, resp.StatusCode)
	}
	return nil
}

// ntfyPost publishes a JSON message to an ntfy server.
//
// The JSON envelope must go to the server's ROOT url, not to /<topic>: ntfy only
// parses a JSON body when it is posted to the root, and to a topic url it takes
// the body as the message text verbatim. Posting the envelope to /<topic> is
// what made every notification arrive as a wall of raw JSON with the actual
// digest buried inside it. (Documented at
// https://docs.ntfy.sh/publish/#publish-as-json - "To publish as JSON, you must
// PUT/POST to the ntfy root URL, not to the topic URL".)
func ntfyPost(t *store.NtfyTopic, msg map[string]any) (int, error) {
	body, err := json.Marshal(msg)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequest(http.MethodPost, ntfyBaseURL(t), bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// publishNtfyOnce posts the digest to an ntfy topic and reports the outcome.
//
// A non-2xx means the message was rejected (or, before the root-url fix,
// silently stored as plain text), which is worth surfacing: the dashboard
// otherwise shows a delivered notification nobody can read.
func (p *Proxy) publishNtfyOnce(t *store.NtfyTopic, school string, classID int64, digest changeDigest) error {
	title := "Timetable change — " + digest.Title
	if et, eid := t.Target(); et != "" {
		if name := p.elementTargetName(school, et, eid); name != "" {
			title = fmt.Sprintf("Timetable change — %s %s", strings.ToLower(et), name)
		}
	}
	status, err := ntfyPost(t, map[string]any{
		"topic":   t.Topic,
		"title":   title,
		"message": digest.Message(),
		"tags":    []string{"calendar"},
		"click":   publicURL(fmt.Sprintf("/api/timetable/changes?school=%s&classId=%d", school, classID)),
	})
	if err != nil {
		return fmt.Errorf("ntfy %s: %w", t.Topic, err)
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("ntfy %s returned %d", t.Topic, status)
	}
	return nil
}

// deliverTest sends a single test notification to a webhook or ntfy topic and
// reports the result, so admins (or owners) can verify a receiver end-to-end.
// Explicitly NOT retried: a test should fail fast with a clear status.

// testWebhook posts a sample change payload to the webhook's URL exactly as a
// real delivery would (same headers, same HMAC signature) and returns the
// receiver HTTP status (0 with the error if the request itself failed).
func (p *Proxy) testWebhook(h *store.Webhook) (int, error) {
	payload := map[string]any{
		"event":    "test",
		"school":   h.School,
		"classId":  h.ClassID,
		"version":  0,
		"changes":  []store.PeriodRow{},
		"testNote": "This is a test webhook from the untis-proxy admin dashboard.",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	sig := ""
	if h.Secret != "" {
		mac := hmac.New(sha256.New, []byte(h.Secret))
		mac.Write(body)
		sig = "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	req, err := http.NewRequest(http.MethodPost, h.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "untis-proxy/1.0")
	req.Header.Set("X-Untis-Event", "test")
	req.Header.Set("X-Untis-Summary", "test notification")
	if sig != "" {
		req.Header.Set("X-Untis-Signature", sig)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// testNtfy publishes a test notification to the topic (honouring per-topic
// server overrides) and returns the receiver HTTP status. Like publishNtfy it
// posts the JSON envelope to the server root.
//
// The test carries the same fields a real notification does — title, tags,
// click — so the dashboard button proves the whole path renders, not just that
// the socket is open. A test that only sent a bare string would still look fine
// on the server while the notification itself came out as raw JSON.
func (p *Proxy) testNtfy(t *store.NtfyTopic) (int, error) {
	title := "untis-proxy test — " + t.Topic
	et, eid := t.Target()
	if name := p.store.ElementName(t.School, et, eid); et != "" && name != "" {
		title = fmt.Sprintf("untis-proxy test — %s %s", strings.ToLower(et), name)
	}
	return ntfyPost(t, map[string]any{
		"topic":    t.Topic,
		"title":    title,
		"message":  "This is a test notification from the untis-proxy admin dashboard. If you can read this title and these tags, publishing is working.",
		"tags":     []string{"white_check_mark", "calendar"},
		"priority": 3,
		"click":    publicURL("/"),
	})
}

// ntfyBaseURL returns the publish base for a topic: its own server override if
// set, otherwise the global default. Trailing slashes are trimmed so
// "https://ntfy.example.org/" + "/topic" never double-slashes.
func ntfyBaseURL(t *store.NtfyTopic) string {
	if t != nil && t.BaseURL != "" {
		return strings.TrimRight(t.BaseURL, "/")
	}
	return ntfyBase
}

// cleanNtfyBase trims a user-supplied server override before it is stored.
// Two rows differing only by a trailing slash are the same subscription, and
// listing them side by side in the dashboard reads as a duplicate. Publish
// time already trims (see ntfyBaseURL); this keeps the stored value canonical
// so the dashboard shows what is actually being talked to. An empty value
// stays empty and means "use the global default".
func cleanNtfyBase(s string) string {
	return strings.TrimRight(strings.TrimSpace(s), "/")
}

// summarize flattens a set of timetable changes into a short human-readable
// notification line (e.g. "3 lessons changed · 1 new exam").
func summarize(rows []store.PeriodRow) string {
	var added, removed, exams int
	for _, r := range rows {
		switch r.Kind {
		case "ADDED":
			added++
		case "REMOVED":
			removed++
		default:
			exams++
		}
	}
	parts := []string{}
	if added > 0 {
		parts = append(parts, fmt.Sprintf("%d added", added))
	}
	if removed > 0 {
		parts = append(parts, fmt.Sprintf("%d removed", removed))
	}
	if exams > 0 {
		parts = append(parts, fmt.Sprintf("%d changed", exams))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("%d timetable change(s)", len(rows))
	}
	return fmt.Sprintf("%d change(s): %s", len(rows), strings.Join(parts, " · "))
}

// StartPollLoop runs the change-detection poller until done. It performs an
// immediate first pass, then polls every interval. The default school is always
// polled; any school that auto-registers later is picked up on the next tick.
func (p *Proxy) StartPollLoop(school string, interval time.Duration, done <-chan struct{}) {
	if interval <= 0 {
		interval = time.Minute
	}
	p.pollOnce(school)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			p.pollOnce(school)
			if schools, err := p.store.ListSchools(); err == nil {
				for _, sc := range schools {
					if sc.Name != school {
						p.pollOnce(sc.Name)
					}
				}
			}
		}
	}
}

func (p *Proxy) pollOnce(school string) {
	classes, err := p.store.Pool(school)
	if err != nil {
		return
	}
	st := p.stateFor(school)
	for _, c := range classes {
		changed, ferr := p.checkClassErr(school, c.ID)
		st.pollMu.Lock()
		st.pollRuns++
		st.lastPoll = time.Now()
		if changed {
			st.pollChanges++
		}
		if ferr == nil {
			// Only a poll that actually read the timetable counts as a healthy
			// one; checkClassErr swallows the upstream error and reports "no
			// change", which must not be mistaken for a fresh timetable.
			st.lastPollOK = st.lastPoll
		} else {
			st.pollFails++
		}
		st.pollMu.Unlock()
	}
}

// handleTimetableChanges is the pollable diff API. It requires a session whose
// class matches classId; returns pending changes since `since`.
func (p *Proxy) handleTimetableChanges(w http.ResponseWriter, r *http.Request) {
	u := p.sessionUser(r)
	if u == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	school := r.URL.Query().Get("school")
	if school == "" {
		school = p.opts.School
	}
	classID, err := strconv.ParseInt(r.URL.Query().Get("classId"), 10, 64)
	if err != nil || classID <= 0 {
		classID = u.ClassID
	}
	if classID != u.ClassID {
		p.forbidden(w)
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	rows, cur, err := p.store.PendingChanges(school, classID, since)
	if err != nil {
		p.writeJSON(w, map[string]any{"error": "store error"})
		return
	}
	if len(rows) == 0 && cur == since {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	digest := p.buildDigest(school, classID, nil, rows)
	p.writeJSON(w, map[string]any{
		"school":  school,
		"classId": classID,
		"since":   since,
		"current": cur,
		"summary": digest.Summary,
		"digest":  digest,
		"changes": rows,
	})
}

// handleTimetableStream is an SSE endpoint: it emits a change event whenever
// the requester's own class timetable changes. Heartbeats every 30s.
func (p *Proxy) handleTimetableStream(w http.ResponseWriter, r *http.Request) {
	u := p.sessionUser(r)
	if u == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	school := p.opts.School
	if sc := r.URL.Query().Get("school"); sc != "" {
		school = sc
	}
	classID := u.ClassID

	flusher, ok := w.(http.Flusher)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ch, unsub := p.hub.subscribe(school, classID)
	defer unsub()

	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()

	// initial snapshot so the client syncs state on connect
	rows, cur, _ := p.store.PendingChanges(school, classID, 0)
	if data, err := json.Marshal(map[string]any{
		"event": "snapshot", "school": school, "classId": classID,
		"current": cur, "changes": rows,
	}); err == nil {
		fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", data)
		flusher.Flush()
	}

	notifyClosed := r.Context().Done()
	for {
		select {
		case <-notifyClosed:
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case msg := <-ch:
			data, err := json.Marshal(map[string]any{
				"event": "change", "school": msg.School,
				"classId": msg.ClassID, "version": msg.Version, "changes": msg.Changes,
			})
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: change\ndata: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
