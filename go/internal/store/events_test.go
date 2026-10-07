package store

import (
	"context"
	"testing"
	"time"
)

// TestEventQueryValidateBeforeDatabase: QueryEvents refuses a page outside the
// store's bounds, or an inverted window, before it touches the database — the
// Store here has no pool, so reaching the query would panic.
func TestEventQueryValidateBeforeDatabase(t *testing.T) {
	s := &Store{}
	now := time.Now()
	for name, q := range map[string]EventQuery{
		"zero limit":      {Limit: 0},
		"negative limit":  {Limit: -1},
		"huge limit":      {Limit: 10_000},
		"negative offset": {Limit: 10, Offset: -1},
		"huge offset":     {Limit: 10, Offset: EventsMaxOffset + 1},
		"inverted window": {Limit: 10, Since: now, Until: now.Add(-time.Hour)},
	} {
		if _, _, err := s.QueryEvents(context.Background(), q); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for name, q := range map[string]EventQuery{
		"max page":    {Limit: EventsMaxLimit, Offset: EventsMaxOffset},
		"open window": {Limit: 1, Since: now},
		"ordered":     {Limit: 1, Since: now.Add(-time.Hour), Until: now},
	} {
		if err := q.Validate(); err != nil {
			t.Errorf("%s rejected: %v", name, err)
		}
	}
}

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
