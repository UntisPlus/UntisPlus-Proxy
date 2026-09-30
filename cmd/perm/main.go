package main

import (
	"flag"
	"fmt"
	"log"
	"strings"

	"untis-proxy/internal/store"
)

// types are the permission features that can be granted, plus the lowercase
// aliases used on the command line.
var typeAliases = map[string]string{
	"recon":          store.FeatureRecon,
	"reconstruction": store.FeatureRecon,
	"boosted":        store.FeatureBoosted,
	"boost":          store.FeatureBoosted,
	"editor":         store.FeatureEditor,
}

// boostedFeatures are per-user-only and cannot be applied via the global switch.
var boostedFeatures = map[string]bool{
	store.FeatureBoosted: true,
	store.FeatureEditor:  true,
}

func main() {
	db := flag.String("db", "untis.db", "sqlite database path")
	username := flag.String("user", "", "username to grant/override access for (omit with -global)")
	global := flag.Bool("global", false, "operate on the global switch that applies to all users")
	elType := flag.String("type", "all", "permission: recon|boosted|editor|all (case-insensitive)")
	grant := flag.Bool("grant", false, "grant the permission")
	revoke := flag.Bool("revoke", false, "revoke the permission")
	clear := flag.Bool("clear", false, "clear all per-user overrides for -user (fall back to global)")
	all := flag.Bool("all", false, "apply to every user (revoke/clear only)")
	flag.Parse()

	st, err := store.Open(*db)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	types := resolveTypes(*elType)

	// --all --revoke: wipe everything to the first-login (class-pool-only) state.
	if *all {
		if *grant || *username != "" {
			log.Fatal("--all can only be combined with --revoke (no --user/--grant)")
		}
		n, err := st.RevokeAll()
		if err != nil {
			log.Fatalf("revoke all: %v", err)
		}
		fmt.Printf("cleared all permissions for all users (%d rows): class-pool-only default restored\n", n)
		return
	}

	// --user X --clear: drop the user's overrides, fall back to global switches.
	if *clear {
		if *username == "" {
			log.Fatal("--clear requires --user")
		}
		if *grant || *revoke {
			log.Fatal("--clear cannot be combined with --grant/--revoke")
		}
		n, err := st.ClearReconOverrides(*username)
		if err != nil {
			log.Fatalf("clear overrides: %v", err)
		}
		fmt.Printf("cleared %d per-user override(s) for %q (falls back to global switches)\n", n, *username)
		return
	}

	if *username == "" && !*global {
		log.Fatal("require --user (per-user override) or --global (global switch)")
	}
	if *username != "" && *global {
		log.Fatal("choose either --user (per-user override) or --global (global switch), not both")
	}
	if *grant == *revoke {
		log.Fatal("exactly one of --grant or --revoke is required")
	}
	if *global {
		for _, t := range types {
			if boostedFeatures[t] {
				log.Fatalf("boosting features (%s) are per-user only; use --user", t)
			}
		}
	}

	allowed := *grant
	for _, t := range types {
		var err error
		switch {
		case *global:
			err = st.SetReconType(t, allowed)
		case t == store.FeatureBoosted || t == store.FeatureEditor:
			err = st.SetPerm(*username, t, allowed)
		default:
			err = st.SetReconOverride(*username, t, allowed)
		}
		if err != nil {
			log.Fatalf("set %s: %v", t, err)
		}
		if *username != "" {
			var on bool
			switch t {
			case store.FeatureBoosted:
				on, _ = st.BoostedAccess(*username)
			case store.FeatureEditor:
				on, _ = st.EditorAccess(*username)
			default:
				on, _ = st.ReconAccess(*username, t)
			}
			fmt.Printf("%s %s (effective for %q = %v)\n", t, verb(allowed), *username, on)
		} else {
			fmt.Printf("%s %s globally\n", t, verb(allowed))
		}
	}
}

func resolveTypes(s string) []string {
	if s == "all" {
		return []string{store.FeatureRecon}
	}
	if t, ok := typeAliases[strings.ToLower(s)]; ok {
		return []string{t}
	}
	log.Fatalf("unknown permission %q (use recon|boosted|editor|all)", s)
	return nil
}

func verb(allowed bool) string {
	if allowed {
		return "granted"
	}
	return "revoked"
}
