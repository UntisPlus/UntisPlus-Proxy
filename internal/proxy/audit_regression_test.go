package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"untis-proxy/internal/access"
	"untis-proxy/internal/store"
)

func TestRESTPooledClassRequiresViewerPermission(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	for _, u := range []*store.User{
		{Username: "viewer", School: "testschool", PersonType: 5, PersonID: 7, ClassID: 5000, Password: "pw"},
		{Username: "donor", School: "testschool", PersonType: 5, PersonID: 8, ClassID: 6000, Password: "pw"},
	} {
		if err := st.UpsertUser(u); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetUserCapGrant("testschool", "viewer", string(access.CapTimetableClassPooledView), -1); err != nil {
		t.Fatal(err)
	}
	rec := restReq(t, p, "viewer", http.MethodGet, "/WebUntis/api/public/timetable/weekly/data?elementType=1&elementId=6000&date=2026-09-21", "", "")
	if rec.Code != http.StatusForbidden || len(f.Rests()) != 0 {
		t.Fatalf("unpermitted pooled request: status %d upstream calls %d", rec.Code, len(f.Rests()))
	}
}

func TestSessionCannotBecomeRecreatedAccount(t *testing.T) {
	p, st := newTestProxy(t)
	p.sessions.SetPersister(st)
	if err := st.UpsertUser(&store.User{Username: "person", School: "testschool"}); err != nil {
		t.Fatal(err)
	}
	s := p.sessions.New("person", 0)
	if s == nil || s.UserID == 0 {
		t.Fatal("session not bound to account")
	}
	if _, err := st.DeleteUser("person"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertUser(&store.User{Username: "person", School: "testschool", Admin: true}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/me", nil)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: s.ID})
	if p.sessionUser(req) != nil {
		t.Fatal("old session became recreated account")
	}
}
