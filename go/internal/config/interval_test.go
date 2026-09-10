package config

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// IntervalRe allows an unbounded decimal count, so a
// syntactically valid interval could overflow int parsing or wrap the
// Duration multiplication. Validation then either discarded the parse error or
// compared wrapped durations, while the database received the original text —
// leaving the policy and the in-memory replay horizon meaning different things,
// and letting a compress/retention pair be accepted in the opposite order to
// what was configured.
func TestParseIntervalBounds(t *testing.T) {
	// The largest count each unit can carry without exceeding MaxInterval.
	maxFor := map[string]int64{
		"minute": int64(MaxInterval / time.Minute),
		"hour":   int64(MaxInterval / time.Hour),
		"day":    int64(MaxInterval / (24 * time.Hour)),
		"week":   int64(MaxInterval / (7 * 24 * time.Hour)),
	}
	for unit, per := range intervalUnits {
		top := maxFor[unit]

		// Exactly at the maximum: accepted, and exactly representable.
		at := fmt.Sprintf("%d %ss", top, unit)
		got, err := ParseInterval(at)
		if err != nil {
			t.Errorf("ParseInterval(%q) rejected the maximum safe count: %v", at, err)
		} else if want := time.Duration(top) * per; got != want {
			t.Errorf("ParseInterval(%q) = %v, want %v", at, got, want)
		}

		// One unit above: rejected, never wrapped.
		over := fmt.Sprintf("%d %ss", top+1, unit)
		if got, err := ParseInterval(over); err == nil {
			t.Errorf("ParseInterval(%q) = %v, want an overflow error", over, got)
		}

		// Far past int64 entirely: rejected by the width-checked parse.
		huge := fmt.Sprintf("%s0 %ss", strings.Repeat("9", 25), unit)
		if got, err := ParseInterval(huge); err == nil {
			t.Errorf("ParseInterval(%q) = %v, want a range error", huge, got)
		}

		// The smallest valid count still works (IntervalRe forbids 0 and signs).
		one := fmt.Sprintf("1 %s", unit)
		if got, err := ParseInterval(one); err != nil || got != per {
			t.Errorf("ParseInterval(%q) = %v, %v; want %v", one, got, err, per)
		}
	}

	// math.MaxInt64 as a raw count parses as a number but overflows every unit.
	for unit := range intervalUnits {
		s := fmt.Sprintf("%d %ss", int64(math.MaxInt64), unit)
		if got, err := ParseInterval(s); err == nil {
			t.Errorf("ParseInterval(%q) = %v, want an overflow error", s, got)
		}
	}

	for _, bad := range []string{"", "not an interval", "0 days", "-1 days", "7 fortnights", "7days", "7  days"} {
		if got, err := ParseInterval(bad); err == nil {
			t.Errorf("ParseInterval(%q) = %v, want a validation error", bad, got)
		}
	}
}

// The compress-before-retention ordering check must run on non-overflowing
// values. Before the fix, a pair whose wrapped durations ordered the opposite
// way was accepted by -check-config and then applied to the database as its
// original, very large text.
func TestFinalizeRejectsOverflowingRetentionIntervals(t *testing.T) {
	// 15250 weeks fits; 15251 does not (MaxInterval is ~292.47 years).
	overflowWeeks := int64(MaxInterval/(7*24*time.Hour)) + 1
	for _, tc := range []struct {
		name                     string
		compressAfter, retention string
		wantErr                  bool
	}{
		{"valid ordering", "1 day", "7 days", false},
		{"valid at the maximum", "1 day", fmt.Sprintf("%d weeks", int64(MaxInterval/(7*24*time.Hour))), false},
		{"equal is rejected", "7 days", "7 days", true},
		{"inverted is rejected", "8 days", "7 days", true},
		{"retention overflows", "1 day", fmt.Sprintf("%d weeks", overflowWeeks), true},
		{"compress_after overflows", fmt.Sprintf("%d weeks", overflowWeeks), "7 days", true},
		{"both overflow", fmt.Sprintf("%d days", overflowWeeks*7), fmt.Sprintf("%d weeks", overflowWeeks), true},
		{"count beyond int64", "1 day", strings.Repeat("9", 30) + " days", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			toml := fmt.Sprintf(`
[store]
dsn = "postgres://u@localhost/db"
compress_after = %q
raw_retention = %q
`, tc.compressAfter, tc.retention)
			_, err := loadFromString(t, toml, 0o600)
			if tc.wantErr && err == nil {
				t.Fatalf("compress_after=%q raw_retention=%q was accepted; want a rejection",
					tc.compressAfter, tc.retention)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("compress_after=%q raw_retention=%q rejected: %v",
					tc.compressAfter, tc.retention, err)
			}
		})
	}
}
