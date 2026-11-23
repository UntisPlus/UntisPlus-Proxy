package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"untis-proxy/internal/access"
	"untis-proxy/internal/store"
)

func TestTechnikOperationGrantsAreIndependent(t *testing.T) {
	p, st := eventProxy(t)
	u, _ := st.GetUser("dee")
	for _, cap := range []access.Capability{access.CapEventsTechnikCreate, access.CapEventsTechnikEdit, access.CapEventsTechnikDelete, access.CapEventsTechnikImport, access.CapEventsTechnikAttendanceRead, access.CapEventsTechnikAttendanceWrite} {
		st.SetUserCapGrant("testschool", u.Username, string(cap), 1)
		if !p.technikAllowed(u, "testschool", cap) {
			t.Fatalf("grant ignored %s", cap)
		}
		for _, other := range []access.Capability{access.CapEventsTechnikDelete, access.CapEventsTechnikAttendanceWrite} {
			if other != cap && p.technikAllowed(u, "testschool", other) {
				t.Fatal("independent action escalated")
			}
		}
		if p.technikAllowed(u, "other", cap) {
			t.Fatal("operation crossed schools")
		}
		st.SetUserCapGrant("testschool", u.Username, string(cap), 0)
	}
	st.SetUserCapGrant("testschool", u.Username, string(access.CapEventsTechnikManage), 1)
	st.SetUserCapGrant("testschool", u.Username, string(access.CapEventsTechnikDelete), -1)
	if p.technikAllowed(u, "testschool", access.CapEventsTechnikDelete) || !p.technikAllowed(u, "testschool", access.CapEventsTechnikEdit) {
		t.Fatal("legacy manage or explicit denial mishandled")
	}
	st.SetFeatureEnabled("", "technik", false)
	admin, _ := st.GetUser("bob")
	if p.technikAllowed(admin, "testschool", access.CapEventsTechnikEdit) {
		t.Fatal("admin bypassed master switch")
	}
}
func TestTechnikCreateEndpointRejectsUnrelatedAndForeignAccess(t *testing.T) {
	p, st := eventProxy(t)
	st.SetUserCapGrant("testschool", "dee", string(access.CapEventsTechnikCreate), 1)
	body := `{"username":"sam","date":"2026-10-01","startTime":"14:00","endTime":"15:00","title":"Test"}`
	for _, school := range []string{"testschool", "other"} {
		r := httptest.NewRequest(http.MethodPost, "/api/technik/events?school="+school, strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: p.sessions.New("dee", 5000).ID})
		w := httptest.NewRecorder()
		p.handleStudentEvents(w, r)
		if school == "other" && w.Code != 403 {
			t.Fatal("cross-school create accepted")
		}
		if school == "testschool" && w.Code != 200 {
			t.Fatalf("create grant failed %d %s", w.Code, w.Body.String())
		}
	}
}
func TestAdmissionBlocksExistingSessionOTPAndPasswordLogin(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st, "student", 7, 5000)
	st.UpsertSecret("student", boostSourceSecret)
	cookie := p.sessions.New("student", 5000).ID
	st.SetAdmissionPolicy("testschool", store.AdmissionPolicy{BlockedRoles: []string{"student"}})
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: cookie})
	if p.sessionUser(req) != nil || p.userFromRequestOTP("student", "123456", 0) != nil {
		t.Fatal("blocked session/OTP retained access")
	}
	rec := publicJSONRPCReq(t, p, `{"jsonrpc":"2.0","id":"a","method":"authenticate","params":{"user":"new-student","password":"pw"}}`)
	if errCode(t, rec) != -8504 {
		t.Fatalf("blocked login accepted: %s", rec.Body.String())
	}
	if u, _ := st.GetUser("new-student"); u != nil {
		t.Fatal("blocked account persisted")
	}
}
func TestAdminControlRoutesAndReadOnlyPreview(t *testing.T) {
	p, st := newTestProxy(t)
	seedSessionUser(t, st, "student", 5, 7, 5000)
	cases := []struct{ method, path, body string }{{"PUT", "/admin/access/features?school=", `{"feature":"analytics","enabled":true}`}, {"PUT", "/admin/access/analytics-years?school=testschool", `{"years":[{"name":"2026/27","from":"2026-08-01","to":"2027-07-31"}]}`}, {"POST", "/admin/access/preview", `{"school":"testschool","grants":{"analytics.personal.view":1},"assignTo":"student"}`}}
	for _, c := range cases {
		w := adminRequest(t, p, "bob", c.method, c.path, c.body)
		if w.Code != 200 || strings.Contains(w.Body.String(), `"error"`) {
			t.Fatalf("control failed: %s", w.Body.String())
		}
		denied := adminRequest(t, p, "student", c.method, c.path, c.body)
		if denied.Code != 403 {
			t.Fatal("non-admin altered feature controls")
		}
	}
	if !st.FeatureEnabled("testschool", "analytics") {
		t.Fatal("master control not applied")
	}
}

func TestKeyAdmissionDenialDoesNotPersistPastedSecret(t *testing.T) {
	p, st, _ := newFakeProxy(t, &fakeUntis{})
	st.SetAdmissionPolicy("testschool", store.AdmissionPolicy{BlockedRoles: []string{"student", "teacher", "unknown", "other", "parent", "staff"}})
	rec := internReq(t, p, "getUserData2017", "", loginBody(t, "blocked", boostSourceSecret))
	if errCode(t, rec) != -8504 {
		t.Fatalf("blocked key login accepted %s", rec.Body.String())
	}
	if secret, _ := st.GetSecret("blocked"); secret != "" {
		t.Fatal("blocked login saved secret")
	}
	if u, _ := st.GetUser("blocked"); u != nil {
		t.Fatal("blocked key account registered")
	}
}
func TestMasterSwitchDropsLegacyUnownedNotificationDelivery(t *testing.T) {
	p, _ := newTestProxy(t)
	p.store.SetFeatureEnabled("", "ntfy", false)
	if p.ntfyDeliveryAuthorized("testschool", &store.NtfyTopic{}) {
		t.Fatal("unowned destination bypassed master switch")
	}
}
func TestForeignOriginCannotChangeTechnikOrRequestAnalytics(t *testing.T) {
	p, st := eventProxy(t)
	st.SetFeatureEnabled("", "analytics", true)
	for _, path := range []string{"/api/technik/events", "/api/analytics/teacher-cancellations"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.Header.Set("Origin", "https://evil.invalid")
		req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: p.sessions.New("dee", 5000).ID})
		w := httptest.NewRecorder()
		p.Handler().ServeHTTP(w, req)
		if w.Code != 403 {
			t.Fatalf("cross-origin request status=%d", w.Code)
		}
	}
}
