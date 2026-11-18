package proxy

import (
	"encoding/json"
	"log"
	"sort"
	"time"

	"untis-proxy/internal/store"
)

const anonProfileTTL = 24 * time.Hour

// RefreshSchoolAnonProfile probes upstream anonymous login for a school and
// stores a detected/cached anonymous profile. It is best-effort and bounded to
// the upstream login request; callers should run it out of request hot paths
// unless explicitly requested (e.g. admin refresh).
func (p *Proxy) RefreshSchoolAnonProfile(school string) (*store.SchoolAnonProfile, error) {
	if school == "" {
		school = p.opts.School
	}
	prevPayload, prevProf, _ := p.store.GetSchoolAnonProfilePayload(school)
	prof := &store.SchoolAnonProfile{
		School:     school,
		DetectedAt: time.Now(),
		Source:     "detect",
	}

	cookie, body, err := p.untis.KeyLogin(school, "#anonymous#", "", time.Now().UnixMilli(), nil)
	if err != nil {
		// Transient probe failure: do not clobber an explicit admin override, and do
		// not treat a failed probe as a definitive successful detection.
		if prevProf != nil && prevProf.Source == "override" {
			return prevProf, nil
		}
		if cur, _ := p.store.GetSchoolAnonProfile(school); cur != nil && cur.Source == "override" {
			return cur, nil
		}
		prof.LoginOK = false
		prof.Source = "probe_error"
		prof.ExpiresAt = time.Now().Add(10 * time.Minute)
		payload := store.AnonProfilePayload{Capabilities: map[string]bool{}}
		// Preserve last-known-good detected hints on transient probe failure.
		if cur, _ := p.store.GetSchoolAnonProfile(school); cur != nil && cur.Source != "override" {
			if curPayload, _, perr := p.store.GetSchoolAnonProfilePayload(school); perr == nil {
				payload = curPayload
			}
		} else if prevProf != nil && prevProf.Source != "override" {
			payload = prevPayload
		}
		if payload.Capabilities == nil {
			payload.Capabilities = map[string]bool{}
		}
		payload.Notes = "anonymous login probe failed: " + err.Error()
		prof.ProfileJSON = store.EncodeAnonProfilePayload(payload)
		// Recheck override/newer state immediately before write to avoid clobbering
		// an admin override or a fresher profile written concurrently.
		if cur, _ := p.store.GetSchoolAnonProfile(school); cur != nil {
			if cur.Source == "override" {
				return cur, nil
			}
			if cur.DetectedAt.After(prof.DetectedAt) {
				return cur, nil
			}
		}
		if serr := p.store.UpsertSchoolAnonProfile(prof); serr != nil {
			return nil, serr
		}
		log.Printf("[anon-profile] %s: login probe failed: %v", school, err)
		return prof, nil
	}
	if cookie != "" {
		go p.untis.Logout(school, cookie)
	}

	payload := detectAnonProfilePayload(body)
	prof.LoginOK = true
	prof.ExpiresAt = time.Now().Add(anonProfileTTL)
	prof.ProfileJSON = store.EncodeAnonProfilePayload(payload)
	// Admin overrides are authoritative until explicitly cleared/replaced.
	if cur, _ := p.store.GetSchoolAnonProfile(school); cur != nil {
		if cur.Source == "override" {
			return cur, nil
		}
		if cur.DetectedAt.After(prof.DetectedAt) {
			return cur, nil
		}
	}
	if err := p.store.UpsertSchoolAnonProfile(prof); err != nil {
		return nil, err
	}
	log.Printf("[anon-profile] %s: login_ok=true classes=%d", school, len(payload.ClassesDisplayable))
	return prof, nil
}

func detectAnonProfilePayload(loginBody []byte) store.AnonProfilePayload {
	payload := store.AnonProfilePayload{Capabilities: map[string]bool{}}
	var resp struct {
		Result struct {
			MasterData struct {
				Klassen []struct {
					ID          int64 `json:"id"`
					Displayable bool  `json:"displayable"`
				} `json:"klassen"`
			} `json:"masterData"`
		} `json:"result"`
	}
	if err := json.Unmarshal(loginBody, &resp); err != nil {
		payload.Notes = "failed to parse anonymous login masterData"
		// Successful anonymous login but unreadable exposure details: keep capability
		// hints unset so legacy policy fallback can apply conservatively.
		return payload
	}
	seen := map[int64]bool{}
	for _, k := range resp.Result.MasterData.Klassen {
		if k.ID > 0 && k.Displayable {
			seen[k.ID] = true
		}
	}
	if len(seen) > 0 {
		payload.ClassesDisplayable = make([]int64, 0, len(seen))
		for id := range seen {
			payload.ClassesDisplayable = append(payload.ClassesDisplayable, id)
		}
		sort.Slice(payload.ClassesDisplayable, func(i, j int) bool { return payload.ClassesDisplayable[i] < payload.ClassesDisplayable[j] })
	}
	// Bounded probe (anonymous login masterData): explicitly record detected class
	// exposure for this school so profile-aware checks do not rely on global legacy
	// toggles when detection is definitive.
	payload.Capabilities["classes"] = len(payload.ClassesDisplayable) > 0
	return payload
}

// EnsureSchoolAnonProfile returns a cached profile when fresh enough, otherwise
// triggers a best-effort background refresh and returns current/empty state.
func (p *Proxy) EnsureSchoolAnonProfile(school string) (*store.SchoolAnonProfile, bool) {
	if school == "" {
		school = p.opts.School
	}
	prof, err := p.store.GetSchoolAnonProfile(school)
	if err != nil {
		return nil, false
	}
	if prof != nil && (prof.Source == "override" || prof.ExpiresAt.IsZero() || time.Now().Before(prof.ExpiresAt)) {
		return prof, false
	}
	// Best-effort async refresh; don't block callers.
	go func(s string) {
		if _, err := p.RefreshSchoolAnonProfile(s); err != nil {
			log.Printf("[anon-profile] background refresh %s: %v", s, err)
		}
	}(school)
	return prof, prof != nil
}
