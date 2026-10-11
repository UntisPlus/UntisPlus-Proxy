package proxy

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"untis-proxy/internal/store"
)

// Self-service subscription API.
//
// Webhooks and ntfy topics target one element: a class (their own, any pooled
// one when privileged), a personal/student timetable, a teacher, a room, a
// subject, or the school as a whole. The target vocabulary and the visibility
// rules are exactly the calendar mint's, so what a user may subscribe to here
// is what they can already see in the app. Discovery is via
// GET /api/notifications/options. GET lists only the caller's own
// subscriptions. Everything admin can do is also in /admin/webhooks and
// /admin/ntfy.

// notifTargetReq is the element a notification subscription points at, using
// the same keys as the calendar mint. At most one target may be set; none means
// the school-wide target (privileged users only).
type notifTargetReq struct {
	ClassID   int64 `json:"classId"`
	PersonID  int64 `json:"personId"`
	Personal  bool  `json:"personal"`
	TeacherID int64 `json:"teacherId"`
	RoomID    int64 `json:"roomId"`
	SubjectID int64 `json:"subjectId"`
}

// resolveNotifTarget maps a notification request onto the (elementType,
// elementID) pair the change poller matches on, applying the same visibility
// rules as the calendar mint. classID is the legacy class column, set only for
// a class target so old readers keep working. ok is false when the request
// names more than one target or the caller may not watch the one named.
func (p *Proxy) resolveNotifTarget(school string, u *store.User, canAll bool, req notifTargetReq) (et string, eid, classID int64, ok bool) {
	set := 0
	for _, on := range []bool{req.ClassID > 0, req.Personal || req.PersonID > 0,
		req.TeacherID > 0, req.RoomID > 0, req.SubjectID > 0} {
		if on {
			set++
		}
	}
	if set > 1 {
		return "", 0, 0, false
	}
	switch {
	case req.Personal || req.PersonID > 0:
		pid := req.PersonID
		if pid == 0 {
			pid = u.PersonID
		}
		if pid <= 0 || pid != u.PersonID {
			return "", 0, 0, false
		}
		return "STUDENT", pid, 0, true
	case req.ClassID > 0:
		// The synthetic Technik aggregate is not a polled class, so a
		// notification for it could never fire; reject it rather than store a
		// subscription that never delivers.
		if req.ClassID == technikClassID || !p.canSubscribeClass(school, u, req.ClassID, canAll) {
			return "", 0, 0, false
		}
		return "CLASS", req.ClassID, req.ClassID, true
	case req.TeacherID > 0:
		return "TEACHER", req.TeacherID, 0, p.canMintElement(u, "TEACHER")
	case req.RoomID > 0:
		return "ROOM", req.RoomID, 0, p.canMintElement(u, "ROOM")
	case req.SubjectID > 0:
		return "SUBJECT", req.SubjectID, 0, p.canMintElement(u, "SUBJECT")
	default:
		return "", 0, 0, canAll
	}
}

func (p *Proxy) handleSubsWebhooks(w http.ResponseWriter, r *http.Request) {
	u := p.sessionUser(r)
	if u == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	school := r.URL.Query().Get("school")
	if school == "" {
		school = p.opts.School
	}

	canAll := p.isEditor(u.Username) || p.isBoosted(u.Username) || p.isAdmin(u.Username)

	if r.Method == http.MethodGet {
		hooks, err := p.store.ListWebhooks(school)
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		out := make([]map[string]any, 0)
		for _, h := range hooks {
			if h.CreatedBy != u.Username {
				continue
			}
			et, eid := h.Target()
			out = append(out, map[string]any{
				"id": h.ID, "school": h.School, "elementType": et, "elementId": eid,
				"name":    p.elementTargetName(h.School, et, eid),
				"classId": h.ClassID, "url": h.URL, "createdBy": h.CreatedBy,
			})
		}
		p.writeJSON(w, map[string]any{"webhooks": out})
		return
	}

	if r.Method == http.MethodPost {
		rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/webhooks"), "/")
		if rest != "" {
			// POST /api/webhooks/{id}/test
			parts := strings.Split(rest, "/")
			id, err := strconv.ParseInt(parts[0], 10, 64)
			if err != nil || id <= 0 || len(parts) < 2 || parts[1] != "test" {
				p.writeJSON(w, map[string]any{"error": "not found"})
				return
			}
			hooks, err := p.store.ListWebhooks(school)
			if err != nil {
				p.writeJSON(w, map[string]any{"error": "store error"})
				return
			}
			for _, h := range hooks {
				if h.ID != id {
					continue
				}
				if h.CreatedBy != u.Username && !canAll {
					p.forbidden(w)
					return
				}
				status, err := p.testWebhook(h)
				if err != nil {
					p.writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
					return
				}
				p.writeJSON(w, map[string]any{"ok": true, "status": status})
				return
			}
			p.writeJSON(w, map[string]any{"error": "not found"})
			return
		}

		var req struct {
			notifTargetReq
			URL    string `json:"url"`
			Secret string `json:"secret"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			p.writeJSON(w, map[string]any{"error": "bad request"})
			return
		}
		if err := validateTargetURL(req.URL, allowPrivateTargets); err != nil {
			p.writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		et, eid, classID, ok := p.resolveNotifTarget(school, u, canAll, req.notifTargetReq)
		if !ok {
			p.forbidden(w)
			return
		}
		id, err := p.store.AddWebhook(&store.Webhook{
			School: school, ElementType: et, ElementID: eid, ClassID: classID,
			URL: req.URL, Secret: req.Secret, Enabled: true, CreatedBy: u.Username,
		})
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		p.writeJSON(w, map[string]any{"id": id})
		return
	}

	if r.Method == http.MethodDelete {
		rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/webhooks"), "/")
		id, err := strconv.ParseInt(rest, 10, 64)
		if err != nil || id <= 0 {
			p.writeJSON(w, map[string]any{"error": "bad id"})
			return
		}
		hooks, err := p.store.ListWebhooks(school)
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		for _, h := range hooks {
			if h.ID != id {
				continue
			}
			if h.CreatedBy != u.Username && !canAll {
				p.forbidden(w)
				return
			}
			if _, err := p.store.DeleteWebhook(id); err != nil {
				p.writeJSON(w, map[string]any{"error": "store error"})
				return
			}
			p.writeJSON(w, map[string]any{"deleted": true})
			return
		}
		p.writeJSON(w, map[string]any{"error": "not found"})
		return
	}

	p.forbidden(w)
}

func (p *Proxy) handleSubsNtfy(w http.ResponseWriter, r *http.Request) {
	u := p.sessionUser(r)
	if u == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	school := r.URL.Query().Get("school")
	if school == "" {
		school = p.opts.School
	}

	canAll := p.isEditor(u.Username) || p.isBoosted(u.Username) || p.isAdmin(u.Username)

	if r.Method == http.MethodGet {
		topics, err := p.store.ListNtfyTopics(school)
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		out := make([]map[string]any, 0)
		for _, t := range topics {
			if t.CreatedBy != u.Username {
				continue
			}
			et, eid := t.Target()
			out = append(out, map[string]any{
				"id": t.ID, "school": t.School, "elementType": et, "elementId": eid,
				"name":    p.elementTargetName(t.School, et, eid),
				"classId": t.ClassID, "topic": t.Topic, "baseUrl": t.BaseURL,
				"createdBy": t.CreatedBy,
			})
		}
		p.writeJSON(w, map[string]any{"topics": out})
		return
	}

	if r.Method == http.MethodDelete {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/ntfy/"), "/")
		id, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil || id <= 0 {
			p.writeJSON(w, map[string]any{"error": "bad id"})
			return
		}
		topics, err := p.store.ListNtfyTopics(school)
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		for _, t := range topics {
			if t.ID != id {
				continue
			}
			if t.CreatedBy != u.Username && !canAll {
				p.forbidden(w)
				return
			}
			if _, err := p.store.DeleteNtfyTopic(id); err != nil {
				p.writeJSON(w, map[string]any{"error": "store error"})
				return
			}
			p.writeJSON(w, map[string]any{"deleted": true})
			return
		}
		p.writeJSON(w, map[string]any{"error": "not found"})
		return
	}

	if r.Method == http.MethodPost {
		rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/ntfy"), "/")
		if rest != "" {
			// POST /api/ntfy/{id}/test
			parts := strings.Split(rest, "/")
			id, err := strconv.ParseInt(parts[0], 10, 64)
			if err != nil || id <= 0 || len(parts) < 2 || parts[1] != "test" {
				p.writeJSON(w, map[string]any{"error": "not found"})
				return
			}
			topics, err := p.store.ListNtfyTopics(school)
			if err != nil {
				p.writeJSON(w, map[string]any{"error": "store error"})
				return
			}
			for _, t := range topics {
				if t.ID != id {
					continue
				}
				if t.CreatedBy != u.Username && !canAll {
					p.forbidden(w)
					return
				}
				status, err := p.testNtfy(t)
				if err != nil {
					p.writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
					return
				}
				p.writeJSON(w, map[string]any{
					"ok": true, "status": status,
					"topic": t.Topic, "server": ntfyBaseURL(t),
				})
				return
			}
			p.writeJSON(w, map[string]any{"error": "not found"})
			return
		}

		var req struct {
			notifTargetReq
			Topic   string `json:"topic"`
			BaseURL string `json:"baseUrl"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			p.writeJSON(w, map[string]any{"error": "bad request"})
			return
		}
		if req.Topic == "" {
			p.writeJSON(w, map[string]any{"error": "topic required"})
			return
		}
		baseURL := cleanNtfyBase(req.BaseURL)
		if baseURL != "" {
			if err := validateTargetURL(baseURL, allowPrivateTargets); err != nil {
				p.writeJSON(w, map[string]any{"error": err.Error()})
				return
			}
		}
		et, eid, classID, ok := p.resolveNotifTarget(school, u, canAll, req.notifTargetReq)
		if !ok {
			p.forbidden(w)
			return
		}
		id, err := p.store.AddNtfyTopic(&store.NtfyTopic{
			School: school, ElementType: et, ElementID: eid, ClassID: classID,
			Topic: req.Topic, BaseURL: baseURL, Enabled: true, CreatedBy: u.Username,
		})
		if err != nil {
			p.writeJSON(w, map[string]any{"error": "store error"})
			return
		}
		p.writeJSON(w, map[string]any{"id": id})
		return
	}

	p.forbidden(w)
}

// canSubscribeClass reports whether `u` may create a subscription scoped to
// classID: their own class always, any pooled class when they hold a privileged
// flag, classId 0 (school-wide) only for privileged users.
func (p *Proxy) canSubscribeClass(school string, u *store.User, classID int64, privileged bool) bool {
	if classID == 0 {
		return privileged
	}
	if classID == u.ClassID {
		return true
	}
	if !privileged {
		return false
	}
	ok, _ := p.store.PoolContains(school, classID)
	return ok
}
