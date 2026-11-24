package main

import (
	"bufio"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"untis-proxy/internal/access"
	"untis-proxy/internal/permcli"
	"untis-proxy/internal/seedcli"
	"untis-proxy/internal/store"
	"untis-proxy/internal/untis"
)

// untisctl is a small operator tool for managing a running untis-proxy's
// database: users, secrets, perms, the class pool and calendar tokens. It edits
// the SQLite DB directly, like cmd/perm and cmd/seed, so it can be run against
// a live server's data file.

// defaultPublicBase is the fallback --base for generated .ics links. It is
// empty by default so nothing bakes one deployment's hostname into the binary;
// set UNTIS_PUBLIC_BASE or pass --base when minting tokens for real users.
const defaultPublicBase = ""

func main() {
	// Container aliases share this executable and its SQLite runtime. Dispatch
	// before registering flags so each command keeps its own argument syntax.
	switch filepath.Base(os.Args[0]) {
	case "untis-seed":
		seedcli.Run()
		return
	case "untis-perm":
		permcli.Run()
		return
	}

	db := flag.String("db", os.Getenv("UNTIS_DB"), "path to the untis sqlite database (default: $UNTIS_DB, else untis.db)")
	server := flag.String("server", os.Getenv("UNTIS_SERVER"), "fallback upstream host (default: $UNTIS_SERVER, else auto-resolved per school)")
	school := flag.String("school", os.Getenv("UNTIS_SCHOOL"), "upstream school name (default: $UNTIS_SCHOOL)")
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, `untisctl – manage an untis-proxy database

Usage:
  untisctl [-db PATH] <command> [options]

Commands:
  admission-reset                    clear login restrictions for -school (empty = global)
  perms list [--user U]              show global switches and per-user overrides
  perms grant|revoke [--user U|--global] TYPE
                                     grant/revoke recon|boosted|editor|techniker (per-user only)
  perms clear --user U               drop a user's overrides (fall back to global)
  perms reset                        wipe all permissions (class-pool-only default)
  perms default list                 show permissions granted to newly seen users
  perms default grant|revoke TYPE    add/remove recon|boosted|editor|techniker
                                     from the default set (existing users unaffected)
  perms default clear                give newly seen users no permissions

  anonymous list                     show the global anonymous capability matrix
  anonymous grant|revoke FEATURE     enable/disable classes|ntfy|webhooks|changes
                                     for every school (anonymous login stays
                                     per school and is set from the dashboard)

  users list                         list accounts
  users add --user U --secret S [--method password|key]
  users remove --user U              delete user + secret + overrides + personal tokens
  users admin --user U [--on|--off]  grant or revoke admin dashboard access
  users check [--remove-classless]   ask the real Untis API for each account's class,
                                     using the account's own school (-school is only a
                                     fallback for legacy rows without one);
                                     --remove-classless deletes students the API
                                     reports without a class

  pool list                          show pooled classes and their owners
  pool owners                        show which user(s) own each class

  calendar create [FLAGS]            create a calendar subscription token
    --class ID                         class timetable (must be in pool)
    --teacher NAME_OR_ID               teacher timetable (fuzzy name lookup)
    --room NAME_OR_ID                  room timetable (fuzzy name lookup)
    --subject NAME_OR_ID               subject timetable (fuzzy name lookup)
    --personal                         personal student timetable (self)
    --personal --user U                personal timetable for user U
    --base URL                         base URL for the printed .ics link
  calendar student [--user U]        pick a student from the DB (interactive
                                     menu, or --user U non-interactively) and
                                     mint their personal calendar token + .ics URL
  calendar list                      list all calendar tokens
  calendar revoke TOKEN              revoke a calendar token

  tokens list                        alias for calendar list
  tokens revoke TOKEN                alias for calendar revoke

  totp USER [--scan]                 current 6-digit TOTP code (+ seconds left)
                                     for USER; --scan prints the otpauth:// URI
                                     (ANSI QR if qrencode is installed)

  recon [--mode hard|slow] [--full]  enumerate teachers/rooms/subjects for every
    --concurrency N --delay D           pooled class with a live percentage;
    --year-start D --year-end D         hard = parallel, slow = one at a time
    --dry-run                           with a delay; --full ignores saved
                                        progress and starts at the year start

  status                             db stats: users, pool, perms, recon, tokens

  backup [--out PATH] [--keep N]     consistent copy of the database via
                                     VACUUM INTO (safe while the server runs);
                                     default <db>.untis-backup-<UTC>.db;
                                     --keep N prunes all but the N newest backups

Global:
  -db PATH        sqlite database path (default: $UNTIS_DB, else "untis.db")
  -server HOST    fallback upstream Untis server (default: $UNTIS_SERVER, else
                  auto-resolved per school)
  -school NAME    upstream school name (default: $UNTIS_SCHOOL)

Run 'untisctl <command> -h' for per-command flags.
`)
	}
	flag.Parse()

	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}

	st, err := store.Open(*db)
	if err != nil {
		fatal("open db: %v", err)
	}
	defer st.Close()

	switch flag.Arg(0) {
	case "admission-reset":
		if err := st.SetAdmissionPolicy(*school, store.AdmissionPolicy{}); err != nil {
			fatal("reset admission: %v", err)
		}
		fmt.Println("Login restrictions cleared for selected scope (empty school means server-wide).")
	case "perms":
		cmdPerms(st, flag.Args()[1:])
	case "anonymous":
		cmdAnonymous(st, flag.Args()[1:])
	case "access", "v2":
		cmdAccess(st, flag.Args()[1:])
	case "users":
		cmdUsers(st, flag.Args()[1:], *server, *school)
	case "pool":
		cmdPool(st, flag.Args()[1:], *school)
	case "calendar", "tokens":
		cmdCalendar(st, flag.Args()[1:])
	case "totp":
		cmdTotp(st, flag.Args()[1:])
	case "recon":
		cmdRecon(st, flag.Args()[1:], *server, *school)
	case "status":
		cmdStatus(st, *school)
	case "backup":
		cmdBackup(st, *db, flag.Args()[1:])
	default:
		fatal("unknown command %q\n\nrun 'untisctl -h' for usage", flag.Arg(0))
	}
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "untisctl: "+format+"\n", a...)
	os.Exit(1)
}

// ---------------------------------------------------------------------------
// perms
// ---------------------------------------------------------------------------

var reconTypes = map[string]string{
	"recon":          store.FeatureRecon,
	"reconstruction": store.FeatureRecon,
	"boosted":        store.FeatureBoosted,
	"boost":          store.FeatureBoosted,
	"editor":         store.FeatureEditor,
	"techniker":      store.FeatureTechniker,
	"technik":        store.FeatureTechniker,
}

// perUserOnly are permissions that cannot be applied globally: they grant access
// to data pooled from several accounts, so a global switch would hand it to
// every user at once.
var perUserOnly = map[string]bool{
	store.FeatureBoosted:   true,
	store.FeatureEditor:    true,
	store.FeatureTechniker: true,
}

func cmdPerms(st *store.Store, args []string) {
	if len(args) == 0 {
		fatal("perms requires a subcommand (list|grant|revoke|clear|reset)\n\nrun 'untisctl perms -h' for help")
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("perms list", flag.ExitOnError)
		user := fs.String("user", "", "filter to a single user")
		fs.Parse(args[1:])
		permsList(st, *user)
	case "grant", "revoke":
		permSet(st, args[0], args[1:])
	case "clear":
		permClear(st, args[1:])
	case "reset":
		n, err := st.RevokeAll()
		if err != nil {
			fatal("reset perms: %v", err)
		}
		fmt.Printf("reset %d permission rows: all users back to class-pool-only\n", n)
	case "default", "defaults":
		cmdDefaultPerms(st, args[1:])
	default:
		fatal("unknown perms subcommand %q", args[0])
	}
}

// defaultPermOrder keeps the list output stable.
var defaultPermOrder = []string{
	store.FeatureRecon,
	store.FeatureBoosted,
	store.FeatureEditor,
	store.FeatureTechniker,
}

// cmdDefaultPerms manages the permissions a login inherits the first time the
// proxy sees its account. Existing rows are never rewritten: the set is copied
// at insert time only.
func cmdDefaultPerms(st *store.Store, args []string) {
	if len(args) == 0 {
		fatal("perms default requires a subcommand (list|grant|revoke|clear)")
	}
	switch args[0] {
	case "list":
		defaults, err := st.DefaultPerms()
		if err != nil {
			fatal("list default perms: %v", err)
		}
		fmt.Printf("%-10s %s\n", "TYPE", "DEFAULT")
		for _, feature := range defaultPermOrder {
			fmt.Printf("%-10s %v\n", feature, defaults[feature])
		}
	case "grant", "revoke":
		if len(args) < 2 {
			fatal("perms default %s requires a TYPE (recon|boosted|editor|techniker)", args[0])
		}
		typ, ok := reconTypes[strings.ToLower(args[1])]
		if !ok {
			fatal("unknown type %q (use recon|boosted|editor|techniker)", args[1])
		}
		if err := st.SetDefaultPerm(typ, args[0] == "grant"); err != nil {
			fatal("default %s: %v", typ, err)
		}
		fmt.Printf("%s %s for newly seen users\n", past(args[0]), typ)
	case "clear":
		if err := st.ClearDefaultPerms(); err != nil {
			fatal("clear default perms: %v", err)
		}
		fmt.Println("cleared default permissions: newly seen users start with none")
	default:
		fatal("unknown perms default subcommand %q", args[0])
	}
}

// anonymousFeatureOrder keeps the capability list output stable.
var anonymousFeatureOrder = []string{"classes", "ntfy", "webhooks", "changes"}

// cmdAnonymous manages the deployment-wide anonymous capabilities. The
// per-school login switch is intentionally not exposed here: it needs the
// dashboard's school context and is stored on the school row.
func cmdAnonymous(st *store.Store, args []string) {
	if len(args) == 0 {
		fatal("anonymous requires a subcommand (list|grant|revoke|profiles)")
	}
	switch args[0] {
	case "list":
		features, err := st.AnonymousFeatures()
		if err != nil {
			fatal("list anonymous features: %v", err)
		}
		fmt.Printf("%-10s %s\n", "FEATURE", "ENABLED")
		for _, feature := range anonymousFeatureOrder {
			fmt.Printf("%-10s %v\n", feature, features[feature])
		}
	case "grant", "revoke":
		if len(args) < 2 {
			fatal("anonymous %s requires a FEATURE (classes|ntfy|webhooks|changes)", args[0])
		}
		feature := strings.ToLower(args[1])
		if feature == "class" {
			feature = "classes"
		}
		if err := st.SetAnonymousFeature(feature, args[0] == "grant"); err != nil {
			fatal("anonymous %s %s: %v", args[0], feature, err)
		}
		fmt.Printf("%s anonymous %s\n", past(args[0]), feature)
	case "profiles":
		profiles, err := st.ListSchoolAnonProfiles()
		if err != nil {
			fatal("list anonymous profiles: %v", err)
		}
		if len(profiles) == 0 {
			fmt.Println("no anonymous profiles stored")
			return
		}
		fmt.Printf("%-20s %-7s %-10s %-20s %-20s %s\n", "SCHOOL", "LOGIN", "SOURCE", "DETECTED", "EXPIRES", "CAPABILITIES")
		for _, p := range profiles {
			payload := store.DecodeAnonProfilePayload(p.ProfileJSON)
			caps := make([]string, 0, len(payload.Capabilities))
			for k, v := range payload.Capabilities {
				if v {
					caps = append(caps, k+"=1")
				} else {
					caps = append(caps, k+"=0")
				}
			}
			sort.Strings(caps)
			fmt.Printf("%-20s %-7v %-10s %-20s %-20s %s\n",
				p.School, p.LoginOK, p.Source,
				p.DetectedAt.UTC().Format(time.RFC3339),
				p.ExpiresAt.UTC().Format(time.RFC3339),
				strings.Join(caps, ","))
		}
	default:
		fatal("unknown anonymous subcommand %q", args[0])
	}
}

func cmdAccess(st *store.Store, args []string) {
	if len(args) == 0 {
		fatal("access requires a subcommand (anon-profile|gates|groups|effective|grants|grant|user-groups|role-defaults|person-type-roles)")
	}
	switch args[0] {
	case "anon-profile", "anon-profiles":
		cmdAccessAnonProfile(st, args[1:])
	case "gates":
		cmdAccessGates(st, args[1:])
	case "groups":
		cmdCapGroups(st, args[1:])
	case "effective", "why":
		cmdAccessEffective(st, args[1:])
	case "grants":
		cmdAccessGrants(st, args[1:])
	case "grant":
		cmdAccessGrantSet(st, args[1:])
	case "user-groups":
		cmdAccessUserGroups(st, args[1:])
	case "role-defaults":
		cmdAccessRoleDefaults(st, args[1:])
	case "person-type-roles", "role-mappings", "person-type-role":
		cmdAccessPersonTypeRoles(st, args[1:])
	default:
		fatal("unknown access subcommand %q", args[0])
	}
}

func cmdCapGroups(st *store.Store, args []string) {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list":
		school := ""
		if len(args) > 1 {
			school = args[1]
		}
		groups, err := st.ListCapGroups(school)
		if err != nil {
			fatal("list access groups: %v", err)
		}
		if len(groups) == 0 {
			fmt.Println("no groups")
			return
		}
		fmt.Printf("%-5s %-20s %-30s %s\n", "ID", "SCHOOL", "NAME", "DESCRIPTION")
		for _, g := range groups {
			fmt.Printf("%-5d %-20s %-30s %s\n", g.ID, g.School, g.Name, g.Description)
		}
	case "show":
		if len(args) < 2 {
			fatal("access groups show requires GROUP_ID")
		}
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			fatal("invalid group id: %v", err)
		}
		g, grants, err := st.GetCapGroup(id)
		if err != nil || g == nil {
			fatal("group not found: %d", id)
		}
		fmt.Printf("id=%d school=%q name=%q description=%q\n", g.ID, g.School, g.Name, g.Description)
		if len(grants) == 0 {
			fmt.Println("no grants")
			return
		}
		fmt.Printf("%-45s %s\n", "CAPABILITY", "EFFECT")
		for _, gr := range grants {
			eff := "inherit"
			if gr.Effect > 0 {
				eff = "allow"
			} else if gr.Effect < 0 {
				eff = "deny"
			}
			fmt.Printf("%-45s %s(%d)\n", gr.Capability, eff, gr.Effect)
		}
	case "create":
		if len(args) < 3 {
			fatal("access groups create requires SCHOOL NAME [DESCRIPTION...] (use empty school \"\" for global)")
		}
		school := args[1]
		name := args[2]
		desc := ""
		if len(args) > 3 {
			desc = strings.Join(args[3:], " ")
		}
		id, err := st.CreateCapGroup(school, name, desc)
		if err != nil {
			fatal("create group: %v", err)
		}
		fmt.Printf("created group id=%d school=%q name=%q\n", id, school, name)
	case "delete":
		if len(args) < 2 {
			fatal("access groups delete requires GROUP_ID")
		}
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			fatal("invalid group id: %v", err)
		}
		if err := st.DeleteCapGroup(id); err != nil {
			fatal("delete group: %v", err)
		}
		fmt.Printf("deleted group %d\n", id)
	case "grant":
		// access groups grant GROUP_ID CAPABILITY allow|deny|inherit
		if len(args) < 4 {
			fatal("access groups grant requires GROUP_ID CAPABILITY allow|deny|inherit")
		}
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			fatal("invalid group id: %v", err)
		}
		cap := args[2]
		effect, ok := parseEffectArg(args[3])
		if !ok {
			fatal("invalid effect %q (use allow|deny|inherit)", args[3])
		}
		if err := st.SetCapGroupGrant(id, cap, effect); err != nil {
			fatal("set group grant: %v", err)
		}
		fmt.Printf("set group grant group=%d cap=%s effect=%d\n", id, cap, effect)
	default:
		// backward compat: treat single arg as school for list
		if len(args) == 1 {
			groups, err := st.ListCapGroups(args[0])
			if err != nil {
				fatal("list access groups: %v", err)
			}
			if len(groups) == 0 {
				fmt.Println("no groups")
				return
			}
			fmt.Printf("%-5s %-20s %-30s %s\n", "ID", "SCHOOL", "NAME", "DESCRIPTION")
			for _, g := range groups {
				fmt.Printf("%-5d %-20s %-30s %s\n", g.ID, g.School, g.Name, g.Description)
			}
			return
		}
		fatal("unknown v2 groups subcommand %q (use list|show|create|delete|grant)", args[0])
	}
}

func parseEffectArg(s string) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "allow", "grant", "1", "true":
		return 1, true
	case "deny", "revoke", "-1", "false":
		return -1, true
	case "inherit", "clear", "0":
		return 0, true
	}
	return 0, false
}

func cmdAccessEffective(st *store.Store, args []string) {
	if len(args) < 2 {
		fatal("access effective/why requires SCHOOL USERNAME")
	}
	school, username := args[0], args[1]
	u, err := st.GetUser(username)
	if err != nil || u == nil {
		fatal("user not found: %s", username)
	}
	res, err := st.ResolveEffective(school, u)
	if err != nil || res == nil {
		fatal("resolve effective: %v", err)
	}
	fmt.Printf("user=%s school=%s role=%s\n", username, school, res.Role)
	groups, _ := st.ListUserCapGroups(school, username)
	fmt.Printf("groups:")
	for _, g := range groups {
		fmt.Printf(" %s(%d)", g.Name, g.ID)
	}
	fmt.Println()
	if len(res.DirectGrants) > 0 {
		fmt.Printf("direct_grants:")
		for _, g := range res.DirectGrants {
			eff := "inherit"
			if g.Effect > 0 {
				eff = "allow"
			} else if g.Effect < 0 {
				eff = "deny"
			}
			fmt.Printf(" %s=%s", g.Capability, eff)
		}
		fmt.Println()
	}
	roleDefIDs, _ := st.ListRoleDefaultGroups(school, string(res.Role))
	if len(roleDefIDs) == 0 {
		roleDefIDs, _ = st.ListRoleDefaultGroups("", string(res.Role))
	}
	if len(roleDefIDs) > 0 {
		fmt.Printf("role_default_groups(%s):", res.Role)
		for _, id := range roleDefIDs {
			fmt.Printf(" %d", id)
		}
		fmt.Println()
	}
	caps := make([]string, 0, len(res.Decisions))
	for c := range res.Decisions {
		caps = append(caps, string(c))
	}
	sort.Strings(caps)
	fmt.Printf("%-45s %-9s %-9s %-9s %-9s %s\n", "CAPABILITY", "SERVER", "ADMIN", "OPT-IN", "EFFECT", "SOURCE")
	for _, c := range caps {
		d := res.Decisions[access.Capability(c)]
		fmt.Printf("%-45s %-9v %-9v %-9v %-9v %s\n", c, d.ServerEnabled, d.AdminAllowed, d.UserOptedIn, d.Effective, d.Source)
	}
}

func cmdAccessGrants(st *store.Store, args []string) {
	if len(args) < 2 {
		fatal("access grants requires list SCHOOL USERNAME")
	}
	if args[0] == "list" {
		args = args[1:]
		if len(args) < 2 {
			fatal("access grants list requires SCHOOL USERNAME")
		}
	}
	school, username := args[0], args[1]
	grants, err := st.ListUserCapGrants(school, username)
	if err != nil {
		fatal("list user grants: %v", err)
	}
	if len(grants) == 0 {
		fmt.Println("no direct user grants")
		return
	}
	fmt.Printf("%-45s %s\n", "CAPABILITY", "EFFECT")
	for _, g := range grants {
		eff := "inherit"
		if g.Effect > 0 {
			eff = "allow"
		} else if g.Effect < 0 {
			eff = "deny"
		}
		fmt.Printf("%-45s %s(%d)\n", g.Capability, eff, g.Effect)
	}
}

func cmdAccessGrantSet(st *store.Store, args []string) {
	if len(args) < 4 {
		fatal("access grant set requires SCHOOL USERNAME CAPABILITY allow|deny|inherit")
	}
	if args[0] == "set" {
		args = args[1:]
		if len(args) < 4 {
			fatal("access grant set requires SCHOOL USERNAME CAPABILITY allow|deny|inherit")
		}
	}
	school, username, cap, effStr := args[0], args[1], args[2], strings.ToLower(args[3])
	effect := 0
	switch effStr {
	case "allow", "grant", "1", "true":
		effect = 1
	case "deny", "revoke", "-1", "false":
		effect = -1
	case "inherit", "clear", "0":
		effect = 0
	default:
		fatal("invalid effect %q (use allow|deny|inherit)", effStr)
	}
	if err := st.SetUserCapGrant(school, username, cap, effect); err != nil {
		fatal("set user grant: %v", err)
	}
	fmt.Printf("set direct grant school=%s user=%s cap=%s effect=%d\n", school, username, cap, effect)
}

func cmdAccessUserGroups(st *store.Store, args []string) {
	if len(args) == 0 {
		fatal("access user-groups requires a subcommand (list|assign|unassign)")
	}
	switch args[0] {
	case "list":
		if len(args) < 3 {
			fatal("access user-groups list requires SCHOOL USERNAME")
		}
		groups, err := st.ListUserCapGroups(args[1], args[2])
		if err != nil {
			fatal("list user groups: %v", err)
		}
		if len(groups) == 0 {
			fmt.Println("no user groups")
			return
		}
		for _, g := range groups {
			fmt.Printf("%d\t%s\t%s\n", g.ID, g.Name, g.Description)
		}
	case "assign":
		if len(args) < 4 {
			fatal("access user-groups assign requires SCHOOL USERNAME GROUP_ID")
		}
		gid, err := strconv.ParseInt(args[3], 10, 64)
		if err != nil {
			fatal("invalid group id: %v", err)
		}
		if err := st.AssignUserCapGroup(args[1], args[2], gid); err != nil {
			fatal("assign user group: %v", err)
		}
		fmt.Printf("assigned group %d to %s (%s)\n", gid, args[2], args[1])
	case "unassign":
		if len(args) < 4 {
			fatal("access user-groups unassign requires SCHOOL USERNAME GROUP_ID")
		}
		gid, err := strconv.ParseInt(args[3], 10, 64)
		if err != nil {
			fatal("invalid group id: %v", err)
		}
		if err := st.UnassignUserCapGroup(args[1], args[2], gid); err != nil {
			fatal("unassign user group: %v", err)
		}
		fmt.Printf("unassigned group %d from %s (%s)\n", gid, args[2], args[1])
	default:
		fatal("unknown access user-groups subcommand %q", args[0])
	}
}

func cmdAccessRoleDefaults(st *store.Store, args []string) {
	if len(args) == 0 {
		fatal("access role-defaults requires a subcommand (list|set|delete)")
	}
	switch args[0] {
	case "list":
		school, role := "", ""
		if len(args) > 1 {
			school = args[1]
		}
		if len(args) > 2 {
			role = args[2]
		}
		roles := []string{role}
		if role == "" {
			roles = []string{"student", "teacher", "staff", "parent", "unknown", "other"}
		}
		fmt.Printf("%-20s %-12s %s\n", "SCHOOL", "ROLE", "GROUP_IDS")
		for _, r := range roles {
			ids, err := st.ListRoleDefaultGroups(school, r)
			if err != nil {
				fatal("list role defaults: %v", err)
			}
			if len(ids) == 0 && role == "" {
				continue
			}
			parts := make([]string, 0, len(ids))
			for _, id := range ids {
				parts = append(parts, strconv.FormatInt(id, 10))
			}
			fmt.Printf("%-20s %-12s %s\n", school, r, strings.Join(parts, ","))
		}
	case "set":
		if len(args) < 5 {
			fatal("access role-defaults set requires SCHOOL ROLE GROUP_ID PRIORITY")
		}
		gid, err := strconv.ParseInt(args[3], 10, 64)
		if err != nil {
			fatal("invalid group id: %v", err)
		}
		pri, err := strconv.Atoi(args[4])
		if err != nil {
			fatal("invalid priority: %v", err)
		}
		if err := st.SetRoleDefaultGroup(args[1], args[2], gid, pri); err != nil {
			fatal("set role default: %v", err)
		}
		fmt.Printf("set role default school=%s role=%s group=%d priority=%d\n", args[1], args[2], gid, pri)
	case "delete", "unset":
		if len(args) < 4 {
			fatal("access role-defaults delete requires SCHOOL ROLE GROUP_ID")
		}
		gid, err := strconv.ParseInt(args[3], 10, 64)
		if err != nil {
			fatal("invalid group id: %v", err)
		}
		if err := st.DeleteRoleDefaultGroup(args[1], args[2], gid); err != nil {
			fatal("delete role default: %v", err)
		}
		fmt.Printf("deleted role default school=%s role=%s group=%d\n", args[1], args[2], gid)
	default:
		fatal("unknown access role-defaults subcommand %q (use list|set|delete)", args[0])
	}
}

func cmdAccessPersonTypeRoles(st *store.Store, args []string) {
	if len(args) == 0 {
		fatal("access person-type-roles requires a subcommand (list|set|delete)")
	}
	switch args[0] {
	case "list":
		scope := ""
		if len(args) > 1 {
			scope = args[1]
		}
		mappings, err := st.ListPersonTypeRoles(scope)
		if err != nil {
			fatal("list person type roles: %v", err)
		}
		if len(mappings) == 0 {
			fmt.Println("no person type role mappings")
			return
		}
		fmt.Printf("%-20s %-12s %s\n", "SCOPE", "PERSON_TYPE", "ROLE")
		for _, m := range mappings {
			fmt.Printf("%-20s %-12d %s\n", m.Scope, m.PersonType, m.Role)
		}
	case "set":
		if len(args) < 4 {
			fatal("access person-type-roles set requires SCOPE PERSON_TYPE ROLE")
		}
		pt, err := strconv.ParseInt(args[2], 10, 64)
		if err != nil {
			fatal("invalid person type: %v", err)
		}
		if err := st.SetPersonTypeRole(args[1], pt, args[3]); err != nil {
			fatal("set person type role: %v", err)
		}
		fmt.Printf("set person type role scope=%q personType=%d role=%s\n", args[1], pt, access.NormalizeRole(args[3]))
	case "delete":
		if len(args) < 3 {
			fatal("access person-type-roles delete requires SCOPE PERSON_TYPE")
		}
		pt, err := strconv.ParseInt(args[2], 10, 64)
		if err != nil {
			fatal("invalid person type: %v", err)
		}
		if err := st.DeletePersonTypeRole(args[1], pt); err != nil {
			fatal("delete person type role: %v", err)
		}
		fmt.Printf("deleted person type role scope=%q personType=%d\n", args[1], pt)
	default:
		fatal("unknown access person-type-roles subcommand %q (use list|set|delete)", args[0])
	}
}

func cmdAccessAnonProfile(st *store.Store, args []string) {
	if len(args) == 0 {
		fatal("access anon-profile requires a subcommand (list|show|clear)")
	}
	switch args[0] {
	case "list":
		profiles, err := st.ListSchoolAnonProfiles()
		if err != nil {
			fatal("list anon profiles: %v", err)
		}
		if len(profiles) == 0 {
			fmt.Println("no anonymous profiles stored")
			return
		}
		for _, p := range profiles {
			payload := store.DecodeAnonProfilePayload(p.ProfileJSON)
			fmt.Printf("school=%s login_ok=%v source=%s detected=%s expires=%s classes=%v caps=%v notes=%q\n",
				p.School, p.LoginOK, p.Source,
				p.DetectedAt.UTC().Format(time.RFC3339), p.ExpiresAt.UTC().Format(time.RFC3339),
				payload.ClassesDisplayable, payload.Capabilities, payload.Notes)
		}
	case "show":
		if len(args) < 2 {
			fatal("access anon-profile show requires SCHOOL")
		}
		prof, err := st.GetSchoolAnonProfile(args[1])
		if err != nil {
			fatal("get anon profile: %v", err)
		}
		if prof == nil {
			fmt.Println("not found")
			return
		}
		payload := store.DecodeAnonProfilePayload(prof.ProfileJSON)
		b, _ := json.MarshalIndent(map[string]any{
			"school": prof.School, "loginOk": prof.LoginOK, "source": prof.Source,
			"detectedAt": prof.DetectedAt.UTC().Format(time.RFC3339), "expiresAt": prof.ExpiresAt.UTC().Format(time.RFC3339),
			"classesDisplayable": payload.ClassesDisplayable, "capabilities": payload.Capabilities, "notes": payload.Notes,
		}, "", "  ")
		fmt.Println(string(b))
	case "clear":
		if len(args) < 2 {
			fatal("access anon-profile clear requires SCHOOL")
		}
		if err := st.DeleteSchoolAnonProfile(args[1]); err != nil {
			fatal("delete anon profile: %v", err)
		}
		fmt.Printf("cleared anon profile for %s\n", args[1])
	default:
		fatal("unknown access anon-profile subcommand %q", args[0])
	}
}

func cmdAccessGates(st *store.Store, args []string) {
	if len(args) == 0 {
		fatal("access gates requires a subcommand (list|set)")
	}
	switch args[0] {
	case "list":
		gates, err := st.ListServerCapGates()
		if err != nil {
			fatal("list cap gates: %v", err)
		}
		if len(gates) == 0 {
			fmt.Println("no server cap gates configured")
			return
		}
		fmt.Printf("%-20s %-40s %s\n", "SCOPE", "CAPABILITY", "ENABLED")
		for _, g := range gates {
			fmt.Printf("%-20s %-40s %v\n", g.Scope, g.Cap, g.Enabled)
		}
	case "set":
		if len(args) < 4 {
			fatal("access gates set requires SCOPE CAPABILITY true|false")
		}
		scope := args[1]
		cap := args[2]
		v, err := strconv.ParseBool(args[3])
		if err != nil {
			fatal("invalid bool %q", args[3])
		}
		if err := st.SetServerCapGate(scope, cap, v); err != nil {
			fatal("set cap gate: %v", err)
		}
		fmt.Printf("set gate scope=%s cap=%s enabled=%v\n", scope, cap, v)
	default:
		fatal("unknown access gates subcommand %q", args[0])
	}
}

func permsList(st *store.Store, user string) {
	rows, err := st.AllPerms()
	if err != nil {
		fatal("list perms: %v", err)
	}
	if len(rows) == 0 {
		fmt.Println("no permissions set (class-pool-only for everyone)")
		return
	}
	// Summarise: show global switches and per-user overrides, most relevant first.
	type summary struct {
		feature, scope string
		allowed        bool
	}
	var seen []summary
	matched := false
	for _, r := range rows {
		scope := r.Username
		if r.Username == "*" {
			scope = "(global)"
		}
		if user != "" && r.Username != "*" && !strings.EqualFold(r.Username, user) {
			continue
		}
		if r.Username != "*" {
			matched = true
		}
		seen = append(seen, summary{r.Feature, scope, r.Allowed})
	}
	if user != "" && !matched {
		fmt.Printf("no per-user overrides for %q (falls back to global switches)\n", user)
	}
	fmt.Printf("%-9s %-12s %s\n", "TYPE", "SCOPE", "ALLOWED")
	for _, s := range seen {
		fmt.Printf("%-9s %-12s %v\n", s.feature, s.scope, s.allowed)
	}
}

func permSet(st *store.Store, action string, args []string) {
	fs := flag.NewFlagSet("perms "+action, flag.ExitOnError)
	user := fs.String("user", "", "per-user override")
	global := fs.Bool("global", false, "global switch for all users")
	fs.Parse(args)

	rest := fs.Args()
	if len(rest) == 0 {
		fatal("perms %s requires a TYPE (e.g. 'perms %s --user U recon')", action, action)
	}
	typ, ok := reconTypes[strings.ToLower(rest[0])]
	if !ok {
		fatal("unknown type %q (use recon|boosted|editor|techniker)", rest[0])
	}
	if *user != "" && *global {
		fatal("pick --user (per-user) or --global (all users), not both")
	}
	if *user == "" && !*global {
		fatal("require --user U or --global")
	}
	if *global && perUserOnly[typ] {
		fatal("%s is per-user only; use --user", typ)
	}
	allowed := action == "grant"
	var err error
	if *global {
		err = st.SetReconType(typ, allowed)
	} else if perUserOnly[typ] {
		err = st.SetPerm(*user, typ, allowed)
	} else {
		err = st.SetReconOverride(*user, typ, allowed)
	}
	if err != nil {
		fatal("%s %s: %v", action, typ, err)
	}
	target := "(global)"
	if *user != "" {
		target = *user
		var on bool
		switch typ {
		case store.FeatureBoosted:
			on, _ = st.BoostedAccess(*user)
		case store.FeatureEditor:
			on, _ = st.EditorAccess(*user)
		default:
			on, _ = st.ReconAccess(*user, typ)
		}
		fmt.Printf("%s %s for %q (effective=%v)\n", past(action), typ, *user, on)
		return
	}
	fmt.Printf("%s %s for %s\n", past(action), typ, target)
}

func past(action string) string {
	if action == "revoke" {
		return "revoked"
	}
	return "granted"
}

func permClear(st *store.Store, args []string) {
	fs := flag.NewFlagSet("perms clear", flag.ExitOnError)
	user := fs.String("user", "", "user whose overrides to clear")
	fs.Parse(args)
	if *user == "" {
		fatal("perms clear requires --user")
	}
	n, err := st.ClearReconOverrides(*user)
	if err != nil {
		fatal("clear overrides: %v", err)
	}
	fmt.Printf("cleared %d override(s) for %q (falls back to global switches)\n", n, *user)
}

// ---------------------------------------------------------------------------
// users
// ---------------------------------------------------------------------------

func cmdUsers(st *store.Store, args []string, server, school string) {
	if len(args) == 0 {
		fatal("users requires a subcommand (list|add|remove|check)")
	}
	switch args[0] {
	case "list":
		usersList(st)
	case "add":
		userAdd(st, args[1:])
	case "remove":
		userRemove(st, args[1:])
	case "admin":
		userAdmin(st, args[1:])
	case "check":
		userCheck(st, server, school, args[1:])
	default:
		fatal("unknown users subcommand %q", args[0])
	}
}

func usersList(st *store.Store) {
	users, err := st.ListUsers()
	if err != nil {
		fatal("list users: %v", err)
	}
	if len(users) == 0 {
		fmt.Println("no users")
		return
	}
	fmt.Printf("%-12s %-8s %-6s %-8s %-8s %-22s\n", "USER", "METHOD", "TYPE", "CLASS", "PERSON", "DISPLAY")
	for _, u := range users {
		fmt.Printf("%-12s %-8s %-6d %-8d %-8d %-22s\n", u.Username, u.Method, u.PersonType, u.ClassID, u.PersonID, u.DisplayName)
	}
}

func userAdd(st *store.Store, args []string) {
	fs := flag.NewFlagSet("users add", flag.ExitOnError)
	user := fs.String("user", "", "username")
	secret := fs.String("secret", "", "password or base32 TOTP key")
	method := fs.String("method", "key", "password or key")
	fs.Parse(args)
	if *user == "" || *secret == "" {
		fatal("users add requires --user and --secret")
	}
	if *method != "password" && *method != "key" {
		fatal("--method must be 'password' or 'key'")
	}
	u := &store.User{Username: *user, Password: *secret, Method: *method}
	if err := st.UpsertUser(u); err != nil {
		fatal("add user: %v", err)
	}
	if *method == "key" {
		if err := st.UpsertSecret(*user, *secret); err != nil {
			fatal("add secret: %v", err)
		}
	} else {
		if err := st.SetAccountPassword(*user, *secret); err != nil {
			fatal("add password: %v", err)
		}
	}
	fmt.Printf("added user %q (method=%s)\n", *user, *method)
}

func userAdmin(st *store.Store, args []string) {
	fs := flag.NewFlagSet("users admin", flag.ExitOnError)
	user := fs.String("user", "", "username")
	on := fs.Bool("on", true, "grant admin (default)")
	off := fs.Bool("off", false, "revoke admin")
	fs.Parse(args)
	if *user == "" {
		fatal("users admin requires --user")
	}
	if err := st.SetAdmin(*user, *on && !*off); err != nil {
		fatal("set admin: %v", err)
	}
	fmt.Printf("user %q admin=%v\n", *user, *on && !*off)
}

func userRemove(st *store.Store, args []string) {
	fs := flag.NewFlagSet("users remove", flag.ExitOnError)
	user := fs.String("user", "", "username")
	fs.Parse(args)
	if *user == "" {
		fatal("users remove requires --user")
	}
	n, err := st.DeleteUser(*user)
	if err != nil {
		fatal("remove user: %v", err)
	}
	fmt.Printf("removed user %q (%d related row(s) cleaned)\n", *user, n)
}

// userCheckAPI is the subset of the upstream Untis API used by `users check`.
// Keeping it behind an interface lets the per-school routing be tested without
// a network.
type userCheckAPI interface {
	Session(school, username, secret, method string) (string, error)
	GetKlassen(school, cookie string) ([]byte, error)
	PersonInfo(school, cookie string) (*untis.PersonInfo, error)
	Logout(school, cookie string)
}

// userCheck asks the real Untis API what class each stored account belongs to
// (via a real session from its saved credential). Each account is checked
// against its own school; the global -school is only a fallback for legacy rows
// that predate per-user schools. Students the API reports as having no class are
// flagged CLASSLESS; with --remove-classless they are deleted. Teachers
// (personType != 5) are never deleted.
func userCheck(st *store.Store, server, school string, args []string) {
	fs := flag.NewFlagSet("users check", flag.ExitOnError)
	remove := fs.Bool("remove-classless", false, "delete student accounts the API reports without a class")
	fs.Parse(args)

	uc := untis.New(untis.Config{Server: server, School: school})
	checkUsers(st, uc, school, *remove, os.Stdout)
}

// checkUsers validates every account against the school recorded on that
// account. Class IDs are only unique within one school, so class names are
// cached per school; sharing one map would label a class with another school's
// name whenever the numeric IDs collide.
func checkUsers(st *store.Store, uc userCheckAPI, fallbackSchool string, remove bool, w io.Writer) {
	users, err := st.ListUsers()
	if err != nil {
		fatal("users check: %v", err)
	}

	klassen := map[string]map[int64]string{}

	fmt.Fprintf(w, "%-12s %-8s %-9s %-9s %-20s %-14s %s\n", "USER", "METHOD", "API_TYPE", "CLASS_ID", "SCHOOL", "CLASS", "VERDICT")
	removed := 0
	for _, u := range users {
		if u.Password == "" {
			fmt.Fprintf(w, "%-12s %-8s  (no saved credential, skipped)\n", u.Username, u.Method)
			continue
		}
		userSchool := u.School
		if userSchool == "" {
			// Rows created before schools were stored per user have no school.
			// They can only be checked against the explicit global fallback.
			userSchool = fallbackSchool
		}
		if userSchool == "" {
			fmt.Fprintf(w, "%-12s %-8s  (no school on the account and no fallback -school, skipped)\n", u.Username, u.Method)
			continue
		}

		ck, err := uc.Session(userSchool, u.Username, u.Password, u.Method)
		if err != nil {
			fmt.Fprintf(w, "%-12s %-8s school %s: login failed: %v\n", u.Username, u.Method, userSchool, err)
			continue
		}
		// Fetch the class list once per school, from the first account that can
		// read it. Failed attempts are retried with the next account of that
		// school instead of leaving every later row without a class name.
		if _, ok := klassen[userSchool]; !ok {
			if names, err := fetchKlassen(uc, userSchool, ck); err == nil && len(names) > 0 {
				klassen[userSchool] = names
			}
		}
		info, _ := uc.PersonInfo(userSchool, ck)
		uc.Logout(userSchool, ck)
		if info != nil && info.LinkedPersons != nil {
			if links, e := json.Marshal(info.LinkedPersons); e == nil {
				_ = st.SaveAccountLinks(userSchool, u.Username, info.PersonID, links)
			}
		}
		if info == nil {
			info = &untis.PersonInfo{}
		}

		verdict := "keep"
		if info.PersonID == 0 && info.PersonType == 0 && info.ClassID == 0 {
			verdict = "info unavailable"
		} else if info.PersonType == 5 && info.ClassID == 0 {
			verdict = "CLASSLESS"
			if remove {
				if _, err := st.DeleteUser(u.Username); err != nil {
					fmt.Fprintf(w, "%-12s remove failed: %v\n", u.Username, err)
					continue
				}
				verdict = "REMOVED"
				removed++
			}
		}
		fmt.Fprintf(w, "%-12s %-8s %-9d %-9d %-20s %-14s %s\n", u.Username, u.Method, info.PersonType, info.ClassID, userSchool, klassen[userSchool][info.ClassID], verdict)
	}
	if removed > 0 {
		fmt.Fprintf(w, "removed %d classless account(s)\n", removed)
	}
}

// fetchKlassen parses one school's getKlassen result into an id -> name map.
func fetchKlassen(uc userCheckAPI, school, cookie string) (map[int64]string, error) {
	b, err := uc.GetKlassen(school, cookie)
	if err != nil {
		return nil, err
	}
	var res struct {
		Result []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return nil, err
	}
	names := make(map[int64]string, len(res.Result))
	for _, c := range res.Result {
		names[c.ID] = c.Name
	}
	return names, nil
}

// ---------------------------------------------------------------------------
// pool
// ---------------------------------------------------------------------------

func cmdPool(st *store.Store, args []string, school string) {
	if len(args) == 0 {
		poolList(st, school)
		return
	}
	switch args[0] {
	case "list":
		poolList(st, school)
	case "owners":
		// list already shows owners; no distinct owners view needed
		poolList(st, school)
	default:
		fatal("unknown pool subcommand %q", args[0])
	}
}

func poolList(st *store.Store, school string) {
	classes, err := st.Pool(school)
	if err != nil {
		fatal("pool list: %v", err)
	}
	if len(classes) == 0 {
		fmt.Println("no pooled classes (no users with a class_id)")
		return
	}
	fmt.Printf("%-8s %-20s %s\n", "CLASS", "NAME", "OWNER")
	for _, c := range classes {
		owner, _ := st.OwnerForClass(school, c.ID)
		ownerName := "-"
		if owner != nil {
			ownerName = owner.Username
		}
		fmt.Printf("%-8d %-20s %s\n", c.ID, c.Name, ownerName)
	}
}

// ---------------------------------------------------------------------------
// calendar (also aliased as "tokens" for backward compat)
// ---------------------------------------------------------------------------

func cmdCalendar(st *store.Store, args []string) {
	if len(args) == 0 {
		calendarList(st)
		return
	}
	switch args[0] {
	case "create":
		calendarCreate(st, args[1:])
	case "student":
		calendarStudent(st, args[1:])
	case "list":
		calendarList(st)
	case "revoke":
		if len(args) < 2 {
			fatal("calendar revoke requires a TOKEN")
		}
		n, err := st.DeleteClassToken(args[1])
		if err != nil {
			fatal("revoke token: %v", err)
		}
		if n == 0 {
			fmt.Println("token not found")
		} else {
			fmt.Println("token revoked")
		}
	default:
		fatal("unknown calendar subcommand %q (use create|student|list|revoke)", args[0])
	}
}

func calendarCreate(st *store.Store, args []string) {
	fs := flag.NewFlagSet("calendar create", flag.ExitOnError)
	classID := fs.Int64("class", 0, "class ID (must be in pool)")
	teacher := fs.String("teacher", "", "teacher name or ID (fuzzy lookup)")
	room := fs.String("room", "", "room name or ID (fuzzy lookup)")
	subject := fs.String("subject", "", "subject name or ID (fuzzy lookup)")
	personal := fs.Bool("personal", false, "personal student timetable (self)")
	user := fs.String("user", "", "personal timetable for this user (with --personal)")
	school := fs.String("school", os.Getenv("UNTIS_SCHOOL"), "school name (default $UNTIS_SCHOOL)")
	timezone := fs.String("timezone", "Europe/Berlin", "timezone for ICS events")
	days := fs.Int("days", 30, "lookahead horizon in days (1-365)")
	base := fs.String("base", defaultPublicBase, "public base URL for the .ics link")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), `Create a calendar subscription token.

Exactly one of --class, --teacher, --room, --subject, or --personal is required.

Examples:
  untisctl calendar create --class 5000
  untisctl calendar create --teacher 'Muster, Max'
  untisctl calendar create --room Aula
  untisctl calendar create --subject BIO
  untisctl calendar create --personal
  untisctl calendar create --personal --user jdoe`)
	}
	if err := fs.Parse(args); err != nil {
		return
	}
	if *days < 1 {
		*days = 1
	}
	if *days > 365 {
		*days = 365
	}

	// Count targets
	nTargets := 0
	if *classID > 0 {
		nTargets++
	}
	if *teacher != "" {
		nTargets++
	}
	if *room != "" {
		nTargets++
	}
	if *subject != "" {
		nTargets++
	}
	if *personal {
		nTargets++
	}
	if nTargets != 1 {
		fatal("exactly one of --class, --teacher, --room, --subject, or --personal is required")
	}

	// Resolve element
	var elType string
	var elID int64
	switch {
	case *classID > 0:
		elType = "CLASS"
		elID = *classID
		// Validate class is in pool
		ok, err := st.PoolContains(*school, elID)
		if err != nil || !ok {
			fatal("class %d is not in the pool", elID)
		}
	case *teacher != "":
		elType = "TEACHER"
		var err error
		elID, err = st.LookupElement(*school, "TEACHER", *teacher)
		if err != nil {
			fatal("teacher lookup: %v", err)
		}
	case *room != "":
		elType = "ROOM"
		var err error
		elID, err = st.LookupElement(*school, "ROOM", *room)
		if err != nil {
			fatal("room lookup: %v", err)
		}
	case *subject != "":
		elType = "SUBJECT"
		var err error
		elID, err = st.LookupElement(*school, "SUBJECT", *subject)
		if err != nil {
			fatal("subject lookup: %v", err)
		}
	case *personal:
		elType = "STUDENT"
		if *user != "" {
			// Look up the user's person ID
			u, err := st.GetUser(*user)
			if err != nil || u == nil {
				fatal("user %q not found", *user)
			}
			if u.PersonID <= 0 {
				fatal("user %q has no person ID", *user)
			}
			elID = u.PersonID
		} else {
			// For personal without --user, we need at least one user with a
			// person ID. In the CLI context (no session), just use the first
			// student user.
			fatal("--personal requires --user when used from the CLI")
		}
	}

	// Check for existing token
	existing, err := st.ClassTokenForElement(*school, elType, elID, "")
	if err != nil {
		fatal("store: %v", err)
	}
	if existing != nil {
		fmt.Printf("existing token for %s %d:\n", elType, elID)
		printToken(existing, *base)
		return
	}

	// Create new token
	now := time.Now().Unix()
	tok := &store.ClassToken{
		Token:       generateToken(),
		School:      *school,
		ElementType: elType,
		ElementID:   elID,
		ClassID:     elID, // backward compat
		Timezone:    *timezone,
		Days:        *days,
		CreatedAt:   now,
		LastAccess:  now,
	}
	if err := st.CreateClassToken(tok); err != nil {
		fatal("create token: %v", err)
	}

	fmt.Printf("created %s calendar token:\n", strings.ToLower(elType))
	printToken(tok, *base)
}

// calendarStudent mints a personal calendar token for a student picked from
// the database. Without --user it shows an interactive numbered menu of every
// student account, so an operator can hand a student their .ics URL.
func calendarStudent(st *store.Store, args []string) {
	fs := flag.NewFlagSet("calendar student", flag.ExitOnError)
	user := fs.String("user", "", "pick this student non-interactively")
	school := fs.String("school", os.Getenv("UNTIS_SCHOOL"), "school name (default $UNTIS_SCHOOL)")
	days := fs.Int("days", 30, "lookahead horizon in days (1-365)")
	base := fs.String("base", defaultPublicBase, "public base URL for the .ics link")
	if err := fs.Parse(args); err != nil {
		return
	}
	if *days < 1 {
		*days = 1
	}
	if *days > 365 {
		*days = 365
	}

	students, err := st.ListStudents(*school)
	if err != nil {
		fatal("list students: %v", err)
	}
	if len(students) == 0 {
		fatal("no students with a person ID in school %q", *school)
	}

	var picked *store.User
	if *user != "" {
		for _, s := range students {
			if s.Username == *user {
				picked = s
				break
			}
		}
		if picked == nil {
			fatal("student %q not found (use 'users list' to see accounts)", *user)
		}
	} else {
		fmt.Println("students in the DB:")
		fmt.Printf("%3s  %-12s  %-32s  %s\n", "#", "user", "name", "class")
		for i, s := range students {
			name := s.DisplayName
			if name == "" {
				name = "<no name>"
			}
			fmt.Printf("%3d  %-12s  %-32s  %s\n", i+1, s.Username, name, s.ClassName)
		}
		fmt.Print("select student [1-N, blank to cancel]: ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			fmt.Println("cancelled")
			return
		}
		n, err := strconv.Atoi(line)
		if err != nil || n < 1 || n > len(students) {
			fatal("invalid selection %q", line)
		}
		picked = students[n-1]
	}
	if picked.PersonID <= 0 {
		fatal("user %q has no person ID", picked.Username)
	}

	existing, err := st.ClassTokenForElement(*school, "STUDENT", picked.PersonID, "")
	if err != nil {
		fatal("store: %v", err)
	}
	if existing != nil {
		fmt.Printf("existing personal token for %s:\n", picked.Username)
		printToken(existing, *base)
		return
	}
	now := time.Now().Unix()
	tok := &store.ClassToken{
		Token:       generateToken(),
		School:      *school,
		ElementType: "STUDENT",
		ElementID:   picked.PersonID,
		PersonID:    picked.PersonID,
		Timezone:    "Europe/Berlin",
		Days:        *days,
		CreatedAt:   now,
		LastAccess:  now,
		CreatedBy:   "untisctl",
	}
	if err := st.CreateClassToken(tok); err != nil {
		fatal("create token: %v", err)
	}
	fmt.Printf("created personal calendar token for %s (%s):\n", picked.Username, picked.DisplayName)
	printToken(tok, *base)
}

func calendarList(st *store.Store) {
	tokens, err := st.ListClassTokens()
	if err != nil {
		fatal("calendar list: %v", err)
	}
	if len(tokens) == 0 {
		fmt.Println("no calendar tokens")
		return
	}
	fmt.Printf("%-10s %-12s %-8s %-34s %-5s %-22s %s\n", "TYPE", "SCHOOL", "ID", "TOKEN", "DAYS", "LAST ACCESS", "TIMEZONE")
	for _, t := range tokens {
		elType := t.ElementType
		if elType == "" {
			if t.PersonID > 0 {
				elType = "STUDENT"
			} else {
				elType = "CLASS"
			}
		}
		elID := t.ElementID
		if elID == 0 {
			if t.PersonID > 0 {
				elID = t.PersonID
			} else {
				elID = t.ClassID
			}
		}
		last := "never"
		if t.LastAccess > 0 {
			last = time.Unix(t.LastAccess, 0).Format(time.RFC3339)
		}
		tz := t.Timezone
		if tz == "" {
			tz = "Europe/Berlin"
		}
		d := t.Days
		if d <= 0 {
			d = 30
		}
		fmt.Printf("%-10s %-12s %-8d %-34s %-5d %-22s %s\n", elType, t.School, elID, t.Token, d, last, tz)
	}
}

func printToken(t *store.ClassToken, base string) {
	days := t.Days
	if days <= 0 {
		days = 30
	}
	fmt.Printf("  token:     %s\n", t.Token)
	fmt.Printf("  url:       %s/api/calendar/%s.ics\n", strings.TrimRight(base, "/"), t.Token)
	fmt.Printf("  type:      %s\n", t.ElementType)
	fmt.Printf("  id:        %d\n", t.ElementID)
	fmt.Printf("  timezone:  %s\n", t.Timezone)
	fmt.Printf("  days:      %d (horizon: %s)\n", days, time.Now().AddDate(0, 0, days).Format("2006-01-02"))
}

func generateToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------------------
// totp
// ---------------------------------------------------------------------------

// totpCode computes a standard 6-digit TOTP code (SHA1, 30s step) for the
// given raw secret key at a unix timestamp, matching the Ente Auth / Google
// Authenticator base32 secrets the proxy stores per user.
func totpCode(key []byte, t int64) string {
	msg := make([]byte, 8)
	binary.BigEndian.PutUint64(msg, uint64(t/30))
	mac := hmac.New(sha1.New, key)
	_, _ = mac.Write(msg)
	d := mac.Sum(nil)
	o := d[len(d)-1] & 0x0f
	num := (binary.BigEndian.Uint32(d[o:o+4]) & 0x7fffffff) % 1000000
	return fmt.Sprintf("%06d", num)
}

// cmdTotp prints the current 6-digit TOTP code for an account (and seconds
// left) or, with --scan, the otpauth:// URI an authenticator app can scan.
// It reads the stored OTP secret (users.password, falling back to the secrets
// table), so it matches the live 'keyLogin' codes.
func cmdTotp(st *store.Store, args []string) {
	scan := false
	var rest []string
	for _, a := range args {
		if a == "--scan" || a == "-scan" {
			scan = true
		} else {
			rest = append(rest, a)
		}
	}
	if len(rest) != 1 {
		fatal("totp requires a USERNAME (got %d arguments)", len(rest))
	}
	user := rest[0]

	secret := ""
	if u, err := st.GetUser(user); err == nil && u != nil && u.Password != "" {
		secret = u.Password
	} else if s, err := st.GetSecret(user); err == nil && s != "" {
		secret = s
	}
	if secret == "" {
		fatal("no TOTP secret stored for %q (users.password or the secrets table is empty)", user)
	}

	uri := fmt.Sprintf("otpauth://totp/untis:%s?secret=%s&issuer=untis", user, secret)

	if scan {
		fmt.Println(uri)
		if qr, err := exec.LookPath("qrencode"); err == nil {
			if out, err := exec.Command(qr, "-o", "-", "-t", "ANSIUTF8", uri).Output(); err == nil {
				fmt.Print(string(out))
			}
		}
		return
	}

	key := strings.ReplaceAll(strings.ToUpper(secret), " ", "")
	pad := (8 - len(key)%8) % 8
	dec, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(key)
	if err != nil {
		dec, err = base32.StdEncoding.DecodeString(key + strings.Repeat("=", pad))
		if err != nil {
			fatal("bad base32 secret for %q: %v", user, err)
		}
	}
	now := time.Now().Unix()
	fmt.Printf("%s  (%ds left)\n", totpCode(dec, now), 30-now%30)
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

func cmdStatus(st *store.Store, school string) {
	uc, _ := st.UserCount()
	pool, _ := st.Pool(school)
	rows, _ := st.AllPerms()
	tokens, _ := st.ListClassTokens()
	recon, _ := st.LoadReconElements(school)

	var permCount int
	var globalCount, overrideCount int
	for _, r := range rows {
		permCount++
		if r.Username == "*" {
			globalCount++
		} else {
			overrideCount++
		}
	}

	reconTotal := 0
	for _, ids := range recon {
		reconTotal += len(ids)
	}

	fmt.Printf("users:      %d\n", uc)
	fmt.Printf("pool:       %d classes\n", len(pool))
	fmt.Printf("recon:      %d elements (%d teachers, %d rooms, %d subjects)\n",
		reconTotal, len(recon["TEACHER"]), len(recon["ROOM"]), len(recon["SUBJECT"]))
	fmt.Printf("perms:      %d rows (%d global, %d per-user)\n", permCount, globalCount, overrideCount)
	fmt.Printf("tokens:     %d calendar tokens\n", len(tokens))
}

// cmdBackup writes a consistent copy of the database next to the live file.
// It is the command to run before every upgrade: the DB holds every account
// secret, calendar token and notification subscription, and an upgrade runs
// migrations that cannot be undone.
func cmdBackup(st *store.Store, dbPath string, args []string) {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	out := fs.String("out", "", "destination file (default <db>.untis-backup-<UTC timestamp>.db)")
	keep := fs.Int("keep", 0, "delete all but the N newest backups of this database (0 = keep all)")
	if err := fs.Parse(args); err != nil {
		fatal("parse flags: %v", err)
	}

	dst := *out
	if dst == "" {
		dst = dbPath + ".untis-backup-" + store.BackupSuffix(time.Now()) + ".db"
	}
	size, err := st.BackupTo(dst)
	if err != nil {
		fatal("%v", err)
	}
	fmt.Printf("backup written: %s (%s)\n", dst, humanBytes(size))

	if *keep > 0 {
		pruneBackups(dbPath, *keep)
	}
}

// backupPrefix is the marker in automatic backup file names, so pruning can
// never touch a file the operator created by hand.
func backupPrefix(dbPath string) string {
	return filepath.Base(dbPath) + ".untis-backup-"
}

// pruneBackups keeps the newest n automatic backups of dbPath.
func pruneBackups(dbPath string, n int) {
	dir := filepath.Dir(dbPath)
	if dir == "" {
		dir = "."
	}
	prefix := backupPrefix(dbPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: read %s: %v\n", dir, err)
		return
	}
	if n <= 0 {
		// 0 means "keep everything"; pruning to zero backups would be a
		// spectacularly unhelpful reading of the flag.
		return
	}
	var found []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".db") {
			continue
		}
		found = append(found, name)
	}
	// Suffixes are UTC timestamps, so lexical order is chronological order.
	sort.Strings(found)
	if len(found) <= n {
		return
	}
	for _, name := range found[:len(found)-n] {
		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil {
			fmt.Fprintf(os.Stderr, "warning: remove %s: %v\n", path, err)
			continue
		}
		fmt.Printf("pruned old backup: %s\n", path)
	}
}

// humanBytes renders a byte count for the backup summary.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}
