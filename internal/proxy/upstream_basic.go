package proxy

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"

	"untis-proxy/internal/access"
	"untis-proxy/internal/store"
	"untis-proxy/internal/untis"
)

// A mobile client can send a fresh code on each request without carrying the
// proxy session or enrolling a replay key. Upstream can authenticate that
// request for the account's own timetable. This path returns upstream bytes
// directly and never serves pooled, cached or proxy-local data.
func (p *Proxy) forwardOwnTimetableWithLiveOTP(w http.ResponseWriter, r *http.Request, school string, id json.RawMessage, pr timetableParams) bool {
	if pr.Auth.User == "" || pr.ID <= 0 || normalizedOTP(pr.Auth.OTP) == "" ||
		(pr.Type != "STUDENT" && pr.Type != "CLASS" && pr.Type != "TEACHER") {
		return false
	}
	sessionUser := p.sessionUser(r)
	if sessionUser != nil && (pr.Type != "TEACHER" || !strings.EqualFold(sessionUser.Username, pr.Auth.User) || p.userSchool(sessionUser, p.opts.School) != school) {
		return false
	}
	u, err := p.store.GetUserInSchool(school, pr.Auth.User)
	if err != nil || u == nil || u.Password != "" || !p.store.UserAdmissionAllowed(school, u) {
		return false
	}
	ownTeacher := pr.Type == "TEACHER" && u.PersonID == pr.ID && access.DefaultRoleForPersonType(u.PersonType) == access.RoleTeacher
	if !ownTeacher && !p.ownsTimetable(u, pr) {
		return false
	}
	links, err := p.store.AccountLinks(school, u.Username)
	if (err != nil && err != sql.ErrNoRows) || links.Guardian {
		return false
	}
	key, password, err := p.store.SchoolCredential(school, u.Username)
	if err != nil || key != "" || password != "" {
		return false
	}
	capability := access.CapTimetableStudentPersonalUpstreamView
	if pr.Type == "CLASS" {
		capability = access.CapTimetableClassUpstreamView
	}
	allowed := p.effective(u, school, capability)
	if ownTeacher {
		// The existing stock teacher path allows an own read by default.
		// Honour explicit policy and server gates for the live-code path.
		if resolved, err := p.store.ResolveEffective(school, u); err == nil {
			decision := resolved.Decisions[access.CapTimetableUpstreamView]
			allowed = decision.Effective || (decision.ServerEnabled && decision.Source == access.SourceImplicit)
		} else {
			allowed = false
		}
	}
	if !allowed {
		p.writeJSONRPCError(w, id, "no right for timetable", -8509)
		return true
	}
	if p.loginRateLimited(w, r, school, u.Username) {
		p.writeJSONRPCError(w, id, "too many login attempts, try again later", -8504)
		return true
	}
	// The typed params carry the live code and supported timetable fields.
	// They cannot forward a reusable key supplied in an unrecognised field.
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "getTimetable2017", "params": []timetableParams{pr}})
	raw, status, _, err := p.untis.RawIntern(school, p.untis.SchoolCookie(school), "getTimetable2017", body)
	if err != nil {
		p.writeJSONRPCError(w, id, "own upstream timetable unavailable", -8509)
		return true
	}
	if status == http.StatusUnauthorized || untis.IsAuthFailure(raw) {
		p.recordLoginFailure(r, school, u.Username)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
	return true
}

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
