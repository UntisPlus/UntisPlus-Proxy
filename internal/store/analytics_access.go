package store

import (
	"encoding/json"
	"fmt"
	"time"
)

type AnalyticsYear struct {
	Name string `json:"name"`
	From string `json:"from"`
	To   string `json:"to"`
}
type AnalyticsSegment struct {
	ClassID int64  `json:"classId"`
	From    string `json:"from"`
	To      string `json:"to"`
}
type AnalyticsProfile struct {
	ID       string             `json:"id"`
	Segments []AnalyticsSegment `json:"segments"`
}
type AnalyticsAccess struct {
	TeacherIDs []int64            `json:"teacherIds"`
	RoomIDs    []int64            `json:"roomIds"`
	StudentIDs []int64            `json:"studentIds"`
	ClassIDs   []int64            `json:"classIds"`
	Profiles   []AnalyticsProfile `json:"profiles"`
}

const MaxAnalyticsTargetIDs = 2000

func validAnalyticsDates(from, to string) bool {
	a, e := time.Parse("2006-01-02", from)
	b, f := time.Parse("2006-01-02", to)
	return e == nil && f == nil && !b.Before(a)
}
func (s *Store) AnalyticsAccess(school, user string) (AnalyticsAccess, error) {
	var a AnalyticsAccess
	raw, found, err := s.GetSetting(controlKey("analytics-access", school, norm(user)))
	if err == nil && found {
		err = json.Unmarshal([]byte(raw), &a)
	}
	return a, err
}
func (s *Store) SetAnalyticsAccess(school, user string, a AnalyticsAccess) error {
	u, err := s.GetUserInSchool(school, user)
	if err != nil || u == nil || u.School != school {
		return fmt.Errorf("account not found in this school")
	}
	for _, ids := range [][]int64{a.TeacherIDs, a.RoomIDs, a.StudentIDs, a.ClassIDs} {
		if len(ids) > MaxAnalyticsTargetIDs {
			return fmt.Errorf("too many analytics targets")
		}
		for _, id := range ids {
			if id <= 0 {
				return fmt.Errorf("invalid analytics target ID")
			}
		}
	}
	if len(a.Profiles) > 50 {
		return fmt.Errorf("too many analytics profiles")
	}
	seen := map[string]bool{}
	for _, p := range a.Profiles {
		if p.ID == "" || len(p.ID) > 80 || seen[p.ID] || len(p.Segments) == 0 || len(p.Segments) > 64 {
			return fmt.Errorf("invalid history profile")
		}
		seen[p.ID] = true
		for _, seg := range p.Segments {
			if seg.ClassID <= 0 || !validAnalyticsDates(seg.From, seg.To) {
				return fmt.Errorf("invalid history segment")
			}
		}
	}
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return s.SetSetting(controlKey("analytics-access", school, norm(user)), string(b))
}
func (s *Store) AnalyticsYears(school string) ([]AnalyticsYear, error) {
	years := []AnalyticsYear{}
	raw, found, err := s.GetSetting(controlKey("analytics-years", school))
	if err == nil && found {
		err = json.Unmarshal([]byte(raw), &years)
	}
	return years, err
}
func (s *Store) SetAnalyticsYears(school string, years []AnalyticsYear) error {
	if school == "" {
		return fmt.Errorf("invalid school/year count")
	}
	if err := ValidateAnalyticsYears(years); err != nil {
		return err
	}
	b, err := json.Marshal(years)
	if err != nil {
		return err
	}
	return s.SetSetting(controlKey("analytics-years", school), string(b))
}

func ValidateAnalyticsYears(years []AnalyticsYear) error {
	if len(years) > 100 {
		return fmt.Errorf("too many school years")
	}
	names := map[string]bool{}
	for i, y := range years {
		if y.Name == "" || names[y.Name] || !validAnalyticsDates(y.From, y.To) {
			return fmt.Errorf("invalid school year")
		}
		names[y.Name] = true
		for _, other := range years[:i] {
			if y.From <= other.To && y.To >= other.From {
				return fmt.Errorf("school years overlap")
			}
		}
	}
	return nil
}
