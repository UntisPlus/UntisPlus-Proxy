package proxy

import (
	"encoding/json"
	"net/http"
	"untis-proxy/internal/access"
	"untis-proxy/internal/store"
)

func (p *Proxy) prefersOwnUpstream(u *store.User, school, kind string, own bool) bool {
	if u == nil || p.userSchool(u, p.opts.School) != school {
		return false
	}
	r, err := p.store.ResolveEffective(school, u)
	if err != nil {
		return false
	}
	has := func(c access.Capability) bool { return r.Decisions[c].Effective }
	if !has(access.CapTimetableUpstreamView) {
		return false
	}
	raw := map[string]access.Capability{"CLASS": access.CapTimetableClassBoostedRawView, "TEACHER": access.CapTimetableTeacherBoostedRawView, "ROOM": access.CapTimetableRoomBoostedRawView, "SUBJECT": access.CapTimetableSubjectBoostedRawView}
	if cap, ok := raw[kind]; ok && has(cap) {
		return false
	}
	switch kind {
	case "CLASS":
		return !has(access.CapTimetableClassPooledView)
	case "TEACHER":
		return !has(access.CapTimetableTeacherReconstructedView)
	case "ROOM":
		return !has(access.CapTimetableRoomReconstructedView)
	case "SUBJECT":
		return !has(access.CapTimetableSubjectReconstructedView)
	case "STUDENT":
		return !own || !has(access.CapTimetableStudentPersonalBoostedRawView)
	}
	return true
}
func (p *Proxy) basicMasterData(school, user string) bool {
	u, err := p.store.GetUser(user)
	if err != nil || u == nil || p.userSchool(u, p.opts.School) != school {
		return false
	}
	r, err := p.store.ResolveEffective(school, u)
	if err != nil || !r.Decisions[access.CapTimetableUpstreamView].Effective {
		return false
	}
	for _, cap := range []access.Capability{access.CapPoolClassView, access.CapTimetableClassPooledView, access.CapTimetableClassBoostedRawView, access.CapTimetableTeacherReconstructedView, access.CapTimetableRoomReconstructedView, access.CapTimetableSubjectReconstructedView, access.CapTimetableTeacherBoostedRawView, access.CapTimetableRoomBoostedRawView, access.CapTimetableSubjectBoostedRawView, access.CapEventsTechnikView, access.CapEventsTechnikManage} {
		if r.Decisions[cap].Effective {
			return false
		}
	}
	return true
}

func (p *Proxy) proxyLocalAllowed(school, user string, cap access.Capability) bool {
	u, err := p.store.GetUser(user)
	return err == nil && u != nil && p.userSchool(u, p.opts.School) == school && p.effective(u, school, cap)
}
func (p *Proxy) writeOwnUpstreamTimetable(w http.ResponseWriter, r *http.Request, school string, u *store.User, id json.RawMessage, body []byte, pr timetableParams) {
	out, err := p.rewriteAuthForOwner(school, body, u)
	if err != nil {
		out = body
	}
	b, status, _, err := p.untis.RawIntern(school, p.untis.SchoolCookie(school), "getTimetable2017", out)
	if err != nil {
		p.writeJSONRPCError(w, id, "own upstream timetable unavailable", -8509)
		return
	}
	if p.ownsTimetable(u, pr) {
		b = p.decorateStudentEvents(b, school, u.Username, pr.StartDate, pr.EndDate)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
