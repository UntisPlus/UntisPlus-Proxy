package store

import "testing"

func TestSetupStateFreshInstallClaimAndComplete(t *testing.T) {
	s := openTestStore(t)

	needed, err := s.SetupNeeded()
	if err != nil || !needed {
		t.Fatalf("fresh install SetupNeeded=%v err=%v, want true,nil", needed, err)
	}
	onboarding, err := s.SetupOnboarding()
	if err != nil || !onboarding {
		t.Fatalf("fresh install SetupOnboarding=%v err=%v, want true,nil", onboarding, err)
	}

	if err := s.SetAdmin("alice", true); err != nil {
		t.Fatalf("SetAdmin: %v", err)
	}
	if err := s.SetSetupFreshClaim(true); err != nil {
		t.Fatalf("SetSetupFreshClaim: %v", err)
	}

	needed, err = s.SetupNeeded()
	if err != nil || needed {
		t.Fatalf("after fresh claim SetupNeeded=%v err=%v, want false,nil", needed, err)
	}
	onboarding, err = s.SetupOnboarding()
	if err != nil || !onboarding {
		t.Fatalf("after fresh claim SetupOnboarding=%v err=%v, want true,nil", onboarding, err)
	}
	done, err := s.SetupCompleted()
	if err != nil || done {
		t.Fatalf("after fresh claim SetupCompleted=%v err=%v, want false,nil", done, err)
	}

	if err := s.SetSetupCompleted(true); err != nil {
		t.Fatalf("SetSetupCompleted: %v", err)
	}
	needed, _ = s.SetupNeeded()
	onboarding, _ = s.SetupOnboarding()
	done, _ = s.SetupCompleted()
	if needed || onboarding || !done {
		t.Fatalf("after complete needed=%v onboarding=%v done=%v, want false,false,true", needed, onboarding, done)
	}
}

func TestSetupStateNormalizesExistingUpgradedInstance(t *testing.T) {
	s := openTestStore(t)
	if err := s.SetAdmin("bob", true); err != nil {
		t.Fatalf("SetAdmin: %v", err)
	}

	needed, err := s.SetupNeeded()
	if err != nil || needed {
		t.Fatalf("upgraded instance SetupNeeded=%v err=%v, want false,nil", needed, err)
	}
	done, err := s.SetupCompleted()
	if err != nil || !done {
		t.Fatalf("upgraded instance SetupCompleted=%v err=%v, want true,nil after normalization", done, err)
	}
	onboarding, err := s.SetupOnboarding()
	if err != nil || onboarding {
		t.Fatalf("upgraded instance SetupOnboarding=%v err=%v, want false,nil", onboarding, err)
	}
	fresh, err := s.SetupFreshClaim()
	if err != nil || fresh {
		t.Fatalf("upgraded instance SetupFreshClaim=%v err=%v, want false,nil", fresh, err)
	}
}

func TestSetupStateResetReopensOnboardingForAdmin(t *testing.T) {
	s := openTestStore(t)
	if err := s.SetAdmin("bob", true); err != nil {
		t.Fatalf("SetAdmin: %v", err)
	}
	if _, err := s.SetupNeeded(); err != nil { // normalize upgraded state
		t.Fatalf("SetupNeeded normalize: %v", err)
	}
	if err := s.SetSetupCompleted(false); err != nil {
		t.Fatalf("SetSetupCompleted(false): %v", err)
	}
	if err := s.SetSetupFreshClaim(true); err != nil {
		t.Fatalf("SetSetupFreshClaim(true): %v", err)
	}
	onboarding, err := s.SetupOnboarding()
	if err != nil || !onboarding {
		t.Fatalf("after reset SetupOnboarding=%v err=%v, want true,nil", onboarding, err)
	}
	needed, err := s.SetupNeeded()
	if err != nil || needed {
		t.Fatalf("after reset with existing admin SetupNeeded=%v err=%v, want false,nil", needed, err)
	}
}
