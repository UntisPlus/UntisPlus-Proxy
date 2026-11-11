package proxy

// Admin action audit trail. Mutating dashboard endpoints call p.audit with the
// authenticated actor, so the trail cannot be forged by a request body. Detail
// must never include a credential: secrets are recorded as "rotated" or omitted.

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// audit appends one admin action. A store failure is logged but never fails the
// action that was already performed: the mutation is the user's intent, and
// losing the audit row is worse as a 500 than as a log line.
func (p *Proxy) audit(r *http.Request, school, action, target, detail string) {
	actor := ""
	if u := p.sessionUser(r); u != nil {
		actor = u.Username
	}
	if err := p.store.LogAdminAudit(actor, school, action, target, detail); err != nil {
		log.Printf("[audit] %s %s by %s: %v", action, target, actor, err)
	}
}

// tokenAuditTarget names a calendar subscription by its element rather than by
// its token value. The token is a live credential, so it must never be written
// to the audit trail.
func tokenAuditTarget(elType string, elID int64) string {
	return fmt.Sprintf("%s %d", elType, elID)
}

// tokenAuditKey resolves a token value to an element name for the audit trail.
func (p *Proxy) tokenAuditKey(token string) string {
	tok, err := p.store.ClassTokenByToken(token)
	if err != nil || tok == nil {
		return "unknown"
	}
	return tok.School + " " + tokenAuditTarget(tok.ElementType, tok.ElementID)
}

// auditURLHost reduces a destination URL to scheme and host for the audit trail.
// The path of a webhook URL is often itself a credential (Slack, Teams and most
// chat bridges embed a secret in it), so the full URL must never be recorded.
func auditURLHost(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "invalid"
	}
	return u.Scheme + "://" + u.Host
}

// adminAudit serves the audit trail for the dashboard.
//
//	GET /admin/audit?school=&limit=
func (p *Proxy) adminAudit(w http.ResponseWriter, r *http.Request) {
	school := r.URL.Query().Get("school")
	limit := 100
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = n
	}
	entries, err := p.store.AdminAuditEntries(school, limit)
	if err != nil {
		p.writeJSON(w, map[string]any{"error": "store error"})
		return
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]any{
			"id": e.ID, "at": e.At.Unix(), "actor": e.Actor, "school": e.School,
			"action": e.Action, "target": e.Target, "detail": e.Detail,
		})
	}
	p.writeJSON(w, map[string]any{"entries": out})
}
