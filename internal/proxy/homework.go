package proxy

// Proxy-local homework completion flags.
//
// Untis' own `homeWorks[].completed` is the teacher's decision about the whole
// class, so it is passed through untouched and must never be written. The
// student's own answer lives here instead, keyed by (school, viewer, homework
// id), and rides along with the homework wherever upstream already returns it.

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"time"
)

// readBody reads a request body, treating a read failure as empty so the
// caller reports a normal bad-request error rather than panicking.
func readBody(r *http.Request) []byte {
	if r.Body == nil {
		return nil
	}
	body, _ := io.ReadAll(r.Body)
	return body
}

// sortHomeworkFlags orders flags by homework id so responses are stable.
func sortHomeworkFlags(flags []homeworkFlags) {
	sort.Slice(flags, func(i, j int) bool { return flags[i].HomeworkID < flags[j].HomeworkID })
}

// decorateHomeWorks adds the viewer's done flags onto every homeWorks[] entry it
// can find in `result`, and reports how many it marked.
//
// getHomeWork2017 returns {homeWorks, lessonsById}; getPeriodData2017 returns a
// single lesson, which may carry its own homeWorks[]. Both are decorated, so the
// app reads the same flag from whichever path it happens to use.
//
// This must only ever be called with an identity the proxy established, and only
// on a response that is genuinely the viewer's own data — see
// handleJSONRPCIntern, where a class-scoped editor request has had its
// credentials swapped for a boosted teacher's, and is therefore not decorated.
func (p *Proxy) decorateHomeWorks(result jsonObject, done map[int64]time.Time) {
	// The list form, and also the single-lesson form: a period's detail
	// response nests its homework under `homeWorks` too, so one walk covers both.
	p.markHomeWorkList(result, done)
}

// markHomeWorkList walks a homeWorks[] array in place.
//
// The slice is deliberately not reassigned back onto the container. jsonArray
// drops anything that is not an object, so writing its result back would
// silently turn `[1,2,3]` into `[]`. The objects it does return are the very same
// maps the decoder produced, so mutating them is enough for the caller to see
// the change when it re-encodes.
func (p *Proxy) markHomeWorkList(container jsonObject, done map[int64]time.Time) {
	for _, hw := range jsonArray(container["homeWorks"]) {
		id, ok := jsonID(hw["id"])
		if !ok {
			// No usable id means the flag could not be keyed or matched, so the
			// entry is left exactly as upstream sent it.
			continue
		}
		if at, isDone := done[id]; isDone {
			hw["done"] = true
			hw["doneAt"] = at.UTC().Format(time.RFC3339)
			continue
		}
		// An explicit false plus a null timestamp is easier for a client to
		// branch on than a missing field, and it documents that the proxy
		// considered this assignment rather than never having seen it.
		hw["done"] = false
		hw["doneAt"] = nil
	}
}

// enrichHomeWorkResponse decorates a homework-bearing JSON-RPC response with
// the viewer's flags. It returns the original bytes untouched if the viewer is
// unknown or the response cannot be read, so a missing enrichment degrades to
// plain upstream data rather than to an error.
func (p *Proxy) enrichHomeWorkResponse(raw []byte, school, viewer string) []byte {
	if viewer == "" || school == "" {
		return raw
	}
	done, err := p.store.HomeworkDone(school, viewer)
	if err != nil {
		enrichmentFailed("homework "+viewer, err)
		return raw
	}
	return enrichJSON(raw, func(result jsonObject) {
		p.decorateHomeWorks(result, done)
	})
}

// homeworkFlags are the flags the proxy reports for one piece of homework, used
// by the read endpoint that does not depend on any upstream response shape.
type homeworkFlags struct {
	HomeworkID int64   `json:"homeworkId"`
	Done       bool    `json:"done"`
	DoneAt     *string `json:"doneAt"`
}

// handleHomeworkFlags serves the viewer's own homework flags as plain JSON, so a
// client can sync a checkbox list without having to re-fetch and re-parse a
// full Untis homework response.
//
// GET /api/homework/flags
//
// Session-authenticated and scoped to the session user. There is no username
// parameter, by design: the only identity honoured is the one the session
// proves, so there is nothing for a caller to tamper with.
func (p *Proxy) handleHomeworkFlags(w http.ResponseWriter, r *http.Request) {
	user := p.sessionUser(r)
	if user == nil {
		writeJSONError(w, http.StatusUnauthorized, "not logged in")
		return
	}
	school := r.URL.Query().Get("school")
	if school == "" {
		school = p.opts.School
	}
	done, err := p.store.HomeworkDone(school, user.Username)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store error")
		return
	}
	flags := make([]homeworkFlags, 0, len(done))
	for id, at := range done {
		stamp := at.UTC().Format(time.RFC3339)
		flags = append(flags, homeworkFlags{HomeworkID: id, Done: true, DoneAt: &stamp})
	}
	// Sorted by id so the response is stable, which keeps client diffing and
	// test assertions from depending on map iteration order.
	sortHomeworkFlags(flags)
	p.writeJSON(w, map[string]any{"school": school, "flags": flags})
}

// writeJSONError replies with a JSON error body and a real HTTP status, so a
// client can tell a rejected write from a successful one by status alone rather
// than having to inspect the payload.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, _ := json.Marshal(map[string]string{"error": msg})
	_, _ = w.Write(body)
}

// handleHomeworkDone sets or clears the session user's flag for one assignment.
//
// POST /api/homework/done   {"homeworkId":12345,"done":true}
//
// The username is taken from the session and from nowhere else. A `username` in
// the body is ignored rather than rejected, so a client that sends one out of
// habit still behaves correctly and can never redirect the write at somebody
// else.
func (p *Proxy) handleHomeworkDone(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	user := p.sessionUser(r)
	if user == nil {
		writeJSONError(w, http.StatusUnauthorized, "not logged in")
		return
	}
	school := r.URL.Query().Get("school")
	if school == "" {
		school = p.opts.School
	}
	var req struct {
		HomeworkID int64 `json:"homeworkId"`
		Done       *bool `json:"done"`
		// Accepted and ignored on purpose; see the doc comment.
		Username string `json:"username"`
	}
	if !decodeJSON(readBody(r), &req) {
		writeJSONError(w, http.StatusBadRequest, "bad request")
		return
	}
	if req.HomeworkID <= 0 || req.Done == nil {
		writeJSONError(w, http.StatusBadRequest, "homeworkId and done are required")
		return
	}

	var doneAt *string
	if *req.Done {
		at, err := p.store.SetHomeworkDone(school, user.Username, req.HomeworkID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "store error")
			return
		}
		stamp := at.UTC().Format(time.RFC3339)
		doneAt = &stamp
	} else if err := p.store.ClearHomeworkDone(school, user.Username, req.HomeworkID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store error")
		return
	}
	p.writeJSON(w, map[string]any{
		"homeworkId": req.HomeworkID,
		"done":       *req.Done,
		"doneAt":     doneAt,
	})
}
