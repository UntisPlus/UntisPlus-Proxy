package proxy

import (
	"testing"
	"untis-proxy/internal/access"
	"untis-proxy/internal/store"
)

func TestHiddenPoolClassCannotBeReadFromWarmCacheByAnotherStudent(t *testing.T) {
	f := &fakeUpstream{}
	f.setTimetable(t, []map[string]any{reconPeriod(10, "2026-09-21", "08:00", "08:45", 5009, 169, 7)})
	p, st := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st, "owner", 7, 5000)
	seedClassOwner(t, st, "viewer", 8, 6000)
	// Warm data before opting out to exercise revocation, not just cold requests.
	own := internReq(t, p, "getTimetable2017", "owner", internBody("getTimetable2017", ttParam(5000, "CLASS", "2026-09-21", "2026-09-27", "owner")))
	if errCode(t, own) != 0 {
		t.Fatalf("own timetable unavailable: %s", own.Body.String())
	}
	if err := st.SetUserCapGrant("testschool", "owner", string(access.CapPoolTimetableShare), -1); err != nil {
		t.Fatal(err)
	}
	other := internReq(t, p, "getTimetable2017", "viewer", internBody("getTimetable2017", ttParam(5000, "CLASS", "2026-09-21", "2026-09-27", "viewer")))
	if errCode(t, other) != -8509 {
		t.Fatalf("hidden class leaked: %s", other.Body.String())
	}
	own = internReq(t, p, "getTimetable2017", "owner", internBody("getTimetable2017", ttParam(5000, "CLASS", "2026-09-21", "2026-09-27", "owner")))
	if errCode(t, own) != 0 {
		t.Fatal("pool opt-out blocked own class")
	}
	if err := st.SetUserCapGrant("testschool", "viewer", string(access.CapPoolTimetableShare), -1); err != nil {
		t.Fatal(err)
	}
	periods, err := p.elementPeriods("testschool", "TEACHER", 5009, "2026-09-21", "2026-09-27")
	if err != nil || len(periods) != 0 {
		t.Fatalf("hidden class remained in reconstruction: %v %v", periods, err)
	}
}
func TestGuardianOnlyReadsItsLinkedChildThroughOwnAccount(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	if err := st.UpsertUser(&store.User{Username: "guardian", School: "testschool", PersonID: 1, PersonType: 5, ClassID: -1, Method: "key", Password: "fake-test-key"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveAccountLinks("testschool", "guardian", 1, []byte(`[{"id":10,"type":5},{"id":20,"type":5},{"id":30,"type":5}]`)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{10, 20, 30} {
		rec := internReq(t, p, "getTimetable2017", "guardian", internBody("getTimetable2017", ttParam(id, "STUDENT", "2026-09-21", "2026-09-27", "guardian")))
		if errCode(t, rec) != 0 {
			t.Fatalf("linked child %d denied: %s", id, rec.Body.String())
		}
	}
	beforeUnauth := len(f.Forwards())
	unauth := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017", ttParam(10, "STUDENT", "2026-09-21", "2026-09-27", "guardian")))
	if errCode(t, unauth) != -8520 || len(f.Forwards()) != beforeUnauth {
		t.Fatal("unverified guardian username reached upstream using stored credentials")
	}
	before := len(f.Forwards())
	rec := internReq(t, p, "getTimetable2017", "guardian", internBody("getTimetable2017", ttParam(999, "STUDENT", "2026-09-21", "2026-09-27", "guardian")))
	if errCode(t, rec) != -8509 || len(f.Forwards()) != before {
		t.Fatal("unlinked child was forwarded")
	}
}
