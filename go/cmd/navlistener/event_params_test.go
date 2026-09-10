package main

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"
)

// sanitizeEventParams inspected only TOP-LEVEL float64 values,
// but Params is a generic nested map[string]any. A non-finite number one level
// down — or a value of a type encoding/json cannot marshal at all — still failed
// the whole document, and the SSE broker then treated that marshal failure as
// successful delivery: the event kept its place in the durable and broker
// sequences while emitting no bytes, so a later event advanced the client's
// cursor across a transition it never received.
func TestSanitizeEventParamsIsRecursive(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params map[string]any
	}{
		{"top level", map[string]any{"orbit_disco_m": math.NaN()}},
		{"nested map", map[string]any{"detail": map[string]any{"delta_m": math.Inf(1)}}},
		{"deeply nested", map[string]any{
			"a": map[string]any{"b": map[string]any{"c": map[string]any{"d": math.Inf(-1)}}}}},
		{"inside a slice", map[string]any{"samples": []any{1.0, math.NaN(), 3.0}}},
		{"slice of maps", map[string]any{"rows": []any{
			map[string]any{"v": 1.0}, map[string]any{"v": math.Inf(1)}}}},
		{"float32", map[string]any{"cn0": float32(math.Inf(1))}},
		{"unsupported dynamic type", map[string]any{"ch": make(chan int)}},
		{"unsupported inside a map", map[string]any{"d": map[string]any{"f": func() {}}}},
		{"complex number", map[string]any{"z": complex(1, 2)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := json.Marshal(tc.params); err == nil {
				t.Fatal("fixture already marshals; it does not exercise the defect")
			}
			cleaned := sanitizeEventParams(tc.params)
			if _, err := json.Marshal(cleaned); err != nil {
				t.Fatalf("params still unmarshalable after sanitizing: %v", err)
			}
			// The transition must survive: a confirmed integrity event is never
			// dropped over one bad value.
			if len(cleaned) != len(tc.params) {
				t.Errorf("sanitizing dropped keys: got %d, want %d", len(cleaned), len(tc.params))
			}
		})
	}
}

// Valid params must pass through untouched — same map, no allocation, no
// reformatting of legitimate values.
func TestSanitizeEventParamsPreservesValidPayloads(t *testing.T) {
	params := map[string]any{
		"orbit_disco_m": 1234.5,
		"count":         int64(7),
		"name":          "G05@0",
		"ok":            true,
		"absent":        nil,
		"nested":        map[string]any{"x": 1.0, "deep": map[string]any{"y": -2.5}},
		"list":          []any{1.0, "two", map[string]any{"three": 3.0}},
	}
	before, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	cleaned := sanitizeEventParams(params)
	after, err := json.Marshal(cleaned)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cleaned, params) {
		t.Errorf("valid params were altered:\n got %#v\nwant %#v", cleaned, params)
	}
	if string(before) != string(after) {
		t.Errorf("valid params re-encoded differently:\n got %s\nwant %s", after, before)
	}
	// Nil and empty stay identity, so the common path allocates nothing.
	if got := sanitizeEventParams(nil); got != nil {
		t.Errorf("nil params became %#v", got)
	}
}
