package proxy

// The /week page: a server-rendered, JS-free week view for anyone holding a
// calendar token. It exists because a calendar subscription is a poor fit for a
// parent on a phone — they want one bookmarkable page that says "when and where
// is the next lesson" without setting up a subscription.

import (
	"bytes"
	_ "embed"
	"encoding/csv"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// weekPageHTML is the embedded week view template, rendered server-side for
// every request so the page works without JavaScript.
//
//go:embed static/week.html
var weekPageHTML string

var weekTemplate = template.Must(template.New("week").Parse(weekPageHTML))

// weekLesson is one rendered lesson.
type weekLesson struct {
	StartTime    string
	EndTime      string
	TimeRange    string
	Subject      string
	Teacher      string
	Room         string
	Class        string
	Detail       string
	Notes        string
	ExamLabel    string
	Substitution bool
	Cancelled    bool
	Exam         bool
	Custom       bool
}

// weekDay is one rendered day column.
type weekDay struct {
	Label    string
	Relative string
	Today    bool
	Lessons  []weekLesson
}

// weekGridDay is one cell of the month grid. OtherMonth marks the leading and
// trailing days that only pad the grid to whole weeks; they are shown greyed and
// never carry lessons.
type weekGridDay struct {
	Day        int
	Date       string
	Today      bool
	OtherMonth bool
	Lessons    []weekLesson
}

// weekGridWeek is one row of seven cells in the month grid.
type weekGridWeek struct {
	Days []weekGridDay
}

// weekView is the template model.
type weekView struct {
	Name       string
	Timezone   string
	WeekLabel  string
	MonthLabel string
	RenderedAt string
	ICSURL     string
	CSVURL     string
	WeekURL    string
	MonthURL   string
	PrevURL    string
	NextURL    string
	TodayURL   string
	Mode       string
	Version    int64
	Days       []weekDay
	Weeks      []weekGridWeek
}

// weekWindow is the date range one /week request covers. The same resolution is
// used for the HTML page and the CSV export so the download always matches what
// the reader is looking at.
type weekWindow struct {
	Mode       string
	From       string
	To         string
	Label      string
	MonthStart time.Time
}

// resolveWeekWindow reads ?view, ?from and ?month. A bad value falls back to the
// current week rather than failing the page.
func resolveWeekWindow(r *http.Request, loc *time.Location, now time.Time) weekWindow {
	today := now.In(loc)
	dayStart := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, loc)
	if mode := r.URL.Query().Get("view"); mode == "month" {
		base := dayStart
		if m := r.URL.Query().Get("month"); m != "" {
			if t, err := time.ParseInLocation("2006-01", m, loc); err == nil {
				base = t
			}
		}
		start := time.Date(base.Year(), base.Month(), 1, 0, 0, 0, 0, loc)
		end := start.AddDate(0, 1, -1)
		return weekWindow{
			Mode: "month", From: start.Format("2006-01-02"), To: end.Format("2006-01-02"),
			Label: germanMonth(start), MonthStart: start,
		}
	}
	if f := r.URL.Query().Get("from"); f != "" {
		if t, err := time.ParseInLocation("2006-01-02", f, loc); err == nil {
			dayStart = t
		}
	}
	end := dayStart.AddDate(0, 0, 6)
	return weekWindow{
		Mode: "week", From: dayStart.Format("2006-01-02"), To: end.Format("2006-01-02"),
		Label: dayStart.Format("02.01.") + " – " + end.Format("02.01.2006"),
	}
}

var germanMonthNames = [...]string{
	"Januar", "Februar", "März", "April", "Mai", "Juni",
	"Juli", "August", "September", "Oktober", "November", "Dezember",
}

func germanMonth(t time.Time) string {
	return germanMonthNames[t.Month()-1] + " " + strconv.Itoa(t.Year())
}

// mondayOf returns the Monday on or before d.
func mondayOf(d time.Time) time.Time {
	offset := (int(d.Weekday()) + 6) % 7 // Sunday=0 -> 6, Monday=1 -> 0
	return d.AddDate(0, 0, -offset)
}

// handleWeekPage renders the week view for a calendar token. Access is the same
// as the .ics feed — possession of the token is the credential — so it must be
// treated as a secret all the same.
func (p *Proxy) handleWeekPage(w http.ResponseWriter, r *http.Request) {
	raw := r.PathValue("token")
	if strings.HasSuffix(raw, ".csv") {
		p.handleWeekCSV(w, r, strings.TrimSuffix(raw, ".csv"))
		return
	}
	token := strings.TrimSuffix(raw, ".html")
	tok, err := p.store.ClassTokenByToken(token)
	if err != nil || tok == nil || tokenExpired(tok) || !p.schoolAdmitted(tok.School) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("unknown calendar token\n"))
		return
	}
	now := time.Now()
	_ = p.store.TouchClassToken(token, now.Unix())

	tz := tok.Timezone
	if tz == "" {
		tz = "Europe/Berlin"
	}
	loc, lerr := time.LoadLocation(tz)
	if lerr != nil {
		loc = time.UTC
		tz = "UTC"
	}
	window := resolveWeekWindow(r, loc, now)
	feed, err := p.resolveFeed(tok, window.From, window.To)
	if err != nil {
		// Do not echo the upstream error: this page is public to anyone holding
		// the token, and the error can name internal hosts or request details.
		log.Printf("[week] resolve feed for %s: %v", tok.School, err)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("Stundenplan konnte nicht geladen werden.\n"))
		return
	}
	md := p.masterData(tok.School)

	base := "/week/" + token + ".html"
	view := weekView{
		Name:       feed.Name,
		Timezone:   tz,
		WeekLabel:  window.Label,
		RenderedAt: now.In(loc).Format("02.01.2006 15:04"),
		ICSURL:     "/api/calendar/" + token + ".ics",
		WeekURL:    base,
		MonthURL:   base + "?view=month",
		TodayURL:   base,
		Mode:       window.Mode,
		Version:    feed.Version,
	}
	byDay := weekLessonsByDay(feed.Periods, md, loc)
	todayKey := now.In(loc).Format("2006-01-02")

	if window.Mode == "month" {
		view.MonthLabel = window.Label
		view.CSVURL = "/week/" + token + ".csv?view=month&month=" + window.MonthStart.Format("2006-01")
		view.PrevURL = base + "?view=month&month=" + window.MonthStart.AddDate(0, -1, 0).Format("2006-01")
		view.NextURL = base + "?view=month&month=" + window.MonthStart.AddDate(0, 1, 0).Format("2006-01")
		monthEnd := window.MonthStart.AddDate(0, 1, -1)
		gridStart := mondayOf(window.MonthStart)
		gridEnd := monthEnd
		for gridEnd.Weekday() != time.Sunday {
			gridEnd = gridEnd.AddDate(0, 0, 1)
		}
		for ws := gridStart; !ws.After(gridEnd); ws = ws.AddDate(0, 0, 7) {
			week := weekGridWeek{Days: make([]weekGridDay, 0, 7)}
			for i := 0; i < 7; i++ {
				d := ws.AddDate(0, 0, i)
				key := d.Format("2006-01-02")
				inMonth := d.Month() == window.MonthStart.Month() && d.Year() == window.MonthStart.Year()
				var lessons []weekLesson
				if inMonth {
					lessons = byDay[key]
					sort.SliceStable(lessons, func(i, j int) bool { return lessons[i].TimeRange < lessons[j].TimeRange })
				}
				week.Days = append(week.Days, weekGridDay{
					Day: d.Day(), Date: d.Format("02.01."), Today: key == todayKey,
					OtherMonth: !inMonth, Lessons: lessons,
				})
			}
			view.Weeks = append(view.Weeks, week)
		}
	} else {
		weekStart, _ := time.ParseInLocation("2006-01-02", window.From, loc)
		weekEnd, _ := time.ParseInLocation("2006-01-02", window.To, loc)
		view.CSVURL = "/week/" + token + ".csv?from=" + window.From
		view.PrevURL = base + "?from=" + weekStart.AddDate(0, 0, -7).Format("2006-01-02")
		view.NextURL = base + "?from=" + weekStart.AddDate(0, 0, 7).Format("2006-01-02")
		for d := weekStart; !d.After(weekEnd); d = d.AddDate(0, 0, 1) {
			key := d.Format("2006-01-02")
			lessons := byDay[key]
			sort.SliceStable(lessons, func(i, j int) bool { return lessons[i].TimeRange < lessons[j].TimeRange })
			view.Days = append(view.Days, weekDay{
				Label:    d.Format("Mon"),
				Relative: d.Format("02.01."),
				Today:    key == todayKey,
				Lessons:  lessons,
			})
		}
	}

	var buf bytes.Buffer
	if err := weekTemplate.Execute(&buf, view); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

// handleWeekCSV exports the same window the HTML page shows as spreadsheet CSV.
// It carries the visible columns (not the raw upstream JSON), so a parent or
// teacher can sort and print it without parsing an .ics file.
func (p *Proxy) handleWeekCSV(w http.ResponseWriter, r *http.Request, token string) {
	tok, err := p.store.ClassTokenByToken(token)
	if err != nil || tok == nil || tokenExpired(tok) || !p.schoolAdmitted(tok.School) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("unknown calendar token\n"))
		return
	}
	now := time.Now()
	_ = p.store.TouchClassToken(token, now.Unix())

	loc := feedLocation(tok, nil)
	if tok.Timezone == "" {
		loc = time.UTC
		if l, err := time.LoadLocation("Europe/Berlin"); err == nil {
			loc = l
		}
	}
	window := resolveWeekWindow(r, loc, now)
	feed, err := p.resolveFeed(tok, window.From, window.To)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("timetable could not be loaded\n"))
		return
	}
	md := p.masterData(tok.School)
	byDay := weekLessonsByDay(feed.Periods, md, loc)

	dates := make([]string, 0, len(byDay))
	for key := range byDay {
		dates = append(dates, key)
	}
	sort.Strings(dates)

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="stundenplan-%s_%s.csv"`, window.From, window.To))
	// A UTF-8 BOM keeps Excel from mangling umlauts in German subject names.
	_, _ = w.Write([]byte("\ufeff"))
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"Datum", "Von", "Bis", "Fach", "Lehrer", "Raum", "Klasse", "Status", "Notizen"})
	for _, date := range dates {
		lessons := byDay[date]
		sort.SliceStable(lessons, func(i, j int) bool { return lessons[i].TimeRange < lessons[j].TimeRange })
		for _, l := range lessons {
			_ = cw.Write([]string{
				date, l.StartTime, l.EndTime, csvSafe(l.Subject), csvSafe(l.Teacher),
				csvSafe(l.Room), csvSafe(l.Class), csvSafe(weekLessonStatus(l)), csvSafe(l.Notes),
			})
		}
	}
	cw.Flush()
}

// csvSafe neutralises spreadsheet formula injection. A subject, room or note
// comes from upstream and could begin with =, +, - or @; Excel evaluates such a
// cell as a formula when the export is opened. A leading apostrophe keeps the
// text visible while forcing the cell to stay literal.
func csvSafe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

// weekLessonStatus spells out the badges the HTML view shows, so the CSV has the
// same meaning as the page.
func weekLessonStatus(l weekLesson) string {
	var parts []string
	if l.Custom {
		parts = append(parts, "Termin")
	}
	if l.Cancelled {
		parts = append(parts, "entfällt")
	}
	if l.Exam {
		parts = append(parts, l.ExamLabel)
	}
	if l.Substitution {
		parts = append(parts, "Vertretung")
	}
	return strings.Join(parts, "; ")
}

// rawString reads a top-level string field of a raw upstream period.
func rawString(pd map[string]any, key string) string {
	v, _ := pd[key].(string)
	return v
}

// lessonSubject resolves a period's subject name, falling back to the raw
// lesson/description text the school server sends.
func lessonSubject(pd map[string]any, md *masterDataCache) string {
	if n := joinedElementNames(pd, md, "SUBJECT"); n != "" {
		return n
	}
	for _, k := range []string{"lesson", "description", "title"} {
		if t := textField(pd, k); t != "" {
			return t
		}
	}
	if t := rawString(pd, "lesson"); t != "" {
		return t
	}
	return "Stunde"
}

// joinedElementNames lists the display names of every element of one type in a
// period, e.g. "R204" or "Müller, Schmidt".
func joinedElementNames(pd map[string]any, md *masterDataCache, elType string) string {
	var names []string
	for _, el := range periodElementIDs(pd) {
		if el.Type != elType {
			continue
		}
		n := elementName(md, elType, el.ID)
		if n == "" {
			n = fmt.Sprintf("#%d", el.ID)
		}
		names = append(names, n)
	}
	return strings.Join(names, ", ")
}

// periodHasFlag reports whether the school server marked a period with a status
// flag such as CANCELLED or EXAM.
func periodHasFlag(pd map[string]any, flag string) bool {
	flags, _ := pd["is"].([]any)
	for _, v := range flags {
		if s, _ := v.(string); s == flag {
			return true
		}
	}
	return false
}

// weekLessonsByDay renders periods into the rows the week view shows, grouped by
// day. It is a function rather than inline in the handler so the rendering of a
// period — real lesson or custom event — has exactly one definition.
func weekLessonsByDay(periods []map[string]any, md *masterDataCache, loc *time.Location) map[string][]weekLesson {
	byDay := map[string][]weekLesson{}
	for _, pd := range periods {
		st, err := parsePeriodTime(rawString(pd, "startDateTime"))
		if err != nil {
			continue
		}
		st = st.In(loc)

		l := weekLesson{TimeRange: st.Format("15:04"), StartTime: st.Format("15:04")}
		if e, err := parsePeriodTime(rawString(pd, "endDateTime")); err == nil {
			l.TimeRange = st.Format("15:04") + "–" + e.In(loc).Format("15:04")
			l.EndTime = e.In(loc).Format("15:04")
		}
		l.Subject = lessonSubject(pd, md)
		var who []string
		if t := joinedElementNames(pd, md, "TEACHER"); t != "" {
			who = append(who, t)
			l.Teacher = t
		}
		if r := joinedElementNames(pd, md, "ROOM"); r != "" {
			who = append(who, r)
			l.Room = r
		}
		if c := joinedElementNames(pd, md, "CLASS"); c != "" {
			who = append(who, c)
			l.Class = c
		}
		l.Detail = strings.Join(who, " · ")
		// A custom event has no elements to resolve, so its text is read straight
		// off the period instead. It reuses the same fields as a lesson rather than
		// adding its own layout, and is distinguished by the Custom flag.
		if isCustomPeriod(pd) {
			l.Custom = true
			l.Subject, _ = pd["customTitle"].(string)
			var evWho []string
			if teacher, _ := pd["teacher"].(string); teacher != "" {
				evWho = append(evWho, teacher)
				l.Teacher = teacher
			}
			if room, _ := pd["room"].(string); room != "" {
				evWho = append(evWho, room)
				l.Room = room
			}
			l.Detail = strings.Join(evWho, " · ")
			desc, _ := pd["description"].(string)
			l.Notes = desc
		}
		l.Cancelled = periodHasFlag(pd, "CANCELLED")
		l.Substitution = textField(pd, "substitution") != ""
		if exam, ok := pd["exam"].(map[string]any); ok && exam != nil {
			l.Exam = true
			l.ExamLabel = "Klausur"
			if t, _ := exam["examtype"].(string); t != "" {
				l.ExamLabel = t
			}
			if n, _ := exam["name"].(string); n != "" {
				l.ExamLabel += " " + n
			}
		}
		var notes []string
		if t := textField(pd, "substitution"); t != "" {
			notes = append(notes, "Vertretung: "+t)
		}
		if t := textField(pd, "info"); t != "" {
			notes = append(notes, t)
		}
		if t := textField(pd, "lesson"); t != "" {
			notes = append(notes, "Thema: "+t)
		}
		// The custom-event branch above filled these from the event's own text;
		// overwriting them here from empty element lookups would erase it.
		if !l.Custom {
			l.Notes = strings.Join(notes, " · ")
		}
		key := st.Format("2006-01-02")
		byDay[key] = append(byDay[key], l)
	}
	return byDay
}
