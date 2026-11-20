package store

import (
	"testing"
	"untis-proxy/internal/access"
)

func TestPermissionGroupEditorAudienceAndAtomicSave(t *testing.T) {
	st := openTestStore(t)
	grants := map[string]int{string(access.CapNotifyNtfyCreate): -1, string(access.CapCalendarClassMint): -1}
	id, err := st.SavePermissionGroup(0, "school-a", "No subscriptions", "", grants, "*")
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range []*User{{Username: "student", PersonType: 5}, {Username: "teacher", PersonType: 2}, {Username: "unknown", PersonType: 0}} {
		result, err := st.ResolveEffective("school-a", user)
		if err != nil {
			t.Fatal(err)
		}
		if result.Decisions[access.CapNotifyNtfyCreate].AdminAllowed || result.Decisions[access.CapCalendarClassMint].AdminAllowed {
			t.Fatalf("everyone rules did not apply to %s", user.Username)
		}
		other, _ := st.ResolveEffective("school-b", user)
		if !other.Decisions[access.CapNotifyNtfyCreate].AdminAllowed {
			t.Fatal("school-scoped audience leaked into another school")
		}
	}
	if _, err = st.SavePermissionGroup(id, "school-a", "Changed", "", map[string]int{"not.a.permission": 1}, ""); err == nil {
		t.Fatal("accepted unknown permission")
	}
	group, existing, err := st.GetCapGroup(id)
	if err != nil || group.Name != "No subscriptions" || len(existing) != 2 {
		t.Fatalf("invalid edit changed group: %+v %+v %v", group, existing, err)
	}
	if _, err = st.SavePermissionGroup(id, "school-b", "Changed", "", nil, ""); err == nil {
		t.Fatal("accepted edit from another school")
	}
	if _, err = st.SavePermissionGroup(id, "school-a", "Updated", "", map[string]int{string(access.CapNotifyNtfyCreate): 1}, ""); err != nil {
		t.Fatal(err)
	}
	group, existing, err = st.GetCapGroup(id)
	if err != nil || group.Name != "Updated" || len(existing) != 1 || existing[0].Effect != 1 {
		t.Fatal("edit did not replace the complete rule set")
	}
}
