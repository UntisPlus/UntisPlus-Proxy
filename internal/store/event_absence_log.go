package store

import "time"

// Actions recorded in the event absence changelog. They are stored verbatim, so
// they are the contract the dashboard reads.
const (
	EventAbsenceActionCreated   = "created"
	EventAbsenceActionDeleted   = "deleted"
	EventAbsenceActionEdited    = "edited"
	EventAbsenceActionSubmitted = "submitted"
)

// EventAbsenceLogEntry is one recorded change to the absence register.
//
// The log is append-only: there is no update and no delete, so an entry stays
// even after the register row it describes has been undone. AppointmentKey and
// Username may no longer exist in event_absences; Date and the human detail
// string are what keep the entry readable after the fact.
type EventAbsenceLogEntry struct {
	ID             int64
	School         string
	AppointmentKey string
	Date           string
	Username       string
	Action         string // one of the EventAbsenceAction constants
	Detail         string // free text: the times, the old/new student, …
	Actor          string
	CreatedAt      time.Time
}

// LogEventAbsenceChange appends one entry to the register's audit trail. The
// write is unconditional: this is a log, and a failure to write a later entry
// must never be mistaken for the change not having happened.
func (s *Store) LogEventAbsenceChange(school, appointmentKey, date, username, action, detail, actor string) error {
	_, err := s.db.Exec(`INSERT INTO event_absence_log
		(school, appointment_key, date, username, action, detail, actor, created_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		school, appointmentKey, date, eventKey(username), action, detail, norm(actor), time.Now().Truncate(time.Second).Unix())
	return err
}

// EventAbsenceLog returns the whole audit trail for a school whose appointment
// date lies in [from, to], newest first. There is deliberately no delete or
// update counterpart: the log is write-only from the app's side and read-only
// from the dashboard's, so the history cannot be rewritten.
func (s *Store) EventAbsenceLog(school, from, to string) ([]EventAbsenceLogEntry, error) {
	rows, err := s.db.Query(`SELECT id, school, appointment_key, date, username, action, detail, actor, created_at
		FROM event_absence_log
		WHERE school=? AND date>=? AND date<=?
		ORDER BY created_at DESC, id DESC`, school, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EventAbsenceLogEntry{}
	for rows.Next() {
		var e EventAbsenceLogEntry
		var at int64
		if err := rows.Scan(&e.ID, &e.School, &e.AppointmentKey, &e.Date, &e.Username,
			&e.Action, &e.Detail, &e.Actor, &at); err != nil {
			return nil, err
		}
		e.CreatedAt = time.Unix(at, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}
