package proxy

// Import of the Technik transit-calendar export.
//
// The admins keep the Technik appointments in a cloud spreadsheet and export it
// either as JSON ({"techniker":[...],"veranstaltungen":[...]}) or as the flat
// semicolon CSV. Each appointment names the technicians on duty by a short code
// ("AaaBbb", "CccDdd"); this turns those into per-student events, one row per
// technician, under the same privacy rule as a hand-authored event.
//
// A technician who has never logged in has no account yet, so their event is
// filed under their folded short code (`eventKey` folds the same way). The day
// that student logs in their appointments appear with no second import.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"untis-proxy/internal/store"
)

// importEvent is one appointment from the export, before it is fanned out to the
// technicians on duty.
type importEvent struct {
	Date        string
	StartTime   string
	EndTime     string
	Title       string
	Room        string
	Teacher     string
	Description string
	Codes       []string
}

// technikExport is a parsed export. `technicians` maps a folded short code to the
// technician's full name, and is nil for a CSV, which carries no roster. A nil
// roster means codes are taken at face value instead of being checked against the
// known technicians.
type technikExport struct {
	Events      []importEvent
	technicians map[string]string
}

// handleStudentEventImport ingests the Technik export.
//
//	POST /api/admin/events/import?school=…
//
// The body is the export itself: JSON when it starts with '{', otherwise the
// semicolon CSV. It is admin-only like the rest of the event API.
func (p *Proxy) handleStudentEventImport(w http.ResponseWriter, r *http.Request) {
	u := p.sessionUser(r)
	if u == nil {
		writeJSONError(w, http.StatusUnauthorized, "not logged in")
		return
	}
	if !p.isAdmin(u.Username) {
		p.forbidden(w)
		return
	}
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	school := r.URL.Query().Get("school")
	if school == "" {
		school = p.opts.School
	}
	body := readBody(r)
	if len(bytes.TrimSpace(body)) == 0 {
		writeJSONError(w, http.StatusBadRequest, "empty import")
		return
	}
	doc, err := parseTechnikExport(body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "cannot parse import: "+err.Error())
		return
	}
	if len(doc.Events) == 0 {
		writeJSONError(w, http.StatusBadRequest, "no appointments found in the import")
		return
	}

	students, err := p.store.ListStudents(school)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store error")
		return
	}
	byCode := map[string]string{}
	byName := map[string]string{}
	for _, su := range students {
		if su.PersonType != 5 {
			continue
		}
		if k := foldCode(su.Username); k != "" {
			if _, seen := byCode[k]; !seen {
				byCode[k] = su.Username
			}
		}
		if n := foldName(su.DisplayName); n != "" {
			if _, seen := byName[n]; !seen {
				byName[n] = su.Username
			}
		}
	}

	created := make([]map[string]any, 0)
	skipped := make([]map[string]any, 0)
	unknown := map[string]bool{}
	ignored := map[string]bool{}
	skippedTimes := 0

	for _, ev := range doc.Events {
		if !validDate(ev.Date) || validateEventTimes(ev.StartTime, ev.EndTime) != "" {
			// An appointment with no usable time cannot be placed on a calendar,
			// and inventing midnight would fabricate a slot.
			skippedTimes++
			continue
		}
		title := strings.TrimSpace(ev.Title)
		if title == "" {
			title = customSubjectDefault
		}
		for _, raw := range ev.Codes {
			key := foldCode(raw)
			if key == "" {
				continue
			}
			// With a roster, a code that names no technician (a teacher, a helper,
			// "SISS Technik") is a note, not a technician on duty, so no event is
			// filed for it.
			if doc.technicians != nil {
				if _, ok := lookupFolded(doc.technicians, key); !ok {
					ignored[strings.TrimSpace(raw)] = true
					continue
				}
			}
			username, fallback := p.resolveTechnicianUsername(key, doc, byCode, byName)
			if username == "" {
				// A code that names an existing non-student account is a teacher or
				// admin, whose timetable never carries per-student events.
				if usr, _ := p.store.GetUserInSchool(school, fallback); usr != nil && usr.PersonType != 5 {
					ignored[strings.TrimSpace(raw)] = true
					continue
				}
				username = fallback
				unknown[username] = true
			}
			if dup, found, err := p.store.StudentEventDuplicate(school, username, ev.Date, ev.StartTime, ev.EndTime); err != nil {
				writeJSONError(w, http.StatusInternalServerError, "store error")
				return
			} else if found {
				skipped = append(skipped, map[string]any{"username": username, "eventId": dup.ID, "reason": "duplicate"})
				continue
			}
			made, err := p.store.CreateStudentEvent(school, username, store.NewStudentEvent{
				Date: ev.Date, StartTime: ev.StartTime, EndTime: ev.EndTime,
				Title: title, Subject: customSubjectDefault, Room: ev.Room,
				Teacher: ev.Teacher, Description: ev.Description,
			}, u.Username)
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, "store error")
				return
			}
			created = append(created, map[string]any{"username": made.Username, "eventId": made.ID})
			p.announceStudentEventChange(school, username, "created")
		}
	}

	p.writeJSON(w, map[string]any{
		"school":       school,
		"created":      created,
		"skipped":      skipped,
		"unknown":      sortedSetKeys(unknown),
		"ignored":      sortedSetKeys(ignored),
		"skippedTimes": skippedTimes,
	})
}

// resolveTechnicianUsername maps an import code to a username. It first tries the
// code against a known student's login, then the technician's full name against a
// student's display name. It returns "" when neither matches, along with the
// folded code the caller should file the event under: that is the roster's
// canonical code when the raw token carried a trailing disambiguator
// ("SveSch1" folds to the roster's "SveSch"), so the row matches the login the
// day the student signs up.
func (p *Proxy) resolveTechnicianUsername(key string, doc technikExport, byCode, byName map[string]string) (string, string) {
	fallback := key
	if v, ok := lookupFolded(byCode, key); ok {
		return v, fallback
	}
	if doc.technicians != nil {
		if rosterKey, ok := foldedKey(doc.technicians, key); ok {
			fallback = rosterKey
			if v, ok := lookupFolded(byName, foldName(doc.technicians[rosterKey])); ok {
				return v, fallback
			}
		}
	}
	return "", fallback
}

// parseTechnikExport reads either export form. JSON is detected by its opening
// brace; everything else is treated as the semicolon CSV.
func parseTechnikExport(body []byte) (technikExport, error) {
	if bytes.HasPrefix(bytes.TrimSpace(body), []byte("{")) {
		return parseTechnikJSON(body)
	}
	return parseTechnikCSV(body)
}

func parseTechnikJSON(body []byte) (technikExport, error) {
	var doc struct {
		Techniker []struct {
			Name string `json:"name"`
			Code string `json:"code"`
		} `json:"techniker"`
		Veranstaltungen []struct {
			Datum        string   `json:"datum"`
			Beginn       string   `json:"beginn"`
			Ende         string   `json:"ende"`
			Titel        string   `json:"titel"`
			Veranstalter string   `json:"veranstalter"`
			Ort          string   `json:"ort"`
			Material     string   `json:"material"`
			Notizen      string   `json:"notizen"`
			Einsatz      []string `json:"einsatz"`
		} `json:"veranstaltungen"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return technikExport{}, err
	}
	out := technikExport{technicians: map[string]string{}}
	for _, t := range doc.Techniker {
		if k := foldCode(t.Code); k != "" {
			out.technicians[k] = t.Name
		}
	}
	for _, v := range doc.Veranstaltungen {
		out.Events = append(out.Events, importEvent{
			Date:        normalizeImportDate(v.Datum),
			StartTime:   strings.TrimSpace(v.Beginn),
			EndTime:     strings.TrimSpace(v.Ende),
			Title:       strings.TrimSpace(v.Titel),
			Room:        cleanImportText(v.Ort),
			Teacher:     strings.TrimSpace(v.Veranstalter),
			Description: joinImportText(v.Material, v.Notizen),
			Codes:       v.Einsatz,
		})
	}
	if len(out.Events) == 0 && len(doc.Techniker) == 0 {
		return out, fmt.Errorf("no techniker or veranstaltungen")
	}
	return out, nil
}

// parseTechnikCSV reads the flat export. The spreadsheet exports Windows-1252,
// while a body pasted into the browser arrives as UTF-8, so the text is decoded
// only when the bytes are not already valid UTF-8. Column order is fixed by the
// spreadsheet header: Datum;Beginn;Ende;Dauer;Aufbau;Abbau;Nachmittag?;Ort;Titel;
// Material;Veranstalter;Interessierte;Einsatz;…
func parseTechnikCSV(body []byte) (technikExport, error) {
	text := string(body)
	if !utf8.Valid(body) {
		text = decodeLatin1(body)
	}
	var out technikExport
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, ";")
		if len(fields) < 13 {
			continue
		}
		date := normalizeImportDate(fields[0])
		if date == "" {
			continue
		}
		out.Events = append(out.Events, importEvent{
			Date:        date,
			StartTime:   strings.TrimSpace(fields[1]),
			EndTime:     strings.TrimSpace(fields[2]),
			Title:       strings.TrimSpace(fields[8]),
			Room:        cleanImportText(fields[7]),
			Teacher:     strings.TrimSpace(fields[10]),
			Description: strings.TrimSpace(fields[9]),
			Codes:       splitImportCodes(fields[12]),
		})
	}
	if len(out.Events) == 0 {
		return out, fmt.Errorf("no dated rows found")
	}
	return out, nil
}

// normalizeImportDate turns the export's dotted date (and a plain ISO date) into
// the stored YYYY-MM-DD, or "" when it is not a date.
func normalizeImportDate(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for _, layout := range []string{"02.01.2006", "2.1.2006", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format("2006-01-02")
		}
	}
	return ""
}

// cleanImportText drops the placeholder values the spreadsheet uses for "no
// value" so they do not become a room called "TRUE" or a room called "/".
func cleanImportText(s string) string {
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "", "-", "/", "true", "false", "n/a", "keine technik":
		return ""
	}
	return s
}

// joinImportText combines the two free-text columns an appointment may carry.
func joinImportText(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n")
}

// splitImportCodes splits the CSV "Einsatz" cell into codes.
func splitImportCodes(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == '|'
	}) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// decodeLatin1 decodes a Windows-1252/ISO-8859-1 byte string to UTF-8 by mapping
// each byte to the same code point. The high range differs slightly between the
// two encodings, but every byte the export uses (the German umlauts) maps the
// same, so a full table would buy nothing.
func decodeLatin1(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b))
	for _, c := range b {
		sb.WriteRune(rune(c))
	}
	return sb.String()
}

// foldCode normalizes a short code: it drops a trailing parenthetical note,
// lowercases, keeps only letters and digits, and folds the umlauts. "AbcDef1
// (nur A)" and "AbcDef" both fold to "abcdef", which is also what `eventKey`
// produces from the matching login.
func foldCode(code string) string {
	c := strings.TrimSpace(code)
	if i := strings.IndexAny(c, "(（"); i >= 0 {
		c = c[:i]
	}
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(c)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return foldUmlauts(b.String())
}

// foldName normalizes a full name to a token set so "Ada Lovelace" and the
// stored "Lovelace Ada" match regardless of order or punctuation.
func foldName(name string) string {
	tokens := strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for i := range tokens {
		tokens[i] = foldUmlauts(tokens[i])
	}
	sort.Strings(tokens)
	return strings.Join(tokens, " ")
}

// foldUmlauts mirrors the store's event key folding so a code folded here is the
// same string the store will key the login by.
func foldUmlauts(s string) string {
	return strings.NewReplacer("ä", "a", "ö", "o", "ü", "u", "ß", "ss").Replace(s)
}

// lookupFolded finds a key, allowing a code to carry a trailing disambiguator the
// roster does not ("AbcDef1" vs "AbcDef"): the digits are dropped as a fallback.
func lookupFolded(m map[string]string, key string) (string, bool) {
	if k, ok := foldedKey(m, key); ok {
		return m[k], true
	}
	return "", false
}

// foldedKey is lookupFolded's key half: it returns the key that matched, so a
// caller can record the roster's spelling of a code rather than the raw token.
func foldedKey(m map[string]string, key string) (string, bool) {
	if _, ok := m[key]; ok {
		return key, true
	}
	if trimmed := strings.TrimRight(key, "0123456789"); trimmed != key {
		if _, ok := m[trimmed]; ok {
			return trimmed, true
		}
	}
	return "", false
}

// sortedKeys returns the set's members in a stable order for the response.
func sortedSetKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
