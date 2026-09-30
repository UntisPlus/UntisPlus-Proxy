package main

// Config precedence: an explicit command-line flag beats the environment, which
// beats the built-in default. The compose files and the Dockerfile rely on this
// to configure the proxy without rewriting its command line — before this
// existed only the Dockerfile's `sh -c` expansion did that, so any flag added
// later (e.g. -public-base) was silently ignored when set as an env var.

import (
	"flag"
	"os"
	"strings"
	"testing"
	"time"
)

func newFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("cfg", flag.ContinueOnError)
	fs.String("addr", ":8509", "")
	fs.String("school", "", "")
	fs.Int("recon-refresh", 21, "")
	fs.Bool("recon-rescan", false, "")
	fs.Duration("poll-interval", 60*time.Second, "")
	return fs
}

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestApplyEnvDefaults(t *testing.T) {
	setEnv(t, map[string]string{
		"UNTIS_ADDR":          ":9999",
		"UNTIS_SCHOOL":        "musterschule",
		"UNTIS_RECON_REFRESH": "7",
		"UNTIS_RECON_RESCAN":  "true",
		"UNTIS_POLL_INTERVAL": "15s",
		"UNTIS_PUBLIC_BASE":   "https://proxy.example.org", // not declared here
	})
	fs := newFlagSet()
	applyEnvTo(fs)
	addr := fs.Lookup("addr")
	school := fs.Lookup("school")
	refresh := fs.Lookup("recon-refresh")
	rescan := fs.Lookup("recon-rescan")
	poll := fs.Lookup("poll-interval")

	if addr.Value.String() != ":9999" {
		t.Errorf("addr = %q, want :9999 from UNTIS_ADDR", addr.Value.String())
	}
	if school.Value.String() != "musterschule" {
		t.Errorf("school = %q, want from UNTIS_SCHOOL", school.Value.String())
	}
	if refresh.Value.String() != "7" {
		t.Errorf("recon-refresh = %q, want 7", refresh.Value.String())
	}
	if rescan.Value.String() != "true" {
		t.Errorf("recon-rescan = %q, want true", rescan.Value.String())
	}
	if poll.Value.String() != "15s" {
		t.Errorf("poll-interval = %q, want 15s", poll.Value.String())
	}
	// An env var with no matching flag must be ignored, not invented into one.
	if fs.Lookup("public-base") != nil {
		t.Error("applyEnvTo invented a flag for UNTIS_PUBLIC_BASE")
	}
}

func TestCommandLineFlagBeatsEnv(t *testing.T) {
	setEnv(t, map[string]string{"UNTIS_ADDR": ":9999"})
	fs := newFlagSet()
	applyEnvTo(fs)
	// What flag.Parse does after the defaults were seeded.
	if err := fs.Parse([]string{"-addr", ":7777"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := fs.Lookup("addr").Value.String(); got != ":7777" {
		t.Errorf("addr = %q, want the command-line value :7777 to win over UNTIS_ADDR", got)
	}
}

// The school default is empty on purpose: it is the storage key, so an
// unset value must fail at boot rather than inherit a real school's name.
func TestUnsetEnvKeepsDefault(t *testing.T) {
	os.Unsetenv("UNTIS_ADDR")
	fs := newFlagSet()
	applyEnvTo(fs)
	if got := fs.Lookup("addr").Value.String(); got != ":8509" {
		t.Errorf("addr = %q, want the built-in default", got)
	}
	if got := fs.Lookup("school").Value.String(); got != "" {
		t.Errorf("school default = %q, want empty so the server fails fast when unset", got)
	}
}

// TestMalformedEnvFallsBackToDefault: a typo in a compose file must not take
// the proxy down at boot; it degrades to the default and logs.
func TestMalformedEnvFallsBackToDefault(t *testing.T) {
	setEnv(t, map[string]string{
		"UNTIS_RECON_REFRESH": "not-a-number",
		"UNTIS_POLL_INTERVAL": "soon",
	})
	fs := newFlagSet()
	applyEnvTo(fs)
	if got := fs.Lookup("recon-refresh").Value.String(); got != "21" {
		t.Errorf("recon-refresh = %q, want the default 21 after a bad value", got)
	}
	if got := fs.Lookup("poll-interval").Value.String(); got != "1m0s" {
		t.Errorf("poll-interval = %q, want the default after a bad value", got)
	}
}

func TestEnvKeyDerivation(t *testing.T) {
	cases := map[string]string{
		"addr":          "UNTIS_ADDR",
		"recon-refresh": "UNTIS_RECON_REFRESH",
		"ntfy-base":     "UNTIS_NTFY_BASE",
		"public-base":   "UNTIS_PUBLIC_BASE",
		"poll-interval": "UNTIS_POLL_INTERVAL",
	}
	for flagName, want := range cases {
		got := "UNTIS_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
		if got != want {
			t.Errorf("flag %q -> %q, want %q", flagName, got, want)
		}
	}
}
