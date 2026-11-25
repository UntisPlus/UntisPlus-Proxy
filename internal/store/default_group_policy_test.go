package store

import (
	"path/filepath"
	"testing"
	"untis-proxy/internal/access"
)

func TestStarterPresetsStackOnBasic(t *testing.T) {
	st := openTestStore(t)
	const school, username = "school", "preset-user"
	if err := st.SetFeatureEnabled("", "analytics", true); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertUser(&User{Username: username, School: school, PersonType: 5}); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyUpstreamBasic(school, username); err != nil {
		t.Fatal(err)
	}
	for _, preset := range []struct {
		name string
		caps []access.Capability
	}{
		{CapGroupNotifications, []access.Capability{
			access.CapNotifyNtfyCreate, access.CapNotifyNtfyManageOwn, access.CapNotifyWebhookCreate,
			access.CapNotifyWebhookManageOwn, access.CapChangesClassView, access.CapChangesClassStream,
		}},
		{CapGroupAttendance, []access.Capability{access.CapAnalyticsPersonal}},
		{CapGroupPool, []access.Capability{access.CapPoolClassView, access.CapTimetableClassPooledView}},
		{CapGroupReconTeachers, []access.Capability{access.CapTimetableTeacherReconstructedView}},
		{CapGroupReconRooms, []access.Capability{access.CapTimetableRoomReconstructedView}},
	} {
		t.Run(preset.name, func(t *testing.T) {
			id, found, err := st.groupIDByName("", preset.name)
			if err != nil || !found {
				t.Fatalf("missing built-in preset: %v", err)
			}
			_, grants, err := st.GetCapGroup(id)
			if err != nil || len(grants) != len(preset.caps) {
				t.Fatalf("unexpected preset grants: %v (%v)", grants, err)
			}
			if err := st.AssignUserCapGroup(school, username, id); err != nil {
				t.Fatal(err)
			}
			u, err := st.GetUser(username)
			if err != nil {
				t.Fatal(err)
			}
			result, err := st.ResolveEffective(school, u)
			if err != nil {
				t.Fatal(err)
			}
			for _, cap := range preset.caps {
				if !result.Decisions[cap].Effective {
					t.Errorf("stacked preset did not enable %s", cap)
				}
			}
			if !result.Decisions[access.CapTimetableUpstreamView].Effective ||
				result.Decisions[access.CapPoolTimetableShare].Effective ||
				result.Decisions[access.CapAnalyticsTeacher].Effective {
				t.Fatal("stacking changed Basic or enabled extra sharing or analytics")
			}
		})
	}
}

func TestUpstreamBasicResetAndStackedExtras(t *testing.T) {
	st := openTestStore(t)
	st.UpsertUser(&User{Username: "a", School: "school", PersonType: 5, Admin: true})
	st.SetUserCapGrant("school", "a", string(access.CapNotifyNtfyCreate), 1)
	if err := st.ApplyUpstreamBasic("school", "a"); err != nil {
		t.Fatal(err)
	}
	u, _ := st.GetUser("a")
	r, err := st.ResolveEffective("school", u)
	if err != nil {
		t.Fatal(err)
	}
	if !u.Admin || r.InheritRoleDefaults || !r.Decisions[access.CapTimetableUpstreamView].Effective {
		t.Fatal("Basic reset did not preserve admin/read access")
	}
	for _, cap := range []access.Capability{access.CapPoolTimetableShare, access.CapTimetableClassPooledView, access.CapNotifyNtfyCreate, access.CapCalendarClassMint, access.CapHomeworkOwnManage, access.CapAbsenceNotesOwnManage, access.CapEventsPersonalView} {
		if r.Decisions[cap].Effective {
			t.Fatalf("Basic added extra %s", cap)
		}
	}
	id, err := st.SavePermissionGroup(0, "school", "NTFY only", "", map[string]int{string(access.CapNotifyNtfyCreate): 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	st.AssignUserCapGroup("school", "a", id)
	r, _ = st.ResolveEffective("school", u)
	if !r.Decisions[access.CapNotifyNtfyCreate].Effective || r.Decisions[access.CapNotifyWebhookCreate].Effective {
		t.Fatal("stacked extras not independent")
	}
	future, _ := st.SavePermissionGroup(0, "school", "Future everyone", "", map[string]int{string(access.CapNotifyWebhookCreate): 1}, "*")
	_ = future
	r, _ = st.ResolveEffective("school", u)
	if r.Decisions[access.CapNotifyWebhookCreate].Effective {
		t.Fatal("new default bypassed explicit inheritance off")
	}
}
func TestInheritedExclusionDoesNotBlockExplicitAssignmentsOrOtherUsers(t *testing.T) {
	st := openTestStore(t)
	for _, user := range []string{"a", "b"} {
		st.UpsertUser(&User{Username: user, School: "school", PersonType: 5})
	}
	id, err := st.SavePermissionGroup(0, "school", "Calendar teachers", "", map[string]int{string(access.CapAnalyticsPersonal): 1}, "*")
	if err != nil {
		t.Fatal(err)
	}
	st.SetFeatureEnabled("", "analytics", true)
	if err := st.SetDefaultGroupPolicy("school", "a", DefaultGroupPolicy{Inherit: true, ExcludedGroupIDs: []int64{id}}); err != nil {
		t.Fatal(err)
	}
	a, _ := st.GetUser("a")
	b, _ := st.GetUser("b")
	if st.EffectiveHas("school", a, access.CapAnalyticsPersonal) || !st.EffectiveHas("school", b, access.CapAnalyticsPersonal) {
		t.Fatal("individual exclusion changed another account")
	}
	st.AssignUserCapGroup("school", "a", id)
	if !st.EffectiveHas("school", a, access.CapAnalyticsPersonal) {
		t.Fatal("exclusion blocked explicit assignment")
	}
	if err := st.SetDefaultGroupPolicy("other", "a", DefaultGroupPolicy{}); err == nil {
		t.Fatal("cross-school policy accepted")
	}
	foreign, _ := st.SavePermissionGroup(0, "other", "Foreign", "", nil, "")
	if err := st.SetDefaultGroupPolicy("school", "a", DefaultGroupPolicy{Inherit: true, ExcludedGroupIDs: []int64{foreign}}); err == nil {
		t.Fatal("foreign group excluded")
	}
	st.DeleteUser("a")
	st.UpsertUser(&User{Username: "a", School: "school"})
	policy, _ := st.DefaultGroupPolicy("school", "a")
	if !policy.Inherit || len(policy.ExcludedGroupIDs) > 0 {
		t.Fatal("recreated account inherited exclusions")
	}
}
func TestPreviewHonoursExcludedInheritedGroup(t *testing.T) {
	st := openTestStore(t)
	st.UpsertUser(&User{Username: "a", School: "school", PersonType: 5})
	id, _ := st.SavePermissionGroup(0, "school", "Extra", "", map[string]int{string(access.CapEventsTechnikCreate): 1}, "*")
	st.SetDefaultGroupPolicy("school", "a", DefaultGroupPolicy{Inherit: true, ExcludedGroupIDs: []int64{id}})
	changes, err := st.PreviewPermissionGroup(GroupPreview{ID: id, School: "school", Grants: map[string]int{string(access.CapEventsTechnikDelete): 1}})
	if err != nil || len(changes) != 0 {
		t.Fatalf("excluded default changed in preview: %+v %v", changes, err)
	}
}

func TestBasicPolicySurvivesRestartAndNewDefaultSeeding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.UpsertUser(&User{Username: "a", School: "school", PersonType: 5})
	if err := st.ApplyUpstreamBasic("school", "a"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	u, _ := st.GetUser("a")
	r, err := st.ResolveEffective("school", u)
	if err != nil || r.InheritRoleDefaults || !r.Decisions[access.CapTimetableUpstreamView].Effective || r.Decisions[access.CapNotifyNtfyCreate].Effective || r.Decisions[access.CapEventsPersonalView].Effective {
		t.Fatalf("restart restored proxy defaults: %+v %v", r, err)
	}
}
