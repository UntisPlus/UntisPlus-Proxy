package access

import (
	"sort"
	"strings"
)

// Capability identifies one fine-grained permission in v2.
// Keys are stable strings so they can be stored, grouped, audited and extended.
// Canonical form: <surface>.<target>.<mode>.<action> (segments may be omitted
// for non-applicable dimensions, but stored keys should use the canonical set).
type Capability string

// Modes describe the data source / serving mode. They are mutually meaningful
// dimensions and must be distinguishable (requirement examples).
const (
	ModeUpstream      = "upstream"      // raw/stock upstream view
	ModePooled        = "pooled"        // served from proxy class pool
	ModeReconstructed = "reconstructed" // derived from pooled data (recon)
	ModeBoostedRaw    = "boosted_raw"   // forwarded via saved teacher/boosted account
)

// Common targets.
const (
	TargetClass           = "class"
	TargetTeacher         = "teacher"
	TargetRoom            = "room"
	TargetSubject         = "subject"
	TargetStudentPersonal = "student.personal"
	TargetSchool          = "school"
)

// Actions.
const (
	ActionView      = "view"
	ActionRead      = "read"
	ActionList      = "list"
	ActionDiscover  = "discover"
	ActionSubscribe = "subscribe"
	ActionStream    = "stream"
	ActionCreate    = "create"
	ActionMint      = "mint"
	ActionEdit      = "edit"
	ActionWrite     = "write"
	ActionManage    = "manage"
)

// Canonical capability keys used by the proxy. This set covers v1 behavior and
// the required v2 granularity; resolution treats keys generically so new keys
// can be added without schema changes.
const (
	// Timetable viewing / data access.
	CapTimetableClassPooledView               Capability = "timetable.class.pooled.view"
	CapTimetableClassUpstreamView             Capability = "timetable.class.upstream.view"
	CapTimetableClassBoostedRawView           Capability = "timetable.class.boosted_raw.view"
	CapTimetableStudentPersonalUpstreamView   Capability = "timetable.student.personal.upstream.view"
	CapTimetableStudentPersonalBoostedRawView Capability = "timetable.student.personal.boosted_raw.view"
	CapTimetableTeacherReconstructedView      Capability = "timetable.teacher.reconstructed.view"
	CapTimetableTeacherBoostedRawView         Capability = "timetable.teacher.boosted_raw.view"
	CapTimetableRoomReconstructedView         Capability = "timetable.room.reconstructed.view"
	CapTimetableRoomBoostedRawView            Capability = "timetable.room.boosted_raw.view"
	CapTimetableSubjectReconstructedView      Capability = "timetable.subject.reconstructed.view"
	CapTimetableSubjectBoostedRawView         Capability = "timetable.subject.boosted_raw.view"

	// Pool/index visibility (discoverability of pooled classes for real users).
	CapPoolClassView      Capability = "pool.class.view"
	CapPoolTimetableShare Capability = "pool.timetable.share"

	// Change detection / live stream.
	CapChangesClassView     Capability = "changes.class.view"
	CapChangesClassStream   Capability = "changes.class.stream"
	CapChangesElementView   Capability = "changes.element.view"
	CapChangesElementStream Capability = "changes.element.stream"
	CapChangesSchoolView    Capability = "changes.school.view"
	CapChangesSchoolStream  Capability = "changes.school.stream"

	// Notifications (self-service subscriptions).
	CapNotifyNtfyCreate       Capability = "notify.ntfy.create"
	CapNotifyNtfyManageOwn    Capability = "notify.ntfy.manage_own"
	CapNotifyWebhookCreate    Capability = "notify.webhook.create"
	CapNotifyWebhookManageOwn Capability = "notify.webhook.manage_own"

	// Calendar tokens/feeds for authenticated real accounts.
	CapCalendarClassMint           Capability = "calendar.class.mint"
	CapCalendarStudentPersonalMint Capability = "calendar.student.personal.mint"
	CapCalendarTeacherMint         Capability = "calendar.teacher.mint"
	CapCalendarRoomMint            Capability = "calendar.room.mint"
	CapCalendarSubjectMint         Capability = "calendar.subject.mint"
	CapCalendarTechnikMint         Capability = "calendar.technik.mint"
	CapCalendarManageOwn           Capability = "calendar.manage_own"

	// Absences / editing (class-scoped write flows are editor/teacher-backed).
	CapAbsenceOwnRead Capability = "absence.own.read"
	CapAbsenceWrite   Capability = "absence.write"
	CapLessonWrite    Capability = "lesson.write"

	// Technik / proxy events (v1 "techniker").
	CapEventsTechnikView   Capability = "events.technik.view"
	CapEventsTechnikManage Capability = "events.technik.manage"
)

// AllCapabilities lists canonical capabilities in deterministic order.
var AllCapabilities = []Capability{
	CapTimetableClassPooledView,
	CapTimetableClassUpstreamView,
	CapTimetableClassBoostedRawView,
	CapTimetableStudentPersonalUpstreamView,
	CapTimetableStudentPersonalBoostedRawView,
	CapTimetableTeacherReconstructedView,
	CapTimetableTeacherBoostedRawView,
	CapTimetableRoomReconstructedView,
	CapTimetableRoomBoostedRawView,
	CapTimetableSubjectReconstructedView,
	CapTimetableSubjectBoostedRawView,
	CapPoolClassView,
	CapPoolTimetableShare,
	CapChangesClassView,
	CapChangesClassStream,
	CapChangesElementView,
	CapChangesElementStream,
	CapChangesSchoolView,
	CapChangesSchoolStream,
	CapNotifyNtfyCreate,
	CapNotifyNtfyManageOwn,
	CapNotifyWebhookCreate,
	CapNotifyWebhookManageOwn,
	CapCalendarClassMint,
	CapCalendarStudentPersonalMint,
	CapCalendarTeacherMint,
	CapCalendarRoomMint,
	CapCalendarSubjectMint,
	CapCalendarTechnikMint,
	CapCalendarManageOwn,
	CapAbsenceOwnRead,
	CapAbsenceWrite,
	CapLessonWrite,
	CapEventsTechnikView,
	CapEventsTechnikManage,
}

// Role is an admin-defined/login-type label used for default group assignment.
// Concrete upstream person_type values are mapped to roles via configuration.
type Role string

const (
	RoleUnknown Role = "unknown"
	RoleStudent Role = "student"
	RoleTeacher Role = "teacher"
	RoleStaff   Role = "staff"
	RoleOther   Role = "other"
)

// DefaultRoleForPersonType provides a built-in mapping from WebUntis person_type
// to a role label. Admin-configurable mappings may override this.
func DefaultRoleForPersonType(personType int64) Role {
	switch personType {
	case 5:
		return RoleStudent
	case 2, 1:
		// 2 observed as teacher in existing tests/flows; 1 reserved/common for
		// teacher/staff-like identities. Treated as teacher-class by default.
		return RoleTeacher
	case 0:
		return RoleUnknown
	default:
		return RoleOther
	}
}

// Effect is the outcome of a grant/setting in resolution.
type Effect int8

const (
	EffectUnset Effect = iota
	EffectDeny
	EffectAllow
)

// Source identifies where an effective decision came from (for /me and debug).
type Source string

const (
	SourceServerGate  Source = "server_gate"
	SourceDirectGrant Source = "direct_grant"
	SourceGroup       Source = "group"
	SourceRoleDefault Source = "role_default"
	SourceUserPref    Source = "user_pref"
	SourceImplicit    Source = "implicit"
)

// Decision is the resolved state for one capability.
type Decision struct {
	Capability    Capability `json:"capability"`
	ServerEnabled bool       `json:"serverEnabled"`
	AdminAllowed  bool       `json:"adminAllowed"`
	UserOptedIn   bool       `json:"userOptedIn"`
	Effective     bool       `json:"effective"`
	Toggleable    bool       `json:"toggleable"`
	Source        Source     `json:"source,omitempty"`
}

// Grant represents an allow/deny assignment from a direct grant, group, or role default.
type Grant struct {
	Capability Capability
	Allow      bool
}

// GroupDef is an in-memory group definition.
type GroupDef struct {
	ID     int64
	Name   string
	Scope  string // ""/global or school name
	Grants []Grant
}

// Inputs collects all inputs needed to resolve effective capabilities for one
// principal in one school. Callers precompute/cache these.
type Inputs struct {
	ServerEnabled map[Capability]bool
	// Direct grants for the user (highest admin precedence).
	Direct map[Capability]Effect
	// Grants contributed by assigned groups, in application order.
	GroupGrants []map[Capability]Effect
	// Grants from default groups for the user's role, in application order.
	RoleDefaultGrants []map[Capability]Effect
	// User opt-in/out preferences. Missing means default opt-in when allowed.
	UserPrefs map[Capability]bool
	// Toggleable marks capabilities the user may opt out of when allowed.
	Toggleable map[Capability]bool
}

// Resolve computes effective decisions for all canonical capabilities.
// Precedence (admin side): explicit deny > direct allow > group allow/deny > role
// default allow/deny. User preference can only disable within admin-allowed,
// toggleable capabilities. Server enablement gates everything.
func Resolve(in Inputs) map[Capability]Decision {
	out := make(map[Capability]Decision, len(AllCapabilities))
	for _, cap := range AllCapabilities {
		out[cap] = resolveOne(cap, in)
	}
	return out
}

// ResolveSet is like Resolve but for an arbitrary capability set.
func ResolveSet(caps []Capability, in Inputs) map[Capability]Decision {
	out := make(map[Capability]Decision, len(caps))
	for _, cap := range caps {
		out[cap] = resolveOne(cap, in)
	}
	return out
}

func resolveOne(cap Capability, in Inputs) Decision {
	enabled := true
	if in.ServerEnabled != nil {
		if v, ok := in.ServerEnabled[cap]; ok {
			enabled = v
		}
	}
	// Admin side resolution.
	adminEff, src := resolveAdmin(cap, in)
	toggleable := in.Toggleable != nil && in.Toggleable[cap]
	optIn := true
	if in.UserPrefs != nil {
		if v, ok := in.UserPrefs[cap]; ok {
			optIn = v
		}
	}
	if !toggleable {
		optIn = true
	}
	effective := enabled && adminEff == EffectAllow && optIn
	return Decision{
		Capability:    cap,
		ServerEnabled: enabled,
		AdminAllowed:  adminEff == EffectAllow,
		UserOptedIn:   optIn,
		Effective:     effective,
		Toggleable:    toggleable,
		Source:        src,
	}
}

func resolveAdmin(cap Capability, in Inputs) (Effect, Source) {
	// Direct
	if e, ok := in.Direct[cap]; ok && e != EffectUnset {
		return e, SourceDirectGrant
	}
	// Groups: evaluate in order; explicit deny wins over allow within groups at
	// same level? We scan all group layers: a deny in any group beats allows in
	// groups unless a direct grant overrode (handled above). To be predictable:
	// first pass for deny across groups, second for allow.
	if len(in.GroupGrants) > 0 {
		sawAllow := false
		for _, g := range in.GroupGrants {
			e := g[cap]
			if e == EffectDeny {
				return EffectDeny, SourceGroup
			}
			if e == EffectAllow {
				sawAllow = true
			}
		}
		if sawAllow {
			return EffectAllow, SourceGroup
		}
	}
	// Role defaults
	if len(in.RoleDefaultGrants) > 0 {
		sawAllow := false
		for _, g := range in.RoleDefaultGrants {
			e := g[cap]
			if e == EffectDeny {
				return EffectDeny, SourceRoleDefault
			}
			if e == EffectAllow {
				sawAllow = true
			}
		}
		if sawAllow {
			return EffectAllow, SourceRoleDefault
		}
	}
	return EffectUnset, SourceImplicit
}

// Effective reports whether a capability is effective in a resolved map.
func Effective(m map[Capability]Decision, cap Capability) bool {
	d, ok := m[cap]
	return ok && d.Effective
}

// Allowed reports whether a capability is admin-allowed and server-enabled,
// ignoring user opt-in (useful for computing toggleable ceilings).
func Allowed(m map[Capability]Decision, cap Capability) bool {
	d, ok := m[cap]
	return ok && d.ServerEnabled && d.AdminAllowed
}

// Keys returns sorted capability keys present in a map.
func Keys(m map[Capability]Decision) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	return keys
}

// ParseCapability normalizes and validates a capability key string.
func ParseCapability(s string) (Capability, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	return Capability(s), true
}

// NormalizeRole returns a known role label or RoleOther/RoleUnknown.
func NormalizeRole(s string) Role {
	switch Role(strings.ToLower(strings.TrimSpace(s))) {
	case RoleStudent:
		return RoleStudent
	case RoleTeacher:
		return RoleTeacher
	case RoleStaff:
		return RoleStaff
	case RoleOther:
		return RoleOther
	case RoleUnknown, "":
		return RoleUnknown
	default:
		return Role(strings.ToLower(strings.TrimSpace(s)))
	}
}

// CapabilitySet is a convenience for set membership.
type CapabilitySet map[Capability]bool

func NewCapabilitySet(caps ...Capability) CapabilitySet {
	s := make(CapabilitySet, len(caps))
	for _, c := range caps {
		s[c] = true
	}
	return s
}

func (s CapabilitySet) Has(c Capability) bool { return s != nil && s[c] }
