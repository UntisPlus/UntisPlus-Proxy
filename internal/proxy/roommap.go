package proxy

// Room-name overrides let an admin pin the plain text typed on an event to the
// exact Untis room id the real lessons use, per school. The school catalogue is
// the normal source of truth (see elementNameIndex), but a name can be
// ambiguous ("Aula" also names a retired room), or absent from a cold masterData
// cache. An override wins over both; a name in neither is still synthesized so
// the event stays visible.
//
// The map is stored as JSON under a per-school settings key, so it is edited
// from /admin and takes effect on the next request without a restart.

import (
	"encoding/json"
	"strings"
)

// roomOverridesSettingPrefix keys a school's override map in the settings table.
const roomOverridesSettingPrefix = "room_overrides:"

// roomOverrides returns the configured name→id map for a school. Names are
// lowercase and trimmed, matching how event text is looked up. A read error or a
// corrupt value yields nil: event synthesis must never fail on a bad override.
func (p *Proxy) roomOverrides(school string) map[string]int64 {
	school = strings.TrimSpace(school)
	if school == "" {
		return nil
	}
	raw, ok, err := p.store.GetSetting(roomOverridesSettingPrefix + school)
	if err != nil || !ok || strings.TrimSpace(raw) == "" {
		return nil
	}
	var m map[string]int64
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	return normalizeRoomOverrides(m)
}

// setRoomOverrides persists a school's name→id map, normalized the same way
// lookups are done.
func (p *Proxy) setRoomOverrides(school string, m map[string]int64) error {
	b, err := json.Marshal(normalizeRoomOverrides(m))
	if err != nil {
		return err
	}
	return p.store.SetSetting(roomOverridesSettingPrefix+strings.TrimSpace(school), string(b))
}

// normalizeRoomOverrides lowercases and trims names and drops non-positive ids,
// so "01-Aula", "Aula" and " aula " cannot become three different keys.
func normalizeRoomOverrides(m map[string]int64) map[string]int64 {
	if len(m) == 0 {
		return map[string]int64{}
	}
	out := make(map[string]int64, len(m))
	for k, v := range m {
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" || v <= 0 {
			continue
		}
		out[k] = v
	}
	return out
}
