package proxy

// Human-readable rendering of timetable changes.
//
// The stored change rows only carry the *current* state of a period, so a naive
// summary can say "1 changed" but never "the room moved from R204 to R112". The
// digest therefore diffs the changed rows against the previous snapshot, which
// the poll loop already has in hand, and produces a title plus one line per
// lesson. It feeds the ntfy message, the X-Untis-Summary webhook header and the
// /api/timetable/changes answer.

import (
	"fmt"
	"strings"

	"untis-proxy/internal/store"
)

// digestMaxLines caps how many lessons a single notification spells out.
const digestMaxLines = 8

// changeDigest is the rendered form of one change event.
type changeDigest struct {
	Title   string   `json:"title"`
	Summary string   `json:"summary"`
	Lines   []string `json:"lines"`
	Changed int      `json:"changed"`
	Added   int      `json:"added"`
	Removed int      `json:"removed"`
}

// Message renders the digest body for a push notification.
func (d changeDigest) Message() string {
	lines := d.Lines
	extra := ""
	if len(lines) > digestMaxLines {
		extra = fmt.Sprintf("\n… and %d more", len(lines)-digestMaxLines)
		lines = lines[:digestMaxLines]
	}
	if len(lines) == 0 {
		return d.Summary
	}
	return strings.Join(lines, "\n") + extra
}

// buildDigest renders changed rows, diffing them against prev (the snapshot as
// it was before the change; may be nil or missing an entry).
func (p *Proxy) buildDigest(school string, classID int64, prev []store.PeriodRow, rows []store.PeriodRow) changeDigest {
	return p.buildDigestNamed(school, p.classDisplayName(school, classID), classID, prev, rows)
}

// buildDigestNamed is buildDigest with the class name supplied by the caller.
//
// It exists because the enqueue callback that renders the digest runs inside the
// snapshot transaction, and resolving a class name means querying the store — on
// a single-connection pool that deadlocks against the very connection the
// transaction holds. The caller resolves the name before opening the
// transaction. Everything else here is pure, so nothing else can reach the
// database from inside the callback.
func (p *Proxy) buildDigestNamed(school, className string, classID int64, prev []store.PeriodRow, rows []store.PeriodRow) changeDigest {
	prevByID := make(map[int64]store.PeriodRow, len(prev))
	for _, r := range prev {
		prevByID[r.PeriodID] = r
	}
	d := changeDigest{Lines: make([]string, 0, len(rows))}
	for _, r := range rows {
		switch r.Kind {
		case "ADDED":
			d.Added++
		case "REMOVED":
			d.Removed++
		default:
			d.Changed++
		}
		d.Lines = append(d.Lines, digestLine(r, prevByID[r.PeriodID]))
	}
	d.Title = digestTitleNamed(className, d)
	// The single-line summary stays count-based (it goes into the
	// X-Untis-Summary header where anything greppable belongs); the per-lesson
	// detail lives in the notification message.
	summary := d.Title
	if counts := d.countsLine(); counts != "" {
		summary += ": " + counts
	}
	d.Summary = asciiHeader(summary)
	return d
}

// countsLine renders the change-kind breakdown, e.g. "1 added · 1 removed".
func (d changeDigest) countsLine() string {
	parts := make([]string, 0, 3)
	if d.Added > 0 {
		parts = append(parts, fmt.Sprintf("%d added", d.Added))
	}
	if d.Removed > 0 {
		parts = append(parts, fmt.Sprintf("%d removed", d.Removed))
	}
	if d.Changed > 0 {
		parts = append(parts, fmt.Sprintf("%d changed", d.Changed))
	}
	return strings.Join(parts, " · ")
}

// digestTitle names the class and the size of the change.
func digestTitleNamed(className string, d changeDigest) string {
	total := len(d.Lines)
	noun := "changes"
	if total == 1 {
		noun = "change"
	}
	return fmt.Sprintf("%s · %d %s", className, total, noun)
}

// classDisplayName resolves a readable class name, falling back to its id.
func (p *Proxy) classDisplayName(school string, classID int64) string {
	if n := p.store.ElementName(school, "CLASS", classID); n != "" {
		return n
	}
	if md := p.masterData(school); md != nil {
		if n := elementName(md, "CLASS", classID); n != "" {
			return n
		}
	}
	st := p.stateFor(school)
	st.klMu.Lock()
	defer st.klMu.Unlock()
	if n := st.klasses[classID]; n != "" {
		return n
	}
	return fmt.Sprintf("class %d", classID)
}

// digestLine renders one changed period, e.g.
// "Tue 29.09. 08:00–08:45 · Mathe · room R204 → R112 · Müller · Sub: entfällt".
func digestLine(r store.PeriodRow, old store.PeriodRow) string {
	parts := make([]string, 0, 5)
	if when := digestWhen(r.Start, r.End); when != "" {
		parts = append(parts, when)
	}
	if r.Subject != "" {
		parts = append(parts, r.Subject)
	}
	if old.PeriodID != 0 {
		// A diff against the previous snapshot: the whole point of the digest.
		if old.Room != "" && r.Room != "" && old.Room != r.Room {
			parts = append(parts, "room "+old.Room+" → "+r.Room)
		} else if r.Room != "" {
			parts = append(parts, r.Room)
		}
		if old.Teacher != "" && r.Teacher != "" && old.Teacher != r.Teacher {
			parts = append(parts, "teacher "+old.Teacher+" → "+r.Teacher)
		} else if r.Teacher != "" {
			parts = append(parts, r.Teacher)
		}
		if old.Subject != "" && r.Subject != "" && old.Subject != r.Subject {
			parts = append(parts, "subject "+old.Subject+" → "+r.Subject)
		}
		if !sameDayTime(old.Start, r.Start) {
			from := digestWhen(old.Start, old.End)
			if from == "" {
				from = old.Start
			}
			parts = append(parts, "moved from "+from)
		}
	} else {
		if r.Room != "" {
			parts = append(parts, r.Room)
		}
		if r.Teacher != "" {
			parts = append(parts, r.Teacher)
		}
	}
	if d := strings.ReplaceAll(strings.TrimSpace(r.Description), "\n", "; "); d != "" {
		parts = append(parts, d)
	}
	marker := ""
	switch r.Kind {
	case "ADDED":
		marker = "new"
	case "REMOVED":
		marker = "removed"
	}
	switch {
	case marker == "":
	case len(parts) == 0:
		parts = []string{marker + " lesson"}
	default:
		parts[0] = marker + ": " + parts[0]
	}
	return strings.Join(parts, " · ")
}

// digestWhen formats a period range as "Tue 29.09. 08:00–08:45", degrading to
// the raw start string when it cannot be parsed.
func digestWhen(start, end string) string {
	s, err := parsePeriodTime(start)
	if err != nil {
		return ""
	}
	out := s.Format("Mon 02.01. 15:04")
	if e, err := parsePeriodTime(end); err == nil {
		if e.After(s) {
			out += "–" + e.Format("15:04")
		}
	}
	return out
}

func sameDayTime(a, b string) bool {
	ta, err1 := parsePeriodTime(a)
	tb, err2 := parsePeriodTime(b)
	if err1 != nil || err2 != nil {
		return a == b
	}
	return ta.Equal(tb)
}

// asciiHeader makes a digest string safe for an HTTP header value: single line,
// printable ASCII, with the German umlauts transliterated so receivers that
// treat headers as latin-1 do not see mojibake.
func asciiHeader(s string) string {
	repl := strings.NewReplacer(
		"ä", "ae", "ö", "oe", "ü", "ue", "Ä", "Ae", "Ö", "Oe", "Ü", "Ue", "ß", "ss",
		"→", "->", "–", "-", "·", "-", "…", "...", "\r", " ", "\n", " ", "\t", " ")
	return repl.Replace(s)
}
