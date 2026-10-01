package proxy

import (
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"untis-proxy/internal/session"
	"untis-proxy/internal/store"
	"untis-proxy/internal/untis"
)

type Options struct {
	School string
	TTL    time.Duration
	Admin  []string
	// ReconRefreshDays is how stale a class's recon scan horizon may get before
	// the next scan re-enumerates it. 0 means defaultReconRefresh.
	ReconRefreshDays int
	// ForceRescan re-enumerates every pooled class from the year start on the
	// next scan instead of resuming from the stored horizon.
	ForceRescan bool
}

type Proxy struct {
	store    *store.Store
	untis    *untis.Client
	sessions *session.Manager
	opts     Options
	tt       *ttCache

	secretsMu sync.Mutex
	secrets   map[string]string

	// mdRaw caches the *unmodified* getUserData2017 result body per school.
	// displayAllowed is stamped per request from it, so permission changes
	// apply without a re-login and one user's grants never leak into another
	// user's element menus.
	mdRawMu sync.Mutex
	mdRaw   map[string][]byte

	hub *notifyHub

	// schools holds per-school live state (klasses, recon, master data).
	schoolsMu   sync.Mutex
	schools     map[string]*schoolState
	reconActive map[string]bool

	// rescanReq is set by RequestRescan so the next StartRecon sweep ignores
	// the stored horizons and re-enumerates every class.
	rescanReq atomic.Bool

	// absenceDiag throttles the "found no absence list" log, so an unexpected
	// response shape is reported occasionally instead of on every request.
	absenceDiag *absenceDiagnostics
}

// RequestRescan makes the next recon sweep re-enumerate every pooled class
// from the year start instead of resuming from the stored scan horizon.
func (p *Proxy) RequestRescan() { p.rescanReq.Store(true) }

// schoolState is the per-school live state (caches included) so a single
// process can serve many Untis schools with independent pooling, recon and
// master data.
type schoolState struct {
	klMu    sync.Mutex
	klasses map[int64]string
	klAt    time.Time

	// poll tracks change-detector health for this school so /healthz and
	// /metrics can report a stale upstream instead of a silent dead poller.
	pollMu      sync.Mutex
	pollRuns    int64
	pollChanges int64
	pollFails   int64
	lastPoll    time.Time
	lastPollOK  time.Time

	recon *elementDB

	mdMu   sync.Mutex
	md     *masterDataCache
	mdNext time.Time
}

type schoolKey string

// masterDataCache holds name lookups from getUserData2017 masterData, used to
// render the weekly REST response's element descriptors.
type masterDataCache struct {
	teachers map[int64]string
	rooms    map[int64]string
	subjects map[int64]string
	klassen  map[int64]string
	// absenceReasons resolves an absence's absenceReasonId to readable text, so a
	// client does not have to show the raw two-digit code.
	absenceReasons map[int64]string
}

func New(st *store.Store, uc *untis.Client, sm *session.Manager, opts Options) *Proxy {
	if opts.TTL <= 0 {
		opts.TTL = 5 * time.Minute
	}
	return &Proxy{
		store:       st,
		untis:       uc,
		sessions:    sm,
		opts:        opts,
		tt:          newTTCache(opts.TTL),
		secrets:     map[string]string{},
		hub:         newNotifyHub(),
		schools:     map[string]*schoolState{},
		absenceDiag: &absenceDiagnostics{},
	}
}

// stateFor returns the per-school live state, creating it on first use and
// registering the school in the DB. Request paths that resolve `school` call
// this so a brand-new school (login from a school never seen before)
// auto-registers with its own pool, recon and master data.
func (p *Proxy) stateFor(school string) *schoolState {
	if school == "" {
		school = p.opts.School
	}
	p.schoolsMu.Lock()
	defer p.schoolsMu.Unlock()
	st, ok := p.schools[school]
	if ok {
		return st
	}
	st = &schoolState{klasses: map[int64]string{}}
	st.recon = newElementDB()
	p.schools[school] = st
	if elems, err := p.store.LoadReconElements(school); err == nil && len(elems) > 0 {
		st.recon.seedFrom(elems)
	}
	_ = p.store.UpsertSchool(school)
	log.Printf("[multi-school] auto-registered school %q", school)
	return st
}

// isNewSchool reports whether a school has never been registered before,
// without registering it (login handlers use this to decide whether to kick
// off a fresh recon scan).
func (p *Proxy) isNewSchool(school string) bool {
	known, err := p.store.KnownSchool(school)
	return err == nil && !known
}

// schoolYearRange mirrors the server's schoolYear helper so login-triggered
// recon scans (and any future per-school scans) use the same German school year
// bounds as the boot-time scan.
func schoolYearRange(now time.Time) (string, string) {
	start := time.Date(now.Year(), time.August, 1, 0, 0, 0, 0, now.Location())
	if now.Month() < time.August {
		start = start.AddDate(-1, 0, 0)
	}
	end := start.AddDate(1, 0, 0).AddDate(0, 0, -1)
	return start.Format("2006-01-02"), end.Format("2006-01-02")
}

// ensureReconScan starts a per-school background recon enumeration if the
// school's pool is non-empty. It is safe to call concurrently and idempotent
// per school. login/augment paths call it so a school that logs in for the
// first time gets its teacher/room/subject set populated automatically.
func (p *Proxy) ensureReconScan(school string) {
	classes, err := p.store.Pool(school)
	if err != nil || len(classes) == 0 {
		return
	}
	if school == p.opts.School {
		return // owned by StartRecon at boot
	}
	p.schoolsMu.Lock()
	_, active := p.reconActive[school]
	if !active {
		if p.reconActive == nil {
			p.reconActive = map[string]bool{}
		}
		p.reconActive[school] = true
	}
	p.schoolsMu.Unlock()
	if active {
		return
	}
	now := time.Now()
	ys, ye := schoolYearRange(now)
	log.Printf("[multi-school] starting recon for newly-logged-in school %q", school)
	p.StartRecon(school, ys, ye)
}

// masterData returns cached masterData name maps, refreshing them at most once
// an hour via any pool account.
func (p *Proxy) masterData(school string) *masterDataCache {
	st := p.stateFor(school)
	st.mdMu.Lock()
	defer st.mdMu.Unlock()
	if st.md != nil && time.Now().Before(st.mdNext) {
		return st.md
	}
	u, err := p.store.AnyUser(school)
	if err != nil || u == nil {
		return st.md
	}
	body, _ := json.Marshal(map[string]any{
		"id": "untis-proxy-md", "jsonrpc": "2.0", "method": "getUserData2017",
		"params": []any{map[string]any{
			"elementId": 0, "deviceOs": "AND", "deviceOsVersion": "",
			"auth": map[string]any{"user": u.Username},
		}},
	})
	newBody, err := p.rewriteAuthForOwner(school, body, u)
	if err != nil {
		return st.md
	}
	b, _, err := p.escalatedIntern(school, u, "getUserData2017", newBody)
	if err != nil {
		return st.md
	}
	var resp struct {
		Result struct {
			MasterData struct {
				Teachers []struct {
					ID   int64  `json:"id"`
					Name string `json:"name"`
				} `json:"teachers"`
				Rooms []struct {
					ID   int64  `json:"id"`
					Name string `json:"name"`
				} `json:"rooms"`
				Subjects []struct {
					ID       int64  `json:"id"`
					Name     string `json:"name"`
					LongName string `json:"longName"`
				} `json:"subjects"`
				Klassen []struct {
					ID   int64  `json:"id"`
					Name string `json:"name"`
				} `json:"klassen"`
				AbsenceReasons []struct {
					ID          int64  `json:"id"`
					Name        string `json:"name"`
					LongName    string `json:"longName"`
					Text        string `json:"text"`
					DisplayText string `json:"displayText"`
				} `json:"absenceReasons"`
			} `json:"masterData"`
		} `json:"result"`
	}
	if json.Unmarshal(b, &resp) != nil {
		return st.md
	}
	md := &masterDataCache{
		teachers:       map[int64]string{},
		rooms:          map[int64]string{},
		subjects:       map[int64]string{},
		klassen:        map[int64]string{},
		absenceReasons: map[int64]string{},
	}
	for _, t := range resp.Result.MasterData.Teachers {
		md.teachers[t.ID] = t.Name
	}
	for _, r := range resp.Result.MasterData.Rooms {
		md.rooms[r.ID] = r.Name
	}
	for _, s := range resp.Result.MasterData.Subjects {
		// The school server sends a short code in "name" ("M3") and the readable
		// title in "longName" ("Mathematik 3"). Everything user-facing — iCal
		// summaries, the week page, the admin element list — wants the long one.
		name := s.Name
		if s.LongName != "" {
			name = s.LongName
		}
		md.subjects[s.ID] = name
	}
	for _, k := range resp.Result.MasterData.Klassen {
		md.klassen[k.ID] = k.Name
	}
	for _, r := range resp.Result.MasterData.AbsenceReasons {
		// The reason objects name themselves differently across Untis versions, so
		// the first non-empty text field wins. An empty catalogue only costs the
		// client the readable reason, never the absence itself.
		for _, candidate := range []string{r.LongName, r.DisplayText, r.Text, r.Name} {
			if candidate != "" {
				md.absenceReasons[r.ID] = candidate
				break
			}
		}
	}
	st.md = md
	st.mdNext = time.Now().Add(time.Hour)
	// Persist names to DB for CLI fuzzy lookup (fire-and-forget).
	_ = p.store.SaveMasterNames(school, "TEACHER", md.teachers)
	_ = p.store.SaveMasterNames(school, "ROOM", md.rooms)
	_ = p.store.SaveMasterNames(school, "SUBJECT", md.subjects)
	_ = p.store.SaveMasterNames(school, "CLASS", md.klassen)
	return p.stateFor(school).md
}

func (p *Proxy) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/WebUntis/jsonrpc.do", p.handleJSONRPC)
	mux.HandleFunc("/WebUntis/jsonrpc_intern.do", p.handleJSONRPCIntern)
	mux.HandleFunc("/WebUntis/api/", p.handleREST)
	mux.HandleFunc("/status", p.handleStatus)
	mux.HandleFunc("/healthz", p.handleHealthz)
	mux.HandleFunc("/me", p.handleMe)
	mux.HandleFunc("POST /api/calendar/token", p.handleCalendarToken)
	mux.HandleFunc("GET /api/calendar/{token}", p.handleCalendarICS)
	mux.HandleFunc("GET /week/{token}", p.handleWeekPage)
	mux.HandleFunc("GET /api/timetable/changes", p.handleTimetableChanges)
	mux.HandleFunc("GET /api/timetable/stream", p.handleTimetableStream)
	mux.HandleFunc("/api/webhooks", p.handleSubsWebhooks)
	mux.HandleFunc("/api/webhooks/", p.handleSubsWebhooks)
	mux.HandleFunc("/api/ntfy", p.handleSubsNtfy)
	mux.HandleFunc("/api/ntfy/", p.handleSubsNtfy)
	// Proxy-local homework completion. Both are session-scoped and take the
	// viewer from the session only.
	mux.HandleFunc("GET /api/homework/flags", p.handleHomeworkFlags)
	mux.HandleFunc("POST /api/homework/done", p.handleHomeworkDone)
	// Absence notes are the same contract: session-scoped, viewer from the
	// session only, GET to read and POST to write.
	mux.HandleFunc("/api/absence/notes", p.handleAbsenceNotes)
	mux.HandleFunc("/admin", p.handleAdminDashboard)
	mux.HandleFunc("/admin/login", p.handleAdminLogin)
	mux.HandleFunc("/admin/", p.handleAdmin)
	return mux
}

// MetricsHandler serves the Prometheus endpoint and nothing else. It is a
// separate handler on a separate listener rather than a route on Handler(),
// because the metrics carry the school name as a label on every series and the
// pool size and poll counters beside it. That is not a secret, but it is a free
// inventory of the deployment for anyone who can reach a public hostname, and it
// was reachable without authentication. Binding it to loopback or a private
// network puts the boundary at the network layer, where a scraper does not need
// credentials and cannot be reached from outside by accident.
//
// Nothing registers it until -metrics-addr is set, so the default deployment
// serves no metrics at all rather than serving them to the world.
func (p *Proxy) MetricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", p.handleMetrics)
	mux.HandleFunc("/healthz", p.handleHealthzDetail)
	return mux
}

func idVal(id json.RawMessage) any {
	if len(id) == 0 || string(id) == "null" {
		return nil
	}
	var v any
	if json.Unmarshal(id, &v) == nil {
		return v
	}
	return string(id)
}

func (p *Proxy) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(v)
	_, _ = w.Write(b)
}

func (p *Proxy) writeJSONRPCError(w http.ResponseWriter, id json.RawMessage, message string, code int) {
	m := map[string]any{
		"jsonrpc": "2.0",
		"id":      idVal(id),
		"error":   map[string]any{"message": message, "code": code},
	}
	p.writeJSON(w, m)
}

func (p *Proxy) sessionUser(r *http.Request) *store.User {
	ck, err := r.Cookie("JSESSIONID")
	if err != nil || ck.Value == "" {
		return nil
	}
	s := p.sessions.Get(ck.Value)
	if s == nil {
		return nil
	}
	u, err := p.store.GetUser(s.Username)
	if err != nil || u == nil {
		return nil
	}
	return u
}

func (p *Proxy) setSessionCookies(w http.ResponseWriter, sid, school string) {
	// The app (native client) doesn't care about SameSite, and the admin
	// dashboard only ever uses the cookie same-origin. Keep SameSite unset
	// (browsers treat that as Lax) instead of None: None is rejected by
	// browsers over HTTPS unless also Secure, which silently dropped the
	// session when the tunnel serves the site (admin -> 401).
	http.SetCookie(w, &http.Cookie{
		Name: "JSESSIONID", Value: sid, Path: "/",
		HttpOnly: true,
	})
	sc := "_" + base64.StdEncoding.EncodeToString([]byte(school))
	http.SetCookie(w, &http.Cookie{Name: "schoolname", Value: sc, Path: "/", MaxAge: 1209600})
}

func (p *Proxy) classNameFor(school, cookie string, classID int64) string {
	st := p.stateFor(school)
	st.klMu.Lock()
	defer st.klMu.Unlock()
	if classID == 0 {
		return ""
	}
	if name, ok := st.klasses[classID]; ok && !p.klassesExpired(st.klAt) {
		return name
	}
	p.refreshKlassesLocked(st, school, cookie)
	return st.klasses[classID]
}

// hasPerm reports whether a user holds an explicit per-user permission feature
// (ignoring global switches).
func (p *Proxy) hasPerm(username, feature string) bool {
	ok, _ := p.store.HasPerm(username, feature)
	return ok
}

// isAdmin reports whether a user holds the admin flag (DB `admin` column).
// The -admin flag seeds these rows once at boot.
func (p *Proxy) isAdmin(username string) bool {
	ok, _ := p.store.IsAdmin(username)
	return ok
}

// isBoosted reports whether a user effectively gets Boosted raw timetable
// forwarding (holds the boosted flag).
func (p *Proxy) isBoosted(username string) bool {
	ok, _ := p.store.BoostedAccess(username)
	return ok
}

// isEditor reports whether a user holds the editor flag (absence/lesson/subject
// write methods).
func (p *Proxy) isEditor(username string) bool {
	ok, _ := p.store.EditorAccess(username)
	return ok
}

// writeMethodPrefixes are the verbs a JSON-RPC method can start with and still
// change something upstream. Everything the app reads starts with get/list/
// find/query/fetch, so a mutation outside this set has to be named here: the
// editor flag is the only thing that may call one. Note this gate runs on the
// requester's own account - a boosted user's write never reaches a teacher
// account - but keeping the list complete keeps a boosted non-editor from
// seeing editing succeed against his own login.
var writeMethodPrefixes = []string{
	"set", "add", "update", "delete", "change", "put", "remove", "save",
	"create", "modify", "insert", "replace", "assign", "unassign", "move",
	"copy", "rename", "import", "upload", "clear", "reset", "cancel",
}

// classScopedMethods are the class-scoped methods the app calls to build its
// lesson and absence editor, i.e. everything that hangs off a ttId rather than
// off the requester. Upstream answers them with the class roster, the class
// register, the absence state and the per-period capabilities only for a
// teacher identity: the very same getPeriodData2017 request, sent with a
// student identity, comes back with an empty student list, an empty class
// register and absences=null, which is why an editor's absence screen stayed
// blank no matter which capabilities the response claimed. Editors therefore
// send these through the boosted teacher source, so the screen has the data it
// needs and the writes carry the permissions the editor was granted. Every
// other method keeps using the requester's own account.
var classScopedMethods = map[string]bool{
	"getPeriodData2017": true,
	"setAbsencesList":   true,
	"setAbsence":        true,
	"addAbsence":        true,
	"updateAbsence":     true,
	"deleteAbsence":     true,
	"putLessonInfo":     true,
}

// classScopedOwner returns the teacher account a class-scoped request has to
// run as, or nil when it should keep the requester's own identity: personal
// data, boosted users and non-editors never switch.
func (p *Proxy) classScopedOwner(school, username, method string) *store.User {
	if username == "" || !classScopedMethods[method] || !p.isEditor(username) {
		return nil
	}
	owner := p.boostedSource(school)
	if owner == nil || strings.EqualFold(owner.Username, username) {
		return nil
	}
	return owner
}

// isWriteMethod reports whether a JSON-RPC method is a lesson/subject/absence
// write (mutation) method, gated by the editor flag. Reading your own absences
// is always allowed.
func isWriteMethod(method string) bool {
	m := strings.ToLower(method)
	for _, prefix := range writeMethodPrefixes {
		if strings.HasPrefix(m, prefix) {
			return true
		}
	}
	return false
}

func (p *Proxy) klassesExpired(at time.Time) bool {
	return time.Since(at) > time.Hour
}

func (p *Proxy) refreshKlassesLocked(st *schoolState, school, cookie string) {
	m := map[int64]string{}
	refresh := func(ck string) {
		b, err := p.untis.GetKlassen(school, ck)
		if err == nil && untis.IsAuthFailure(b) && ck != "" {
			if u, _ := p.store.AnyUser(school); u != nil {
				if ck2, err2 := p.untis.FreshSession(school, u.Username, u.Password, u.Method); err2 == nil {
					ck = ck2
					b, err = p.untis.GetKlassen(school, ck)
				}
			}
		}
		if err != nil {
			return
		}
		var res struct {
			Result []struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
			} `json:"result"`
		}
		if json.Unmarshal(b, &res) != nil {
			return
		}
		for _, c := range res.Result {
			m[c.ID] = c.Name
		}
	}
	if cookie != "" {
		refresh(cookie)
	}
	if len(m) == 0 {
		if u, _ := p.store.AnyUser(school); u != nil {
			if ck, err := p.untis.Session(school, u.Username, u.Password, u.Method); err == nil {
				refresh(ck)
			}
		}
	}
	if len(m) > 0 {
		st.klasses = m
		st.klAt = time.Now()
	}
}
