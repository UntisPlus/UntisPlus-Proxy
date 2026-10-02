package proxy

// Edge cases of the shared enrichment helpers: the id reader, the encode guard,
// and the fallbacks that decide whether a response is re-emitted or returned
// byte-for-byte.
//
// These exist because the rule they protect is easy to break silently. A helper
// that starts normalising a response it was supposed to leave alone produces a
// difference the app can see but nothing here would fail on.

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"
)

// TestJSONIDReadsIntegralValues: an id may arrive as a json.Number (the normal
// case), as a float64 when a response was assembled in-process, or as an int64.
// All three must yield the same answer.
func TestJSONIDReadsIntegralValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   any
		want int64
		ok   bool
	}{
		{"json.Number", json.Number("300001"), 300001, true},
		{"float-formatted integer", json.Number("300001.0"), 300001, true},
		{"float64", float64(300001), 300001, true},
		{"int64", int64(300001), 300001, true},
		{"zero", json.Number("0"), 0, true},
		{"very large", json.Number("9007199254740993"), 9007199254740993, true},
		// Below 2^53 a float64 cannot hold the id exactly, but float64s this large
		// never occur from decoding; the integer forms above carry those.
		{"fractional number", json.Number("1.5"), 0, false},
		{"fractional float", float64(1.5), 0, false},
		{"non-numeric string", "300001", 0, false},
		{"bool", true, 0, false},
		{"nil", nil, 0, false},
		{"object", jsonObject{}, 0, false},
		{"unparseable number", json.Number("abc"), 0, false},
	} {
		got, ok := jsonID(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: jsonID(%v) = (%d, %v), want (%d, %v)", tc.name, tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestJSONIDFloatIsNotSilentlyTruncated pins the important property of the
// fallback: a fractional value is rejected rather than truncated, because a
// truncated id would match the wrong record.
func TestJSONIDFloatIsNotSilentlyTruncated(t *testing.T) {
	for _, in := range []any{json.Number("2.9"), float64(2.9), json.Number("0.1"), float64(-1.5)} {
		if got, ok := jsonID(in); ok {
			t.Errorf("jsonID(%v) = %d, true — a fractional value must not become an id", in, got)
		}
	}
}

// TestEncodeJSONReportsFailure: the guard is only useful if it actually fails, so
// pin that an unmarshalable value is reported rather than silently dropped.
func TestEncodeJSONReportsFailure(t *testing.T) {
	if b, ok := encodeJSON(map[string]any{"bad": math.Inf(1)}); ok {
		t.Errorf("encodeJSON(Inf) = %s, true — want a reported failure", b)
	}
	if _, ok := encodeJSON(func() {}); ok {
		t.Error("encodeJSON(func) succeeded — want a reported failure")
	}
	b, ok := encodeJSON(map[string]any{"a": 1})
	if !ok || !bytes.Equal(b, []byte(`{"a":1}`)) {
		t.Errorf("encodeJSON(simple) = %s, %v", b, ok)
	}
}

// TestEnrichJSONReturnsInputUnchangedWhenDecorationDoesNothing is the property
// the whole module rests on: a response that gains nothing is returned as the
// exact bytes that arrived, not re-encoded.
func TestEnrichJSONReturnsInputUnchangedWhenDecorationDoesNothing(t *testing.T) {
	// Odd spacing and key order, so any re-encoding would show up as a diff.
	raw := `{"id" : "upstream","jsonrpc":"2.0",  "result": { "zebra":1, "alpha": [3,1,2] } }`
	got := enrichJSON([]byte(raw), func(jsonObject) {})
	if string(got) != raw {
		t.Errorf("an undecorated response was re-encoded\n got: %s\nwant: %s", got, raw)
	}
}

// TestEnrichJSONReturnsInputOnUnusableEnvelopes: every shape that cannot be
// enriched must reach the app exactly as upstream sent it.
func TestEnrichJSONReturnsInputOnUnusableEnvelopes(t *testing.T) {
	for _, raw := range []string{
		``,
		`null`,
		`[]`,
		`"a string"`,
		`not json at all`,
		`{"jsonrpc":"2.0"}`,               // no result
		`{"jsonrpc":"2.0","result":null}`, // null result
		`{"jsonrpc":"2.0","result":""}`,   // result is not an object
		`{"jsonrpc":"2.0","result":[]}`,   // result is an array
		`{"jsonrpc":"2.0","result":42}`,   // result is a number
		`{"jsonrpc":"2.0","error":null,"result":{"a":1}}`,        // a null error is not an error
		`{"jsonrpc":"2.0","error":{"code":-1},"result":{"a":1}}`, // a real error is left alone
	} {
		called := false
		got := enrichJSON([]byte(raw), func(jsonObject) { called = true })
		if string(got) != raw {
			t.Errorf("response altered for %s\n got: %s\nwant: %s", raw, got, raw)
		}
		if called && (raw == `{"jsonrpc":"2.0","error":{"code":-1},"result":{"a":1}}`) {
			t.Error("the decorator ran on an error response")
		}
	}
}

// TestEnrichJSONKeepsEverythingAroundTheDecoration: decoration adds to the
// result, and every other part of the envelope must survive it untouched.
func TestEnrichJSONKeepsEverythingAroundTheDecoration(t *testing.T) {
	raw := `{"jsonrpc":"2.0","id":"upstream-7","params":{"unknown":[1,2]},"result":{"keepMe":{"deep":[true,null]}}}`
	got := enrichJSON([]byte(raw), func(result jsonObject) { result["added"] = "yes" })

	var env map[string]json.RawMessage
	if !decodeJSON(got, &env) {
		t.Fatalf("output is not decodable: %s", got)
	}
	if string(env["id"]) != `"upstream-7"` {
		t.Errorf("id = %s, want \"upstream-7\"", env["id"])
	}
	if string(env["params"]) != `{"unknown":[1,2]}` {
		t.Errorf("params were altered: %s", env["params"])
	}
	result := decodeResult(t, got)
	if result["added"] != "yes" {
		t.Errorf("decoration did not land: %s", result["added"])
	}
	if _, ok := result["keepMe"]; !ok {
		t.Errorf("an untouched result field was dropped: %s", result["keepMe"])
	}
	// The original byte formatting of the untouched parts should still be there,
	// which is what json.RawMessage preserves.
	if !bytes.Contains(got, []byte(`"unknown":[1,2]`)) {
		t.Errorf("params were re-encoded rather than carried through: %s", got)
	}
}

// TestEnrichmentFailedIsLoggedNotPanicked: the fallback path must not take the
// request down. Calling it is enough — a panic here would fail the test.
func TestEnrichmentFailedIsLoggedNotPanicked(t *testing.T) {
	enrichmentFailed("unit test", errFakeForTest{})
}

type errFakeForTest struct{}

func (errFakeForTest) Error() string { return "deliberate" }

// TestIsNullOrAbsent: a missing key, a JSON null and a zero-length value are all
// "nothing here", but the other null-ish words must not be.
func TestIsNullOrAbsent(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{`null`, true},
		{``, true},
		{`  null `, true},
		{`0`, false},
		{`false`, false},
		{`""`, false},
		{`{}`, false},
		{`"null"`, false},
	} {
		if got := isNullOrAbsent(json.RawMessage(tc.in)); got != tc.want {
			t.Errorf("isNullOrAbsent(%s) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestReadBodyToleratesNoBody: a body-less request must report a normal
// bad-request error rather than panicking on the nil body.
func TestReadBodyToleratesNoBody(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "/api/absence/notes", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if req.Body != nil {
		t.Fatalf("precondition: expected a nil body, got %T", req.Body)
	}
	if got := readBody(req); len(got) != 0 {
		t.Errorf("readBody(bodyless) = %v, want empty", got)
	}
}

// TestAbsenceNoteBodyIsTrimmedNotStoredRaw: the stored note is the trimmed text,
// so what a student reads back is what they stored.
func TestAbsenceNoteBodyIsTrimmedNotStoredRaw(t *testing.T) {
	p, st := absenceProxy(t)
	rec := sessionedRequest(t, p, "dee", "POST", "/api/absence/notes",
		`{"absenceKey":300001,"note":"   bring workbook   "}`)
	if rec.Code != 200 {
		t.Fatalf("write failed: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"note":"bring workbook"`) {
		t.Errorf("the response kept the padding: %s", rec.Body.String())
	}
	notes, err := st.AbsenceNotes("testschool", "dee")
	if err != nil {
		t.Fatalf("AbsenceNotes: %v", err)
	}
	if notes[300001].Note != "bring workbook" {
		t.Errorf("stored note = %q, want the trimmed text", notes[300001].Note)
	}
}
