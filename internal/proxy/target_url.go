package proxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
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
// be localhost or resolve to any private, loopback, link-local, multicast
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
	if u.User != nil {
		return fmt.Errorf("url must not include credentials")
	}
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

// isBlockedTarget rejects mixed public/private answers as well as private-only
// answers. DNS failures may be retried at delivery, which always validates again.
func isBlockedTarget(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return !isPublicIP(ip)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, h)
	if err != nil {
		return false
	}
	return !publicTargetAddresses(addrs)
}

func publicTargetAddresses(addrs []net.IPAddr) bool {
	if len(addrs) == 0 {
		return false
	}
	for _, a := range addrs {
		if !isPublicIP(a.IP) {
			return false
		}
	}
	return true
}

// isPublicIP reports whether ip is a routable public address rather than one of
// the ranges a proxy must not be induced to reach.
func isPublicIP(ip net.IP) bool {
	if !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	for _, prefix := range reservedTargetRanges {
		if prefix.Contains(address) {
			return false
		}
	}

	// isPublicIP also refuses IPv4-mapped IPv6 forms of the ranges above, which
	// net.IP.IsPrivate and friends already cover for the mapped address.
	return true
}

// Special-use/documentation and IPv4 translation ranges are not delivery
// destinations. Translation/tunnel addresses can otherwise hide a private IPv4.
var reservedTargetRanges = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"), netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
}

// targetDialContext resolves once, checks every answer, then dials a literal
// approved IP. Re-resolving the original name in net.Dialer would reopen the
// DNS-rebinding window. Dependencies are injected for deterministic security tests.
func targetDialContext(allowPrivate func() bool,
	lookup func(context.Context, string) ([]net.IPAddr, error),
	dial func(context.Context, string, string) (net.Conn, error),
) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if allowPrivate() {
			return dial(ctx, network, address)
		}
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") || strings.HasSuffix(strings.ToLower(strings.TrimSuffix(host, ".")), ".localhost") {
			return nil, fmt.Errorf("target host is not allowed")
		}
		var addrs []net.IPAddr
		if ip := net.ParseIP(host); ip != nil {
			addrs = []net.IPAddr{{IP: ip}}
		} else {
			addrs, err = lookup(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("resolve target: %w", err)
			}
		}
		if !publicTargetAddresses(addrs) {
			return nil, fmt.Errorf("target host is not allowed")
		}
		for _, addr := range addrs {
			conn, dialErr := dial(ctx, network, net.JoinHostPort(addr.IP.String(), port))
			if dialErr == nil {
				return conn, nil
			}
			err = dialErr
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		return nil, err
	}
}

// A shared transport reuses safe established connections across outbox jobs.
// Environment HTTP proxies are disabled: otherwise DialContext would validate
// the proxy instead of the destination. Private Docker targets remain an explicit
// operator opt-in through allowPrivateTargets.
var notificationHTTPClient = func() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = targetDialContext(func() bool { return allowPrivateTargets },
		net.DefaultResolver.LookupIPAddr, (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext)
	transport.MaxIdleConnsPerHost = 4
	transport.MaxResponseHeaderBytes = 1 << 20
	return &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("stopped after 5 redirects")
			}
			return validateTargetURL(req.URL.String(), allowPrivateTargets)
		},
	}
}()

func outboundHTTPClient() *http.Client { return notificationHTTPClient }
