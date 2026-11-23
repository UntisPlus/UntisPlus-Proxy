package proxy

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestNotificationDialPinsApprovedAddress(t *testing.T) {
	for _, tc := range []struct {
		name      string
		addresses []string
		lookupErr bool
		allowed   bool
	}{
		{"public", []string{"1.1.1.1", "2606:4700:4700::1111"}, false, true},
		{"private", []string{"10.0.0.1"}, false, false},
		{"mixed", []string{"1.1.1.1", "127.0.0.1"}, false, false},
		{"mapped", []string{"::ffff:127.0.0.1"}, false, false},
		{"empty", nil, false, false},
		{"DNS failure", nil, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookups, dials := 0, 0
			dial := targetDialContext(func() bool { return false }, func(ctx context.Context, host string) ([]net.IPAddr, error) {
				lookups++
				if host != "notify.example" {
					t.Fatalf("lookup host=%s", host)
				}
				if tc.lookupErr {
					return nil, errors.New("DNS unavailable")
				}
				var result []net.IPAddr
				for _, a := range tc.addresses {
					result = append(result, net.IPAddr{IP: net.ParseIP(a)})
				}
				return result, nil
			}, func(ctx context.Context, network, address string) (net.Conn, error) {
				dials++
				if address != "1.1.1.1:443" {
					t.Fatalf("dial must use checked literal, got %s", address)
				}
				return nil, nil
			})
			_, err := dial(context.Background(), "tcp", "notify.example:443")
			if (err == nil) != tc.allowed {
				t.Fatalf("error=%v, allowed=%v", err, tc.allowed)
			}
			if lookups != 1 || (dials > 0) != tc.allowed {
				t.Fatalf("lookups=%d dials=%d", lookups, dials)
			}
		})
	}
}

func TestNotificationDialRechecksDNSForEachConnection(t *testing.T) {
	lookups, dials := 0, 0
	dial := targetDialContext(func() bool { return false }, func(context.Context, string) ([]net.IPAddr, error) {
		lookups++
		ip := "1.1.1.1"
		if lookups > 1 {
			ip = "169.254.169.254"
		}
		return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
	}, func(context.Context, string, string) (net.Conn, error) { dials++; return nil, nil })
	if _, err := dial(context.Background(), "tcp", "notify.example:80"); err != nil {
		t.Fatal(err)
	}
	if _, err := dial(context.Background(), "tcp", "notify.example:80"); err == nil {
		t.Fatal("DNS rebinding reached private network")
	}
	if dials != 1 {
		t.Fatalf("dials=%d", dials)
	}
}

func TestNotificationDialPrivateOverrideAndLiteralGuard(t *testing.T) {
	for _, allow := range []bool{false, true} {
		calls := 0
		dial := targetDialContext(func() bool { return allow }, func(context.Context, string) ([]net.IPAddr, error) { t.Fatal("unexpected DNS lookup"); return nil, nil }, func(context.Context, string, string) (net.Conn, error) { calls++; return nil, nil })
		_, err := dial(context.Background(), "tcp", "127.0.0.1:80")
		if (err == nil) != allow || (calls == 1) != allow {
			t.Fatalf("allow=%v error=%v calls=%d", allow, err, calls)
		}
	}
}
