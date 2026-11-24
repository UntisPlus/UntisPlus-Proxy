package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"untis-proxy/internal/access"
	"untis-proxy/internal/store"
)

func basicFixture(t *testing.T) (*Proxy, *store.Store, *fakeUpstream) {
	t.Helper()
	f := &fakeUpstream{timetable: `{"result":{"periods":[],"stock":true}}`}
	p, st := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st, "student", 7, 5000)
	seedTeacherSource(t, st, "staff", 42)
	if err := st.ApplyUpstreamBasic("testschool", "student"); err != nil {
		t.Fatal(err)
	}
	return p, st, f
}
func TestBasicUsesOwnUpstreamAndPreservesCatalog(t *testing.T) {
	p, _, f := basicFixture(t)
	body := internBody("getTimetable2017", ttParam(42, "TEACHER", "2026-09-21", "2026-09-27", "student"))
	rec := internReq(t, p, "getTimetable2017", "student", body)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"stock":true`) {
		t.Fatalf("stock timetable failed: %s", rec.Body.String())
	}
	for _, call := range f.Forwards() {
		if call.Method == "getTimetable2017" && (!strings.Contains(string(call.Body), `"user":"student"`) || strings.Contains(string(call.Body), `"user":"staff"`)) {
			t.Fatal("Basic borrowed teacher identity")
		}
	}
	raw := []byte(`{"result":{"masterData":{"teachers":[{"id":42,"displayAllowed":true}],"klassen":[{"id":9,"displayable":true}]}}}`)
	if string(p.markPooledElementsDisplayable("testschool", raw, "student")) != string(raw) {
		t.Fatal("Basic changed upstream catalog")
	}
}
func TestBasicLocalFeaturesRequireExplicitGrants(t *testing.T) {
	p, st, _ := basicFixture(t)
	if string(p.enrichHomeWorkResponse([]byte(`{"result":{"homeWorks":[{"id":1}]}}`), "testschool", "student")) != `{"result":{"homeWorks":[{"id":1}]}}` {
		t.Fatal("Basic enriched homework")
	}
	u, _ := st.GetUser("student")
	if p.canViewPersonalBoosted(u, "testschool") || p.canViewPool(u, "testschool") {
		t.Fatal("Basic grants proxy supplements")
	}
	for _, path := range []string{"/api/homework/flags", "/api/absence/notes"} {
		w := sessionedRequest(t, p, "student", http.MethodGet, path, "")
		if w.Code != 403 {
			t.Fatalf("Basic local feature status %d %s", w.Code, w.Body.String())
		}
	}
	st.SetUserCapGrant("testschool", "student", string(access.CapHomeworkOwnManage), 1)
	w := sessionedRequest(t, p, "student", http.MethodGet, "/api/homework/flags", "")
	if w.Code != 200 {
		t.Fatal("explicit private feature grant ignored")
	}
}
func TestCompatibilityRoutesAreBoundedPrivateAndDoNotWriteUpstream(t *testing.T) {
	f := &fakeUpstream{timetable: `{"result":{"periods":[]}}`, restBody: `{"data":{"loginServiceConfig":{"user":{"personId":7,"persons":[{"id":7,"type":5}]}}}}`}
	p, st := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st, "student", 7, 5000)
	st.SetAdmin("bob", true)
	before := len(f.Forwards())
	w := adminRequest(t, p, "bob", "GET", "/admin/access/compatibility?school=testschool&user=student", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "replayable") || len(f.Forwards()) != before {
		t.Fatalf("stored diagnostics fetched upstream or leaked credentials %s", w.Body.String())
	}
	if denied := adminRequest(t, p, "student", "GET", "/admin/access/compatibility?school=testschool&user=student", ""); denied.Code != 403 {
		t.Fatal("non-admin diagnostics accepted")
	}
	for _, body := range []string{`{"from":"bad","to":"2026-09-21"}`, `{"from":"2025-01-01","to":"2026-01-01"}`} {
		w = adminRequest(t, p, "bob", "POST", "/admin/access/compatibility-probe?school=testschool&user=student", body)
		if w.Code != 400 {
			t.Fatal("unbounded probe accepted")
		}
	}
	w = adminRequest(t, p, "bob", "POST", "/admin/access/compatibility-probe?school=testschool&user=student", `{"from":"2026-09-21","to":"2026-09-22"}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var probe compatibilityProbe
	json.Unmarshal(w.Body.Bytes(), &probe)
	if probe.Status != "empty_or_unavailable" || probe.UpstreamAdmin != nil {
		t.Fatalf("empty/unknown probe claimed support: %s", w.Body.String())
	}
	for _, call := range f.Forwards() {
		if call.Method != "getTimetable2017" {
			t.Fatalf("unexpected upstream method: %s", call.Method)
		}
	}
	raw, found, _ := st.CompatibilityResult("testschool", "student")
	if !found || strings.Contains(string(raw), "replayable") {
		t.Fatal("unsafe or missing diagnostic cache")
	}
}

func TestCompatibilityNeverProbesArbitraryPersonOrForeignSchool(t *testing.T) {
	f := &fakeUpstream{restBody: `{"data":{"loginServiceConfig":{"user":{"personId":7,"persons":[{"id":7,"type":5}]}}}}`}
	p, st := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st, "student", 7, 5000)
	st.SetAdmin("bob", true)
	body := `{"from":"2026-09-21","to":"2026-09-21","personId":9999}`
	w := adminRequest(t, p, "bob", "POST", "/admin/access/compatibility-probe?school=testschool&user=student", body)
	var result compatibilityProbe
	json.Unmarshal(w.Body.Bytes(), &result)
	if w.Code != 200 || result.Status != "identity_selection_required" || len(f.Forwards()) != 0 {
		t.Fatalf("foreign person fetched: %s", w.Body.String())
	}
	before := len(f.Rests())
	w = adminRequest(t, p, "bob", "POST", "/admin/access/compatibility-probe?school=foreign&user=student", body)
	if w.Code != 400 || len(f.Rests()) != before {
		t.Fatal("foreign-school probe reached upstream")
	}
}
func TestBasicBlocksSyntheticTechniqueButAllowsAnExplicitExtra(t *testing.T) {
	p, st, _ := basicFixture(t)
	w := internReq(t, p, "getTimetable2017", "student", internBody("getTimetable2017", ttParam(technikClassID, "CLASS", "2026-09-21", "2026-09-21", "student")))
	if errCode(t, w) != -8509 {
		t.Fatalf("Basic accessed synthetic aggregate: %s", w.Body.String())
	}
	st.SetUserCapGrant("testschool", "student", string(access.CapEventsPersonalView), 1)
	if !p.proxyLocalAllowed("testschool", "student", access.CapEventsPersonalView) || p.proxyLocalAllowed("foreign", "student", access.CapEventsPersonalView) {
		t.Fatal("personal extra scope not enforced")
	}
}

func TestCompatibilityDoesNotCountMalformedLessonsAsObserved(t *testing.T) {
	f := &fakeUpstream{timetable: `{"result":{"periods":[{}]}}`, restBody: `{"data":{"loginServiceConfig":{"user":{"personId":7,"persons":[{"id":7,"type":5}]}}}}`}
	p, st := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st, "student", 7, 5000)
	st.SetAdmin("bob", true)
	w := adminRequest(t, p, "bob", "POST", "/admin/access/compatibility-probe?school=testschool&user=student", `{"from":"2026-09-21","to":"2026-09-21"}`)
	var report compatibilityProbe
	json.Unmarshal(w.Body.Bytes(), &report)
	if w.Code != 200 || report.Status != "unavailable" || report.PeriodCount != 0 {
		t.Fatalf("malformed data claimed compatibility: %s", w.Body.String())
	}
}
