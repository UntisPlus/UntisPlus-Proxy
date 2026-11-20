package proxy

import (
	"strings"

	"untis-proxy/internal/access"
	"untis-proxy/internal/store"
)

func (p *Proxy) userSchool(u *store.User, fallback string) string {
	if u != nil && u.School != "" {
		return u.School
	}
	return fallback
}

func (p *Proxy) effective(u *store.User, school string, cap access.Capability) bool {
	if u == nil || u.Username == "" || u.Username == "#anonymous#" {
		return false
	}
	return p.store.EffectiveHas(p.userSchool(u, school), u, cap)
}

func (p *Proxy) canViewClassAs(u *store.User, school string, classID int64, own bool) bool {
	if u == nil || u.Username == "#anonymous#" {
		return false
	}
	if own {
		if u.PersonType != 5 || u.ClassID <= 0 || u.ClassID != classID {
			return false
		}
		return p.effective(u, school, access.CapTimetableClassUpstreamView) ||
			p.effective(u, school, access.CapTimetableClassPooledView) ||
			p.canViewClassBoosted(u, school)
	}
	return p.effective(u, school, access.CapTimetableClassPooledView) || p.canViewClassBoosted(u, school)
}

func (p *Proxy) canViewClassBoosted(u *store.User, school string) bool {
	return p.effective(u, school, access.CapTimetableClassBoostedRawView)
}

func (p *Proxy) canViewPersonalUpstream(u *store.User, school string) bool {
	if u == nil || u.Username == "#anonymous#" {
		return false
	}
	return p.effective(u, school, access.CapTimetableStudentPersonalUpstreamView)
}

func (p *Proxy) canViewPersonalBoosted(u *store.User, school string) bool {
	return p.effective(u, school, access.CapTimetableStudentPersonalBoostedRawView)
}

func (p *Proxy) canViewTeacherRecon(u *store.User, school string) bool {
	return p.effective(u, school, access.CapTimetableTeacherReconstructedView)
}
func (p *Proxy) canViewRoomRecon(u *store.User, school string) bool {
	return p.effective(u, school, access.CapTimetableRoomReconstructedView)
}
func (p *Proxy) canViewSubjectRecon(u *store.User, school string) bool {
	return p.effective(u, school, access.CapTimetableSubjectReconstructedView)
}

func (p *Proxy) canViewTeacherBoosted(u *store.User, school string) bool {
	return p.effective(u, school, access.CapTimetableTeacherBoostedRawView)
}
func (p *Proxy) canViewRoomBoosted(u *store.User, school string) bool {
	return p.effective(u, school, access.CapTimetableRoomBoostedRawView)
}
func (p *Proxy) canViewSubjectBoosted(u *store.User, school string) bool {
	return p.effective(u, school, access.CapTimetableSubjectBoostedRawView)
}

func (p *Proxy) canViewElementRecon(u *store.User, school, elType string) bool {
	switch strings.ToUpper(elType) {
	case "TEACHER":
		return p.canViewTeacherRecon(u, school)
	case "ROOM":
		return p.canViewRoomRecon(u, school)
	case "SUBJECT":
		return p.canViewSubjectRecon(u, school)
	default:
		return false
	}
}

func (p *Proxy) canViewElementBoosted(u *store.User, school, elType string) bool {
	switch strings.ToUpper(elType) {
	case "TEACHER":
		return p.canViewTeacherBoosted(u, school)
	case "ROOM":
		return p.canViewRoomBoosted(u, school)
	case "SUBJECT":
		return p.canViewSubjectBoosted(u, school)
	default:
		return false
	}
}

func (p *Proxy) canWriteAbsence(u *store.User, school string) bool {
	return p.effective(u, school, access.CapAbsenceWrite)
}

func (p *Proxy) canWriteLesson(u *store.User, school string) bool {
	return p.effective(u, school, access.CapLessonWrite)
}

func (p *Proxy) canViewPool(u *store.User, school string) bool {
	if u == nil || u.Username == "#anonymous#" {
		return false
	}
	return p.effective(u, school, access.CapPoolClassView) || p.effective(u, school, access.CapTimetableClassPooledView)
}

func (p *Proxy) canViewTechnik(u *store.User, school string) bool {
	if u == nil || u.Username == "#anonymous#" {
		return false
	}
	return p.effective(u, school, access.CapEventsTechnikView) || p.effective(u, school, access.CapEventsTechnikManage)
}

func (p *Proxy) canManageTechnik(u *store.User, school string) bool {
	if u == nil || u.Username == "#anonymous#" {
		return false
	}
	return p.effective(u, school, access.CapEventsTechnikManage)
}

// compat coarse helpers derived from v2 effective state (used during transition)
func (p *Proxy) hasBoostedAccess(u *store.User, school string) bool {
	if u == nil || u.Username == "#anonymous#" {
		return false
	}
	return p.canViewClassBoosted(u, school) || p.canViewTeacherBoosted(u, school) || p.canViewRoomBoosted(u, school) || p.canViewSubjectBoosted(u, school) || p.canViewPersonalBoosted(u, school) || p.canWriteAbsence(u, school) || p.canWriteLesson(u, school)
}

func (p *Proxy) hasEditorAccess(u *store.User, school string) bool {
	if u == nil || u.Username == "#anonymous#" {
		return false
	}
	return p.canWriteAbsence(u, school) || p.canWriteLesson(u, school)
}

// legacy string-username helpers (best effort; prefer user-aware helpers)
func (p *Proxy) userHasBoostedAccess(username string) bool {
	if username == "" || username == "#anonymous#" {
		return false
	}
	u, err := p.store.GetUser(username)
	if err != nil || u == nil {
		return p.isBoosted(username)
	}
	return p.hasBoostedAccess(u, u.School)
}

func (p *Proxy) userHasEditorAccess(username string) bool {
	if username == "" || username == "#anonymous#" {
		return false
	}
	u, err := p.store.GetUser(username)
	if err != nil || u == nil {
		return p.isEditor(username)
	}
	return p.hasEditorAccess(u, u.School)
}

func (p *Proxy) canBoostedRawForTimetable(u *store.User, school, reqType string, own bool) bool {
	if u == nil || u.Username == "#anonymous#" {
		return false
	}
	switch strings.ToUpper(reqType) {
	case "CLASS":
		return p.canViewClassBoosted(u, school)
	case "TEACHER":
		return p.canViewTeacherBoosted(u, school)
	case "ROOM":
		return p.canViewRoomBoosted(u, school)
	case "SUBJECT":
		return p.canViewSubjectBoosted(u, school)
	case "STUDENT":
		// own personal timetable can be served boosted-raw when allowed
		if own {
			return p.canViewPersonalBoosted(u, school)
		}
		// viewing other students' timetables as boosted raw is not a defined v2 cap; deny by default
		return false
	default:
		return false
	}
}

// ---- calendar/notify v2 helpers ----
// ---- calendar/notify v2 helpers ----

func (p *Proxy) canAccessClassTarget(u *store.User, school string, classID int64) bool {
	if u == nil || u.Username == "#anonymous#" || classID <= 0 {
		return false
	}
	if u.PersonType != 5 { // staff stock visibility
		return true
	}
	if u.ClassID == classID {
		return p.canViewClassAs(u, school, classID, true) || p.canViewClassBoosted(u, school)
	}
	// Other classes: require the class to be in the known pool (subscriptions/
	// calendar targets and pooled/reconstructed access are pool-scoped).
	ok, _ := p.store.PoolContains(school, classID)
	if !ok {
		return false
	}
	if p.canViewClassBoosted(u, school) {
		return true
	}
	if p.canViewClassAs(u, school, classID, false) {
		return true
	}
	return false
}

func (p *Proxy) canAccessPersonalTarget(u *store.User, school string, personID int64) bool {
	if u == nil || u.Username == "#anonymous#" || personID <= 0 || u.PersonID != personID {
		return false
	}
	return p.canViewPersonalUpstream(u, school) || p.canViewPersonalBoosted(u, school)
}

func (p *Proxy) canAccessElementTarget(u *store.User, school, elType string) bool {
	if u == nil || u.Username == "#anonymous#" {
		return false
	}
	if u.PersonType != 5 {
		return true
	}
	return p.canViewElementRecon(u, school, elType) || p.canViewElementBoosted(u, school, elType)
}

func (p *Proxy) canAccessTechnikTarget(u *store.User, school string) bool {
	return p.canViewTechnik(u, school)
}

func (p *Proxy) canCalendarMintClass(u *store.User, school string, classID int64) bool {
	if u == nil || u.Username == "#anonymous#" {
		return false
	}
	if !p.effective(u, school, access.CapCalendarClassMint) {
		return false
	}
	if classID == technikClassID {
		return p.canCalendarMintTechnik(u, school)
	}
	return p.canAccessClassTarget(u, school, classID)
}

func (p *Proxy) canCalendarMintPersonal(u *store.User, school string, personID int64) bool {
	if u == nil || u.Username == "#anonymous#" || personID <= 0 || u.PersonID != personID {
		return false
	}
	if !p.effective(u, school, access.CapCalendarStudentPersonalMint) {
		return false
	}
	return p.canAccessPersonalTarget(u, school, personID)
}

func (p *Proxy) canCalendarMintElement(u *store.User, school, elType string) bool {
	if u == nil || u.Username == "#anonymous#" {
		return false
	}
	switch strings.ToUpper(elType) {
	case "TEACHER":
		if !p.effective(u, school, access.CapCalendarTeacherMint) {
			return false
		}
	case "ROOM":
		if !p.effective(u, school, access.CapCalendarRoomMint) {
			return false
		}
	case "SUBJECT":
		if !p.effective(u, school, access.CapCalendarSubjectMint) {
			return false
		}
	default:
		return false
	}
	return p.canAccessElementTarget(u, school, elType)
}

func (p *Proxy) canCalendarMintTechnik(u *store.User, school string) bool {
	if u == nil || u.Username == "#anonymous#" {
		return false
	}
	return p.effective(u, school, access.CapCalendarTechnikMint) && p.canAccessTechnikTarget(u, school)
}

func (p *Proxy) canNotifyCreate(u *store.User, school, kind string) bool {
	if u == nil {
		return false
	}
	if u.Username == "#anonymous#" {
		return p.anonymousFeature(school, kind)
	}
	switch kind {
	case "ntfy":
		return p.effective(u, school, access.CapNotifyNtfyCreate)
	case "webhooks":
		return p.effective(u, school, access.CapNotifyWebhookCreate)
	}
	return false
}

func (p *Proxy) canNotifyManageOwn(u *store.User, school, kind string) bool {
	if u == nil {
		return false
	}
	if u.Username == "#anonymous#" {
		return p.anonymousFeature(school, kind)
	}
	switch kind {
	case "ntfy":
		return p.effective(u, school, access.CapNotifyNtfyManageOwn)
	case "webhooks":
		return p.effective(u, school, access.CapNotifyWebhookManageOwn)
	}
	return false
}

func (p *Proxy) canSubscribeTarget(u *store.User, school string, et string, eid int64) bool {
	if u == nil {
		return false
	}
	if u.Username == "#anonymous#" {
		// anon cannot subscribe to personal/technik/school; class/element targets are
		// limited to what upstream anonymous exposure allows for this school (stock),
		// not proxy-pooled/reconstructed supplements.
		switch strings.ToUpper(et) {
		case "CLASS":
			if eid == technikClassID {
				return false
			}
			payload, prof, err := p.store.GetSchoolAnonProfilePayload(school)
			if err == nil && prof != nil {
				switch prof.Source {
				case "override":
					if !prof.LoginOK {
						return false
					}
					// Definitive admin-pinned state: only explicitly exposed/displayable
					// classes are targetable as stock anonymous surfaces.
					for _, id := range payload.ClassesDisplayable {
						if id == eid {
							return true
						}
					}
					return false
				case "detect":
					if !prof.LoginOK {
						return false
					}
					for _, id := range payload.ClassesDisplayable {
						if id == eid {
							return true
						}
					}
					return false
				default:
					// Transient/non-definitive probe state (e.g. probe_error): preserve
					// last-known-good hints when available. Without authoritative
					// upstream detection, do not infer anonymous stock targets from
					// proxy pool/recon state.
					if len(payload.ClassesDisplayable) > 0 {
						for _, id := range payload.ClassesDisplayable {
							if id == eid {
								return true
							}
						}
					}
					return false
				}
			}
			// No profile/hints yet: anonymous stock class targets require upstream
			// detection/override and must not be inferred from proxy pool state.
			return false
		case "TEACHER", "ROOM", "SUBJECT":
			// no proxy recon/element supplements for anon in v2; deny by default
			return false
		default:
			return false
		}
	}
	switch strings.ToUpper(et) {
	case "STUDENT":
		return p.canAccessPersonalTarget(u, school, eid)
	case "CLASS":
		if eid == technikClassID {
			return false
		}
		return p.canAccessClassTarget(u, school, eid)
	case "TEACHER":
		return p.canAccessElementTarget(u, school, "TEACHER")
	case "ROOM":
		return p.canAccessElementTarget(u, school, "ROOM")
	case "SUBJECT":
		return p.canAccessElementTarget(u, school, "SUBJECT")
	case "TECHNIK":
		return false
	case "", "SCHOOL":
		return p.effective(u, school, access.CapChangesSchoolView) || p.isAdmin(u.Username)
	}
	return false
}

func (p *Proxy) canWatchChangesTarget(u *store.User, school, et string, eid int64, stream bool) bool {
	if u == nil {
		return false
	}
	if u.Username == "#anonymous#" {
		return p.anonymousFeature(school, "changes") && p.canSubscribeTarget(u, school, et, eid)
	}
	if !p.canSubscribeTarget(u, school, et, eid) {
		return false
	}
	view, str := access.Capability(""), access.Capability("")
	switch strings.ToUpper(et) {
	case "STUDENT", "CLASS":
		view, str = access.CapChangesClassView, access.CapChangesClassStream
	case "TEACHER", "ROOM", "SUBJECT":
		view, str = access.CapChangesElementView, access.CapChangesElementStream
	case "", "SCHOOL":
		if p.isAdmin(u.Username) {
			return true
		}
		view, str = access.CapChangesSchoolView, access.CapChangesSchoolStream
	default:
		return false
	}
	if stream {
		return p.effective(u, school, str) && p.effective(u, school, view)
	}
	return p.effective(u, school, view)
}

func (p *Proxy) calendarHasAccessibleTargets(u *store.User, school string, includeTechnik bool) bool {
	opts := p.subscriptionOptionsForCaps(school, u, includeTechnik, true)
	if _, ok := opts["personal"]; ok {
		return true
	}
	for _, k := range []string{"class", "teacher", "room", "subject"} {
		if v, ok := opts[k].([]subscriptionOption); ok && len(v) > 0 {
			return true
		}
	}
	if includeTechnik {
		if _, ok := opts["technik"]; ok {
			return true
		}
	}
	return false
}
