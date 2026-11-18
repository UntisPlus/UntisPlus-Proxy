package access

import "testing"

func TestResolvePrecedence(t *testing.T) {
	cap := CapTimetableTeacherBoostedRawView
	in := Inputs{
		ServerEnabled:     map[Capability]bool{cap: true},
		Direct:            map[Capability]Effect{cap: EffectDeny},
		GroupGrants:       []map[Capability]Effect{{cap: EffectAllow}},
		RoleDefaultGrants: []map[Capability]Effect{{cap: EffectAllow}},
		UserPrefs:         map[Capability]bool{cap: true},
		Toggleable:        map[Capability]bool{cap: true},
	}
	d := ResolveSet([]Capability{cap}, in)[cap]
	if d.Effective || d.AdminAllowed {
		t.Fatalf("direct deny must win, got %+v", d)
	}
	if d.Source != SourceDirectGrant {
		t.Fatalf("source = %q, want direct_grant", d.Source)
	}

	in.Direct = map[Capability]Effect{cap: EffectAllow}
	in.GroupGrants = []map[Capability]Effect{{cap: EffectDeny}}
	d = ResolveSet([]Capability{cap}, in)[cap]
	if !d.Effective {
		t.Fatalf("direct allow must beat group deny, got %+v", d)
	}

	in.Direct = nil
	d = ResolveSet([]Capability{cap}, in)[cap]
	if d.Effective || d.AdminAllowed {
		t.Fatalf("group deny must win over defaults, got %+v", d)
	}
}

func TestResolveUserOptInWithinCeiling(t *testing.T) {
	cap := CapNotifyNtfyCreate
	in := Inputs{
		ServerEnabled: map[Capability]bool{cap: true},
		Direct:        map[Capability]Effect{cap: EffectAllow},
		UserPrefs:     map[Capability]bool{cap: false},
		Toggleable:    map[Capability]bool{cap: true},
	}
	d := ResolveSet([]Capability{cap}, in)[cap]
	if d.Effective {
		t.Fatalf("user opt-out must disable effective, got %+v", d)
	}
	if !d.AdminAllowed || !d.ServerEnabled {
		t.Fatalf("ceiling must remain allowed/enabled, got %+v", d)
	}

	// Not toggleable -> cannot opt out.
	in.Toggleable = map[Capability]bool{cap: false}
	d = ResolveSet([]Capability{cap}, in)[cap]
	if !d.Effective {
		t.Fatalf("non-toggleable cap must ignore opt-out, got %+v", d)
	}

	// Not allowed -> opt-in cannot escalate.
	in.Toggleable = map[Capability]bool{cap: true}
	in.Direct = map[Capability]Effect{cap: EffectDeny}
	in.UserPrefs = map[Capability]bool{cap: true}
	d = ResolveSet([]Capability{cap}, in)[cap]
	if d.Effective || d.AdminAllowed {
		t.Fatalf("opt-in must not escalate beyond ceiling, got %+v", d)
	}
}

func TestResolveServerGate(t *testing.T) {
	cap := CapCalendarClassMint
	in := Inputs{
		ServerEnabled: map[Capability]bool{cap: false},
		Direct:        map[Capability]Effect{cap: EffectAllow},
		UserPrefs:     map[Capability]bool{cap: true},
	}
	d := ResolveSet([]Capability{cap}, in)[cap]
	if d.Effective {
		t.Fatalf("server gate must block, got %+v", d)
	}
	if d.ServerEnabled {
		t.Fatalf("ServerEnabled should be false, got %+v", d)
	}
}

func TestRoleMapping(t *testing.T) {
	if got := DefaultRoleForPersonType(5); got != RoleStudent {
		t.Fatalf("person 5 -> %q", got)
	}
	if got := DefaultRoleForPersonType(2); got != RoleTeacher {
		t.Fatalf("person 2 -> %q", got)
	}
}
