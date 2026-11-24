package proxy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"untis-proxy/internal/store"
)

type upstreamAnalyticsYear struct {
	Name  string          `json:"name"`
	Start json.RawMessage `json:"startDate"`
	End   json.RawMessage `json:"endDate"`
}

func (p *Proxy) upstreamAnalyticsYears(school string) ([]store.AnalyticsYear, error) {
	sources, err := p.store.BoostedSourceAccounts(school)
	if err != nil || len(sources) == 0 {
		return nil, fmt.Errorf("no saved teacher source for this school")
	}
	for _, method := range []string{"getSchoolyears", "getCurrentSchoolyear"} {
		body, _ := json.Marshal(map[string]any{"id": "analytics-years", "jsonrpc": "2.0", "method": method, "params": map[string]any{}})
		raw, _, err := p.escalatedIntern(school, sources[0], "jsonrpc.do", body)
		if err == nil {
			if years, err := parseUpstreamAnalyticsYears(raw); err == nil {
				return years, nil
			}
		}
	}
	return nil, fmt.Errorf("upstream did not provide readable school-year dates")
}

func parseUpstreamAnalyticsYears(raw []byte) ([]store.AnalyticsYear, error) {
	var response struct {
		Result json.RawMessage  `json:"result"`
		Error  *json.RawMessage `json:"error"`
	}
	invalid := fmt.Errorf("invalid upstream school years")
	if json.Unmarshal(raw, &response) != nil || response.Error != nil {
		return nil, invalid
	}
	var source []upstreamAnalyticsYear
	if json.Unmarshal(response.Result, &source) != nil {
		var current upstreamAnalyticsYear
		if json.Unmarshal(response.Result, &current) != nil {
			return nil, invalid
		}
		source = []upstreamAnalyticsYear{current}
	}
	if len(source) == 0 || len(source) > 100 {
		return nil, invalid
	}
	years := make([]store.AnalyticsYear, 0, len(source))
	for _, row := range source {
		from, to := upstreamYearDate(row.Start), upstreamYearDate(row.End)
		if from == "" || to == "" {
			return nil, invalid
		}
		name := strings.TrimSpace(row.Name)
		if name == "" {
			name = from[:4] + "/" + to[:4]
		}
		years = append(years, store.AnalyticsYear{Name: name, From: from, To: to})
	}
	sort.Slice(years, func(i, j int) bool { return years[i].From < years[j].From })
	if err := store.ValidateAnalyticsYears(years); err != nil {
		return nil, invalid
	}
	return years, nil
}

func upstreamYearDate(raw json.RawMessage) string {
	value := strings.Trim(string(raw), "\"")
	for _, layout := range []string{time.DateOnly, "20060102"} {
		if day, err := time.Parse(layout, value); err == nil {
			return day.Format(time.DateOnly)
		}
	}
	return ""
}
