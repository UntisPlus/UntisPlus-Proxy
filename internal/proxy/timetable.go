package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"untis-proxy/internal/store"
	"untis-proxy/internal/untis"
)

func (p *Proxy) getTimetable(w http.ResponseWriter, r *http.Request, school string, id json.RawMessage, paramsRaw json.RawMessage, body []byte) {
	user := p.sessionUser(r)
	if user == nil {
		p.writeJSONRPCError(w, id, "not logged in", -8520)
		return
	}
	var params struct {
		Options struct {
			Element struct {
				ID   int64 `json:"id"`
				Type int64 `json:"type"`
			} `json:"element"`
			StartDate string `json:"startDate"`
			EndDate   string `json:"endDate"`
		} `json:"options"`
	}
	_ = json.Unmarshal(paramsRaw, &params)
	elType, elID := params.Options.Element.Type, params.Options.Element.ID

	// Non-class elements: proxy with the requesting user's real session and let
	// the real server decide (mirrors real behaviour exactly).
	if elType != 1 {
		b, status, err := p.escalatedIntern(school, user, "jsonrpc.do", body)
		if err != nil {
			p.writeJSONRPCError(w, id, "no right for timetable", -8509)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(b)
		return
	}

	// Class timetables: access is granted for the class pool only.
	ok, err := p.store.PoolContains(school, elID)
	if err != nil || !ok {
		p.writeJSONRPCError(w, id, "no right for timetable", -8509)
		return
	}
	owner, err := p.store.OwnerForClass(school, elID)
	if err != nil || owner == nil {
		p.writeJSONRPCError(w, id, "no right for timetable", -8509)
		return
	}

	key := fmt.Sprintf("%s|%d|%s|%s", school, elID, params.Options.StartDate, params.Options.EndDate)
	if b, ok := p.tt.Get(key); ok {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
		return
	}

	b, status, err := p.escalatedIntern(school, owner, "jsonrpc.do", body)
	if err != nil {
		p.writeJSONRPCError(w, id, "no right for timetable", -8509)
		return
	}
	p.tt.Put(key, b)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// escalatedIntern issues a forward for a user via the real API, retrying once
// with a fresh session when the cached upstream session expired server-side.
// endpoint is "jsonrpc.do" (legacy) or an intern method name.
func (p *Proxy) escalatedIntern(school string, u *store.User, endpoint string, body []byte) ([]byte, int, error) {
	cookie, err := p.untis.Session(school, u.Username, u.Password, u.Method)
	if err != nil {
		return nil, 0, err
	}
	var b []byte
	var status int
	if endpoint == "jsonrpc.do" {
		b, err = p.untis.JSONRPC(school, cookie, body)
		status = http.StatusOK
	} else {
		b, status, _, err = p.untis.RawIntern(school, cookie, endpoint, body)
	}
	if err != nil {
		return b, status, err
	}
	if untis.IsAuthFailure(b) {
		if ck, err := p.untis.FreshSession(school, u.Username, u.Password, u.Method); err == nil {
			if endpoint == "jsonrpc.do" {
				if b2, err2 := p.untis.JSONRPC(school, ck, body); err2 == nil && !untis.IsAuthFailure(b2) {
					return b2, http.StatusOK, nil
				}
			} else {
				if b2, status2, _, err2 := p.untis.RawIntern(school, ck, endpoint, body); err2 == nil && !untis.IsAuthFailure(b2) {
					return b2, status2, nil
				}
			}
		}
	}
	return b, status, nil
}

type ttCache struct {
	mu  sync.Mutex
	m   map[string]ttEntry
	ttl time.Duration
}

type ttEntry struct {
	data []byte
	at   time.Time
}

func newTTCache(ttl time.Duration) *ttCache {
	return &ttCache{m: map[string]ttEntry{}, ttl: ttl}
}

func (c *ttCache) Get(k string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok || time.Since(e.at) > c.ttl {
		return nil, false
	}
	return e.data, true
}

func (c *ttCache) Put(k string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) > 2000 {
		now := time.Now()
		for kk, e := range c.m {
			if now.Sub(e.at) > c.ttl {
				delete(c.m, kk)
			}
		}
	}
	c.m[k] = ttEntry{data: data, at: time.Now()}
}
