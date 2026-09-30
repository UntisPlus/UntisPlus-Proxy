package proxy

// The /week page: a server-rendered, JS-free week view for anyone holding a
// calendar token. It exists because a calendar subscription is a poor fit for a
// parent on a phone — they want one bookmarkable page that says "when and where
// is the next lesson" without setting up a subscription.

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"net/http"
	"sort"
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
	TimeRange    string
	Subject      string
	Detail       string
	Notes        string
	ExamLabel    string
	Substitution bool
	Cancelled    bool
	Exam         bool
}

// weekDay is one rendered day column.
type weekDay struct {
	Label    string
	Relative string
	Today    bool
	Lessons  []weekLesson
}

// weekView is the template model.
type weekView struct {
	Name       string
	Timezone   string
	WeekLabel  string
	RenderedAt string
	ICSURL     string
	Version    int64
	Days       []weekDay
}

// handleWeekPage renders the week view for a calendar token. Access is the same
// as the .ics feed — possession of the token is the credential — so it must be
// treated as a secret all the same.
func (p *Proxy) handleWeekPage(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSuffix(r.PathValue("token"), ".html")
	tok, err := p.store.ClassTokenByToken(token)
	if err != nil || tok == nil {
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
	// A rolling seven-day window starting today: on a Friday evening "this week"
	// is mostly over, and on Sunday it is entirely over. What a person wants
	// here is what is coming.
	today := now.In(loc)
	dayStart := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, loc)
	weekStart := dayStart
	weekEnd := dayStart.AddDate(0, 0, 6)
	feed, err := p.resolveFeed(tok, weekStart.Format("2006-01-02"), weekEnd.Format("2006-01-02"))
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("Stundenplan konnte nicht geladen werden: " + err.Error() + "\n"))
		return
	}
	md := p.masterData(tok.School)

	view := weekView{
		Name:       feed.Name,
		Timezone:   tz,
		WeekLabel:  weekStart.Format("02.01.") + " – " + weekEnd.Format("02.01.2006"),
		RenderedAt: now.In(loc).Format("02.01.2006 15:04"),
		ICSURL:     "/api/calendar/" + token + ".ics",
		Version:    feed.Version,
	}
	byDay := map[string][]weekLesson{}
	for _, pd := range feed.Periods {
		st, err := parsePeriodTime(rawString(pd, "startDateTime"))
		if err != nil {
			continue
		}
		st = st.In(loc)
		l := weekLesson{TimeRange: st.Format("15:04")}
		if e, err := parsePeriodTime(rawString(pd, "endDateTime")); err == nil {
			l.TimeRange = st.Format("15:04") + "–" + e.In(loc).Format("15:04")
		}
		l.Subject = lessonSubject(pd, md)
		var who []string
		if t := joinedElementNames(pd, md, "TEACHER"); t != "" {
			who = append(who, t)
		}
		if r := joinedElementNames(pd, md, "ROOM"); r != "" {
			who = append(who, r)
		}
		if c := joinedElementNames(pd, md, "CLASS"); c != "" {
			who = append(who, c)
		}
		l.Detail = strings.Join(who, " · ")
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
		l.Notes = strings.Join(notes, " · ")
		key := st.Format("2006-01-02")
		byDay[key] = append(byDay[key], l)
	}
	todayKey := dayStart.Format("2006-01-02")
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

	var buf bytes.Buffer
	if err := weekTemplate.Execute(&buf, view); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
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
