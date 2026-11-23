package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"untis-proxy/internal/session"
	"untis-proxy/internal/store"
	"untis-proxy/internal/untis"
)

func TestSavedTeacherTimetableRefusesForgedIdentity(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	seedTeacherSource(t, st, "teacher", 5009)
	rec := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017", ttParam(5009, "TEACHER", "2026-09-21", "2026-09-27", "teacher")))
	if errCode(t, rec) != -8520 || len(f.Forwards()) != 0 {
		t.Fatal("forged teacher identity used saved credentials")
	}
}
func TestSharedTimetableRefusesForgedIdentityAndOtherSession(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	seedClassOwner(t, st, "owner", 7, 5000)
	seedClassOwner(t, st, "victim", 8, 6000)
	seedClassOwner(t, st, "other", 9, 7000)
	for _, session := range []string{"", "other"} {
		rec := internReq(t, p, "getTimetable2017", session, internBody("getTimetable2017", ttParam(5000, "CLASS", "2026-09-21", "2026-09-27", "victim")))
		if errCode(t, rec) != -8520 {
			t.Fatalf("session %q impersonated victim: %s", session, rec.Body.String())
		}
	}
	if len(f.Forwards()) != 0 {
		t.Fatal("unverified request reached upstream")
	}
}
func TestClassScopedReadRefusesForgedEditor(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	seedTeacherSource(t, st, "teacher", 5009)
	seedKeyUser(t, st)
	if err := st.SetPerm("owen", store.FeatureEditor, true); err != nil {
		t.Fatal(err)
	}
	rec := internReq(t, p, "getPeriodData2017", "", internBody("getPeriodData2017", map[string]any{"auth": authMap("owen"), "ttId": 1}))
	if errCode(t, rec) != -8520 || len(f.Forwards()) != 0 {
		t.Fatal("forged editor used teacher-source credentials")
	}
}
func TestInternRejectsMethodDisagreement(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	seedKeyUser(t, st)
	rec := internReq(t, p, "getPeriodData2017", "owen", internBody("deleteAbsence2017", map[string]any{"auth": authMap("owen"), "absenceId": 1}))
	if errCode(t, rec) != -32600 || len(f.Forwards()) != 0 {
		t.Fatal("method disagreement was not rejected")
	}
}
func TestOTPDoesNotTrustHistoricalClientClock(t *testing.T) {
	p, st := newTestProxy(t)
	secret := "JBSWY3DPEHPK3PXP"
	st.UpsertSecret("bob", secret)
	now := time.Now()
	old := now.Add(-24 * time.Hour)
	otp := untis.TOTPAt(secret, old)
	for otp == untis.TOTPAt(secret, now) || otp == untis.TOTPAt(secret, now.Add(-30*time.Second)) || otp == untis.TOTPAt(secret, now.Add(30*time.Second)) {
		old = old.Add(-time.Hour)
		otp = untis.TOTPAt(secret, old)
	}
	if p.verifyUserOTP("bob", otp, old.UnixMilli()) {
		t.Fatal("historical code accepted using caller-controlled timestamp")
	}
	if !p.verifyUserOTP("bob", untis.TOTPAt(secret, now), now.UnixMilli()) {
		t.Fatal("current OTP refused")
	}
}
func TestTimetableVerifiedOTPStillWorksWithoutCookie(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	seedTeacherSource(t, st, "teacher", 5009)
	st.UpsertSecret("teacher", boostSourceSecret)
	params := ttParam(5009, "TEACHER", "2026-09-21", "2026-09-27", "")
	params["auth"] = map[string]any{"user": "teacher", "otp": untis.TOTP(boostSourceSecret), "clientTime": time.Now().UnixMilli()}
	rec := internReq(t, p, "getTimetable2017", "", internBody("getTimetable2017", params))
	if errCode(t, rec) != 0 || len(f.Forwards()) != 1 {
		t.Fatalf("verified mobile auth refused: %s", rec.Body.String())
	}
}
func TestPersonalExtensionDoesNotGrantRawTeacherREST(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	seedTeacherSource(t, st, "teacher", 5009)
	seedSessionUser(t, st, "student", 5, 7, 5000)
	st.SetUserCapGrant("testschool", "student", "timetable.student.personal.boosted_raw.view", 1)
	req := httptest.NewRequest(http.MethodGet, "/WebUntis/api/public/timetable/weekly/data?elementType=2&elementId=5009&date=2026-09-21&school=testschool", nil)
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: p.sessions.New("student", 5000).ID})
	rec := httptest.NewRecorder()
	p.handleREST(rec, req)
	if len(f.Rests()) != 0 {
		t.Fatal("personal extension escalated a teacher timetable request")
	}
}

func TestInternMalformedRequestsNeverReachUpstream(t *testing.T) {
	f := &fakeUpstream{}
	p, _ := newFakeProxyUpstream(t, f)
	for _, body := range []string{`{`, `[]`, `{"method":1}`, `{"method":"getTimetable2017","params":"invalid"}`} {
		rec := internReq(t, p, "getTimetable2017", "", body)
		if errCode(t, rec) == 0 {
			t.Fatalf("malformed request accepted: %s", body)
		}
	}
	if len(f.Forwards()) != 0 {
		t.Fatal("malformed request forwarded")
	}
}

func TestTimetableIdentityBoundaryMatrix(t *testing.T) {
	for _, target := range []struct {
		kind string
		id   int64
	}{{"TEACHER", 5009}, {"ROOM", 169}, {"CLASS", 5000}, {"CLASS", technikClassID}} {
		for _, mode := range []string{"forged", "other-session", "wrong-school", "expired-session"} {
			t.Run(fmt.Sprintf("%s-%d/%s", target.kind, target.id, mode), func(t *testing.T) {
				f := &fakeUpstream{}
				p, st := newFakeProxyUpstream(t, f)
				seedTeacherSource(t, st, "teacher", 5009)
				seedSessionUser(t, st, "other", 5, 7, 5000)
				params := ttParam(target.id, target.kind, "2026-09-21", "2026-09-27", "teacher")
				req := httptest.NewRequest(http.MethodPost, "/WebUntis/jsonrpc_intern.do?m=getTimetable2017&school=testschool", strings.NewReader(internBody("getTimetable2017", params)))
				switch mode {
				case "other-session":
					req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: p.sessions.New("other", 5000).ID})
				case "wrong-school":
					req.URL.RawQuery = "m=getTimetable2017&school=another-school"
					req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: p.sessions.New("teacher", 0).ID})
				case "expired-session":
					p.sessions = session.NewManager(-time.Second)
					req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: p.sessions.New("teacher", 0).ID})
				}
				rec := httptest.NewRecorder()
				p.handleJSONRPCIntern(rec, req)
				if errCode(t, rec) != -8520 || len(f.Forwards()) != 0 {
					t.Fatalf("unverified identity crossed boundary: %s", rec.Body.String())
				}
			})
		}
	}
}

func TestEventAbsenceRejectsForgedTechnikIdentity(t *testing.T) {
	p, st := eventProxy(t)
	if err := st.SetPerm("dee", store.FeatureTechniker, true); err != nil {
		t.Fatal(err)
	}
	ev := seedEvent(t, st, "dee", testEvent())
	periodID := customEventIDSpace - ev.ID
	for _, method := range []string{"getPeriodData2017", "createImmediateAbsence2017", "deleteAbsence2017", "submitAbsencesChecked2017"} {
		rec := internReq(t, p, method, "", internBody(method, map[string]any{"auth": authMap("dee"), "ttIds": []int64{periodID}, "periodId": periodID, "studentId": 10, "absenceId": eventAbsenceIDSpace - 1}))
		if errCode(t, rec) != -8520 {
			t.Fatalf("%s accepted forged identity: %s", method, rec.Body.String())
		}
	}
}

func TestRESTSessionCannotCrossSchoolBoundary(t *testing.T) {
	f := &fakeUpstream{}
	p, st := newFakeProxyUpstream(t, f)
	seedTeacherSource(t, st, "teacher", 5009)
	for _, path := range []string{"/WebUntis/api/public/timetable/weekly/data?elementType=2&elementId=5009&school=another-school", "/WebUntis/api/other?school=another-school"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: p.sessions.New("teacher", 0).ID})
		rec := httptest.NewRecorder()
		p.handleREST(rec, req)
		if rec.Code != http.StatusForbidden || len(f.Rests()) != 0 {
			t.Fatalf("cross-school request accepted: %d", rec.Code)
		}
	}
}

func TestFailedEnrollmentCannotReplaceSavedCredentials(t *testing.T) {
	for _, reply := range []string{
		`{"error":{"code":-8998,"message":"invalid password"}}`,
		`{"result":""}`, `{"result":"invalid-secret"}`, `not-json`,
		`{"result":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","error":{"code":-8998}}`,
	} {
		t.Run(reply, func(t *testing.T) {
			f := &fakeUpstream{internReply: reply}
			p, st := newFakeProxyUpstream(t, f)
			const original = "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
			if err := st.UpsertCredential("victim", original, "original-password"); err != nil {
				t.Fatal(err)
			}
			p.setReplayKey("victim", original)
			for _, password := range []string{"wrong-password", "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"} {
				rec := internReq(t, p, "getAppSharedSecret", "", internBody("getAppSharedSecret", map[string]any{"userName": "victim", "password": password}))
				if rec.Code != http.StatusOK {
					t.Fatalf("upstream reply not preserved: %d", rec.Code)
				}
				key, pw, err := st.GetCredential("victim")
				if err != nil || key != original || pw != "original-password" || p.replayKey("victim") != original {
					t.Fatal("failed enrollment poisoned credentials")
				}
			}
		})
	}
}

func TestLocalOTPRequestsShareLoginRateLimit(t *testing.T) {
	p, st := newTestProxy(t)
	seedSessionUser(t, st, "limited", 5, 7, 5000)
	const secret = "JBSWY3DPEHPK3PXP"
	if err := st.UpsertSecret("limited", secret); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	p.loginAccountLimit = newAttemptLimiter(2, time.Minute)
	for i := 0; i < 2; i++ {
		if p.authenticatedUpstreamRequester(req, "testschool", "limited", "not-a-code", 0) != nil {
			t.Fatal("bad OTP accepted")
		}
	}
	if p.authenticatedUpstreamRequester(req, "testschool", "limited", untis.TOTP(secret), 0) != nil {
		t.Fatal("locked local OTP oracle accepted another attempt")
	}
	// Existing authenticated sessions keep working during a guessing lockout.
	req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: p.sessions.New("limited", 5000).ID})
	if p.authenticatedUpstreamRequester(req, "testschool", "limited", "", 0) == nil {
		t.Fatal("lockout invalidated established session")
	}
}

func TestEnrollmentSharesCredentialGuessingLimit(t *testing.T) {
	f := &fakeUpstream{internReply: `{"error":{"code":-8998}}`}
	p, _ := newFakeProxyUpstream(t, f)
	p.loginAccountLimit = newAttemptLimiter(2, time.Minute)
	for i := 0; i < 3; i++ {
		rec := internReq(t, p, "getAppSharedSecret", "", internBody("getAppSharedSecret", map[string]any{"userName": "victim", "password": "wrong"}))
		if i == 2 && (errCode(t, rec) != -8504 || rec.Header().Get("Retry-After") == "") {
			t.Fatal("enrollment bypassed login throttle")
		}
	}
	if len(f.Forwards()) != 2 {
		t.Fatalf("upstream received %d attempts, want 2", len(f.Forwards()))
	}
}

func TestKeyLoginReplacesUntrustedClientTimestamp(t *testing.T) {
	f := &fakeUntis{}
	p, _, _ := newFakeProxy(t, f)
	old := time.Now().Add(-24 * time.Hour).UnixMilli()
	body := internBody("getUserData2017", map[string]any{"auth": map[string]any{"user": "test-user", "otp": "123456", "clientTime": old}})
	before := time.Now().Add(-time.Second).UnixMilli()
	rec := internReq(t, p, "getUserData2017", "", body)
	if errCode(t, rec) != 0 {
		t.Fatal("fake login failed")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	timestamp, ok := f.loginAuth["clientTime"].(float64)
	if !ok || int64(timestamp) < before || int64(timestamp) > time.Now().UnixMilli() {
		t.Fatal("upstream login used untrusted historical clock")
	}
}

// Both app key login and public password login must preserve explicit admin provisioning.
func TestKeyLoginCannotClaimAdministrator(t *testing.T) {
	for _, provisioned := range []bool{false, true} {
		t.Run(fmt.Sprint("provisioned=", provisioned), func(t *testing.T) {
			p, st, _ := newFakeProxy(t, &fakeUntis{})
			if err := st.SetAdmin("bob", false); err != nil {
				t.Fatal(err)
			}
			if err := st.SetSetupCompleted(false); err != nil {
				t.Fatal(err)
			}
			if provisioned {
				if err := st.SetAdmin("test-user", true); err != nil {
					t.Fatal(err)
				}
			}
			rec := internReq(t, p, "getUserData2017", "", internBody("getUserData2017", map[string]any{"auth": map[string]any{"user": "test-user", "otp": "123456"}}))
			if errCode(t, rec) != 0 {
				t.Fatalf("login failed: %s", rec.Body.String())
			}
			isAdmin, err := st.IsAdmin("test-user")
			if err != nil || isAdmin != provisioned {
				t.Fatalf("login changed administrator provisioning: %v %v", isAdmin, err)
			}
		})
	}
}
