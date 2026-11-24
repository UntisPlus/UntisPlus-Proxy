package proxy

import (
	"fmt"
	"slices"
	"strconv"

	"untis-proxy/internal/analytics"
	"untis-proxy/internal/store"
)

func analyticsSourceSupported(mode, source string) bool {
	if source == "" {
		return true
	}
	switch mode {
	case "personal":
		return source == "personal"
	case "teacher":
		return source == "saved-teacher" || source == "reconstruction"
	case "room":
		return source == "own-teacher" || source == "saved-teacher" || source == "reconstruction"
	case "class":
		return source == "shared-class" || source == "personal" || source == "saved-teacher"
	case "student", "class-history":
		return source == "saved-teacher"
	default:
		return false
	}
}

func (p *Proxy) analyticsApprovedTarget(u *store.User, school string, q analyticsRequest, scope store.AnalyticsAccess) ([]analyticsTarget, error) {
	target := analyticsTarget{From: q.From, To: q.To}
	if q.Mode == "student" {
		if !slices.Contains(scope.StudentIDs, q.StudentID) {
			return nil, fmt.Errorf("student not approved for this account")
		}
		target.Kind, target.ID = "STUDENT", q.StudentID
	} else {
		own := q.ClassID == u.ClassID && p.canViewClassAs(u, school, q.ClassID, true)
		approved := slices.Contains(scope.ClassIDs, q.ClassID)
		if !own && !approved {
			return nil, fmt.Errorf("class not approved for this account")
		}
		target.Kind, target.ID = "CLASS", q.ClassID
		if own && u.Password != "" && (q.DataSource == "" || q.DataSource == "personal") {
			target.Owner, target.DataSource = u, "personal"
			return []analyticsTarget{target}, nil
		}
		if (q.DataSource == "" || q.DataSource == "shared-class") && (own || p.canViewClassAs(u, school, q.ClassID, false)) {
			if owner, err := p.store.OwnerForClass(school, q.ClassID); err == nil && owner != nil && p.store.UserAdmissionAllowed(school, owner) {
				target.Owner, target.DataSource = owner, "shared-class"
				return []analyticsTarget{target}, nil
			}
		}
		if !approved || (q.DataSource != "" && q.DataSource != "saved-teacher") {
			return nil, fmt.Errorf("selected class source is unavailable")
		}
	}
	target.Owner = p.analyticsSharedTeacher(school)
	if target.Owner == nil {
		return nil, fmt.Errorf("no eligible saved teacher source for this school")
	}
	target.DataSource = "saved-teacher"
	return []analyticsTarget{target}, nil
}

func (p *Proxy) analyticsSharedTeacher(school string) *store.User {
	sources, err := p.store.BoostedSourceAccounts(school)
	if err == nil {
		for _, teacher := range sources {
			if p.analyticsRoomSourceAllowed(school, teacher) && p.store.SharesTimetable(school, teacher) {
				return teacher
			}
		}
	}
	return nil
}

func (p *Proxy) analyticsReconTargets(u *store.User, school string, q analyticsRequest) ([]analyticsTarget, error) {
	scope, err := p.store.AnalyticsAccess(school, u.Username)
	if err != nil {
		return nil, fmt.Errorf("analytics scope unavailable")
	}
	kind, id := "TEACHER", q.TeacherID
	if q.Mode == "room" {
		kind = "ROOM"
		id, err = p.analyticsRoomID(school, q.Room)
		if err != nil {
			return nil, err
		}
	}
	if q.ProfileID != "" || !p.canViewElementRecon(u, school, kind) ||
		(kind == "TEACHER" && !slices.Contains(scope.TeacherIDs, id)) ||
		(kind == "ROOM" && !slices.Contains(scope.RoomIDs, id)) {
		return nil, fmt.Errorf("reconstruction is not approved for this target")
	}
	classes, err := p.store.Pool(school)
	if err != nil {
		return nil, fmt.Errorf("shared classes unavailable")
	}
	targets := []analyticsTarget{}
	for _, class := range classes {
		own := class.ID == u.ClassID && p.canViewClassAs(u, school, class.ID, true)
		if !own && !p.canViewClassAs(u, school, class.ID, false) {
			continue
		}
		owner, err := p.store.OwnerForClass(school, class.ID)
		if err == nil && owner != nil && p.store.UserAdmissionAllowed(school, owner) {
			targets = append(targets, analyticsTarget{Kind: "CLASS", ID: class.ID, From: q.From, To: q.To,
				Owner: owner, FilterKind: kind, FilterID: id, DataSource: "reconstruction"})
		}
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no accessible shared classes for reconstruction")
	}
	slices.SortFunc(targets, func(a, b analyticsTarget) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return targets, nil
}

func analyticsPeriodMatches(period map[string]any, lesson analytics.Lesson, kind string, id int64) bool {
	switch kind {
	case "TEACHER":
		want := strconv.FormatInt(id, 10)
		for _, teacher := range lesson.Teachers {
			if teacher.ID == want {
				return true
			}
		}
		for _, cover := range lesson.Covers {
			if cover.Teacher.ID == want || cover.Original.ID == want {
				return true
			}
		}
		return false
	case "ROOM":
		for _, key := range []string{"elements", "rooms", "ro"} {
			for _, entry := range analyticsEntries(period, key, kind) {
				if analyticsEntryID(entry, "id") == id || analyticsOriginalID(entry) == id {
					return true
				}
			}
		}
		return false
	default:
		return true
	}
}
