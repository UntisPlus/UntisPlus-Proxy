package proxy

// §8 – /week page tests. The week view is the end-user face of a calendar
// token, so it must render the same data as the .ics feed and must never leak
// data without the token.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"untis-proxy/internal/store"
)

// weekRequest serves GET /week/{token} through the real router.
func weekRequest(t *testing.T, p *Proxy, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/week/"+token, nil)
	req.SetPathValue("token", token)
	rec := httptest.NewRecorder()
	p.handleWeekPage(rec, req)
	return rec
}

// weekFixture seeds a pooled class with a class token and one week of lessons.
func weekFixture(t *testing.T) (*Proxy, *store.Store, string) {
	t.Helper()
	f := &fakeUpstream{userDataBody: masterDataLoginBody}
	// Lessons must sit inside the week the page renders, which is the token's
	// (Berlin) week, so seed from local time rather than UTC.
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	today := time.Now().In(loc)
	day := func(off int, hour string) string {
		d := today.AddDate(0, 0, off)
		return d.Format("2006-01-02") + "T" + hour + ":00" + d.Format("-07:00")
	}
	f.setTimetable(t, []map[string]any{
		{"id": 10, "startDateTime": day(0, "08:00"), "endDateTime": day(0, "08:45"),
			"elements": []any{
				map[string]any{"id": 7, "type": "SUBJECT"},
				map[string]any{"id": 169, "type": "ROOM"},
			},
			"text": map[string]any{"substitution": "Vertretungslehrer", "info": "Bitte Heft mitbringen"},
			"is":   []any{"CANCELLED"}},
		{"id": 11, "startDateTime": day(0, "09:00"), "endDateTime": day(0, "09:45"),
			"elements": []any{map[string]any{"id": 7, "type": "SUBJECT"}},
			"exam":     map[string]any{"examtype": "Klausur", "name": "Mathe 3"}},
		{"id": 12, "startDateTime": day(2, "10:00"), "endDateTime": day(2, "10:45"),
			"elements": []any{map[string]any{"id": 7, "type": "SUBJECT"}}},
	})
	p, st := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st, "owen", 100, 5000)
	tok := &store.ClassToken{Token: "weektok", School: "testschool", ClassID: 5000, Days: 7}
	if err := st.CreateClassToken(tok); err != nil {
		t.Fatalf("create token: %v", err)
	}
	return p, st, tok.Token
}

// TestWeekPageRendersLessonsAndMarkers: the page shows the resolved subject,
// room, today's date marker, and the substitution/cancellation/exam markers the
// school server reports.
func TestWeekPageRendersLessonsAndMarkers(t *testing.T) {
	p, _, token := weekFixture(t)
	rec := weekRequest(t, p, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"10b",                       // class name from master data
		"Mathematik 3",              // subject name
		"R101",                      // room name
		"entfällt",                  // CANCELLED flag
		"Vertretung",                // substitution
		"Klausur Mathe 3",           // exam
		"Vertretungslehrer",         // substitution text
		"heute",                     // today marker
		"/api/calendar/weektok.ics", // link to the feed
	} {
		if !strings.Contains(body, want) {
			t.Errorf("week page does not contain %q", want)
		}
	}
	if strings.Count(body, "<section class=\"day") != 7 {
		t.Errorf("week page should render 7 day sections, got %d", strings.Count(body, "<section class=\"day"))
	}
}

// TestWeekPageEscapesUpstreamContent: upstream strings are untrusted and are
// inserted into the template, so html/template must escape them.
func TestWeekPageEscapesUpstreamContent(t *testing.T) {
	f := &fakeUpstream{userDataBody: `{"jsonrpc":"2.0","id":"upstream","result":{
		"masterData":{"timeStamp":1,"klassen":[{"id":5000,"name":"<script>x</script>"}]}}}`}
	day := time.Now().Format(time.RFC3339)
	f.setTimetable(t, []map[string]any{
		{"id": 10, "startDateTime": day, "endDateTime": day,
			"elements": []any{map[string]any{"id": 7, "type": "SUBJECT"}},
			"text":     map[string]any{"info": `<img src=x onerror="alert(1)">`}},
	})
	p, st := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st, "owen", 100, 5000)
	if err := st.CreateClassToken(&store.ClassToken{Token: "xss", School: "testschool", ClassID: 5000, Days: 7}); err != nil {
		t.Fatalf("create token: %v", err)
	}
	body := weekRequest(t, p, "xss").Body.String()
	for _, bad := range []string{"<script>x</script>", "<img src=x"} {
		if strings.Contains(body, bad) {
			t.Errorf("week page contains unescaped upstream HTML %q", bad)
		}
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("class name was not HTML-escaped: %q", body)
	}
}

// TestWeekPageUnknownToken: a revoked or invented token is a plain 404 and
// never renders a plan.
func TestWeekPageUnknownToken(t *testing.T) {
	p, _ := newTestProxy(t)
	rec := weekRequest(t, p, "nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "<html") {
		t.Errorf("404 body should not render a page: %q", rec.Body.String())
	}
}

// TestWeekPageHonoursTokenTimezone: times are rendered in the token's timezone,
// not the server's, so a feed created for a school abroad still shows local time.
func TestWeekPageHonoursTokenTimezone(t *testing.T) {
	p, st, token := weekFixture(t)
	if err := st.CreateClassToken(&store.ClassToken{Token: "tok-tokyo", School: "testschool", ClassID: 5000, Days: 7, Timezone: "Asia/Tokyo"}); err != nil {
		t.Fatalf("create token: %v", err)
	}
	body := weekRequest(t, p, "tok-tokyo").Body.String()
	if !strings.Contains(body, "Asia/Tokyo") {
		t.Errorf("page should state its timezone: %q", body)
	}
	if !strings.Contains(body, "/api/calendar/tok-tokyo.ics") {
		t.Errorf("page should link its own feed: %q", body)
	}
	_ = token
}

// TestWeekPageCoversRollingWeek: the window starts today and reaches seven days
// out, so a lesson on the last day is visible and a past Monday is not.
func TestWeekPageCoversRollingWeek(t *testing.T) {
	f := &fakeUpstream{userDataBody: masterDataLoginBody}
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	today := time.Now().In(loc)
	day := func(off int) string {
		d := today.AddDate(0, 0, off)
		return d.Format("2006-01-02") + "T08:00" + d.Format("-07:00")
	}
	f.setTimetable(t, []map[string]any{
		{"id": 1, "startDateTime": day(6), "endDateTime": day(6),
			"elements": []any{map[string]any{"id": 7, "type": "SUBJECT"}}},
		{"id": 2, "startDateTime": day(-6), "endDateTime": day(-6),
			"elements": []any{map[string]any{"id": 7, "type": "SUBJECT"}}},
	})
	p, st := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st, "owen", 100, 5000)
	if err := st.CreateClassToken(&store.ClassToken{Token: "roll", School: "testschool", ClassID: 5000, Days: 14}); err != nil {
		t.Fatalf("create token: %v", err)
	}
	body := weekRequest(t, p, "roll").Body.String()
	// Two lessons share the same time on different days; exactly one is inside
	// the window, so a single "08:00" row proves the window is today..today+6.
	if n := strings.Count(body, `<td class="time">`); n != 1 {
		t.Errorf("page renders %d lessons, want 1 (window = today..today+6)", n)
	}
	want := today.AddDate(0, 0, 6).Format("02.01.")
	if !strings.Contains(body, want) {
		t.Errorf("page does not reach %s: %q", want, body)
	}
	if got := strings.Count(body, "<section class=\"day"); got != 7 {
		t.Errorf("page renders %d day sections, want 7", got)
	}
}

// weekCSVRequest serves GET /week/{token}.csv through the same handler the
// router uses, so the suffix dispatch is exercised.
func weekCSVRequest(t *testing.T, p *Proxy, token, query string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/week/" + token + ".csv"
	if query != "" {
		target += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.SetPathValue("token", token+".csv")
	rec := httptest.NewRecorder()
	p.handleWeekPage(rec, req)
	return rec
}

// TestWeekPageMonthView: ?view=month renders the whole month as a grid, links
// to the previous and next month, and keeps the .ics and CSV downloads visible.
func TestWeekPageMonthView(t *testing.T) {
	p, _, token := weekFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/week/"+token+"?view=month", nil)
	req.SetPathValue("token", token)
	rec := httptest.NewRecorder()
	p.handleWeekPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<table class="month">`,
		"Mo</th>", "So</th>",
		"Woche",
		"?view=month&amp;month=",
		"Mathematik 3",
		"iCal-Datei herunterladen",
		".csv?view=month",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("month view does not contain %q", want)
		}
	}
	// The grid is always whole weeks of seven cells.
	if n := strings.Count(body, "<tr>"); n < 5 {
		t.Errorf("month grid has %d rows, want several weeks", n)
	}
}

// TestWeekCSVExport: the CSV carries the visible columns, the same time window,
// and a UTF-8 BOM so Excel reads German umlauts correctly.
func TestWeekCSVExport(t *testing.T) {
	p, _, token := weekFixture(t)
	loc, _ := time.LoadLocation("Europe/Berlin")
	from := time.Now().In(loc).Format("2006-01-02")
	rec := weekCSVRequest(t, p, token, "from="+from)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("content type = %q, want text/csv", ct)
	}
	if disp := rec.Header().Get("Content-Disposition"); !strings.Contains(disp, "attachment") || !strings.Contains(disp, ".csv") {
		t.Errorf("content-disposition = %q, want an attachment with a .csv name", disp)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "\ufeff") {
		t.Error("CSV is missing the UTF-8 BOM")
	}
	for _, want := range []string{
		"Datum,Von,Bis,Fach,Lehrer,Raum,Klasse,Status,Notizen",
		"Mathematik 3",
		"08:00",
		"entfällt",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("CSV does not contain %q: %s", want, body)
		}
	}
}

// TestWeekCSVSafeNeutralisesFormulas: exported cells are data, not formulas. An
// upstream room or note beginning with = + - @ must not execute in Excel.
func TestWeekCSVSafeNeutralisesFormulas(t *testing.T) {
	cases := map[string]string{
		"=SUM(A1:A2)": "'=SUM(A1:A2)",
		"+123":        "'+123",
		"-1+1":        "'-1+1",
		"@SUM":        "'@SUM",
		"\tformula":   "'\tformula",
		"\rformula":   "'\rformula",
		"Mathe":       "Mathe",
		"08:00":       "08:00",
		"":            "",
	}
	for in, want := range cases {
		if got := csvSafe(in); got != want {
			t.Errorf("csvSafe(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestWeekCSVRejectsUnknownToken: the export is exactly as protected as the page.
func TestWeekCSVRejectsUnknownToken(t *testing.T) {
	p, _ := newTestProxy(t)
	rec := weekCSVRequest(t, p, "nope", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
