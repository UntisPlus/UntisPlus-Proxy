package proxy

// Pure helper functions shared across handlers: date/range math used by recon
// and REST weekly reconstruction, element-name lookups and formatting.

import (
	"encoding/json"
	"testing"
	"time"

	"untis-proxy/internal/store"
)

func storeUserSample() *store.User {
	return &store.User{Username: "owen", Method: "key", PersonType: 5, ClassID: 5000}
}

func jsonUnmarshalR(b []byte, v any) error {
	return json.Unmarshal(b, v)
}

func TestSchoolYearRange(t *testing.T) {
	cases := []struct {
		now time.Time
		ys  string
		ye  string
	}{
		{time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC), "2026-08-01", "2027-07-31"},
		{time.Date(2026, time.September, 27, 0, 0, 0, 0, time.UTC), "2026-08-01", "2027-07-31"},
		{time.Date(2027, time.March, 1, 0, 0, 0, 0, time.UTC), "2026-08-01", "2027-07-31"},
		{time.Date(2027, time.July, 31, 0, 0, 0, 0, time.UTC), "2026-08-01", "2027-07-31"},
		{time.Date(2027, time.January, 15, 0, 0, 0, 0, time.UTC), "2026-08-01", "2027-07-31"},
	}
	for _, c := range cases {
		ys, ye := schoolYearRange(c.now)
		if ys != c.ys || ye != c.ye {
			t.Errorf("schoolYearRange(%v) = %q..%q, want %q..%q", c.now, ys, ye, c.ys, c.ye)
		}
	}
}

func TestWeekRange(t *testing.T) {
	cases := []struct {
		date string
		mon  string
		fri  string
		err  bool
	}{
		{"2026-09-23", "2026-09-21", "2026-09-25", false}, // Wednesday
		{"2026-09-21", "2026-09-21", "2026-09-25", false}, // Monday itself
		{"2026-09-20", "2026-09-14", "2026-09-18", false}, // Sunday wraps back a week
		{"bogus", "", "", true},
	}
	for _, c := range cases {
		mon, fri, err := weekRange(c.date)
		if c.err {
			if err == nil {
				t.Errorf("weekRange(%q) expected error", c.date)
			}
			continue
		}
		if err != nil || mon != c.mon || fri != c.fri {
			t.Errorf("weekRange(%q) = %q..%q, err %v; want %q..%q", c.date, mon, fri, err, c.mon, c.fri)
		}
	}
}

func TestWeeklyPeriodConversion(t *testing.T) {
	pd := map[string]any{
		"id":            float64(11),
		"lessonId":      float64(22),
		"startDateTime": "2026-09-21T08:00:00+02:00",
		"endDateTime":   "2026-09-21T08:45:00+02:00",
		"elements": []any{
			map[string]any{"type": "CLASS", "id": float64(5000)},
			map[string]any{"type": "TEACHER", "id": float64(5009)},
			map[string]any{"type": "SUBJECT", "id": float64(1)},
			map[string]any{"type": "ROOM", "id": float64(169)},
			map[string]any{"type": "LESSON", "id": float64(99)}, // unknown type dropped
		},
	}
	wp, ids, err := weeklyPeriod(pd)
	if err != nil {
		t.Fatalf("weeklyPeriod: %v", err)
	}
	if wp["id"].(int64) != 11 || wp["date"].(int) != 20260921 || wp["startTime"].(int) != 800 || wp["endTime"].(int) != 845 {
		t.Errorf("converted period = %v, want id/date/startTime/endTime set", wp)
	}
	els := wp["elements"].([]map[string]any)
	if len(els) != 4 {
		t.Fatalf("elements = %v, want 4 known types", els)
	}
	if len(ids) != 4 {
		t.Errorf("ids = %v, want 4", ids)
	}
	if !containsInt64(ids, 5009) || !containsInt64(ids, 169) {
		t.Errorf("ids = %v, missing teacher/room", ids)
	}
}

func TestWeeklyPeriodErrors(t *testing.T) {
	if _, _, err := weeklyPeriod(map[string]any{"id": float64(1)}); err == nil {
		t.Error("missing times must error")
	}
	if _, _, err := weeklyPeriod(map[string]any{
		"id":            float64(1),
		"startDateTime": "2026-09-21T08:00:00+02:00",
		"endDateTime":   "2026-09-21T08:45:00+02:00",
		"elements":      []any{},
	}); err == nil {
		t.Error("no elements must error")
	}
}

func TestElTypeInt(t *testing.T) {
	cases := map[string]int{"CLASS": 1, "TEACHER": 2, "SUBJECT": 3, "ROOM": 4, "?": 0, "": 0}
	for in, want := range cases {
		if got := elTypeInt(in); got != want {
			t.Errorf("elTypeInt(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestElementNameHelpers(t *testing.T) {
	md := &masterDataCache{
		teachers: map[int64]string{5009: "A. Hartley"},
		rooms:    map[int64]string{169: "B.112"},
		subjects: map[int64]string{1: "Englisch"},
		klassen:  map[int64]string{5000: "10aR"},
	}
	if elementName(md, "TEACHER", 5009) != "A. Hartley" {
		t.Errorf("teacher name lookup wrong")
	}
	if elementName(md, "ROOM", 169) != "B.112" || elementName(md, "SUBJECT", 1) != "Englisch" {
		t.Errorf("room/subject lookup wrong")
	}
	if elementName(md, "CLASS", 5000) != "10aR" {
		t.Errorf("klasse lookup wrong")
	}
	if elementName(md, "TEACHER", 999) != "" {
		t.Errorf("unknown id must be empty")
	}
	if elementName(nil, "TEACHER", 5009) != "" {
		t.Errorf("nil cache must be empty")
	}
	if elementLongName(md, "ROOM", 169) != "" {
		t.Errorf("longName only applies to subjects")
	}
	if elementLongName(nil, "SUBJECT", 1) != "" {
		t.Errorf("longName on nil cache must be empty")
	}
}

func TestYmdHmInt(t *testing.T) {
	tm := time.Date(2026, time.September, 21, 8, 45, 0, 0, time.UTC)
	if ymdInt(tm) != 20260921 {
		t.Errorf("ymdInt = %d, want 20260921", ymdInt(tm))
	}
	if hmInt(tm) != 845 {
		t.Errorf("hmInt = %d, want 845", hmInt(tm))
	}
}

func TestRewriteAuthErrorPaths(t *testing.T) {
	p, st := newTestProxy(t)
	if _, err := p.rewriteAuthForOwner("testschool", []byte("not-json"), storeUserSample()); err == nil {
		t.Error("rewrite on malformed body must error")
	}
	if _, err := p.rewriteAuthForOwner("testschool", []byte(`{"params":[]}`), storeUserSample()); err == nil {
		t.Error("rewrite with empty params must error")
	}
	// key owner with a stored secret rewrites cleanly, no upstream calls
	_ = st.SetDefaultSchool("testschool")
	_ = st.UpsertUser(storeUserSample())
	_ = st.UpsertSecret("owen", "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP")
	body, err := p.rewriteAuthForOwner("testschool", []byte(`{"params":[{"id":5000,"type":"CLASS"}]}`), storeUserSample())
	if err != nil {
		t.Fatalf("rewrite key owner: %v", err)
	}
	var out struct {
		Params []struct {
			Auth map[string]any `json:"auth"`
		} `json:"params"`
	}
	if err := jsonUnmarshalR(body, &out); err != nil {
		t.Fatalf("bad rewritten body: %v", err)
	}
	if out.Params[0].Auth["user"] != "owen" || out.Params[0].Auth["otp"] == "" {
		t.Errorf("rewritten auth = %v, want owen with otp", out.Params[0].Auth)
	}
}

func containsInt64(xs []int64, v int64) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
