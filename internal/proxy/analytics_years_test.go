package proxy

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"untis-proxy/internal/store"
)

func TestAnalyticsYearsPreviewUsesUpstreamDates(t *testing.T) {
	p, st, upstream := analyticsFixture(t)
	if err := st.SetAdmin("bob", true); err != nil {
		t.Fatal(err)
	}
	upstream.mu.Lock()
	upstream.schoolYearsBody = `{"result":[{"name":"2026/2027","startDate":"20260810","endDate":"2027-07-30"},{"name":"2025/2026","startDate":20250811,"endDate":20260731}]}`
	upstream.mu.Unlock()
	path := "/admin/access/analytics-years?school=testschool"
	before := len(upstream.Public())
	if w := adminRequest(t, p, "student", "POST", path, "{}"); w.Code != http.StatusForbidden || len(upstream.Public()) != before {
		t.Fatal("non-admin accessed upstream school years")
	}
	w := adminRequest(t, p, "bob", "POST", path, "{}")
	var got struct{ Years []store.AnalyticsYear }
	want := []store.AnalyticsYear{{Name: "2025/2026", From: "2025-08-11", To: "2026-07-31"}, {Name: "2026/2027", From: "2026-08-10", To: "2027-07-30"}}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || !reflect.DeepEqual(got.Years, want) {
		t.Fatalf("bad year preview: HTTP %d %s", w.Code, w.Body.String())
	}
	calls := upstream.Public()[before:]
	if len(calls) != 1 || !strings.Contains(string(calls[0].Body), `"method":"getSchoolyears"`) {
		t.Fatal("preview should issue one read-only school-year request")
	}
	if years, err := st.AnalyticsYears("testschool"); err != nil || len(years) != 0 {
		t.Fatal("preview unexpectedly changed analytics configuration")
	}
	preview := w.Body.String()
	if w := adminRequest(t, p, "bob", "PUT", path, preview); w.Code != http.StatusOK {
		t.Fatalf("could not save the school years: %s", w.Body.String())
	}
	if err := st.SetAnalyticsAccess("testschool", "student", store.AnalyticsAccess{TeacherIDs: []int64{42, 43}}); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []struct {
		body, from string
		cancelled  int
	}{
		{`{"mode":"teacher","teacherId":42,"schoolYear":"2025/2026"}`, "2025-08-11", 0},
		{`{"mode":"teacher","teacherId":42,"schoolYear":"2026/2027"}`, "2026-08-10", 1},
		{`{"mode":"teacher","teacherId":43,"schoolYear":"2025/2026"}`, "2025-08-11", 0},
		{`{"mode":"teacher","teacherId":43,"schoolYear":"2026/2027"}`, "2026-08-10", 0},
	} {
		w := analyticsReq(t, p, "student", spec.body)
		var report analyticsResponse
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &report) != nil ||
			!report.Complete || report.From != spec.from || report.CancelledLessons != spec.cancelled {
			t.Fatalf("comparison report failed for %s: HTTP %d %s", spec.body, w.Code, w.Body.String())
		}
	}
}

func TestAnalyticsYearsRejectInvalidOrOverlappingDates(t *testing.T) {
	for _, raw := range []string{
		`{"result":[]}`,
		`{"result":{"startDate":"2025-02-30","endDate":"2026-07-31"}}`,
		`{"result":[{"name":"first","startDate":20250811,"endDate":20260731},{"name":"second","startDate":20260731,"endDate":20270730}]}`,
		`{"error":{"code":-8504},"result":{"startDate":20250811,"endDate":20260731}}`,
	} {
		if _, err := parseUpstreamAnalyticsYears([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid school years: %s", raw)
		}
	}
}
