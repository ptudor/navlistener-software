package serve

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxStringField bounds any receiver-originated string in a feed, so a hostile or
// buggy observer cannot bloat a response.
const maxStringField = 256

// sanitize makes a string safe to place in a JSON feed (docs/INTEGRITY.md §9): it
// coerces to valid UTF-8, drops control characters, and length-bounds the result. No untrusted bytes reach the encoder unfiltered.
func sanitize(s string) string {
	if s == "" {
		return s
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == utf8.RuneError || unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= maxStringField {
			break
		}
	}
	return b.String()
}
