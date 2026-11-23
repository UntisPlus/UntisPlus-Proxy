package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const releaseEndpoint = "https://api.github.com/repos/UntisPlus/UntisPlus-Proxy/releases/latest"

type updateStatus struct {
	CurrentVersion      string     `json:"currentVersion"`
	LatestRelease       string     `json:"latestRelease,omitempty"`
	ReleaseURL          string     `json:"releaseUrl,omitempty"`
	PublishedAt         string     `json:"publishedAt,omitempty"`
	Status              string     `json:"status"`
	MatchesLatest       *bool      `json:"matchesLatest"`
	UpdateAvailable     *bool      `json:"updateAvailable"`
	CheckedAt           time.Time  `json:"checkedAt"`
	LastSuccessfulCheck *time.Time `json:"lastSuccessfulCheck,omitempty"`
	NextCheck           time.Time  `json:"nextCheck"`
	Stale               bool       `json:"stale"`
	Error               string     `json:"error,omitempty"`
}
type releaseChecker struct {
	mu       sync.Mutex
	endpoint string
	client   *http.Client
	cached   updateStatus
}

func newReleaseChecker() *releaseChecker {
	return &releaseChecker{endpoint: releaseEndpoint, client: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return fmt.Errorf("release API redirects are not followed")
	}}}
}
func runningVersion() string {
	if v := os.Getenv("UNTIS_VERSION"); v != "" {
		return v
	}
	return version
}
func releaseNumbers(raw string) ([3]uint64, bool) {
	var out [3]uint64
	v := strings.TrimPrefix(strings.TrimSpace(raw), "v")
	if index := strings.IndexByte(v, '+'); index >= 0 {
		v = v[:index]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, s := range parts {
		if s == "" || (len(s) > 1 && s[0] == '0') {
			return out, false
		}
		for _, c := range s {
			if c < '0' || c > '9' {
				return out, false
			}
		}
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
func versionState(current, latest string) (string, *bool, *bool) {
	match, no := true, false
	// Unreleased builds are never declared current merely because their numeric
	// prefix matches a release. There is no commit-distance claim here.
	if strings.Contains(strings.ToLower(current), "dev") || strings.Contains(current, "-") {
		return "development", nil, nil
	}
	if strings.TrimPrefix(current, "v") == strings.TrimPrefix(latest, "v") {
		return "current", &match, &no
	}
	a, ok := releaseNumbers(current)
	b, other := releaseNumbers(latest)
	if !ok || !other {
		return "unknown", nil, nil
	}
	for i := range a {
		if a[i] < b[i] {
			return "update_available", &no, &match
		}
		if a[i] > b[i] {
			return "ahead", &no, &no
		}
	}
	return "current", &match, &no
}
func validReleaseURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host == "github.com" && u.User == nil && strings.HasPrefix(u.Path, "/UntisPlus/UntisPlus-Proxy/releases/tag/")
}
func (c *releaseChecker) check(current string, now time.Time) updateStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now.Before(c.cached.NextCheck) {
		out := c.cached
		out.CurrentVersion = current
		if out.Error == "" {
			out.Status, out.MatchesLatest, out.UpdateAvailable = versionState(current, out.LatestRelease)
		}
		return out
	}
	out := c.cached
	out.CurrentVersion = current
	out.CheckedAt = now
	out.NextCheck = now.Add(2 * time.Minute)
	out.Status = "unknown"
	out.MatchesLatest = nil
	out.UpdateAvailable = nil
	out.Stale = out.LatestRelease != ""
	out.Error = "github_unavailable"
	req, err := http.NewRequest(http.MethodGet, c.endpoint, nil)
	if err == nil {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "UntisPlus-Proxy-release-checker")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		response, fetchErr := c.client.Do(req)
		if fetchErr == nil {
			defer response.Body.Close()
			switch response.StatusCode {
			case http.StatusOK:
				var release struct {
					Tag         string `json:"tag_name"`
					URL         string `json:"html_url"`
					PublishedAt string `json:"published_at"`
					Draft       bool   `json:"draft"`
					Prerelease  bool   `json:"prerelease"`
				}
				decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
				if decoder.Decode(&release) == nil && release.Tag != "" && len(release.Tag) <= 128 && validReleaseURL(release.URL) && !release.Draft && !release.Prerelease {
					out.LatestRelease = release.Tag
					out.ReleaseURL = release.URL
					out.PublishedAt = release.PublishedAt
					out.Error = ""
					out.Stale = false
					stamp := now
					out.LastSuccessfulCheck = &stamp
					out.NextCheck = now.Add(time.Hour)
					out.Status, out.MatchesLatest, out.UpdateAvailable = versionState(current, release.Tag)
				} else {
					out.Error = "invalid_release"
				}
			case http.StatusForbidden, http.StatusTooManyRequests:
				out.Error = "rate_limited"
				out.NextCheck = now.Add(time.Hour)
			case http.StatusNotFound:
				out.Error = "no_release"
				out.NextCheck = now.Add(time.Hour)
			}
		}
	}
	c.cached = out
	return out
}
func (p *Proxy) handleUpdates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if !p.store.FeatureEnabled("", "update_check") {
		p.writeJSON(w, map[string]any{"status": "disabled", "currentVersion": runningVersion()})
		return
	}
	p.writeJSON(w, p.releaseChecker.check(runningVersion(), time.Now().UTC()))
}
