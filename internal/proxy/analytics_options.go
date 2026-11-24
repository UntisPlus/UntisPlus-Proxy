package proxy

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"untis-proxy/internal/access"
)

type analyticsOption struct {
	Mode        string   `json:"mode"`
	ID          int64    `json:"id,omitempty"`
	ProfileID   string   `json:"profileId,omitempty"`
	Name        string   `json:"name"`
	DataSources []string `json:"dataSources"`
}

func (p *Proxy) handleAnalyticsOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	u := p.sessionUser(r)
	if u == nil {
		writeJSONError(w, http.StatusUnauthorized, "not logged in")
		return
	}
	school := r.URL.Query().Get("school")
	if school == "" {
		school = p.userSchool(u, p.opts.School)
	}
	if school != p.userSchool(u, p.opts.School) || !p.store.FeatureEnabled(school, "analytics") {
		p.forbidden(w)
		return
	}
	years, err := p.store.AnalyticsYears(school)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "school years unavailable")
		return
	}
	scope, err := p.store.AnalyticsAccess(school, u.Username)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "analytics scope unavailable")
		return
	}
	resolved, err := p.store.ResolveEffective(school, u)
	if err != nil {
		p.forbidden(w)
		return
	}
	options := []analyticsOption{}
	// Each of these lists already has its individual target approvals. Source
	// availability then depends on the account and school, not on the ID.
	// Resolve it once per mode so a full school roster does not repeat the
	// same teacher eligibility and shared-class lookups for every target.
	availableSources := map[[2]string]string{}
	add := func(option analyticsOption, request analyticsRequest, sources ...string) {
		if !resolved.Decisions[analyticsCapability(request.Mode)].Effective {
			return
		}
		option.Mode = request.Mode
		for _, source := range sources {
			request.DataSource = source
			key := [2]string{request.Mode, source}
			available, cached := availableSources[key]
			if !cached {
				if targets, err := p.analyticsTargets(u, school, request); err == nil && len(targets) > 0 {
					available = targets[0].DataSource
				}
				switch request.Mode {
				case "teacher", "room", "student":
					availableSources[key] = available
				}
			}
			if available != "" && !slices.Contains(option.DataSources, available) {
				option.DataSources = append(option.DataSources, available)
			}
		}
		if len(option.DataSources) > 0 {
			options = append(options, option)
		}
	}
	catalog, _ := p.store.ListElementsWithNames(school)
	name := func(kind, fallback string, id int64) string {
		if value := catalog[kind][id]; value != "" {
			return value
		}
		return fmt.Sprintf("%s %d", fallback, id)
	}
	add(analyticsOption{Name: "My timetable"}, analyticsRequest{Mode: "personal"}, "personal")
	for _, id := range uniqueAnalyticsIDs(scope.TeacherIDs) {
		add(analyticsOption{ID: id, Name: name("TEACHER", "Teacher", id)}, analyticsRequest{Mode: "teacher", TeacherID: id}, "saved-teacher", "reconstruction")
	}
	for _, id := range uniqueAnalyticsIDs(scope.RoomIDs) {
		add(analyticsOption{ID: id, Name: name("ROOM", "Room", id)}, analyticsRequest{Mode: "room", Room: strconv.FormatInt(id, 10)}, "", "reconstruction")
	}
	for _, id := range uniqueAnalyticsIDs(scope.StudentIDs) {
		option := analyticsOption{ID: id, Name: fmt.Sprintf("Student %d", id)}
		if resolved.Decisions[access.CapAnalyticsStudent].Effective {
			if student, err := p.store.UserByPersonID(id); err == nil && student != nil && student.School == school && student.DisplayName != "" {
				option.Name = student.DisplayName
			}
		}
		add(option, analyticsRequest{Mode: "student", StudentID: id}, "saved-teacher")
	}
	for _, id := range uniqueAnalyticsIDs(append(slices.Clone(scope.ClassIDs), u.ClassID)) {
		if id <= 0 {
			continue
		}
		add(analyticsOption{ID: id, Name: name("CLASS", "Class", id)}, analyticsRequest{Mode: "class", ClassID: id}, "personal", "shared-class", "saved-teacher")
	}
	for _, profile := range scope.Profiles {
		if len(profile.Segments) == 0 {
			continue
		}
		segment := profile.Segments[0]
		add(analyticsOption{ProfileID: profile.ID, Name: profile.ID},
			analyticsRequest{Mode: "class-history", ProfileID: profile.ID, From: segment.From, To: segment.To}, "saved-teacher")
	}
	p.writeJSON(w, map[string]any{"school": school, "schoolYears": years, "targets": options})
}

func uniqueAnalyticsIDs(ids []int64) []int64 {
	sorted := slices.Clone(ids)
	slices.Sort(sorted)
	return slices.Compact(sorted)
}
