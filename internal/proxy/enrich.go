package proxy

// Enrichment seam shared by the proxy-local personal features: homework
// completion flags, absence metadata and private notes, and Technik events.
//
// The rules here are the whole point of the file, so they are worth stating
// once instead of per call site:
//
//   - Numbers are decoded with UseNumber, so a large or high-precision id
//     survives a decode/encode round trip as the same digits. Plain float64
//     would quietly rewrite 10000000000000001 as 10000000000000000, and this
//     data is keyed by exactly those ids.
//   - Unknown fields are preserved. Upstream adds fields without warning, and
//     the app is entitled to see them; a whitelist rebuild would delete them.
//   - Any failure returns the original bytes unchanged. A malformed or
//     unexpected response must reach the app exactly as upstream sent it,
//     because a half-decorated response is worse than an undecorated one: the
//     app would render it as if the decoration were authoritative.
//   - Personal data is only ever added for an identity the proxy itself
//     established. Never for a username taken from the request, and never for
//     a response whose credentials were rewritten to another account.

import (
	"bytes"
	"encoding/json"
	"log"
)

// decodeJSON decodes upstream bytes into any value with number fidelity, or
// returns false if they are not a JSON object/array.
func decodeJSON(raw []byte, v any) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(v) == nil
}

// encodeJSON marshals v, reporting failure rather than emitting partial bytes.
func encodeJSON(v any) ([]byte, bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	return b, true
}

// jsonObject is a decoded JSON object. Using map[string]any rather than a struct
// is deliberate: it keeps every upstream field, including ones this proxy has
// never seen, and lets decoration add fields without a schema change.
type jsonObject = map[string]any

// jsonID reads an integral id out of a decoded JSON value. It accepts the
// json.Number the decoder produces and also tolerates a plain float64, so it
// keeps working if a response is ever assembled in-process rather than decoded.
func jsonID(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			// A float-formatted integer ("1234.0") is still an integer id.
			f, ferr := n.Float64()
			if ferr != nil || f != float64(int64(f)) {
				return 0, false
			}
			return int64(f), true
		}
		return i, true
	case float64:
		if n != float64(int64(n)) {
			return 0, false
		}
		return int64(n), true
	case int64:
		return n, true
	}
	return 0, false
}

// jsonArray returns v as a slice of objects, or nil if it is not an array of
// objects. A non-array (null, absent, wrong type) yields nil rather than an
// error, because "this response has no homework" is a normal state.
func jsonArray(v any) []jsonObject {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]jsonObject, 0, len(arr))
	for _, item := range arr {
		if obj, ok := item.(jsonObject); ok {
			out = append(out, obj)
		}
	}
	return out
}

// enrichJSON applies decorate to the `result` object of a JSON-RPC response and
// re-encodes it, returning the original bytes on any problem.
//
// Only `result` is touched. The rest of the envelope is held as raw bytes and
// written back verbatim, so `id`, `jsonrpc` and anything this proxy does not
// model survive untouched. A response carrying an error is left completely
// alone: its result may be an object, but decorating it would dress up a failure
// as if it were data.
func enrichJSON(raw []byte, decorate func(jsonObject)) []byte {
	var env map[string]json.RawMessage
	if !decodeJSON(raw, &env) || env == nil {
		return raw
	}
	// An error response is left completely alone: its result may still be an
	// object, but decorating it would dress up a failure as if it were data.
	// The key being absent is the normal case and must not be mistaken for one.
	if errRaw, present := env["error"]; present && !isNullOrAbsent(errRaw) {
		return raw
	}
	resultRaw, ok := env["result"]
	if !ok || isNullOrAbsent(resultRaw) {
		return raw
	}
	var result jsonObject
	if !decodeJSON(resultRaw, &result) || result == nil {
		return raw
	}
	// Encode before decorating so an enrichment that changes nothing can be
	// detected. Re-encoding a response for no reason would reorder keys and
	// normalise whitespace, which is an observable change to a response the app
	// may be byte-comparing or caching.
	before, ok := encodeJSON(result)
	if !ok {
		return raw
	}
	decorate(result)
	decorated, ok := encodeJSON(result)
	if !ok {
		return raw
	}
	if bytes.Equal(before, decorated) {
		return raw
	}
	env["result"] = decorated
	out, ok := encodeJSON(env)
	if !ok {
		return raw
	}
	return out
}

// isNullOrAbsent reports whether an envelope field is missing or JSON null.
func isNullOrAbsent(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

// enrichmentFailed records an enrichment that bailed out. Falling back to the
// untouched response is correct behaviour, not an error the operator has to act
// on, so this is logged rather than returned.
func enrichmentFailed(what string, err error) {
	log.Printf("[enrich] %s: %v (passing upstream response through unchanged)", what, err)
}
