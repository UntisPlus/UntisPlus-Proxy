package proxy

import (
	"encoding/json"
	"strings"
)

// Upstream derives a period's `can` list from the account the request was
// authenticated as, and the app hides any UI whose capability is missing.
// Two things fall out of that and both are wrong for us:
//
//   - A pooled class is fetched with the class owner's student key, so its
//     periods carry only (READ_HOMEWORK, READ_PERIODINFO). A recon user
//     therefore sees teacher, class and room but no lesson topic, even though
//     text.lesson sits right there in the same period.
//   - A boosted user is forwarded through a teacher account, so their periods
//     carry the full teacher capability set: write/delete on lessons,
//     registrations and absences of classes they only have read access to.
//
// So we rewrite `can` on the way out: the lesson topic is always readable (it
// is part of the payload we already send), and every non-read capability is
// dropped unless the requester holds the editor flag.

// readLessonTopicCap is the capability the app needs to render the lesson-topic
// line of a period.
const readLessonTopicCap = "READ_LESSONTOPIC"

// viewerCaps returns the capability list a requester may see on one period.
// The editor flag is the only thing that keeps write capabilities: being an
// admin of this proxy is not the same as being allowed to write to Untis, and
// the write-method gate would refuse the call anyway, so showing the editing UI
// would just be a dead button.
func (p *Proxy) viewerCaps(username string, can []any) []any {
	canEdit := p.isEditor(username)
	out := make([]any, 0, len(can)+1)
	seen := make(map[string]bool, len(can))
	for _, c := range can {
		s, ok := c.(string)
		if !ok || s == "" || seen[s] {
			continue
		}
		// Everything upstream expresses without a READ_ prefix is a mutation.
		if !canEdit && !strings.HasPrefix(s, "READ_") {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	if !seen[readLessonTopicCap] {
		out = append(out, readLessonTopicCap)
	}
	return out
}

// capsInPeriod rewrites the `can` list of a single decoded period.
func (p *Proxy) capsInPeriod(username string, pd map[string]any) {
	can, _ := pd["can"].([]any)
	pd["can"] = p.viewerCaps(username, can)
}

// viewerBody applies viewerCaps to every period of a raw getTimetable2017
// answer. A body it cannot parse is passed through unchanged.
func (p *Proxy) viewerBody(username string, b []byte) []byte {
	var doc map[string]any
	if json.Unmarshal(b, &doc) != nil {
		return b
	}
	result, _ := doc["result"].(map[string]any)
	if result == nil {
		return b
	}
	timetable, _ := result["timetable"].(map[string]any)
	if timetable == nil {
		return b
	}
	periods, _ := timetable["periods"].([]any)
	if len(periods) == 0 {
		return b
	}
	for _, raw := range periods {
		if pd, ok := raw.(map[string]any); ok {
			p.capsInPeriod(username, pd)
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return b
	}
	return out
}
