package proxy

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"untis-proxy/internal/access"
	"untis-proxy/internal/analytics"
	"untis-proxy/internal/store"
)

type analyticsRequest struct {
	Mode             string   `json:"mode"`
	From             string   `json:"from"`
	To               string   `json:"to"`
	TeacherID        int64    `json:"teacherId"`
	ProfileID        string   `json:"profileId"`
	SchoolYear       string   `json:"schoolYear"`
	MinLessons       int      `json:"minLessons"`
	Anonymise        bool     `json:"anonymise"`
	Format           string   `json:"format"`
	IncludeIrregular bool     `json:"includeIrregular"`
	ExcludePrefixes  []string `json:"excludeTeacherPrefixes"`
	PersonalOTP      string   `json:"personalOtp"`
}
type analyticsWindow struct {
	From         string `json:"from"`
	To           string `json:"to"`
	Status       string `json:"status"`
	Reason       string `json:"reason,omitempty"`
	UpstreamCode *int32 `json:"upstreamCode,omitempty"`
}
type analyticsResponse struct {
	Version          string            `json:"version"`
	ReportID         string            `json:"reportId"`
	Source           string            `json:"source"`
	From             string            `json:"from"`
	To               string            `json:"to"`
	GeneratedAt      string            `json:"generatedAt"`
	Anonymised       bool              `json:"anonymised"`
	Complete         bool              `json:"complete"`
	Windows          []analyticsWindow `json:"windows"`
	Warnings         []string          `json:"warnings"`
	Definitions      map[string]string `json:"definitions"`
	MinLessons       int               `json:"minLessons"`
	IncludeIrregular bool              `json:"includeIrregular"`
	ExcludePrefixes  []string          `json:"excludeTeacherPrefixes,omitempty"`
	analytics.Report
}
type analyticsTarget struct {
	Kind     string
	ID       int64
	From, To string
	Owner    *store.User
}

const analyticsPersonalWindowDays = 7

func (p *Proxy) handleCancellationAnalytics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	u := p.sessionUser(r)
	if u == nil {
		writeJSONError(w, 401, "not logged in")
		return
	}
	school := r.URL.Query().Get("school")
	if school == "" {
		school = p.userSchool(u, p.opts.School)
	}
	if school != p.userSchool(u, p.opts.School) || !p.store.FeatureEnabled(school, "analytics") {
		p.forbidden(w)
		return
	}
	var q analyticsRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if dec.Decode(&q) != nil || dec.Decode(new(any)) != io.EOF {
		badRequest(w, "invalid analytics request")
		return
	}
	if q.ExcludePrefixes == nil {
		q.ExcludePrefixes = []string{"TA_", "U "}
	}
	if q.Mode == "" {
		q.Mode = "personal"
	}
	if q.Format == "" {
		q.Format = "json"
	}
	if q.Format != "json" && q.Format != "export-json" && q.Format != "csv-zip" {
		badRequest(w, "format must be json, export-json or csv-zip")
		return
	}
	cap := access.CapAnalyticsPersonal
	switch q.Mode {
	case "personal":
	case "teacher":
		cap = access.CapAnalyticsTeacher
	case "class-history":
		cap = access.CapAnalyticsClassHistory
	default:
		badRequest(w, "invalid source mode")
		return
	}
	if q.PersonalOTP != "" && (q.Mode != "personal" || !validAnalyticsOTP(q.PersonalOTP)) {
		badRequest(w, "personalOtp requires personal mode and a six-digit code")
		return
	}
	// Administrator status deliberately does not bypass analytics scope grants.
	if !p.effective(u, school, cap) || (q.Format != "json" && !p.effective(u, school, access.CapAnalyticsExport)) {
		p.forbidden(w)
		return
	}
	years, err := p.store.AnalyticsYears(school)
	if err != nil {
		writeJSONError(w, 500, "school year configuration unavailable")
		return
	}
	if q.SchoolYear != "" {
		found := false
		for _, year := range years {
			if year.Name == q.SchoolYear {
				found = true
				if q.From == "" {
					q.From = year.From
				}
				if q.To == "" {
					q.To = year.To
				}
				if q.From < year.From || q.To > year.To {
					badRequest(w, "dates outside selected school year")
					return
				}
			}
		}
		if !found {
			badRequest(w, "school year not configured")
			return
		}
	}
	from, e1 := time.Parse("2006-01-02", q.From)
	to, e2 := time.Parse("2006-01-02", q.To)
	if e1 != nil || e2 != nil || to.Before(from) || to.Sub(from) > 365*24*time.Hour || from.Year() < 1900 || q.MinLessons < 0 || q.MinLessons > 100000 || q.TeacherID < 0 || len(q.ExcludePrefixes) > 20 {
		badRequest(w, "use valid dates, at most 366 days per request, and valid filters")
		return
	}
	if q.PersonalOTP != "" && to.Sub(from) > 30*24*time.Hour {
		badRequest(w, "personalOtp supports at most 31 days per request")
		return
	}
	for _, prefix := range q.ExcludePrefixes {
		if len(prefix) > 100 {
			badRequest(w, "exclusion prefix too long")
			return
		}
	}
	now := time.Now().UTC()
	today := now.Format("2006-01-02")
	if q.From > today {
		badRequest(w, "analytics require past or current dates")
		return
	}
	if q.To > today {
		q.To = today
		to, _ = time.Parse("2006-01-02", q.To)
	}
	if q.Mode == "personal" && from.Before(now.AddDate(0, 0, -366)) && !p.effective(u, school, access.CapAnalyticsHistory) {
		p.forbidden(w)
		return
	}
	targets, err := p.analyticsTargets(u, school, q)
	if err != nil {
		writeJSONError(w, 403, err.Error())
		return
	}
	// Validate and authorize every target before issuing the first upstream call.
	windowDays, maxWindows := 31, 32
	if q.Mode != "class-history" {
		windowDays, maxWindows = analyticsPersonalWindowDays, 53
	}
	windows := []analyticsTarget{}
	for _, target := range targets {
		a, _ := time.Parse("2006-01-02", target.From)
		b, _ := time.Parse("2006-01-02", target.To)
		for !a.After(b) {
			end := a.AddDate(0, 0, windowDays-1)
			if end.After(b) {
				end = b
			}
			part := target
			part.From = a.Format("2006-01-02")
			part.To = end.Format("2006-01-02")
			windows = append(windows, part)
			a = end.AddDate(0, 0, 1)
		}
	}
	if len(windows) > maxWindows {
		badRequest(w, "too many history windows; request a smaller range")
		return
	}
	key := loginLimitKey(school, u.Username)
	if locked, _ := p.analyticsLimiter.locked(key, now); locked {
		w.Header().Set("Retry-After", "60")
		writeJSONError(w, 429, "analytics request limit reached")
		return
	}
	p.analyticsLimiter.fail(key, now)
	select {
	case p.analyticsSlots <- struct{}{}:
		defer func() { <-p.analyticsSlots }()
	default:
		writeJSONError(w, 429, "analytics workers busy; retry later")
		return
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		writeJSONError(w, 500, "report id unavailable")
		return
	}
	result := analyticsResponse{Version: "1", ReportID: hex.EncodeToString(nonce), Source: q.Mode, From: q.From, To: q.To, GeneratedAt: now.Format(time.RFC3339), Anonymised: q.Anonymise, Complete: true, Windows: []analyticsWindow{}, Warnings: []string{}, MinLessons: q.MinLessons, IncludeIrregular: q.IncludeIrregular, ExcludePrefixes: q.ExcludePrefixes, Definitions: map[string]string{"metric": "observed timetable cancellations, not verified physical attendance", "counts": "teacher-lesson assignments; shared teaching counts once per teacher", "coverage": "successful upstream responses; upstream historical retention cannot be guaranteed", "rates": "percent, null when denominator is zero", "anonymisation": "report-local pseudonyms; lesson patterns may remain recognisable"}}
	lessons := []analytics.Lesson{}
	for _, window := range windows {
		if r.Context().Err() != nil {
			return
		}
		if !p.store.FeatureEnabled(school, "analytics") || !p.store.UserAdmissionAllowed(school, u) {
			p.forbidden(w)
			return
		}
		parsed, observed := p.fetchAnalyticsWindow(school, window, from, today, q.Mode, q.PersonalOTP)
		lessons = append(lessons, parsed...)
		result.Windows = append(result.Windows, observed)
		if observed.Status != "observed" {
			result.Complete = false
		}
	}
	// Missing approved class-history days are reported as gaps, never zeros.
	if q.Mode == "class-history" {
		for day := from; !day.After(to); day = day.AddDate(0, 0, 1) {
			d := day.Format("2006-01-02")
			covered := false
			for _, window := range result.Windows {
				if window.Status == "observed" && d >= window.From && d <= window.To {
					covered = true
					break
				}
			}
			if !covered {
				result.Complete = false
				break
			}
		}
	}
	if !result.Complete {
		result.Warnings = append(result.Warnings, "Some upstream windows could not be read; totals are partial.")
	}
	if q.Anonymise {
		result.ExcludePrefixes = nil
	}
	options := analytics.Options{MinLessons: q.MinLessons, Anonymise: q.Anonymise, IncludeIrregular: q.IncludeIrregular, ExcludePrefixes: q.ExcludePrefixes, DetectClashes: q.Mode == "personal"}
	if q.TeacherID > 0 {
		options.TeacherID = strconv.FormatInt(q.TeacherID, 10)
	}
	for _, y := range years {
		options.Years = append(options.Years, analytics.Year{Name: y.Name, From: y.From, To: y.To})
	}
	result.Report = analytics.Build(lessons, options)
	result.Definitions["clash"] = "overlap with another non-cancelled lesson on the personal timetable; does not establish why a lesson was cancelled"
	result.Definitions["cover"] = "an upstream teacher assignment with a different original teacher id"
	if result.UnmappedSchoolYearLessons > 0 {
		result.Warnings = append(result.Warnings, "Some observed lessons are outside configured school years and are grouped as unmapped.")
	}
	// Recheck all grants immediately before returning/exporting a long-running report.
	fresh := p.sessionUser(r)
	currentTargets, targetErr := p.analyticsTargets(u, school, q)
	if fresh == nil || fresh.PersonID != u.PersonID || fresh.PersonType != u.PersonType || targetErr != nil || analyticsTargetKey(currentTargets) != analyticsTargetKey(targets) || !p.effective(fresh, school, cap) || !p.store.UserAdmissionAllowed(school, fresh) || (q.Format != "json" && !p.effective(fresh, school, access.CapAnalyticsExport)) {
		p.forbidden(w)
		return
	}
	if q.Format == "csv-zip" {
		p.writeAnalyticsCSV(w, result)
		return
	}
	if q.Format == "export-json" {
		w.Header().Set("Content-Disposition", `attachment; filename="cancellation-report.json"`)
	}
	p.writeJSON(w, result)
}
func (p *Proxy) analyticsTargets(u *store.User, school string, q analyticsRequest) ([]analyticsTarget, error) {
	if q.Mode == "personal" {
		if q.ProfileID != "" {
			return nil, fmt.Errorf("profileId requires class-history mode")
		}
		kind := "STUDENT"
		if u.PersonType == 1 || u.PersonType == 2 {
			kind = "TEACHER"
		} else if u.PersonType != 5 {
			return nil, fmt.Errorf("personal timetable identity not supported")
		}
		if links, err := p.store.AccountLinks(school, u.Username); err == nil && links.Guardian {
			return nil, fmt.Errorf("guardian child analytics require a separately approved class history")
		}
		if u.PersonID <= 0 {
			return nil, fmt.Errorf("personal timetable identity unavailable")
		}
		return []analyticsTarget{{kind, u.PersonID, q.From, q.To, u}}, nil
	}
	allowed, err := p.store.AnalyticsAccess(school, u.Username)
	if err != nil {
		return nil, fmt.Errorf("analytics scope unavailable")
	}
	targets := []analyticsTarget{}
	if q.Mode == "teacher" {
		ok := false
		for _, id := range allowed.TeacherIDs {
			if id == q.TeacherID {
				ok = true
			}
		}
		if !ok || q.ProfileID != "" {
			return nil, fmt.Errorf("teacher not approved for this account")
		}
		targets = append(targets, analyticsTarget{Kind: "TEACHER", ID: q.TeacherID, From: q.From, To: q.To})
	} else {
		for _, profile := range allowed.Profiles {
			if profile.ID != q.ProfileID {
				continue
			}
			for _, seg := range profile.Segments {
				a, b := seg.From, seg.To
				if a < q.From {
					a = q.From
				}
				if b > q.To {
					b = q.To
				}
				if a <= b {
					targets = append(targets, analyticsTarget{Kind: "CLASS", ID: seg.ClassID, From: a, To: b})
				}
			}
		}
		if len(targets) == 0 {
			return nil, fmt.Errorf("no approved history segments in range")
		}
	}
	sources, err := p.store.BoostedSourceAccounts(school)
	if err != nil || len(sources) == 0 {
		return nil, fmt.Errorf("no authorized saved teacher source")
	}
	for i := range targets {
		targets[i].Owner = sources[0]
	}
	return targets, nil
}

func validAnalyticsOTP(otp string) bool {
	if len(otp) != 6 {
		return false
	}
	for i := range otp {
		if otp[i] < '0' || otp[i] > '9' {
			return false
		}
	}
	return true
}

// fetchAnalyticsWindow uses a full mobile week for short personal report
// windows when it fits within the report range. The parser only includes
// dates assigned to this window, even when upstream requests overlap.
func (p *Proxy) fetchAnalyticsWindow(school string, window analyticsTarget, reportFrom time.Time, today, mode, otp string) ([]analytics.Lesson, analyticsWindow) {
	start, end := window.From, window.To
	if mode != "class-history" {
		lastDay, _ := time.Parse(time.DateOnly, end)
		weekStart := lastDay.AddDate(0, 0, 1-analyticsPersonalWindowDays)
		if !weekStart.Before(reportFrom) {
			return p.readAnalyticsWindow(school, window, weekStart.Format(time.DateOnly), end, otp)
		}
	}
	if mode == "personal" && start == end {
		day, err := time.Parse(time.DateOnly, start)
		if err == nil {
			end = day.AddDate(0, 0, analyticsPersonalWindowDays-1).Format(time.DateOnly)
			if end > today {
				end = today
				if end == start {
					// Only today's query needs a preceding day, which is
					// within the standard personal history allowance.
					start = day.AddDate(0, 0, -1).Format(time.DateOnly)
				}
			}
		}
	}
	return p.readAnalyticsWindow(school, window, start, end, otp)
}

func (p *Proxy) readAnalyticsWindow(school string, window analyticsTarget, requestFrom, requestTo, otp string) ([]analytics.Lesson, analyticsWindow) {
	observed := analyticsWindow{From: window.From, To: window.To, Status: "unavailable"}
	auth := map[string]any{}
	if otp != "" {
		// The target was resolved from the authenticated personal account. Use
		// the supplied live code only as proof for this upstream read.
		// The mobile client's timetable auth serializes OTP as a number.
		code, _ := strconv.Atoi(otp)
		auth = map[string]any{"user": window.Owner.Username, "otp": code, "clientTime": time.Now().UnixMilli()}
	}
	body, _ := json.Marshal(map[string]any{"id": "analytics", "jsonrpc": "2.0", "method": "getTimetable2017", "params": []any{
		map[string]any{"id": window.ID, "type": window.Kind, "startDate": requestFrom, "endDate": requestTo,
			"masterDataTimestamp": 0, "timetableTimestamp": 0, "timetableTimestamps": [7]int{}, "auth": auth},
	}})
	if otp == "" {
		rewritten, err := p.rewriteAuthForOwner(school, body, window.Owner)
		if err != nil {
			observed.Reason = "source_auth_unavailable"
			return nil, observed
		}
		body = rewritten
	}
	data, status, _, err := p.untis.RawIntern(school, p.untis.SchoolCookie(school), "getTimetable2017", body)
	switch {
	case err != nil:
		observed.Reason = "upstream_unreachable"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		observed.Reason = "upstream_rejected"
	case status != http.StatusOK:
		observed.Reason = "upstream_http_error"
	default:
		lessons, err := p.parseAnalyticsLessons(school, data, window.From, window.To)
		if err == nil {
			observed.Status = "observed"
			return lessons, observed
		}
		if !errors.Is(err, errAnalyticsUpstreamRejected) {
			observed.Reason = "invalid_timetable_response"
			break
		}
		observed.Reason = "upstream_rejected"
		observed.UpstreamCode = analyticsUpstreamCode(data)
	}
	return nil, observed
}

// Only a numeric protocol code is exposed, never upstream message text.
func analyticsUpstreamCode(raw []byte) *int32 {
	var env struct {
		Error struct {
			Code *int32 `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return nil
	}
	return env.Error.Code
}

var errAnalyticsUpstreamRejected = errors.New("upstream rejected timetable request")

func (p *Proxy) parseAnalyticsLessons(school string, raw []byte, from, to string) ([]analytics.Lesson, error) {
	var envelope struct {
		Result *struct {
			Timetable struct {
				Periods []map[string]any `json:"periods"`
			} `json:"timetable"`
			Periods []map[string]any `json:"periods"`
			TTData  []map[string]any `json:"ttData"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("invalid timetable response")
	}
	if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		return nil, errAnalyticsUpstreamRejected
	}
	if envelope.Result == nil {
		return nil, fmt.Errorf("invalid timetable response")
	}
	periods := envelope.Result.Timetable.Periods
	if periods == nil {
		periods = envelope.Result.Periods
	}
	if periods == nil {
		periods = envelope.Result.TTData
	}
	if periods == nil {
		return nil, fmt.Errorf("invalid timetable response")
	}
	masterData := parseMasterDataCache(raw)
	cachedNames := map[string]map[int64]string{"TEACHER": {}, "SUBJECT": {}}
	lookupName := func(kind string, id int64) string {
		if name, found := cachedNames[kind][id]; found {
			return name
		}
		name := elementName(masterData, kind, id)
		if name == "" {
			name = p.store.ElementName(school, kind, id)
		}
		if name == "" {
			prefix := "Teacher"
			if kind == "SUBJECT" {
				prefix = "Subject"
			}
			name = fmt.Sprintf("%s %d", prefix, id)
		}
		cachedNames[kind][id] = name
		return name
	}
	lessons := make([]analytics.Lesson, 0, len(periods))
	for _, period := range periods {
		date := fmt.Sprint(period["date"])
		if v, ok := period["startDateTime"].(string); ok && len(v) >= 10 {
			date = v[:10]
		}
		if len(date) == 8 {
			if d, err := time.Parse("20060102", date); err == nil {
				date = d.Format("2006-01-02")
			}
		}
		if _, err := time.Parse("2006-01-02", date); err != nil {
			return nil, fmt.Errorf("invalid lesson date")
		}
		if date < from || date > to {
			continue
		}
		id := fmt.Sprint(period["id"])
		if parsedID, err := strconv.ParseInt(id, 10, 64); err != nil || parsedID <= 0 {
			return nil, fmt.Errorf("lesson id missing")
		}
		start, end := period["startDateTime"], period["endDateTime"]
		if start == nil {
			start = period["startTime"]
		}
		if end == nil {
			end = period["endTime"]
		}
		sum := sha256.Sum256([]byte(id + "/" + date + "/" + fmt.Sprint(start) + "/" + fmt.Sprint(end)))
		l := analytics.Lesson{Key: hex.EncodeToString(sum[:]), Date: date,
			Start: analyticsClock(start), End: analyticsClock(end), Code: analyticsPeriodCode(period)}
		l.Teachers, l.Covers = analyticsPeriodTeachers(period, lookupName)
		l.Subjects = analyticsPeriodSubjects(period, lookupName)
		lessons = append(lessons, l)
	}
	return lessons, nil
}

func analyticsPeriodCode(period map[string]any) string {
	code := strings.ToLower(strings.TrimSpace(rawString(period, "code")))
	if code == "cancelled" || periodHasFlag(period, "CANCELLED") ||
		strings.EqualFold(rawString(period, "status"), "CANCELLED") ||
		strings.EqualFold(rawString(period, "cellState"), "CANCELLED") {
		return "cancelled"
	}
	if periodHasFlag(period, "IRREGULAR") {
		return "irregular"
	}
	return code
}

// analyticsClock records source wall-clock time, in HH:MM, for same-day
// comparisons within a single personal timetable.
func analyticsClock(value any) string {
	raw := fmt.Sprint(value)
	if len(raw) >= 16 && (raw[10] == 'T' || raw[10] == ' ') {
		raw = raw[11:16]
	}
	if v, err := time.Parse("15:04", raw); err == nil {
		return v.Format("15:04")
	}
	if n, err := strconv.Atoi(raw); err == nil && n >= 0 && n/100 < 24 && n%100 < 60 {
		return fmt.Sprintf("%02d:%02d", n/100, n%100)
	}
	return ""
}

func analyticsEntryID(entry map[string]any, key string) int64 {
	id, _ := strconv.ParseInt(fmt.Sprint(entry[key]), 10, 64)
	return id
}

func analyticsOriginalID(entry map[string]any) int64 {
	id := analyticsEntryID(entry, "orgId")
	if id <= 0 {
		id = analyticsEntryID(entry, "orgid")
	}
	return id
}

func analyticsEntryName(entry map[string]any) string {
	for _, key := range []string{"longName", "longname", "name"} {
		if name := strings.TrimSpace(rawString(entry, key)); name != "" {
			return name
		}
	}
	return ""
}

func analyticsEntries(period map[string]any, key, kind string) []map[string]any {
	items, _ := period[key].([]any)
	var out []map[string]any
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if ok && (key != "elements" || strings.EqualFold(rawString(entry, "type"), kind)) {
			out = append(out, entry)
		}
	}
	return out
}

func analyticsPeriodTeachers(period map[string]any, lookup func(string, int64) string) ([]analytics.TeacherRef, []analytics.Cover) {
	entries := append(analyticsEntries(period, "elements", "TEACHER"), analyticsEntries(period, "teachers", "TEACHER")...)
	entries = append(entries, analyticsEntries(period, "te", "TEACHER")...)
	names := map[int64]string{}
	for _, entry := range entries {
		if id, name := analyticsEntryID(entry, "id"), analyticsEntryName(entry); id > 0 && name != "" {
			names[id] = name
		}
	}
	ref := func(id int64) analytics.TeacherRef {
		name := names[id]
		if name == "" {
			name = lookup("TEACHER", id)
		}
		return analytics.TeacherRef{ID: strconv.FormatInt(id, 10), Name: name}
	}
	coveredIDs := map[int64]bool{}
	pairs := map[[2]int64]bool{}
	var covers []analytics.Cover
	for _, entry := range entries {
		id, original := analyticsEntryID(entry, "id"), analyticsOriginalID(entry)
		if id <= 0 || original <= 0 || id == original {
			continue
		}
		pair := [2]int64{original, id}
		if !pairs[pair] {
			pairs[pair] = true
			coveredIDs[id] = true
			covers = append(covers, analytics.Cover{Original: ref(original), Teacher: ref(id)})
		}
	}
	var teachers []analytics.TeacherRef
	seen := map[int64]bool{}
	for _, entry := range entries {
		id, original := analyticsEntryID(entry, "id"), analyticsOriginalID(entry)
		if original > 0 {
			id = original
		} else if coveredIDs[id] {
			continue // An unpaired duplicate must not count a cover teacher as scheduled.
		}
		if id > 0 && !seen[id] {
			seen[id] = true
			teachers = append(teachers, ref(id))
		}
	}
	return teachers, covers
}

func analyticsPeriodSubjects(period map[string]any, lookup func(string, int64) string) []analytics.SubjectRef {
	var subjects []analytics.SubjectRef
	seen := map[int64]bool{}
	for _, key := range []string{"subjects", "su", "elements"} {
		for _, entry := range analyticsEntries(period, key, "SUBJECT") {
			id := analyticsEntryID(entry, "id")
			name := analyticsEntryName(entry)
			if original := analyticsOriginalID(entry); original > 0 && original != id {
				id, name = original, ""
			}
			if id <= 0 || seen[id] {
				continue
			}
			seen[id] = true
			if name == "" {
				name = lookup("SUBJECT", id)
			}
			subjects = append(subjects, analytics.SubjectRef{ID: strconv.FormatInt(id, 10), Name: name})
		}
	}
	return subjects
}

func analyticsCSVSafe(s string) string {
	if len(s) > 0 && strings.ContainsAny(s[:1], "=+-@\t\r\n") {
		return "'" + s
	}
	return s
}
func (p *Proxy) writeAnalyticsCSV(w http.ResponseWriter, result analyticsResponse) {
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	write := func(name string, rows [][]string) error {
		file, err := archive.Create(name)
		if err != nil {
			return err
		}
		writer := csv.NewWriter(file)
		for _, row := range rows {
			safe := make([]string, len(row))
			for i, value := range row {
				safe[i] = analyticsCSVSafe(value)
			}
			if err := writer.Write(safe); err != nil {
				return err
			}
		}
		writer.Flush()
		return writer.Error()
	}
	totals := [][]string{{"teacherId", "teacher", "rank", "scheduled", "cancelled", "notCancelled", "cancellationRate", "notCancelledRate"}}
	rate := func(p *float64) string {
		if p == nil {
			return ""
		}
		return strconv.FormatFloat(*p, 'f', 4, 64)
	}
	for _, t := range result.Teachers {
		totals = append(totals, []string{t.ID, t.Name, strconv.Itoa(t.Rank), strconv.Itoa(t.Scheduled), strconv.Itoa(t.Cancelled), strconv.Itoa(t.NotCancelled), rate(t.CancellationRate), rate(t.NotCancelledRate)})
	}
	if write("teachers.csv", totals) != nil {
		writeJSONError(w, 500, "export failed")
		return
	}
	for _, kind := range []string{"weekday", "week", "school-year"} {
		rows := [][]string{{"teacherId", "teacher", "bucket", "scheduled", "cancelled", "notCancelled", "cancellationRate", "notCancelledRate"}}
		appendRows := func(id, name string, bs []analytics.Bucket) {
			for _, b := range bs {
				rows = append(rows, []string{id, name, b.Key, strconv.Itoa(b.Scheduled), strconv.Itoa(b.Cancelled), strconv.Itoa(b.NotCancelled), rate(b.CancellationRate), rate(b.NotCancelledRate)})
			}
		}
		switch kind {
		case "weekday":
			appendRows("", "All selected teachers", result.ByWeekday)
		case "week":
			appendRows("", "All selected teachers", result.ByWeek)
		default:
			appendRows("", "All selected teachers", result.BySchoolYear)
		}
		for _, t := range result.Teachers {
			switch kind {
			case "weekday":
				appendRows(t.ID, t.Name, t.ByWeekday)
			case "week":
				appendRows(t.ID, t.Name, t.ByWeek)
			default:
				appendRows(t.ID, t.Name, t.BySchoolYear)
			}
		}
		if write(kind+".csv", rows) != nil {
			writeJSONError(w, 500, "export failed")
			return
		}
	}
	for _, table := range []struct {
		name   string
		counts []analytics.ChangeCount
	}{
		{"missed-teachers.csv", result.MissedTeachers},
		{"missed-subjects.csv", result.MissedSubjects},
		{"cover-teachers.csv", result.CoverTeachers},
	} {
		rows := [][]string{{"id", "name", "lessons", "cancelled", "covered"}}
		for _, count := range table.counts {
			rows = append(rows, []string{count.ID, count.Name, strconv.Itoa(count.Lessons), strconv.Itoa(count.Cancelled), strconv.Itoa(count.Covered)})
		}
		if write(table.name, rows) != nil {
			writeJSONError(w, 500, "export failed")
			return
		}
	}
	changes := [][]string{{"date", "start", "end", "subjects", "cancelled", "clash", "missedTeachers"}}
	covers := [][]string{{"date", "start", "end", "subjects", "originalTeacherId", "originalTeacher", "coverTeacherId", "coverTeacher"}}
	for _, change := range result.Changes {
		var subjectNames, teacherNames []string
		for _, subject := range change.Subjects {
			subjectNames = append(subjectNames, subject.Name)
		}
		subjects := strings.Join(subjectNames, ", ")
		for _, teacher := range change.MissedTeachers {
			teacherNames = append(teacherNames, teacher.Name)
		}
		changes = append(changes, []string{change.Date, change.Start, change.End, subjects, strconv.FormatBool(change.Cancelled), change.Clash, strings.Join(teacherNames, ", ")})
		for _, cover := range change.Covers {
			covers = append(covers, []string{change.Date, change.Start, change.End, subjects, cover.Original.ID, cover.Original.Name, cover.Teacher.ID, cover.Teacher.Name})
		}
	}
	if write("lesson-changes.csv", changes) != nil || write("covers.csv", covers) != nil {
		writeJSONError(w, 500, "export failed")
		return
	}
	metadata, err := archive.Create("report.json")
	if err != nil || json.NewEncoder(metadata).Encode(result) != nil || archive.Close() != nil {
		writeJSONError(w, 500, "export failed")
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="cancellation-report.zip"`)
	_, _ = w.Write(buffer.Bytes())
}
func analyticsTargetKey(targets []analyticsTarget) string {
	var b strings.Builder
	for _, t := range targets {
		fmt.Fprintf(&b, "%s/%d/%s/%s;", t.Kind, t.ID, t.From, t.To)
	}
	return b.String()
}
