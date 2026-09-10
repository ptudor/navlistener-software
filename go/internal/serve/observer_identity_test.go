package serve

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/identity"
)

// server-owned observer identity used to be rendered through the
// lossy display sanitizer, which drops control characters and U+FFFD. Distinct
// identities could therefore collapse to the same served id — an id containing a
// newline and the same id without it — and the Swift client's correct
// duplicate-id rejection then made one malformed authority row poison the entire
// observers snapshot. Even without a collision, the served id no longer
// round-tripped to the identity used by events and station selection.
func TestObserverIdentitiesAreByteStableAndUnique(t *testing.T) {
	ids := []string{
		"observer16",
		"observer\n16", // differs from the first only by a control character
		"observer�",    // literal replacement rune: previously dropped entirely
		"observer",     // ...which would have collided with this
		"roof:1",       // the deliberate opaque forms stay untouched
		"station/path",
		"stația",
		" roof ",
		strings.Repeat("é", 200), // multibyte, well past the display cap
	}
	sources := make([]config.Source, 0, len(ids))
	for _, id := range ids {
		sources = append(sources, config.Source{Name: id, Type: "ubx", Addr: "10.0.0.2:2947"})
	}
	s := testServer(sources)
	s.refreshAll()

	rr := httptest.NewRecorder()
	s.serveFeed("observers")(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/v2/observers", nil))
	var env struct {
		Data struct {
			Observers []observer `json:"observers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data.Observers) != len(ids) {
		t.Fatalf("got %d observers, want %d", len(env.Data.Observers), len(ids))
	}

	served := make(map[string]int, len(ids))
	for i, o := range env.Data.Observers {
		if o.ID != ids[i] {
			t.Errorf("observer %d: served id %q != identity %q; ids must round-trip byte-for-byte",
				i, o.ID, ids[i])
		}
		if prior, dup := served[o.ID]; dup {
			t.Fatalf("identities %q and %q collapsed to the same served id %q; one malformed row "+
				"would poison the whole snapshot for a client that rejects duplicates",
				ids[prior], ids[i], o.ID)
		}
		served[o.ID] = i
	}
}

// The display sanitizer is still applied to operator-supplied metadata, and its
// documented byte cap must now be honored exactly rather than overrun by up to
// three bytes when a multibyte rune straddles the limit.
func TestSanitizeHonorsByteCapExactly(t *testing.T) {
	for _, in := range []string{
		strings.Repeat("a", maxStringField*2),
		strings.Repeat("é", maxStringField),       // 2 bytes per rune
		strings.Repeat("€", maxStringField),       // 3 bytes per rune
		strings.Repeat("𝄞", maxStringField),       // 4 bytes per rune
		"a" + strings.Repeat("€", maxStringField), // straddles the boundary
	} {
		got := sanitize(in)
		if len(got) > maxStringField {
			t.Errorf("sanitize produced %d bytes, over the documented %d-byte cap",
				len(got), maxStringField)
		}
		if !utf8.ValidString(got) {
			t.Errorf("sanitize produced invalid UTF-8")
		}
	}
}

// The authority-boundary contract: opaque ids are preserved, and only an id that
// cannot round-trip through JSON at all is refused.
func TestOpaqueObserverIDContract(t *testing.T) {
	for _, id := range []string{"roof_1", "roof:1", "station/path", "stația", " roof ", " ",
		"obs\n16", strings.Repeat("x", 253), strings.Repeat("é", 300)} {
		if !identity.ValidOpaqueObserverID(id) {
			t.Errorf("opaque observer id %q rejected; opaque identity is a deliberate contract", id)
		}
	}
	for _, id := range []string{"", "bad\xff\xfeutf8", "\xc3"} {
		if identity.ValidOpaqueObserverID(id) {
			t.Errorf("un-round-trippable observer id %q accepted", id)
		}
	}
	// A control-plane row carrying such an id must fail closed on its own.
	c := identity.NewPrivateContext("bad\xffid", identity.CredentialToken)
	if _, err := c.Normalize(); err == nil {
		t.Error("a context with an un-round-trippable observer id normalized successfully")
	}
	ok := identity.NewPrivateContext("roof:1", identity.CredentialToken)
	if got, err := ok.Normalize(); err != nil || got.ObserverID != "roof:1" {
		t.Errorf("opaque id did not survive Normalize: %q, %v", got.ObserverID, err)
	}
}
