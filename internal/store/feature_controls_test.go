package store

import (
	"testing"
	"untis-proxy/internal/access"
)

func TestFeatureAndCapabilityServerCeilings(t *testing.T) {
	st := openTestStore(t)
	if st.FeatureEnabled("school", "analytics") {
		t.Fatal("analytics must start off")
	}
	st.SetFeatureEnabled("school", "analytics", true)
	if st.FeatureEnabled("school", "analytics") {
		t.Fatal("school bypassed server default-off")
	}
	st.SetFeatureEnabled("", "analytics", true)
	if !st.FeatureEnabled("school", "analytics") {
		t.Fatal("master enable failed")
	}
	st.SetFeatureEnabled("school", "analytics", false)
	if st.FeatureEnabled("school", "analytics") {
		t.Fatal("school disable failed")
	}
	st.SetServerCapGate("", string(access.CapNotifyNtfyCreate), false)
	st.SetServerCapGate("school", string(access.CapNotifyNtfyCreate), true)
	gates, err := st.ServerCapGates("school")
	if err != nil || gates[string(access.CapNotifyNtfyCreate)] {
		t.Fatal("school bypassed server capability deny")
	}
}
func TestPermissionPreviewDoesNotSaveAndMatchesResolution(t *testing.T) {
	st := openTestStore(t)
	st.UpsertUser(&User{Username: "student", School: "school", PersonType: 5})
	st.UpsertUser(&User{Username: "outsider", School: "other", PersonType: 5})
	proposal := GroupPreview{School: "school", Grants: map[string]int{string(access.CapEventsTechnikDelete): 1}, AssignTo: "*"}
	before, _ := st.ListCapGroups("school")
	preview, err := st.PreviewPermissionGroup(proposal)
	if err != nil || len(preview) != 1 || preview[0].Username != "student" {
		t.Fatalf("preview=%+v error=%v", preview, err)
	}
	after, _ := st.ListCapGroups("school")
	if len(before) != len(after) {
		t.Fatal("preview wrote group")
	}
	id, err := st.SavePermissionGroup(0, "school", "Custom", "", proposal.Grants, "*")
	if err != nil {
		t.Fatal(err)
	}
	_ = id
	u, _ := st.GetUser("student")
	actual, _ := st.ResolveEffective("school", u)
	for _, change := range preview[0].Changes {
		if actual.Decisions[access.Capability(change.Capability)] != change.After {
			t.Fatal("preview differs from save")
		}
	}
	if _, err := st.PreviewPermissionGroup(GroupPreview{School: "school", Grants: map[string]int{"fake": 1}}); err == nil {
		t.Fatal("unknown capability previewed")
	}
}
func TestAdmissionPolicyFailClosedAndGlobalDeny(t *testing.T) {
	st := openTestStore(t)
	notAdmin := false
	admin := true
	if !st.AdmissionAllowed("school", AdmissionIdentity{Role: "unknown"}) {
		t.Fatal("default must preserve admission")
	}
	st.SetAdmissionPolicy("", AdmissionPolicy{BlockedRoles: []string{"student"}})
	if st.AdmissionAllowed("school", AdmissionIdentity{Role: "student"}) {
		t.Fatal("school bypassed global policy")
	}
	st.SetAdmissionPolicy("school", AdmissionPolicy{BlockedRoles: []string{"upstream_admin"}})
	for _, flag := range []*bool{nil, &admin} {
		if st.AdmissionAllowed("school", AdmissionIdentity{Role: "teacher", UpstreamAdmin: flag}) {
			t.Fatal("unverified/admin admitted")
		}
	}
	if !st.AdmissionAllowed("school", AdmissionIdentity{Role: "teacher", UpstreamAdmin: &notAdmin}) {
		t.Fatal("verified teacher denied")
	}
	if st.SetAdmissionPolicy("school", AdmissionPolicy{BlockedRoles: []string{"made-up"}}) == nil {
		t.Fatal("invalid role accepted")
	}
}
func TestAnalyticsTargetAndYearValidation(t *testing.T) {
	st := openTestStore(t)
	st.UpsertUser(&User{Username: "a", School: "school"})
	if st.SetAnalyticsAccess("other", "a", AnalyticsAccess{}) == nil {
		t.Fatal("cross-school scope saved")
	}
	if st.SetAnalyticsAccess("school", "a", AnalyticsAccess{Profiles: []AnalyticsProfile{{ID: "history", Segments: []AnalyticsSegment{{ClassID: 1, From: "bad", To: "2026-01-01"}}}}}) == nil {
		t.Fatal("invalid dates saved")
	}
	if st.SetAnalyticsYears("school", []AnalyticsYear{{"a", "2025-01-01", "2025-12-31"}, {"b", "2025-06-01", "2026-06-01"}}) == nil {
		t.Fatal("overlapping years accepted")
	}
}

func TestDeletingAccountClearsAnalyticsScopeAndPermissionRules(t *testing.T) {
	st := openTestStore(t)
	st.UpsertUser(&User{Username: "a", School: "school", PersonType: 5})
	st.SetAnalyticsAccess("school", "a", AnalyticsAccess{TeacherIDs: []int64{42}})
	st.SetUserCapGrant("school", "a", string(access.CapAnalyticsTeacher), 1)
	st.SaveAdmissionIdentity("school", "a", AdmissionIdentity{Role: "teacher"})
	if _, err := st.DeleteUser("a"); err != nil {
		t.Fatal(err)
	}
	st.UpsertUser(&User{Username: "a", School: "school", PersonType: 5})
	scope, err := st.AnalyticsAccess("school", "a")
	if err != nil || len(scope.TeacherIDs) != 0 {
		t.Fatal("new account inherited old analytics scope")
	}
	grants, _ := st.ListUserCapGrants("school", "a")
	if len(grants) != 0 {
		t.Fatal("new account inherited permission grants")
	}
}
func TestPreviewManageExpansionAndExplicitBlock(t *testing.T) {
	st := openTestStore(t)
	st.UpsertUser(&User{Username: "a", School: "school", PersonType: 5})
	p := GroupPreview{School: "school", AssignTo: "*", Grants: map[string]int{string(access.CapEventsTechnikManage): 1, string(access.CapEventsTechnikDelete): -1}}
	result, err := st.PreviewPermissionGroup(p)
	if err != nil || len(result) != 1 {
		t.Fatal(err)
	}
	seenEdit := false
	for _, change := range result[0].Changes {
		if change.Capability == string(access.CapEventsTechnikEdit) {
			seenEdit = change.After.Effective
		}
		if change.Capability == string(access.CapEventsTechnikDelete) && change.After.Effective {
			t.Fatal("manage bypassed deletion block")
		}
	}
	if !seenEdit {
		t.Fatal("preview missed compatibility expansion")
	}
}
func TestControlKeysCannotCollideAcrossSchools(t *testing.T) {
	if controlKey("analytics-access", "a/b", "c") == controlKey("analytics-access", "a", "b/c") {
		t.Fatal("scope keys collide")
	}
}

func TestFeatureStateBatchMatchesIndividualResolution(t *testing.T) {
	st := openTestStore(t)
	st.SetFeatureEnabled("", "analytics", true)
	st.SetFeatureEnabled("school", "analytics", false)
	st.SetFeatureEnabled("", "calendar", false)
	st.SetFeatureEnabled("school", "calendar", true)
	states := st.FeatureStates("school")
	for name := range FeatureDefaults {
		if states[name] != st.FeatureEnabled("school", name) {
			t.Fatalf("feature batch differs for %s", name)
		}
	}
}

func TestPermissionPreviewFailsClosedWhenRuleStorageUnavailable(t *testing.T) {
	st := openTestStore(t)
	st.UpsertUser(&User{Username: "a", School: "school", PersonType: 5})
	if _, err := st.db.Exec(`DROP TABLE user_cap_grants`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PreviewPermissionGroup(GroupPreview{School: "school", Grants: map[string]int{string(access.CapAnalyticsPersonal): 1}, AssignTo: "*"}); err == nil {
		t.Fatal("preview silently ignored missing rules")
	}
}
