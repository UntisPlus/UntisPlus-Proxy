# Events and absences CSV import

Open **Events & Absences** and select the destination school. In **Student
events**, choose students and rooms from the dropdowns. Each selection adds
one item and can be removed individually. A new event accepts several students
and rooms. The event list has its own student dropdown. In **Event absences**,
the room and student dropdowns filter the loaded appointments.

## Map a CSV in the dashboard

1. Use **Map CSV columns** under Student events or **Import event absences from
   CSV** under Event absences. Paste text or choose a file.
2. Select comma, semicolon, tab, or pipe as the separator. Set **Header record**
   to the record containing column names, or to 0 for a file without headings.
   Set **First data record** to the first record you want to import.
3. Click **Read columns**. For each destination field, choose a column, enter
   a fixed value for all rows, or leave the field unmapped. Select the date
   format if the source uses slash dates or compact numeric dates.
4. Click **Preview rows**. The preview shows the resolved student, date, event,
   room and times. Correct any flagged rows before clicking **Save import**.
   If you edit the input or mapping, preview it again before saving.

Records are counted from 1. Blank lines are ignored, and a quoted multiline
cell still belongs to one record. Errors in the preview show the file line
where that record starts. The parser accepts up to 501 records and 100 columns.
The dashboard reads UTF-8 files and falls back to Windows-1252 for uploaded files.
The separate **Import Technik export** button keeps the original Technik JSON
and fixed-column CSV workflow.

| Destination | Required fields | Optional fields |
| --- | --- | --- |
| Student event | Student, date, start time, end time, title | Room, subject, teacher, description |
| Event absence | Student, date, event title or appointment key | Room and event start to distinguish matching titles, absence start and end |

Each data row targets one student. Match a student by login or by a unique
display name in the selected school. Use the login if two students share a
name. Use 24-hour times such as `09:15`. For event absences, an empty absence
start or end uses the corresponding appointment time. The student must already
be assigned to the matching appointment. If several appointments match, map the
event start, room, or exact appointment key.

For example, this file maps one event to an existing student login:

```csv
Student;Day;Begins;Ends;Event;Room
student1;2026-11-24;09:00;10:00;Technik rehearsal;Aula
```

Choose semicolon, header record 1, and first data record 2. You can also leave
the Room column unmapped and choose a fixed room value instead.

Events are added to UntisPlus timetables. Absence imports mark students absent
or update their absence time on existing local event registers. They do not
write to WebUntis upstream or mark missing rows as present. Existing event
slots and identical absence records are skipped, and absence changes appear in
the register's change log. If a storage error interrupts an import, the
response reports how many rows were saved. Retrying preserves existing event
slots and skips identical absence records.

## API and permissions

Send JSON to
`POST /api/admin/events/import?format=mapped-csv&school=NAME&destination=events&phase=preview`
with an authenticated proxy session. Set `destination` to `events` or
`absences`. The Technik feature must be enabled. Event imports require
`events.technik.import`; absence imports use `events.technik.attendance.write`.
Server capability gates apply, including to administrators.

| Phase | Behavior |
| --- | --- |
| `columns` | Returns column labels and a sample from the first data record |
| `preview` | Returns mapped rows and an error count without saving |
| `apply` | Revalidates the input and returns counts of `changed` and `skipped` rows |

The request body contains `text`, `delimiter`, `header`, `first`, optional
`dateFormat`, and a `fields` object. Use one-based column indices such as
`"student":{"column":1}` or fixed values such as `"room":{"value":"Aula"}`.
The event field keys are `student`, `date`, `start`, `end`, `title`,
`room`, `subject`, `teacher`, and `description`. The absence field keys
are `student`, `date`, `title`, `appointment`, `room`, `eventStart`,
`start`, and `end`. The date format values are the empty string, `2006-01-02`,
`02.01.2006`, `2/1/2006`, `1/2/2006`, and `20060102`.
