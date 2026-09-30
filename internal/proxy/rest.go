package proxy

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"untis-proxy/internal/store"
	"untis-proxy/internal/untis"
)

// escalatedREST forwards a REST request for a user with the real API, retrying
// once with a fresh session when the cached upstream session expired server-side.
func (p *Proxy) escalatedREST(school string, u *store.User, path, rawQuery string) ([]byte, int, error) {
	cookie, err := p.untis.Session(school, u.Username, u.Password, u.Method)
	if err != nil {
		return nil, 0, err
	}
	b, status, err := p.untis.RESTGet(school, cookie, path, rawQuery)
	if err != nil {
		return nil, status, err
	}
	if untis.IsAuthFailure(b) {
		if ck, err := p.untis.FreshSession(school, u.Username, u.Password, u.Method); err == nil {
			if b2, status2, err2 := p.untis.RESTGet(school, ck, path, rawQuery); err2 == nil && !untis.IsAuthFailure(b2) {
				return b2, status2, nil
			}
		}
	}
	return b, status, nil
}

func (p *Proxy) handleREST(w http.ResponseWriter, r *http.Request) {
	school := p.opts.School
	if s := r.URL.Query().Get("school"); s != "" {
		school = s
	}
	switch r.URL.Path {
	case "/WebUntis/api/public/timetable/weekly/data":
		p.restWeeklyTimetable(w, r, school)
	default:
		user := p.sessionUser(r)
		if user == nil {
			if tok := r.Header.Get("Authorization"); strings.HasPrefix(tok, "Bearer ") {
				// BetterUntis self-authenticates REST calls via a Bearer token
				// (from getAuthToken); forward as-is so the real server serves
				// only that user's own data. We have no session identity here,
				// so write verbs are denied by default; absence GETs (own data)
				// are allowed.
				if r.Method == "POST" || r.Method == "PUT" || r.Method == "DELETE" {
					p.forbidden(w)
					return
				}
				b, status, err := p.untis.RESTGetToken(school, tok, r.URL.Path, r.URL.RawQuery)
				if err != nil {
					p.forbidden(w)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write(b)
				return
			}
			p.forbidden(w)
			return
		}
		// Session path: write verbs are gated by the editor flag. Reading
		// your own absences (GET) is always allowed.
		if r.Method == "POST" || r.Method == "PUT" || r.Method == "DELETE" {
			if !p.isEditor(user.Username) {
				p.forbidden(w)
				return
			}
		}
		b, status, err := p.escalatedREST(school, user, r.URL.Path, r.URL.RawQuery)
		if err != nil {
			p.forbidden(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(b)
	}
}

func (p *Proxy) restWeeklyTimetable(w http.ResponseWriter, r *http.Request, school string) {
	user := p.sessionUser(r)
	if user == nil {
		p.forbidden(w)
		return
	}

	// boost: serve ALL weekly elements (class/teacher/room/subject AND the user's
	// own personal student timetable, elementType 5) raw from the saved teacher
	// accounts so boosted users get teacher-grade future horizon. If no teacher
	// account is saved, fall back to the pool/recon handling below.
	if p.isBoosted(user.Username) {
		if p.serveRESTRawFromBoostedSource(w, r, school) {
			return
		}
	}

	// Teacher accounts (non-students) without the boosted flag behave like the
	// stock WebUntis API: every weekly element is forwarded raw through their
	// own session, never intercepted by the pool or the recon gate.
	if user.PersonType != 5 {
		b, status, err := p.escalatedREST(school, user, r.URL.Path, r.URL.RawQuery)
		if err != nil {
			p.forbidden(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(b)
		return
	}

	q := r.URL.Query()
	elType := q.Get("elementType")
	elID, _ := strconv.ParseInt(q.Get("elementId"), 10, 64)

	switch elType {
	case "2", "3", "4": // teacher, subject, room: reconstructed from pooled data
		reconType := map[string]string{"2": "TEACHER", "3": "SUBJECT", "4": "ROOM"}[elType]
		allowed, _ := p.store.ReconAccess(user.Username, reconType)
		if !allowed {
			p.forbidden(w)
			return
		}
		p.restWeeklyElement(w, r, school, elType, elID, q.Get("date"))
		return
	case "1": // class
		owner, err := p.restPoolOwner(school, elID)
		if err != nil {
			p.forbidden(w)
			return
		}
		b, status, err := p.escalatedREST(school, owner, r.URL.Path, r.URL.RawQuery)
		if err != nil {
			p.forbidden(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(b)
		return
	default:
		b, status, err := p.escalatedREST(school, user, r.URL.Path, r.URL.RawQuery)
		if err != nil {
			p.forbidden(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(b)
	}
}

// serveRESTRawFromBoostedSource serves the weekly timetable REST endpoint raw
// from the saved teacher accounts when the requester is boosted.
func (p *Proxy) serveRESTRawFromBoostedSource(w http.ResponseWriter, r *http.Request, school string) bool {
	sources, err := p.store.BoostedSourceAccounts(school)
	if err != nil || len(sources) == 0 {
		return false
	}
	owner := sources[0]
	b, status, err := p.escalatedREST(school, owner, r.URL.Path, r.URL.RawQuery)
	if err != nil {
		p.forbidden(w)
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
	return true
}

func (p *Proxy) restPoolOwner(school string, classID int64) (*store.User, error) {
	ok, err := p.store.PoolContains(school, classID)
	if err != nil || !ok {
		return nil, fmt.Errorf("class not pooled")
	}
	owner, err := p.store.OwnerForClass(school, classID)
	if err != nil || owner == nil {
		return nil, fmt.Errorf("no owner")
	}
	return owner, nil
}

// restWeeklyElement answers the weekly-data REST endpoint for a
// teacher/subject/room by reconstructing the week from pooled class data.
func (p *Proxy) restWeeklyElement(w http.ResponseWriter, r *http.Request, school, elType string, elID int64, date string) {
	typeName := map[string]string{"2": "TEACHER", "3": "SUBJECT", "4": "ROOM"}[elType]
	if !p.stateFor(school).recon.has(typeName, elID) {
		p.forbidden(w)
		return
	}
	start, end, err := weekRange(date)
	if err != nil {
		p.forbidden(w)
		return
	}
	periods, err := p.elementPeriods(school, typeName, elID, start, end)
	if err != nil {
		p.forbidden(w)
		return
	}
	md := p.masterData(school)

	weekly := make([]map[string]any, 0, len(periods))
	desc := map[int64]struct {
		typ  string
		seen bool
	}{}
	var descOrder []int64
	for _, pd := range periods {
		wp, elems, err := weeklyPeriod(pd)
		if err != nil {
			continue
		}
		weekly = append(weekly, wp)
		for _, el := range elems {
			if _, ok := desc[el]; !ok {
				desc[el] = struct {
					typ  string
					seen bool
				}{}
				descOrder = append(descOrder, el)
			}
		}
	}
	// resolve each referenced element's type from the period element lists
	for _, pd := range periods {
		for _, el := range periodElementIDs(pd) {
			if _, ok := desc[el.ID]; ok {
				d := desc[el.ID]
				d.typ = el.Type
				d.seen = true
				desc[el.ID] = d
			}
		}
	}
	elementList := make([]map[string]any, 0, len(descOrder))
	for _, id := range descOrder {
		d := desc[id]
		el := map[string]any{"id": id, "type": elTypeInt(d.typ), "canViewTimetable": true}
		if name := elementName(md, d.typ, id); name != "" {
			el["name"] = name
		}
		if d.typ == "SUBJECT" {
			if long := elementLongName(md, d.typ, id); long != "" {
				el["longName"] = long
				el["displayname"] = long
			}
		}
		elementList = append(elementList, el)
	}

	out := map[string]any{
		"data": map[string]any{
			"result": map[string]any{
				"data": map[string]any{
					"noDetails":      false,
					"elementIds":     []int64{elID},
					"elementPeriods": map[string]any{fmt.Sprintf("%d", elID): weekly},
				},
			},
		},
	}
	userName := ""
	if u := p.sessionUser(r); u != nil {
		userName = u.Username
	}
	if md := p.rewrittenMasterData(school, userName); md != nil {
		if ts, ok := md["timeStamp"].(int64); ok {
			out["data"].(map[string]any)["result"].(map[string]any)["lastImportTimestamp"] = ts
		}
	}
	if len(elementList) > 0 {
		out["data"].(map[string]any)["result"].(map[string]any)["data"].(map[string]any)["elements"] = elementList
	}
	p.writeJSON(w, out)
}

func elTypeInt(t string) int {
	switch t {
	case "CLASS":
		return 1
	case "TEACHER":
		return 2
	case "SUBJECT":
		return 3
	case "ROOM":
		return 4
	}
	return 0
}

func elementName(md *masterDataCache, t string, id int64) string {
	if md == nil {
		return ""
	}
	switch t {
	case "TEACHER":
		return md.teachers[id]
	case "ROOM":
		return md.rooms[id]
	case "SUBJECT":
		return md.subjects[id]
	case "CLASS":
		return md.klassen[id]
	}
	return ""
}

func elementLongName(md *masterDataCache, t string, id int64) string {
	if md == nil || t != "SUBJECT" {
		return ""
	}
	return md.subjects[id]
}
