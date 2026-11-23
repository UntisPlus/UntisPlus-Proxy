package analytics

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestReportDedupFiltersAndBreakdowns(t *testing.T) {
	a := TeacherRef{"10", "Alpha"}
	b := TeacherRef{"20", "Beta"}
	lessons := []Lesson{{"x", "2020-12-31", "cancelled", []TeacherRef{a, a, b}}, {"x", "2020-12-31", "cancelled", []TeacherRef{a, b}}, {"y", "2021-01-01", "", []TeacherRef{a}}, {"z", "2021-01-02", "irregular", []TeacherRef{a}}}
	options := Options{Years: []Year{{"2020/21", "2020-08-01", "2021-07-31"}}}
	report := Build(lessons, options)
	if report.UniqueLessons != 2 || report.Totals.Scheduled != 3 || report.Totals.Cancelled != 2 || len(report.ByWeek) != 1 || report.ByWeek[0].Key != "2020-W53" {
		t.Fatalf("wrong counts: %+v", report)
	}
	if *report.MeanTeacherNotCancelledRate != 25 || *report.Totals.NotCancelledRate < 33 || *report.Totals.NotCancelledRate > 34 {
		t.Fatal("weighted and teacher means conflated")
	}
	options.MinLessons = 2
	filtered := Build(lessons, options)
	if len(filtered.Teachers) != 1 || filtered.Totals.Scheduled != 2 || filtered.ByWeekday[0].Scheduled != 1 || filtered.FilteredTeachers != 1 {
		t.Fatalf("filters inconsistent: %+v", filtered)
	}
	options.Anonymise = true
	anonymous := Build(lessons, options)
	raw, _ := json.Marshal(anonymous)
	if strings.Contains(string(raw), "Alpha") || strings.Contains(string(raw), `"id"`) {
		t.Fatalf("identifiers leaked: %s", raw)
	}
}
func TestEmptyUnknownAndTeacherSelection(t *testing.T) {
	empty := Build(nil, Options{})
	if empty.Totals.NotCancelledRate != nil || len(empty.Teachers) != 0 {
		t.Fatal("empty data must not invent rates")
	}
	lessons := []Lesson{{"a", "2026-01-05", "cancelled", []TeacherRef{{"1", "=Formula"}, {"2", "TA_Cover"}}}, {"b", "2026-01-06", "", nil}, {"c", "bad", "", []TeacherRef{{"1", "A"}}}}
	r := Build(lessons, Options{TeacherID: "1", ExcludePrefixes: []string{"TA_"}})
	if len(r.Teachers) != 1 || r.Totals.Cancelled != 1 || r.ExcludedUnknown != 1 || len(r.Teachers[0].WorstWeekdays) != 1 {
		t.Fatalf("bad selection: %+v", r)
	}
}
func TestAllYearBucketsAndTiedWeekdays(t *testing.T) {
	ref := []TeacherRef{{"1", "A"}}
	r := Build([]Lesson{{"1", "2025-01-06", "cancelled", ref}, {"2", "2026-01-06", "cancelled", ref}}, Options{Years: []Year{{"2024/25", "2024-08-01", "2025-07-31"}, {"2025/26", "2025-08-01", "2026-07-31"}}})
	if len(r.BySchoolYear) != 2 || len(r.ByWeek) != 2 || len(r.Teachers[0].WorstWeekdays) != 2 {
		t.Fatalf("years or weekday ties merged: %+v", r)
	}
}
