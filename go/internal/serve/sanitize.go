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
//
// This is DISPLAY sanitization and is lossy by design, so it must never be
// applied to an identity or a key : distinct inputs can map to the
// same output. Observer ids are validated at their authority boundary
// (identity.ValidObserverID) and served verbatim; only operator-supplied metadata
// — vendor, remark — comes through here.
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
		// check the bound BEFORE writing. Testing b.Len() only
		// afterwards let a multibyte rune straddle the limit, so the documented
		// 256-byte cap could be exceeded by up to three bytes.
		if b.Len()+utf8.RuneLen(r) > maxStringField {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}
