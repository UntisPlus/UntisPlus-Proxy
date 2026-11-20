package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"untis-proxy/internal/access"
	"untis-proxy/internal/store"
)

func TestGroupEditorRejectsUnknownCapability(t *testing.T) {
	p, _ := newTestProxy(t)
	rec := adminRequest(t, p, "bob", http.MethodPost, "/admin/access/group-editor", `{"school":"testschool","name":"Broken","grants":{"made.up":1}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestNtfyCreateAndManagementAreIndependent(t *testing.T) {
	p, st := newTestProxy(t)
	u := &store.User{Username: "student", School: "testschool", PersonType: 5, PersonID: 7, ClassID: 5000}
	if err := st.UpsertUser(u); err != nil {
		t.Fatal(err)
	}
	id, err := st.SavePermissionGroup(0, "testschool", "Create only", "", map[string]int{string(access.CapNotifyNtfyCreate): 1, string(access.CapNotifyNtfyManageOwn): -1}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = st.AssignUserCapGroup("testschool", u.Username, id); err != nil {
		t.Fatal(err)
	}
	request := func(method, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/ntfy", strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: p.sessions.New(u.Username, u.ClassID).ID})
		rec := httptest.NewRecorder()
		p.handleSubsNtfy(rec, req)
		return rec
	}
	if rec := request(http.MethodPost, `{"classId":5000,"topic":"class-news"}`); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("create failed: %d %s", rec.Code, rec.Body.String())
	}
	if rec := request(http.MethodGet, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("management should be blocked: %d %s", rec.Code, rec.Body.String())
	}
}
