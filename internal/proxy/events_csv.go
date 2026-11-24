package proxy

import (
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"untis-proxy/internal/access"
	"untis-proxy/internal/store"
)

type csvField struct {
	Column int    `json:"column"` // One-based. Zero uses Value.
	Value  string `json:"value"`
}

type mappedCSV struct {
	Text       string              `json:"text"`
	Delimiter  string              `json:"delimiter"`
	Header     int                 `json:"header"`
	First      int                 `json:"first"`
	DateFormat string              `json:"dateFormat"`
	Fields     map[string]csvField `json:"fields"`
}

type csvRecord struct {
	Line   int
	Values []string
}

func readMappedCSV(req mappedCSV) ([]csvRecord, error) {
	reader := csv.NewReader(strings.NewReader(strings.TrimPrefix(req.Text, "\ufeff")))
	switch req.Delimiter {
	case ",", ";", "\t", "|":
		reader.Comma = []rune(req.Delimiter)[0]
	default:
		return nil, fmt.Errorf("choose a supported delimiter")
	}
	reader.FieldsPerRecord = -1
	var records []csvRecord
	for {
		cells, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		line, _ := reader.FieldPos(0)
		if len(cells) > 100 || len(records) >= 501 {
			return nil, fmt.Errorf("use at most 501 records and 100 columns per import")
		}
		records = append(records, csvRecord{line, cells})
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("no CSV records found")
	}
	return records, nil
}

func (req mappedCSV) field(record csvRecord, name string) string {
	source := req.Fields[name]
	if source.Column == 0 {
		return strings.TrimSpace(source.Value)
	}
	if source.Column > len(record.Values) {
		return ""
	}
	return strings.TrimSpace(record.Values[source.Column-1])
}

func mappedDate(value, format string) string {
	if format == "" {
		return normalizeImportDate(value)
	}
	parsed, err := time.Parse(format, value)
	if err != nil {
		return ""
	}
	return parsed.Format("2006-01-02")
}

func mappedClock(value string) string {
	parsed, err := time.Parse("15:04", strings.TrimPrefix(value, "T"))
	if err != nil {
		return ""
	}
	return parsed.Format("15:04")
}

type mappedEventRow struct {
	Line        int    `json:"line"`
	Student     string `json:"student"`
	Date        string `json:"date"`
	Start       string `json:"start"`
	End         string `json:"end"`
	Title       string `json:"title"`
	Room        string `json:"room,omitempty"`
	Subject     string `json:"subject,omitempty"`
	Teacher     string `json:"teacher,omitempty"`
	Description string `json:"description,omitempty"`
	Error       string `json:"error,omitempty"`
	appointment string
}

func (p *Proxy) handleMappedCSV(w http.ResponseWriter, r *http.Request, u *store.User, school string) {
	destination, phase := r.URL.Query().Get("destination"), r.URL.Query().Get("phase")
	cap := access.CapEventsTechnikImport
	if destination == "absences" {
		cap = access.CapEventsTechnikAttendanceWrite
	} else if destination != "events" {
		writeJSONError(w, http.StatusBadRequest, "unknown import destination")
		return
	}
	if !p.technikAllowed(u, school, cap) {
		p.forbidden(w)
		return
	}
	var req mappedCSV
	if !decodeJSON(readBody(r), &req) {
		writeJSONError(w, http.StatusBadRequest, "invalid CSV import request")
		return
	}
	records, err := readMappedCSV(req)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Header < 0 || req.Header > len(records) || req.First <= req.Header || req.First > len(records) {
		writeJSONError(w, http.StatusBadRequest, "choose an existing first data record after the header")
		return
	}
	width := 0
	for _, record := range records {
		width = max(width, len(record.Values))
	}
	columns := make([]string, width)
	for i := range columns {
		columns[i] = fmt.Sprintf("Column %d", i+1)
		if req.Header > 0 && i < len(records[req.Header-1].Values) {
			if label := strings.TrimSpace(records[req.Header-1].Values[i]); label != "" {
				columns[i] += " · " + label
			}
		}
	}
	if phase == "columns" {
		p.writeJSON(w, map[string]any{"columns": columns, "sample": records[req.First-1].Values})
		return
	}
	if phase != "preview" && phase != "apply" {
		writeJSONError(w, http.StatusBadRequest, "unknown import phase")
		return
	}
	for _, source := range req.Fields {
		if source.Column < 0 || source.Column > width {
			writeJSONError(w, http.StatusBadRequest, "column outside CSV")
			return
		}
	}
	switch req.DateFormat {
	case "", "2006-01-02", "02.01.2006", "2/1/2006", "1/2/2006", "20060102":
	default:
		writeJSONError(w, http.StatusBadRequest, "unsupported date format")
		return
	}
	students, err := p.store.ListStudents(school)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not load students")
		return
	}
	byLogin := map[string]string{}
	byName := map[string][]string{}
	for _, student := range students {
		byLogin[strings.ToLower(student.Username)] = student.Username
		if name := strings.ToLower(strings.TrimSpace(student.DisplayName)); name != "" {
			byName[name] = append(byName[name], student.Username)
		}
	}
	seen := map[[4]string]bool{}
	days := map[string][]store.StudentEvent{}
	var rows []mappedEventRow
	failed := 0
	for _, record := range records[req.First-1:] {
		row := mappedEventRow{Line: record.Line, Student: req.field(record, "student"), Date: mappedDate(req.field(record, "date"), req.DateFormat),
			Start: mappedClock(req.field(record, "start")), End: mappedClock(req.field(record, "end")),
			Title: req.field(record, "title"), Room: req.field(record, "room"), Subject: req.field(record, "subject"),
			Teacher: req.field(record, "teacher"), Description: req.field(record, "description")}
		name := strings.ToLower(row.Student)
		if username := byLogin[name]; username != "" {
			row.Student = username
		} else if matches := byName[name]; len(matches) == 1 {
			row.Student = matches[0]
		} else {
			row.Error = "Student is unknown or ambiguous. Use their username."
		}
		if row.Date == "" {
			row.Error = "Date is missing or invalid."
		}
		if row.Error == "" && destination == "events" && row.Title == "" {
			row.Error = "Title is required."
		}
		if row.Error == "" && destination == "absences" {
			events, ok := days[row.Date]
			if !ok {
				events, err = p.store.AllStudentEventsForRange(school, row.Date, row.Date)
				if err != nil {
					writeJSONError(w, http.StatusInternalServerError, "could not load appointments")
					return
				}
				days[row.Date] = events
			}
			row.matchAppointment(events, req.field(record, "appointment"), req.field(record, "eventStart"))
			if req.field(record, "start") != "" && mappedClock(req.field(record, "start")) == "" ||
				req.field(record, "end") != "" && mappedClock(req.field(record, "end")) == "" {
				row.Error = "Absence time is invalid."
			}
		}
		if row.Error == "" {
			row.Error = validateEventTimes(row.Start, row.End)
		}
		key := [4]string{eventKeyFold(row.Student), row.Date, row.Start, row.End}
		if destination == "absences" {
			key = [4]string{eventKeyFold(row.Student), row.appointment}
		}
		if row.Error == "" && seen[key] {
			row.Error = "Duplicate target in this CSV."
		}
		if row.Error != "" {
			failed++
		} else {
			seen[key] = true
		}
		rows = append(rows, row)
	}
	if phase == "preview" {
		p.writeJSON(w, map[string]any{"rows": rows, "errors": failed})
		return
	}
	if failed > 0 {
		writeJSONError(w, http.StatusBadRequest, "fix the rows flagged in the preview before importing")
		return
	}
	created, skipped := 0, 0
	for _, row := range rows {
		changed, err := p.writeMappedRow(school, destination, u.Username, row)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			p.writeJSON(w, map[string]any{"error": fmt.Sprintf("Import stopped at CSV line %d after %d changes. Refresh and retry remaining records.", row.Line, created)})
			return
		}
		if changed {
			created++
		} else {
			skipped++
		}
	}
	p.audit(r, school, "event.csv_import", destination, fmt.Sprintf("%d changed, %d skipped", created, skipped))
	p.writeJSON(w, map[string]any{"changed": created, "skipped": skipped})
}

func (row *mappedEventRow) matchAppointment(events []store.StudentEvent, key, eventStart string) {
	if key == "" && eventStart != "" && mappedClock(eventStart) == "" {
		row.Error = "Event start time is invalid."
		return
	}
	if key == "" && row.Title == "" {
		row.Error = "Map an appointment key or event title."
		return
	}
	var match *store.StudentEvent
	for i := range events {
		event := &events[i]
		if event.Username != eventKeyFold(row.Student) {
			continue
		}
		if key != "" {
			if sharedEventKey(*event) != key {
				continue
			}
		} else if !strings.EqualFold(event.Title, row.Title) || (row.Room != "" && !strings.EqualFold(event.Room, row.Room)) ||
			(eventStart != "" && mappedClock(event.StartTime) != mappedClock(eventStart)) {
			continue
		}
		if match != nil && sharedEventKey(*match) != sharedEventKey(*event) {
			row.Error = "More than one matching appointment. Map the event start, room, or appointment key."
			return
		}
		match = event
	}
	if match == nil {
		row.Error = "No matching appointment for this student on this date."
		return
	}
	row.appointment = sharedEventKey(*match)
	row.Title, row.Room = match.Title, match.Room
	if row.Start == "" {
		row.Start = mappedClock(match.StartTime)
	}
	if row.End == "" {
		row.End = mappedClock(match.EndTime)
	}
}

func (p *Proxy) writeMappedRow(school, destination, actor string, row mappedEventRow) (bool, error) {
	if destination == "absences" {
		start, end := "T"+row.Start, "T"+row.End
		return p.store.ImportEventAbsence(school, row.appointment, row.Date, row.Student, start, end, actor, clockLabel(start)+"–"+clockLabel(end))
	}
	created, err := p.store.ImportStudentEvent(school, row.Student, store.NewStudentEvent{
		Date: row.Date, StartTime: row.Start, EndTime: row.End, Title: row.Title, Room: row.Room,
		Subject: row.Subject, Teacher: row.Teacher, Description: row.Description,
	}, actor)
	if created {
		p.announceStudentEventChange(school, row.Student, "created")
	}
	return created, err
}
