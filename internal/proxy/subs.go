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
// Webhooks and ntfy topics come in two shapes: school-wide (classId 0) and
// per-class. Users with a session may manage subscriptions that touch classes
// they are entitled to: their own class always, any pooled class when they hold
// the editor, boosted or admin flag, and school-wide ones likewise (classId 0
// is only mutatable by editor/boosted/admin). Everything admin can do is also
// in /admin/webhooks and /admin/ntfy.

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
			if !canAll && h.ClassID != u.ClassID {
				continue
			}
			out = append(out, map[string]any{
				"id": h.ID, "school": h.School, "classId": h.ClassID, "url": h.URL,
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
			ClassID int64  `json:"classId"`
			URL     string `json:"url"`
			Secret  string `json:"secret"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			p.writeJSON(w, map[string]any{"error": "bad request"})
			return
		}
		if req.URL == "" {
			p.writeJSON(w, map[string]any{"error": "url required"})
			return
		}
		if !p.canSubscribeClass(school, u, req.ClassID, canAll) {
			p.forbidden(w)
			return
		}
		id, err := p.store.AddWebhook(&store.Webhook{
			School: school, ClassID: req.ClassID, URL: req.URL,
			Secret: req.Secret, Enabled: true, CreatedBy: u.Username,
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
			et, eid := t.Target()
			if !canAll {
				switch et {
				case "CLASS":
					if eid != u.ClassID {
						continue
					}
				case "STUDENT":
					if eid != u.PersonID {
						continue
					}
				default:
					continue // school-wide and teacher/room/subject topics are staff-relevant
				}
			}
			out = append(out, map[string]any{
				"id": t.ID, "school": t.School, "elementType": et, "elementId": eid,
				"classId": t.ClassID, "topic": t.Topic, "baseUrl": t.BaseURL,
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
			ClassID int64  `json:"classId"`
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
		if !p.canSubscribeClass(school, u, req.ClassID, canAll) {
			p.forbidden(w)
			return
		}
		id, err := p.store.AddNtfyTopic(&store.NtfyTopic{
			School: school, ClassID: req.ClassID, Topic: req.Topic,
			BaseURL: cleanNtfyBase(req.BaseURL), Enabled: true, CreatedBy: u.Username,
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
