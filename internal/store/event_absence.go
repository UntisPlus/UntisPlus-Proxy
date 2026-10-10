package store

import (
	"database/sql"
	"hash/fnv"
	"time"
)

// eventAbsenceIDSpace keeps the synthetic ids handed to the app for local
// absences clear of real absence ids, which are small positive numbers. An id in
// this range can only have come from a proxy event register.
const eventAbsenceIDSpace = -3_000_000_000

// EventAbsenceID derives the synthetic id handed to the app for one student's
// absence on an appointment. It depends only on the appointment and the student,
// so re-creating an absence keeps the same id and a delete after a dropped
// response still matches.
func EventAbsenceID(appointmentKey, username string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(appointmentKey))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(eventKey(username)))
	return eventAbsenceIDSpace - int64(h.Sum64()%1_000_000_000)
}

// EventAbsence is one student marked absent from one hand-authored appointment.
//
// A proxy event exists only here, so there is no upstream absence to write to:
// the row is the whole record. The appointment it hangs off is identified by
// AppointmentKey, a stable hash of the fields every viewer sees, so the several
// per-student event rows that make up one appointment all resolve to the same
// register.
type EventAbsence struct {
	School         string
	AppointmentKey string
	Date           string // YYYY-MM-DD, copied from the appointment's own date
	Username       string
	StartTime      string // the absence window, as the client sent it ("T14:00")
	EndTime        string
	CreatedBy      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// SetEventAbsence upserts one student's absence on an appointment and returns the
// time it was written. Marking a student absent twice converges on one row.
func (s *Store) SetEventAbsence(school, appointmentKey, date, username, start, end, by string) (time.Time, error) {
	now := time.Now().Truncate(time.Second)
	_, err := s.db.Exec(`INSERT INTO event_absences
		(school, appointment_key, date, username, start_time, end_time, created_by, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(school, appointment_key, username) DO UPDATE SET
			date=excluded.date, start_time=excluded.start_time, end_time=excluded.end_time,
			created_by=excluded.created_by, updated_at=excluded.updated_at`,
		school, appointmentKey, date, eventKey(username), start, end, norm(by), now.Unix(), now.Unix())
	return now, err
}

// DeleteEventAbsence removes one student's absence from an appointment. Deleting a
// row that is already gone is not an error, so a client retrying after a dropped
// response converges on the same state.
func (s *Store) DeleteEventAbsence(school, appointmentKey, username string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM event_absences
		WHERE school=? AND appointment_key=? AND username=?`,
		school, appointmentKey, eventKey(username))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// EventAbsences returns every absence recorded for one appointment, across all
// its assigned students.
func (s *Store) EventAbsences(school, appointmentKey string) ([]EventAbsence, error) {
	rows, err := s.db.Query(`SELECT `+eventAbsenceCols+` FROM event_absences
		WHERE school=? AND appointment_key=?
		ORDER BY username`, school, appointmentKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEventAbsences(rows)
}

// EventAbsenceByID returns one absence by the synthetic id the app was given.
// The id is derived from the appointment and the student rather than stored, so
// this reads the whole appointment's rows and matches. The register of a single
// appointment is small, so this stays cheap.
func (s *Store) EventAbsenceByID(school string, absenceID int64) (EventAbsence, bool, error) {
	rows, err := s.db.Query(`SELECT `+eventAbsenceCols+` FROM event_absences WHERE school=?`, school)
	if err != nil {
		return EventAbsence{}, false, err
	}
	defer rows.Close()
	all, err := scanEventAbsences(rows)
	if err != nil {
		return EventAbsence{}, false, err
	}
	for _, a := range all {
		if EventAbsenceID(a.AppointmentKey, a.Username) == absenceID {
			return a, true, nil
		}
	}
	return EventAbsence{}, false, nil
}

// AllEventAbsences returns the appointments' absences whose date lies in
// [from, to], across the whole school. It backs the admin dashboard, so it is not
// scoped to one appointment.
func (s *Store) AllEventAbsences(school, from, to string) ([]EventAbsence, error) {
	rows, err := s.db.Query(`SELECT `+eventAbsenceCols+` FROM event_absences
		WHERE school=? AND date>=? AND date<=?
		ORDER BY date, appointment_key, username`, school, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEventAbsences(rows)
}

const eventAbsenceCols = `school, appointment_key, date, username, start_time, end_time,
	created_by, created_at, updated_at`

func scanEventAbsences(rows *sql.Rows) ([]EventAbsence, error) {
	out := []EventAbsence{}
	for rows.Next() {
		var a EventAbsence
		var created, updated int64
		if err := rows.Scan(&a.School, &a.AppointmentKey, &a.Date, &a.Username, &a.StartTime,
			&a.EndTime, &a.CreatedBy, &created, &updated); err != nil {
			return nil, err
		}
		a.CreatedAt = time.Unix(created, 0)
		a.UpdatedAt = time.Unix(updated, 0)
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetEventAbsenceChecked records that the appointment's register was submitted,
// returning the time it was marked.
func (s *Store) SetEventAbsenceChecked(school, appointmentKey, by string) (time.Time, error) {
	now := time.Now().Truncate(time.Second)
	_, err := s.db.Exec(`INSERT INTO event_absence_checked
		(school, appointment_key, checked_by, checked_at)
		VALUES (?,?,?,?)
		ON CONFLICT(school, appointment_key) DO UPDATE SET
			checked_by=excluded.checked_by, checked_at=excluded.checked_at`,
		school, appointmentKey, norm(by), now.Unix())
	return now, err
}

// EventAbsenceChecked reports whether the appointment's register has been
// submitted. An appointment nobody has checked reports false, which is the same
// value the app starts from, so "unchecked" and "not yet read" look alike.
func (s *Store) EventAbsenceChecked(school, appointmentKey string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM event_absence_checked
		WHERE school=? AND appointment_key=?`, school, appointmentKey).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
