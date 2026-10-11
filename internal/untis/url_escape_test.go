package untis

import (
	"strings"
	"testing"
)

// The JSON-RPC intern endpoint takes the method and school from the URL query.
// A caller-supplied method (the info-centre passthrough forwards the request's
// own method) must not be able to append extra query parameters, so both values
// are percent-encoded.
func TestRawInternEscapesQueryValues(t *testing.T) {
	f := &fakeAPI{status: map[string]int{}}
	c := newFakeAPI(t, f)

	if _, _, _, err := c.RawIntern("a&b=c school", "", "getTimetable2017&evil=1", []byte(`{}`)); err != nil {
		t.Fatalf("RawIntern: %v", err)
	}
	q := f.last().Query
	if strings.Contains(q, "evil=1") {
		t.Fatalf("method query value was not escaped: %q", q)
	}
	if !strings.Contains(q, "m=getTimetable2017%26evil%3D1") {
		t.Fatalf("method not percent-encoded: %q", q)
	}
	if !strings.Contains(q, "school=a%26b%3Dc+school") {
		t.Fatalf("school not percent-encoded: %q", q)
	}
}
