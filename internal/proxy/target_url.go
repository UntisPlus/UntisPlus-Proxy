package proxy

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// allowPrivateTargets relaxes the address check in validateTargetURL. It is off
// by default: webhook and ntfy targets are chosen by the user creating the
// subscription and are then fetched by the proxy, so a loopback or RFC1918 host
// turns a subscription into a probe of the proxy's own network (SSRF). An
// operator whose push server lives on the internal network can turn it back on
// with -allow-private-targets / UNTIS_ALLOW_PRIVATE_TARGETS.
var allowPrivateTargets = false

// SetAllowPrivateTargets sets whether user-supplied notification targets may
// point at private, loopback or link-local addresses.
func SetAllowPrivateTargets(v bool) { allowPrivateTargets = v }

// validateTargetURL checks a user-supplied webhook URL or ntfy server override
// before it is stored. Only http and https are accepted, and the host must not
// be localhost or resolve solely to a private, loopback, link-local, multicast
// or unspecified address.
func validateTargetURL(raw string, allowPrivate bool) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("url required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url must start with http:// or https://")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("url must include a host")
	}
	if allowPrivate {
		return nil
	}
	if isBlockedTarget(host) {
		return fmt.Errorf("target host is not allowed")
	}
	return nil
}

// isBlockedTarget reports whether host names or resolves to a local address. A
// literal IP is checked directly; a name is resolved and blocked when every
// address it maps to is private. A name that cannot be resolved is allowed
// through — the delivery attempt will fail on its own, and treating a transient
// DNS error as a permanent rejection would break a valid subscription.
func isBlockedTarget(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return !isPublicIP(ip)
	}
	addrs, err := net.LookupIP(h)
	if err != nil || len(addrs) == 0 {
		return false
	}
	for _, a := range addrs {
		if isPublicIP(a) {
			return false
		}
	}
	return true
}

// isPublicIP reports whether ip is a routable public address rather than one of
// the ranges a proxy must not be induced to reach.
func isPublicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	// isPublicIP also refuses IPv4-mapped IPv6 forms of the ranges above, which
	// net.IP.IsPrivate and friends already cover for the mapped address.
	return true
}

// outboundHTTPClient is the client used for webhook and ntfy delivery. Its
// redirect policy re-validates each hop with the same rules as creation, so a
// public URL that answers 302 to a private one cannot be used to slip past the
// stored-target check.
func outboundHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("stopped after 5 redirects")
			}
			return validateTargetURL(req.URL.String(), allowPrivateTargets)
		},
	}
}
