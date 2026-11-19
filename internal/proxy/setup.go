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
		if err := p.store.SetSetupCompleted(false); err != nil {
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
	msg := "Dashboard and administration"
	linkText := "Open dashboard"
	if needed {
		title = "UntisPlus Proxy — Setup"
		msg = "Initial setup required. Sign in to claim admin access and complete onboarding (trusted reverse-proxy IPs, roles, ntfy, users)."
		linkText = "Continue to setup/login"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>` + title + `</title>
<style>body{font-family:system-ui,sans-serif;background:#111216;color:#e6e6e6;margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center}
.card{max-width:560px;background:#1c1e26;border:1px solid #2a2d37;border-radius:12px;padding:1.5rem}
h1{font-size:1.2rem;margin:0 0 .5rem}p{color:#b5bccb;line-height:1.5;margin:.4rem 0 1rem}
a.btn{display:inline-block;background:#3b82f6;color:#fff;text-decoration:none;padding:.6rem .9rem;border-radius:8px;font-weight:600}
.small{font-size:.85rem;color:#9aa0ae;margin-top:1rem}</style></head><body><div class="card"><h1>` + title + `</h1><p>` + msg + `</p><a class="btn" href="/admin">` + linkText + `</a><div class="small">If running behind a reverse proxy, complete trusted-proxy IP setup during onboarding so the server sees real client IPs.</div></div></body></html>`))
}
