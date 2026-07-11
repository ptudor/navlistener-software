package store

import "testing"

// TestClampSeverityBoundsBeforeInt16Cast guards an out-of-range MinSeverity
// (e.g. from an unvalidated query param upstream) must be clamped into the valid
// 0..2 (info/warning/critical) range before the int16 cast in QueryEvents' SQL --
// a raw cast would wrap 65538 to 2 and 65536 to 0, silently remapping the filter.
func TestClampSeverityBoundsBeforeInt16Cast(t *testing.T) {
	cases := []struct{ in, want int }{
		{-5, 0},
		{0, 0},
		{1, 1},
		{2, 2},
		{3, 2},
		{65536, 2},
		{65538, 2},
	}
	for _, c := range cases {
		if got := clampSeverity(c.in); got != c.want {
			t.Errorf("clampSeverity(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
