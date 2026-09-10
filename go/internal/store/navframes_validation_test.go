package store

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/gnss"
)

// NavFrameQuery is exported and reusable, but its numeric fields
// reached SQL unchecked. An out-of-range GNSS id was cast straight to int16 —
// 65,536 wraps to 0, silently answering a different constellation's question —
// and a MaxInt limit overflowed at limit+1, producing a database error instead of
// the API's documented limit behavior. Validation must happen before any SQL runs,
// which is what a nil pool proves here: reaching the database would panic.
func TestQueryNavFramesValidatesNumericFields(t *testing.T) {
	since := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	id := func(v int) *int { return &v }
	sink := func(StoredNavFrame) error { return nil }

	for _, tc := range []struct {
		name    string
		q       NavFrameQuery
		wantErr string
	}{
		{"gnss id below the domain", NavFrameQuery{Since: since, GnssID: id(-1)}, "outside the constellation domain"},
		{"gnss id above the domain", NavFrameQuery{Since: since, GnssID: id(int(gnss.NavIC) + 1)}, "outside the constellation domain"},
		{"IMES is never emitted", NavFrameQuery{Since: since, GnssID: id(int(gnss.IMES))}, "outside the constellation domain"},
		// The wrapping cases: each of these used to become a valid smallint.
		{"wraps to 0", NavFrameQuery{Since: since, GnssID: id(65536)}, "outside the constellation domain"},
		{"wraps to 2", NavFrameQuery{Since: since, GnssID: id(65538)}, "outside the constellation domain"},
		{"native int maximum", NavFrameQuery{Since: since, GnssID: id(math.MaxInt)}, "outside the constellation domain"},
		{"native int minimum", NavFrameQuery{Since: since, GnssID: id(math.MinInt)}, "outside the constellation domain"},
		{"limit past the maximum", NavFrameQuery{Since: since, Limit: maxNavFrameLimit + 1}, "exceeds the maximum supported limit"},
		{"limit at MaxInt", NavFrameQuery{Since: since, Limit: math.MaxInt}, "exceeds the maximum supported limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := queryNavFrames(context.Background(), nil, tc.q, sink)
			if err == nil {
				t.Fatal("invalid query was accepted; it would have reached the database")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not explain the rejection (want %q)", err, tc.wantErr)
			}
			if errors.Is(err, ErrNavFrameLimit) {
				t.Error("a validation failure must not masquerade as the limit sentinel")
			}
		})
	}

	// Every valid constellation id, the default/one/maximum limits, and the
	// existing Since/Until contract must all still pass validation — proven by
	// reaching the nil pool, which panics only once validation is done.
	valid := []NavFrameQuery{}
	for v := 0; v <= int(gnss.NavIC); v++ {
		if !gnss.GNSSID(v).Valid() {
			continue
		}
		valid = append(valid, NavFrameQuery{Since: since, GnssID: id(v)})
	}
	valid = append(valid,
		NavFrameQuery{Since: since},                          // default limit
		NavFrameQuery{Since: since, Limit: 1},                // smallest explicit
		NavFrameQuery{Since: since, Limit: maxNavFrameLimit}, // largest accepted
		NavFrameQuery{Since: since, Limit: -1},               // nonpositive -> default
		NavFrameQuery{Since: since, Until: since.Add(time.Hour)},
	)
	for i, q := range valid {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("valid query %d was rejected before reaching the database", i)
				}
			}()
			_ = queryNavFrames(context.Background(), nil, q, sink)
		}()
	}

	// The pre-existing Since/Until contract is unchanged, and those rejections
	// still happen before validation of the numeric fields.
	if err := queryNavFrames(context.Background(), nil, NavFrameQuery{}, sink); err == nil ||
		!strings.Contains(err.Error(), "Since is required") {
		t.Errorf("missing Since = %v, want the existing requirement", err)
	}
	if err := queryNavFrames(context.Background(), nil,
		NavFrameQuery{Since: since, Until: since}, sink); err == nil ||
		!strings.Contains(err.Error(), "must be after") {
		t.Errorf("Until == Since = %v, want the existing ordering error", err)
	}
}
