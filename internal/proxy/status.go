package proxy

import (
	"net/http"
	"os"
	"time"
)

var startedAt = time.Now()

// version is the reported build version; set at build time via
// -ldflags "-X untis-proxy/internal/proxy.version=<...>". Defaults to "dev".
var version = "dev"

// handleStatus reports the process status: uptime, mode (dev/beta/prod) and
// version. It is self-measured only; external uptime monitoring is out of scope.
func (p *Proxy) handleStatus(w http.ResponseWriter, r *http.Request) {
	mode := os.Getenv("UNTIS_ENV")
	if mode == "" {
		mode = "dev"
	}
	v := os.Getenv("UNTIS_VERSION")
	if v == "" {
		v = version
	}
	p.writeJSON(w, map[string]any{
		"status":     "ok",
		"mode":       mode,
		"version":    v,
		"uptime_sec": int64(time.Since(startedAt).Seconds()),
	})
}

// handleMe reports the signed-in requester's effective permission level. It
// requires an authenticated session (JSESSIONID cookie).
func (p *Proxy) handleMe(w http.ResponseWriter, r *http.Request) {
	user := p.sessionUser(r)
	if user == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		p.writeJSON(w, map[string]any{"error": "not logged in"})
		return
	}
	boosted := p.isBoosted(user.Username)
	editor := p.isEditor(user.Username)
	recon, _ := p.store.ReconAccess(user.Username, "TEACHER")

	level := "basic"
	switch {
	case editor:
		// The editor flag implies boosted.
		level = "boosted+editor"
	case boosted:
		level = "boosted"
	case recon:
		level = "recon"
	}

	p.writeJSON(w, map[string]any{
		"username": user.Username,
		"level":    level,
		"permissions": map[string]any{
			"recon":   recon,
			"boosted": boosted,
			"editor":  editor,
		},
	})
}
