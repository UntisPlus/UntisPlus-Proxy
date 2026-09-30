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
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

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
	db := flag.String("db", os.Getenv("UNTIS_DB"), "path to the untis sqlite database (default: $UNTIS_DB, else untis.db)")
	server := flag.String("server", os.Getenv("UNTIS_SERVER"), "fallback upstream host (default: $UNTIS_SERVER, else auto-resolved per school)")
	school := flag.String("school", os.Getenv("UNTIS_SCHOOL"), "upstream school name (default: $UNTIS_SCHOOL)")
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, `untisctl – manage an untis-proxy database

Usage:
  untisctl [-db PATH] <command> [options]

Commands:
  perms list [--user U]              show global switches and per-user overrides
  perms grant|revoke [--user U|--global] TYPE
                                     grant/revoke recon|boosted|editor (per-user only)
  perms clear --user U               drop a user's overrides (fall back to global)
  perms reset                        wipe all permissions (class-pool-only default)

  users list                         list accounts
  users add --user U --secret S [--method password|key]
  users remove --user U              delete user + secret + overrides + personal tokens
  users admin --user U [--on|--off]  grant or revoke admin dashboard access
  users check [--remove-classless]   ask the real Untis API for each account's class;
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
	case "perms":
		cmdPerms(st, flag.Args()[1:])
	case "users":
		cmdUsers(st, flag.Args()[1:], *server, *school)
	case "pool":
		cmdPool(st, flag.Args()[1:], *school)
	case "calendar", "tokens":
		cmdCalendar(st, flag.Args()[1:])
	case "totp":
		cmdTotp(st, flag.Args()[1:])
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
}

// boostedFeatures are per-user-only permissions; they cannot be applied globally.
var boostedFeatures = map[string]bool{
	store.FeatureBoosted: true,
	store.FeatureEditor:  true,
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
	default:
		fatal("unknown perms subcommand %q", args[0])
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
		fatal("unknown type %q (use recon|boosted)", rest[0])
	}
	if *user != "" && *global {
		fatal("pick --user (per-user) or --global (all users), not both")
	}
	if *user == "" && !*global {
		fatal("require --user U or --global")
	}
	if *global && boostedFeatures[typ] {
		fatal("boosting features (%s) are per-user only; use --user", typ)
	}
	allowed := action == "grant"
	var err error
	if *global {
		err = st.SetReconType(typ, allowed)
	} else if boostedFeatures[typ] {
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

// userCheck asks the real Untis API what class each stored account belongs to
// (via a real session from its saved credential). Students the API reports as
// having no class are flagged CLASSLESS; with --remove-classless they are
// deleted. Teachers (personType != 5) are never deleted.
func userCheck(st *store.Store, server, school string, args []string) {
	fs := flag.NewFlagSet("users check", flag.ExitOnError)
	remove := fs.Bool("remove-classless", false, "delete student accounts the API reports without a class")
	fs.Parse(args)

	users, err := st.ListUsers()
	if err != nil {
		fatal("users check: %v", err)
	}
	uc := untis.New(untis.Config{Server: server, School: school})

	// Build id -> name from whichever account can fetch the class list.
	klassen := map[int64]string{}
	for _, u := range users {
		if u.Password == "" {
			continue
		}
		ck, err := uc.Session(school, u.Username, u.Password, u.Method)
		if err != nil {
			continue
		}
		if b, err := uc.GetKlassen(school, ck); err == nil {
			var res struct {
				Result []struct {
					ID   int64  `json:"id"`
					Name string `json:"name"`
				} `json:"result"`
			}
			if json.Unmarshal(b, &res) == nil {
				for _, c := range res.Result {
					klassen[c.ID] = c.Name
				}
			}
			uc.Logout(school, ck)
			if len(klassen) > 0 {
				break
			}
		} else {
			uc.Logout(school, ck)
		}
	}

	fmt.Printf("%-12s %-8s %-9s %-9s %-14s %s\n", "USER", "METHOD", "API_TYPE", "CLASS_ID", "CLASS", "VERDICT")
	removed := 0
	for _, u := range users {
		if u.Password == "" {
			fmt.Printf("%-12s %-8s  (no saved credential, skipped)\n", u.Username, u.Method)
			continue
		}
		ck, err := uc.Session(school, u.Username, u.Password, u.Method)
		if err != nil {
			fmt.Printf("%-12s %-8s login failed: %v\n", u.Username, u.Method, err)
			continue
		}
		info, _ := uc.PersonInfo(school, ck)
		uc.Logout(school, ck)

		verdict := "keep"
		if info.PersonID == 0 && info.PersonType == 0 && info.ClassID == 0 {
			verdict = "info unavailable"
		} else if info.PersonType == 5 && info.ClassID == 0 {
			verdict = "CLASSLESS"
			if *remove {
				if _, err := st.DeleteUser(u.Username); err != nil {
					fmt.Printf("%-12s remove failed: %v\n", u.Username, err)
					continue
				}
				verdict = "REMOVED"
				removed++
			}
		}
		fmt.Printf("%-12s %-8s %-9d %-9d %-14s %s\n", u.Username, u.Method, info.PersonType, info.ClassID, klassen[info.ClassID], verdict)
	}
	if removed > 0 {
		fmt.Printf("removed %d classless account(s)\n", removed)
	}
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
	existing, err := st.ClassTokenForElement(*school, elType, elID)
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

	existing, err := st.ClassTokenForElement(*school, "STUDENT", picked.PersonID)
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
