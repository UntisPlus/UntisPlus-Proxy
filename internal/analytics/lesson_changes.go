package analytics

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type SubjectRef struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name"`
}

type Cover struct {
	Original TeacherRef `json:"original"`
	Teacher  TeacherRef `json:"teacher"`
}

type Change struct {
	Date           string       `json:"date"`
	Start          string       `json:"start,omitempty"`
	End            string       `json:"end,omitempty"`
	Cancelled      bool         `json:"cancelled"`
	Clash          string       `json:"clash"`
	Subjects       []SubjectRef `json:"subjects"`
	MissedTeachers []TeacherRef `json:"missedTeachers"`
	Covers         []Cover      `json:"covers"`
}

type ChangeCount struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name"`
	Lessons   int    `json:"lessons"`
	Cancelled int    `json:"cancelled"`
	Covered   int    `json:"covered"`
}

type LessonChanges struct {
	CancelledLessons int           `json:"cancelledLessons"`
	CoveredLessons   int           `json:"coveredLessons"`
	ClashingLessons  int           `json:"clashingLessons"`
	Changes          []Change      `json:"lessonChanges"`
	MissedTeachers   []ChangeCount `json:"missedTeachers"`
	MissedSubjects   []ChangeCount `json:"missedSubjects"`
	CoverTeachers    []ChangeCount `json:"coverTeachers"`
}

func buildLessonChanges(lessons []Lesson, opts Options, allowed map[string]bool, labels map[string]string) LessonChanges {
	out := LessonChanges{Changes: []Change{}, MissedTeachers: []ChangeCount{}, MissedSubjects: []ChangeCount{}, CoverTeachers: []ChangeCount{}}
	teacherCounts, subjectCounts, coverCounts := map[string]ChangeCount{}, map[string]ChangeCount{}, map[string]ChangeCount{}
	subjectLabels := map[string]string{}
	visible := make([]Lesson, 0, len(lessons))
	seen := map[[2]string]bool{}
	for _, lesson := range lessons {
		if _, err := time.Parse(time.DateOnly, lesson.Date); err != nil {
			continue
		}
		key := [2]string{lesson.Date, lesson.Key}
		if !seen[key] {
			seen[key] = true
			visible = append(visible, lesson)
		}
	}
	sort.SliceStable(visible, func(i, j int) bool {
		a, b := visible[i], visible[j]
		if a.Date != b.Date {
			return a.Date < b.Date
		}
		if a.Start != b.Start {
			return a.Start < b.Start
		}
		return a.Key < b.Key
	})
	// Only compare times within the same personal timetable and calendar day.
	active := map[string][]Lesson{}
	if opts.DetectClashes {
		for _, lesson := range visible {
			if lesson.Code != "cancelled" && lesson.Start != "" && lesson.End > lesson.Start {
				active[lesson.Date] = append(active[lesson.Date], lesson)
			}
		}
	}
	teacherAllowed := func(ref TeacherRef) bool {
		if ref.ID == "" || (opts.MinLessons > 0 && !allowed[ref.ID]) {
			return false
		}
		for _, prefix := range opts.ExcludePrefixes {
			if strings.HasPrefix(ref.Name, prefix) {
				return false
			}
		}
		return true
	}
	for _, lesson := range visible {
		cancelled := lesson.Code == "cancelled"
		if !cancelled && len(lesson.Covers) == 0 {
			continue
		}
		row := Change{Date: lesson.Date, Start: lesson.Start, End: lesson.End, Cancelled: cancelled,
			Clash:    clashEvidence(lesson, active[lesson.Date], opts.DetectClashes),
			Subjects: []SubjectRef{}, MissedTeachers: []TeacherRef{}, Covers: []Cover{}}
		missed := map[string]bool{}
		markMissed := func(ref TeacherRef) {
			if !missed[ref.ID] {
				missed[ref.ID] = true
				row.MissedTeachers = append(row.MissedTeachers, namedTeacher(ref, opts.Anonymise, labels))
				countChange(teacherCounts, ref.ID, ref.Name, cancelled)
			}
		}
		if cancelled {
			for _, ref := range lesson.Teachers {
				if teacherAllowed(ref) && (opts.TeacherID == "" || opts.TeacherID == ref.ID) {
					markMissed(ref)
				}
			}
			// Missing teacher metadata must not erase an observed cancellation.
			if len(missed) == 0 && (opts.TeacherID != "" || opts.MinLessons > 0) {
				continue
			}
			out.CancelledLessons++
		} else {
			coverSeen := map[string]bool{}
			for _, pair := range lesson.Covers {
				if !teacherAllowed(pair.Original) ||
					(opts.TeacherID != "" && opts.TeacherID != pair.Original.ID && opts.TeacherID != pair.Teacher.ID) {
					continue
				}
				markMissed(pair.Original)
				row.Covers = append(row.Covers, Cover{
					Original: namedTeacher(pair.Original, opts.Anonymise, labels),
					Teacher:  namedTeacher(pair.Teacher, opts.Anonymise, labels),
				})
				if !coverSeen[pair.Teacher.ID] {
					coverSeen[pair.Teacher.ID] = true
					countChange(coverCounts, pair.Teacher.ID, pair.Teacher.Name, false)
				}
			}
			if len(row.Covers) == 0 {
				continue
			}
			out.CoveredLessons++
		}
		if row.Clash == "overlap_observed" {
			out.ClashingLessons++
		}
		for _, subject := range lesson.Subjects {
			countChange(subjectCounts, subject.ID, subject.Name, cancelled)
			if opts.Anonymise {
				if subjectLabels[subject.ID] == "" {
					subjectLabels[subject.ID] = fmt.Sprintf("Subject %d", len(subjectLabels)+1)
				}
				subject = SubjectRef{Name: subjectLabels[subject.ID]}
			}
			row.Subjects = append(row.Subjects, subject)
		}
		out.Changes = append(out.Changes, row)
	}
	out.MissedTeachers = rankedChanges(teacherCounts, opts.Anonymise, labels)
	out.MissedSubjects = rankedChanges(subjectCounts, opts.Anonymise, subjectLabels)
	out.CoverTeachers = rankedChanges(coverCounts, opts.Anonymise, labels)
	return out
}

func clashEvidence(lesson Lesson, active []Lesson, detect bool) string {
	if !detect || lesson.Start == "" || lesson.End <= lesson.Start {
		return "unknown"
	}
	for _, other := range active {
		if other.Key != lesson.Key && lesson.Start < other.End && other.Start < lesson.End {
			return "overlap_observed"
		}
	}
	return "no_overlap_observed"
}

func namedTeacher(ref TeacherRef, anonymise bool, labels map[string]string) TeacherRef {
	if anonymise {
		if labels[ref.ID] == "" {
			labels[ref.ID] = fmt.Sprintf("Teacher %d", len(labels)+1)
		}
		return TeacherRef{Name: labels[ref.ID]}
	}
	return ref
}

func countChange(counts map[string]ChangeCount, id, name string, cancelled bool) {
	c := counts[id]
	c.ID, c.Name = id, name
	c.Lessons++
	if cancelled {
		c.Cancelled++
	} else {
		c.Covered++
	}
	counts[id] = c
}

func rankedChanges(counts map[string]ChangeCount, anonymise bool, labels map[string]string) []ChangeCount {
	out := make([]ChangeCount, 0, len(counts))
	for _, count := range counts {
		out = append(out, count)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Lessons != out[j].Lessons {
			return out[i].Lessons > out[j].Lessons
		}
		return out[i].ID < out[j].ID
	})
	if anonymise {
		for i := range out {
			out[i].ID, out[i].Name = "", labels[out[i].ID]
		}
	}
	return out
}
