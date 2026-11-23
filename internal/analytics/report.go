// Package analytics aggregates observed upstream timetable cancellations, not
// verified physical teacher attendance. No credentials or upstream calls live here.
package analytics

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type Lesson struct {
	Key, Date, Code string
	Teachers        []TeacherRef
}
type TeacherRef struct {
	ID   string
	Name string
}
type Year struct{ Name, From, To string }
type Counts struct {
	Scheduled        int      `json:"scheduled"`
	Cancelled        int      `json:"cancelled"`
	NotCancelled     int      `json:"notCancelled"`
	CancellationRate *float64 `json:"cancellationRate"`
	NotCancelledRate *float64 `json:"notCancelledRate"`
}
type Bucket struct {
	Key string `json:"key"`
	Counts
}
type Teacher struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name"`
	Rank int    `json:"rank"`
	Counts
	ByWeekday     []Bucket `json:"byWeekday"`
	ByWeek        []Bucket `json:"byWeek"`
	BySchoolYear  []Bucket `json:"bySchoolYear"`
	WorstWeekdays []string `json:"worstWeekdays"`
}
type Report struct {
	Teachers                    []Teacher `json:"teachers"`
	Totals                      Counts    `json:"teacherAssignmentTotals"`
	UniqueLessons               int       `json:"uniqueLessons"`
	MeanTeacherNotCancelledRate *float64  `json:"meanTeacherNotCancelledRate"`
	ByWeekday                   []Bucket  `json:"byWeekday"`
	ByWeek                      []Bucket  `json:"byWeek"`
	BySchoolYear                []Bucket  `json:"bySchoolYear"`
	ExcludedIrregular           int       `json:"excludedIrregularLessons"`
	ExcludedUnknown             int       `json:"excludedUnknownTeacherLessons"`
	FilteredTeachers            int       `json:"filteredTeachers"`
	UnmappedSchoolYearLessons   int       `json:"unmappedSchoolYearLessons"`
}
type Options struct {
	MinLessons       int
	TeacherID        string
	Anonymise        bool
	IncludeIrregular bool
	ExcludePrefixes  []string
	Years            []Year
}
type accumulator struct {
	ref                TeacherRef
	counts             Counts
	days, weeks, years map[string]Counts
}

func increment(c Counts, cancelled bool) Counts {
	c.Scheduled++
	if cancelled {
		c.Cancelled++
	}
	return c
}
func finish(c Counts) Counts {
	c.NotCancelled = c.Scheduled - c.Cancelled
	if c.Scheduled > 0 {
		rate := 100 * float64(c.Cancelled) / float64(c.Scheduled)
		other := 100 - rate
		c.CancellationRate = &rate
		c.NotCancelledRate = &other
	}
	return c
}
func buckets(m map[string]Counts) []Bucket {
	keys := []string{}
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := []Bucket{}
	for _, key := range keys {
		out = append(out, Bucket{key, finish(m[key])})
	}
	return out
}
func addBuckets(dst map[string]Counts, src map[string]Counts) {
	for k, v := range src {
		c := dst[k]
		c.Scheduled += v.Scheduled
		c.Cancelled += v.Cancelled
		dst[k] = c
	}
}
func Build(lessons []Lesson, options Options) Report {
	report := Report{Teachers: []Teacher{}}
	seen := map[string]bool{}
	people := map[string]*accumulator{}
	unique := map[string]bool{}
	for _, lesson := range lessons {
		date, err := time.Parse("2006-01-02", lesson.Date)
		if err != nil {
			continue
		}
		if lesson.Code == "irregular" && !options.IncludeIrregular {
			report.ExcludedIrregular++
			continue
		}
		year := "unmapped"
		for _, y := range options.Years {
			if lesson.Date >= y.From && lesson.Date <= y.To {
				year = y.Name
				break
			}
		}
		iy, iw := date.ISOWeek()
		week := fmt.Sprintf("%04d-W%02d", iy, iw)
		weekday := fmt.Sprintf("%d-%s", (int(date.Weekday())+6)%7+1, date.Weekday().String())
		accepted := false
		teacherSeen := map[string]bool{}
		for _, ref := range lesson.Teachers {
			if ref.ID == "" || teacherSeen[ref.ID] || (options.TeacherID != "" && options.TeacherID != ref.ID) {
				continue
			}
			teacherSeen[ref.ID] = true
			excluded := false
			for _, prefix := range options.ExcludePrefixes {
				if strings.HasPrefix(ref.Name, prefix) {
					excluded = true
				}
			}
			if excluded {
				continue
			}
			key := lesson.Key + "/" + lesson.Date + "/" + ref.ID
			if seen[key] {
				continue
			}
			seen[key] = true
			accepted = true
			a := people[ref.ID]
			if a == nil {
				a = &accumulator{ref: ref, days: map[string]Counts{}, weeks: map[string]Counts{}, years: map[string]Counts{}}
				people[ref.ID] = a
			}
			cancelled := lesson.Code == "cancelled"
			a.counts = increment(a.counts, cancelled)
			a.days[weekday] = increment(a.days[weekday], cancelled)
			a.weeks[week] = increment(a.weeks[week], cancelled)
			a.years[year] = increment(a.years[year], cancelled)
		}
		if len(lesson.Teachers) == 0 {
			report.ExcludedUnknown++
		}
		if accepted && year == "unmapped" {
			report.UnmappedSchoolYearLessons++
		}
	}
	days, weeks, years := map[string]Counts{}, map[string]Counts{}, map[string]Counts{}
	for _, a := range people {
		if a.counts.Scheduled < options.MinLessons {
			report.FilteredTeachers++
			continue
		}
		t := Teacher{ID: a.ref.ID, Name: a.ref.Name, Counts: finish(a.counts), ByWeekday: buckets(a.days), ByWeek: buckets(a.weeks), BySchoolYear: buckets(a.years), WorstWeekdays: []string{}}
		max := 0
		for _, b := range t.ByWeekday {
			if b.Cancelled > max {
				max = b.Cancelled
			}
		}
		if max > 0 {
			for _, b := range t.ByWeekday {
				if b.Cancelled == max {
					t.WorstWeekdays = append(t.WorstWeekdays, b.Key)
				}
			}
		}
		report.Teachers = append(report.Teachers, t)
		report.Totals.Scheduled += a.counts.Scheduled
		report.Totals.Cancelled += a.counts.Cancelled
		addBuckets(days, a.days)
		addBuckets(weeks, a.weeks)
		addBuckets(years, a.years)
	}
	sort.Slice(report.Teachers, func(i, j int) bool {
		a, b := report.Teachers[i], report.Teachers[j]
		if a.Cancelled != b.Cancelled {
			return a.Cancelled > b.Cancelled
		}
		return a.ID < b.ID
	})
	allowed := map[string]bool{}
	mean := 0.0
	for i := range report.Teachers {
		t := &report.Teachers[i]
		allowed[t.ID] = true
		t.Rank = i + 1
		mean += *t.NotCancelledRate
		if options.Anonymise {
			t.ID = ""
			t.Name = fmt.Sprintf("Teacher %d", i+1)
		}
	}
	for _, l := range lessons {
		if l.Code == "irregular" && !options.IncludeIrregular {
			continue
		}
		for _, t := range l.Teachers {
			if allowed[t.ID] {
				unique[l.Key+"/"+l.Date] = true
			}
		}
	}
	report.UniqueLessons = len(unique)
	report.Totals = finish(report.Totals)
	if len(report.Teachers) > 0 {
		mean /= float64(len(report.Teachers))
		report.MeanTeacherNotCancelledRate = &mean
	}
	report.ByWeekday = buckets(days)
	report.ByWeek = buckets(weeks)
	report.BySchoolYear = buckets(years)
	return report
}
