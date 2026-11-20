package proxy

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strings"
)

// maybeClaimSetupAdmin grants admin to the first successfully authenticated
// user when initial setup is pending and no admin exists yet. This enables the
// "log in, then onboarding" first-run flow without requiring -admin bootstrap.
func (p *Proxy) maybeClaimSetupAdmin(username string) {
	username = strings.ToLower(strings.TrimSpace(username))
	if username == "" || username == "#anonymous#" {
		return
	}
	needed, err := p.store.SetupNeeded()
	if err != nil || !needed {
		return
	}
	hasAdmin, err := p.store.HasAnyAdmin()
	if err != nil || hasAdmin {
		return
	}
	if err := p.store.SetAdmin(username, true); err != nil {
		log.Printf("[setup] claim admin for %q: %v", username, err)
		return
	}
	_ = p.store.SetSetupFreshClaim(true)
	log.Printf("[setup] claimed initial admin: %s", username)
}

// setupStatus describes first-run/setup state and client-IP diagnostics useful
// for configuring trusted reverse proxies during onboarding.
type setupStatus struct {
	SetupNeeded    bool   `json:"setupNeeded"`
	SetupCompleted bool   `json:"setupCompleted"`
	HasAdmin       bool   `json:"hasAdmin"`
	CanClaimAdmin  bool   `json:"canClaimAdmin"`
	TrustedProxies string `json:"trustedProxies"`
	ClientIP       struct {
		RemoteAddr      string `json:"remoteAddr"`
		PeerIP          string `json:"peerIP"`
		ResolvedIP      string `json:"resolvedIP"`
		XForwardedFor   string `json:"xForwardedFor,omitempty"`
		XRealIP         string `json:"xRealIP,omitempty"`
		XForwardedProto string `json:"xForwardedProto,omitempty"`
		PeerTrusted     bool   `json:"peerTrusted"`
		UsingForwarded  bool   `json:"usingForwarded"`
	} `json:"clientIP"`
}

func (p *Proxy) buildSetupStatus(r *http.Request) setupStatus {
	var st setupStatus
	st.SetupCompleted, _ = p.store.SetupCompleted()
	st.HasAdmin, _ = p.store.HasAnyAdmin()
	claimNeeded, _ := p.store.SetupNeeded()
	onboarding, _ := p.store.SetupOnboarding()
	st.SetupNeeded = onboarding
	st.CanClaimAdmin = claimNeeded
	st.TrustedProxies = p.TrustedProxies()
	if r != nil {
		st.ClientIP.RemoteAddr = r.RemoteAddr
		peerIP := parseRemoteIP(r.RemoteAddr)
		st.ClientIP.PeerIP = ipString(peerIP, r.RemoteAddr)
		st.ClientIP.ResolvedIP = p.realClientIP(r)
		st.ClientIP.XForwardedFor = r.Header.Get("X-Forwarded-For")
		st.ClientIP.XRealIP = r.Header.Get("X-Real-IP")
		st.ClientIP.XForwardedProto = r.Header.Get("X-Forwarded-Proto")
		st.ClientIP.PeerTrusted = peerIP != nil && p.isTrustedProxyIP(peerIP)
		st.ClientIP.UsingForwarded = st.ClientIP.PeerTrusted && st.ClientIP.ResolvedIP != st.ClientIP.PeerIP
	}
	return st
}

// handleSetupPublic exposes minimal setup/client-IP diagnostics without auth.
// It is available while initial onboarding/claim is pending so operators can
// verify reverse-proxy real IP configuration before/during first admin claim.
func (p *Proxy) handleSetupPublic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	onboarding, err := p.store.SetupOnboarding()
	claimNeeded, _ := p.store.SetupNeeded()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Safe to expose: reflects caller's own request and setup state only.
	if !onboarding && !claimNeeded {
		p.writeJSON(w, map[string]any{"setupNeeded": false})
		return
	}
	p.writeJSON(w, p.buildSetupStatus(r))
}

// handleAdminSetup serves setup/onboarding state and actions for admins.
func (p *Proxy) handleAdminSetup(w http.ResponseWriter, r *http.Request, parts []string) {
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		p.writeJSON(w, p.buildSetupStatus(r))
	case len(parts) == 2 && parts[1] == "trusted-proxies" && r.Method == http.MethodGet:
		p.writeJSON(w, map[string]any{"trustedProxies": p.TrustedProxies()})
	case len(parts) == 2 && parts[1] == "trusted-proxies" && (r.Method == http.MethodPost || r.Method == http.MethodPut):
		var req struct {
			TrustedProxies string `json:"trustedProxies"`
		}
		if err := decodeJSONBody(r, &req); err != nil {
			badRequest(w, "invalid json")
			return
		}
		if err := p.SetTrustedProxies(req.TrustedProxies); err != nil {
			badRequest(w, err.Error())
			return
		}
		if err := p.store.SetSetting(storeSettingTrustedProxies(), p.TrustedProxies()); err != nil {
			http.Error(w, "failed to persist trusted proxies", http.StatusInternalServerError)
			return
		}
		p.writeJSON(w, map[string]any{"ok": true, "trustedProxies": p.TrustedProxies(), "clientIP": p.buildSetupStatus(r).ClientIP})
	case len(parts) == 2 && parts[1] == "complete" && r.Method == http.MethodPost:
		if err := p.store.SetSetupCompleted(true); err != nil {
			http.Error(w, "failed to complete setup", http.StatusInternalServerError)
			return
		}
		p.writeJSON(w, map[string]any{"ok": true, "setupCompleted": true})
	case len(parts) == 2 && parts[1] == "reset" && r.Method == http.MethodPost:
		// Allow re-opening onboarding for admins (does not remove admins).
		// Mark fresh-claim so normalizeSetupState does not auto-complete an
		// existing-admin instance when setup_completed is explicitly reopened.
		if err := p.store.SetSetupCompleted(false); err != nil {
			http.Error(w, "failed to reset setup", http.StatusInternalServerError)
			return
		}
		if err := p.store.SetSetupFreshClaim(true); err != nil {
			http.Error(w, "failed to reset setup", http.StatusInternalServerError)
			return
		}
		p.writeJSON(w, map[string]any{"ok": true, "setupCompleted": false})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// storeSettingTrustedProxies avoids importing store in method signature-heavy
// call sites beyond what we already have; value must match store.SettingTrustedProxies.
func storeSettingTrustedProxies() string { return "trusted_proxies" }

// decodeJSONBody reads and decodes a JSON request body.
func decodeJSONBody(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	return dec.Decode(dst)
}

func badRequest(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": msg})
}

var (
	_ = net.IP(nil)
	_ = http.MethodGet
)

// handleRoot serves a minimal landing page. During initial setup it points
// operators to the dashboard/login to claim admin and complete onboarding.
func (p *Proxy) handleRoot(w http.ResponseWriter, r *http.Request) {
	needed, _ := p.store.SetupNeeded()
	title := "UntisPlus Proxy"
	heading := "UntisPlus Proxy"
	msg := "Administration dashboard, access control, notifications, and operations for this proxy instance."
	linkText := "Open dashboard"
	badge := "Ready"
	badgeStyle := "background:rgba(47,158,99,.16);border:1px solid rgba(47,158,99,.35);color:#8fe0b3;"
	if needed {
		title = "UntisPlus Proxy — Setup"
		heading = "Initial setup required"
		msg = "Sign in to claim admin access and complete onboarding. You'll be guided to configure trusted reverse-proxy client IPs, roles/access, ntfy/webhook alerts, and users/admins."
		linkText = "Continue to setup & login"
		badge = "Setup pending"
		badgeStyle = "background:rgba(217,119,6,.16);border:1px solid rgba(217,119,6,.35);color:#ffd08a;"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>` + title + `</title>
<style>
:root{color-scheme:dark;--bg:#0f1115;--panel:#171a21;--border:#2a2f3a;--text:#e8edf7;--muted:#9aa6b8;--accent:#4f8cff}
*{box-sizing:border-box}body{margin:0;min-height:100vh;display:grid;place-items:center;padding:1rem;background:radial-gradient(1000px 500px at 10% -10%,rgba(79,140,255,.18),transparent),var(--bg);color:var(--text);font-family:Inter,ui-sans-serif,system-ui,-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif}
.card{width:min(640px,100%);background:var(--panel);border:1px solid var(--border);border-radius:16px;padding:1.4rem 1.4rem 1.2rem;box-shadow:0 20px 50px rgba(0,0,0,.35)}
.badge{display:inline-flex;align-items:center;padding:.2rem .55rem;border-radius:999px;font-size:.75rem;margin-bottom:.65rem}
h1{font-size:1.35rem;margin:0 0 .4rem}p{color:var(--muted);line-height:1.55;margin:.3rem 0 .95rem}
.actions{display:flex;gap:.5rem;flex-wrap:wrap;align-items:center}
a.btn{display:inline-block;background:var(--accent);color:#fff;text-decoration:none;padding:.65rem .9rem;border-radius:10px;font-weight:600}
a.btn.secondary{background:#2a3140;border:1px solid #394150;color:var(--text)}
.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(180px,1fr));gap:.6rem;margin-top:1rem}
.item{border:1px solid var(--border);border-radius:10px;padding:.65rem .7rem;background:rgba(255,255,255,.02)}
.item b{display:block;font-size:.86rem;margin-bottom:.15rem;color:var(--text)}.item span{font-size:.8rem;color:var(--muted);line-height:1.4}
.small{font-size:.82rem;color:var(--muted);margin-top:1rem;line-height:1.45}
</style></head><body><div class="card"><div class="badge" style="` + badgeStyle + `">` + badge + `</div><h1>` + heading + `</h1><p>` + msg + `</p><div class="actions"><a class="btn" href="/admin">` + linkText + `</a></div>
<div class="grid"><div class="item"><b>Trusted proxy / real IP</b><span>Configure trusted reverse proxies so the server sees real client IPs for rate limiting and logs.</span></div><div class="item"><b>Access control</b><span>Define capability groups, role defaults, mappings, and grants for precise permissions.</span></div><div class="item"><b>Notifications</b><span>Set up ntfy topics, webhooks, and outage alerts for operations and changes.</span></div><div class="item"><b>Users & schools</b><span>Manage admins, user permissions, schools, anonymous access, tokens, and events.</span></div></div>
<div class="small">If running behind a reverse proxy/load balancer, complete trusted-proxy setup during onboarding to ensure forwarded client IP headers are handled safely.</div></div></body></html>`))
}
