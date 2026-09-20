// Package identity contains the server-resolved administrative and publication
// context stamped on every observation. It deliberately contains no credentials:
// a feeder proves identity during authentication, then the collector carries this
// immutable result through decode, persistence, and audience selection.
package identity

import (
	"fmt"
	"regexp"
	"sort"
	"unicode/utf8"
)

const (
	// UnassignedOrganization is the fail-closed owner for config/legacy sources
	// that have not been attached to the shared AAA plane.
	UnassignedOrganization = "local-unassigned"
	// LocalCollectorInstance is the bootstrap instance id when no explicit stable
	// collector realm has been configured.
	LocalCollectorInstance = "local"
)

// CredentialTier records what proved the operational identity on this session.
// It is evidence, not authorization; publication still comes from server policy.
type CredentialTier string

const (
	CredentialLocalDial    CredentialTier = "local_dial"
	CredentialToken        CredentialTier = "token"
	CredentialSoftwareMTLS CredentialTier = "software_mtls"
	CredentialHardwareMTLS CredentialTier = "hardware_mtls"
)

// AttestationTier records the independently verified manufacturer-provenance
// result. Config-backed bootstrap identities can never claim a verified tier.
type AttestationTier string

const (
	AttestationNone           AttestationTier = "none"
	AttestationVerifiedV1Core AttestationTier = "verified_v1_core"
)

// HardwareTrust is what the collector itself verified about the hardware on a
// session: a manufacturer-signed commissioning record for this observer and,
// for trusted boards, a proof from the commissioned microcontroller bound to
// the session. It is never taken from a device's own report or from config.
type HardwareTrust string

const (
	// HardwareTrustNone covers software feeders, unknown or cloned boards,
	// revoked boards, and any evidence that failed verification.
	HardwareTrustNone HardwareTrust = "none"
	// HardwareTrustOpen is original hardware commissioned as never locked.
	HardwareTrustOpen HardwareTrust = "open"
	// HardwareTrustTest is a bench or development unit.
	HardwareTrustTest HardwareTrust = "test"
	// HardwareTrustTrusted is a locked board whose commissioned
	// microcontroller proved possession of its key on this session.
	HardwareTrustTrusted HardwareTrust = "trusted"
)

// AggregateUse controls whether an observation is eligible for public live
// state. Private is the zero/fallback behavior.
type AggregateUse string

const (
	AggregatePrivate          AggregateUse = "private"
	AggregatePublicAnonymous  AggregateUse = "public_anonymous"
	AggregatePublicAttributed AggregateUse = "public_attributed"
)

// EventVisibility independently controls whether integrity transitions derived
// from an observation may enter a public detector/event stream. PublicRedacted
// contributes without station identity; Public may retain an attributed source.
type EventVisibility string

const (
	EventsPrivate        EventVisibility = "private"
	EventsPublicRedacted EventVisibility = "public_redacted"
	EventsPublic         EventVisibility = "public"
)

// RawExport controls the maximum destination class for original observations.
// A current destination grant is always additionally required; "public" never
// means broadcast to every peer automatically.
type RawExport string

const (
	RawExportDeny       RawExport = "deny"
	RawExportNamedPeers RawExport = "named_peers"
	RawExportPublic     RawExport = "public"
)

// Signal is one normalized constellation/signal selector. An empty policy list
// means all collector-supported signals; it never bypasses an export grant.
type Signal struct {
	GnssID int
	SigID  int
}

// StationMetadata controls the most identifying station representation that a
// public read side may emit. It is independent of aggregate eligibility.
type StationMetadata string

const (
	MetadataNone   StationMetadata = "none"
	MetadataCoarse StationMetadata = "coarse"
	MetadataFull   StationMetadata = "full"
)

// PublicationPolicy is the receipt-time policy snapshot relevant to the first
// audience-safe implementation slice. Raw federation export is evaluated by a
// separate destination grant and is intentionally not represented as a bool here.
type PublicationPolicy struct {
	AggregateUse    AggregateUse
	StationMetadata StationMetadata
	EventVisibility EventVisibility
	RawExport       RawExport
	FederationPeers []string
	Signals         []Signal
	Revision        string
}

// ObserverContext is resolved by the collector from trusted config/AAA state.
// No ordinary GNF1 DATA field can populate or override it.
type ObserverContext struct {
	ObserverID          string
	OrganizationID      string
	EnrollmentID        string
	CollectorInstanceID string
	CollectionIDs       []string
	// FeedGrants and DeclaredCapabilities are server-owned admission evidence.
	// They are stamped at authentication and retained with the receipt; DATA
	// records cannot add a feed or claim hardware the control plane did not grant.
	FeedGrants           []string
	DeclaredCapabilities []Signal
	CredentialTier       CredentialTier
	// CredentialFingerprint is the lowercase SHA-256 of the exact operational
	// leaf certificate used for this session. It is empty for token/local
	// sessions and is resolved by the collector, never accepted from DATA.
	CredentialFingerprint string
	AttestationTier       AttestationTier
	// HardwareTrust and CommissioningFingerprint are what the collector verified
	// from the evidence presented on this one session (docs/COMMISSIONING.md).
	// They are established once, at the handshake, and stamped on every receipt
	// of that session. No authorization source supplies them: a config row, a
	// control-plane row and a periodic recheck all resolve them as none/empty.
	// CommissioningFingerprint is the lowercase SHA-256 of the verified record,
	// empty whenever HardwareTrust is none.
	HardwareTrust            HardwareTrust
	ManufacturerAuthorityID  string
	CommissioningFingerprint string
	Publication              PublicationPolicy
}

// scopeIDRe is deliberately narrower than arbitrary display text: these ids are
// persisted, logged, used in cache/audience keys, and may later appear in URLs.
var scopeIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,252}$`)
var certificateFingerprintRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidScopeID reports whether an administrative id is safe as an opaque key.
func ValidScopeID(s string) bool { return scopeIDRe.MatchString(s) }

// ValidObserverID is the CERTIFICATE-BINDABLE observer-id contract: the push
// handshake compares an mTLS certificate's single DNS SAN byte-for-byte against
// the station name, so a name that must bind to a certificate is limited to
// 1-253 bytes of ASCII letters, digits, '.' and '-' — no case-fold, no Unicode
// aliases.
//
// It is deliberately NOT the general contract. Bearer-token observer ids are
// opaque by design and preserved exactly (see ValidOpaqueObserverID and
// TestOpaqueSelectionMatchesTokenAndCertificateAdmission), so this applies only
// where certificate binding does.
func ValidObserverID(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '.' || c == '-' {
			continue
		}
		return false
	}
	return true
}

// ValidOpaqueObserverID is the contract every authority boundary applies to an
// observer id, whatever credential minted it.
//
// The rule is deliberately minimal, because opaque identity is a chosen property
// here: an id may contain ':', '/', spaces or non-ASCII text and must survive
// byte-for-byte through state, feeds, events and station selection. What it may
// NOT be is un-round-trippable. Invalid UTF-8 cannot pass through a JSON encoder
// unchanged — Go substitutes U+FFFD — so such an id would be served as something
// that no longer equals the identity it names, and two different malformed ids
// can even serve as the same string.
//
// That is the defect this closes. The serve layer used to run every observer id
// through the lossy display sanitizer, which drops control characters and
// U+FFFD; distinct identities could collapse to one served id, and the Swift
// client's correct duplicate-id rejection then made a single malformed authority
// row poison the whole observers snapshot. Ids are now served verbatim, so the
// only thing that must be refused is an id that cannot be represented at all —
// refused at the boundary, fail-closed, rather than quietly cleaned downstream.
func ValidOpaqueObserverID(s string) bool {
	return s != "" && utf8.ValidString(s)
}

// NewPrivateContext returns the fail-closed context for a local source.
func NewPrivateContext(observerID string, tier CredentialTier) ObserverContext {
	return ObserverContext{
		ObserverID:          observerID,
		OrganizationID:      UnassignedOrganization,
		EnrollmentID:        "config:" + observerID,
		CollectorInstanceID: LocalCollectorInstance,
		FeedGrants:          []string{"local"},
		CredentialTier:      tier,
		AttestationTier:     AttestationNone,
		HardwareTrust:       HardwareTrustNone,
		Publication: PublicationPolicy{
			AggregateUse:    AggregatePrivate,
			StationMetadata: MetadataNone,
			EventVisibility: EventsPrivate,
			RawExport:       RawExportDeny,
			Revision:        "config-private-v1",
		},
	}
}

// Normalize validates an externally constructed context and fills only the
// fail-closed defaults. It never defaults an observation to public.
func (c ObserverContext) Normalize() (ObserverContext, error) {
	// Normalize canonicalizes set-like slices. Copy them first so validating a
	// cached/session context can never mutate the caller through a shared backing
	// array (RawFrame contexts are immutable receipt evidence).
	c.CollectionIDs = append([]string(nil), c.CollectionIDs...)
	c.FeedGrants = append([]string(nil), c.FeedGrants...)
	c.DeclaredCapabilities = append([]Signal(nil), c.DeclaredCapabilities...)
	c.Publication.FederationPeers = append([]string(nil), c.Publication.FederationPeers...)
	c.Publication.Signals = append([]Signal(nil), c.Publication.Signals...)
	// the control-plane authority boundary. This checked only for
	// emptiness, so a token-authorized row could carry an observer id containing
	// control characters or invalid UTF-8; the serve layer then had to sanitize it
	// for display, and two distinct ids could collapse to one served id. Applying
	// the canonical contract here means an authorized id is always usable verbatim
	// and a malformed row fails closed on its own, without poisoning any other
	// station's snapshot.
	if c.ObserverID == "" {
		return c, fmt.Errorf("observer id is required")
	}
	if !ValidOpaqueObserverID(c.ObserverID) {
		return c, fmt.Errorf("observer id is not valid UTF-8 and cannot round-trip to the served identity")
	}
	if c.OrganizationID == "" {
		c.OrganizationID = UnassignedOrganization
	}
	if c.EnrollmentID == "" {
		c.EnrollmentID = "config:" + c.ObserverID
	}
	if c.CollectorInstanceID == "" {
		c.CollectorInstanceID = LocalCollectorInstance
	}
	for field, value := range map[string]string{
		"organization":       c.OrganizationID,
		"enrollment":         c.EnrollmentID,
		"collector_instance": c.CollectorInstanceID,
	} {
		if !ValidScopeID(value) {
			return c, fmt.Errorf("%s id %q is invalid", field, value)
		}
	}
	seenCollections := make(map[string]bool, len(c.CollectionIDs))
	if len(c.CollectionIDs) > 64 {
		return c, fmt.Errorf("collection membership count %d exceeds 64", len(c.CollectionIDs))
	}
	for _, id := range c.CollectionIDs {
		if !ValidScopeID(id) {
			return c, fmt.Errorf("collection id %q is invalid", id)
		}
		if seenCollections[id] {
			return c, fmt.Errorf("collection id %q is duplicated", id)
		}
		seenCollections[id] = true
	}
	sort.Strings(c.CollectionIDs)
	if len(c.FeedGrants) == 0 {
		return c, fmt.Errorf("at least one feed grant is required")
	}
	if len(c.FeedGrants) > 16 {
		return c, fmt.Errorf("feed grant count %d exceeds 16", len(c.FeedGrants))
	}
	seenFeeds := make(map[string]bool, len(c.FeedGrants))
	for _, feed := range c.FeedGrants {
		if !ValidScopeID(feed) {
			return c, fmt.Errorf("feed grant %q is invalid", feed)
		}
		if seenFeeds[feed] {
			return c, fmt.Errorf("feed grant %q is duplicated", feed)
		}
		seenFeeds[feed] = true
	}
	sort.Strings(c.FeedGrants)
	if len(c.DeclaredCapabilities) > 256 {
		return c, fmt.Errorf("declared capability count %d exceeds 256", len(c.DeclaredCapabilities))
	}
	seenCapabilities := make(map[Signal]bool, len(c.DeclaredCapabilities))
	for _, capability := range c.DeclaredCapabilities {
		if capability.GnssID < 0 || capability.GnssID > 7 || capability.SigID < 0 || capability.SigID > 255 {
			return c, fmt.Errorf("declared capability %d:%d is outside the wire domain", capability.GnssID, capability.SigID)
		}
		if seenCapabilities[capability] {
			return c, fmt.Errorf("declared capability %d:%d is duplicated", capability.GnssID, capability.SigID)
		}
		seenCapabilities[capability] = true
	}
	sort.Slice(c.DeclaredCapabilities, func(i, j int) bool {
		if c.DeclaredCapabilities[i].GnssID != c.DeclaredCapabilities[j].GnssID {
			return c.DeclaredCapabilities[i].GnssID < c.DeclaredCapabilities[j].GnssID
		}
		return c.DeclaredCapabilities[i].SigID < c.DeclaredCapabilities[j].SigID
	})
	if c.CredentialTier == "" {
		c.CredentialTier = CredentialToken
	}
	switch c.CredentialTier {
	case CredentialLocalDial, CredentialToken, CredentialSoftwareMTLS, CredentialHardwareMTLS:
	default:
		return c, fmt.Errorf("credential tier %q is invalid", c.CredentialTier)
	}
	if c.CredentialFingerprint != "" && !certificateFingerprintRe.MatchString(c.CredentialFingerprint) {
		return c, fmt.Errorf("credential fingerprint must be 64 lowercase hexadecimal characters")
	}
	if c.AttestationTier == "" {
		c.AttestationTier = AttestationNone
	}
	switch c.AttestationTier {
	case AttestationNone, AttestationVerifiedV1Core:
	default:
		return c, fmt.Errorf("attestation tier %q is invalid", c.AttestationTier)
	}
	if c.HardwareTrust == "" {
		c.HardwareTrust = HardwareTrustNone
	}
	switch c.HardwareTrust {
	case HardwareTrustNone, HardwareTrustOpen, HardwareTrustTest, HardwareTrustTrusted:
	default:
		return c, fmt.Errorf("hardware trust %q is invalid", c.HardwareTrust)
	}
	// A fingerprint names the record that established the trust, so the two
	// are present together: trust without a record, or a record that proved
	// nothing, is a construction error rather than a weaker form of evidence.
	if (c.HardwareTrust == HardwareTrustNone) != (c.CommissioningFingerprint == "") {
		return c, fmt.Errorf("commissioning fingerprint must be present exactly when hardware trust is established")
	}
	if (c.HardwareTrust == HardwareTrustNone) != (c.ManufacturerAuthorityID == "") {
		return c, fmt.Errorf("manufacturer authority id must be present exactly when hardware trust is established")
	}
	if c.ManufacturerAuthorityID != "" && !ValidScopeID(c.ManufacturerAuthorityID) {
		return c, fmt.Errorf("manufacturer authority id %q is invalid", c.ManufacturerAuthorityID)
	}
	if c.CommissioningFingerprint != "" && !certificateFingerprintRe.MatchString(c.CommissioningFingerprint) {
		return c, fmt.Errorf("commissioning fingerprint must be 64 lowercase hexadecimal characters")
	}
	if c.Publication.AggregateUse == "" {
		c.Publication.AggregateUse = AggregatePrivate
	}
	switch c.Publication.AggregateUse {
	case AggregatePrivate, AggregatePublicAnonymous, AggregatePublicAttributed:
	default:
		return c, fmt.Errorf("aggregate use %q is invalid", c.Publication.AggregateUse)
	}
	if c.Publication.StationMetadata == "" {
		c.Publication.StationMetadata = MetadataNone
	}
	switch c.Publication.StationMetadata {
	case MetadataNone, MetadataCoarse, MetadataFull:
	default:
		return c, fmt.Errorf("station metadata %q is invalid", c.Publication.StationMetadata)
	}
	if c.Publication.AggregateUse == AggregatePublicAttributed &&
		c.Publication.StationMetadata != MetadataCoarse &&
		c.Publication.StationMetadata != MetadataFull {
		return c, fmt.Errorf("public_attributed aggregate use requires coarse or full station metadata")
	}
	if c.Publication.EventVisibility == "" {
		c.Publication.EventVisibility = EventsPrivate
	}
	switch c.Publication.EventVisibility {
	case EventsPrivate, EventsPublicRedacted, EventsPublic:
	default:
		return c, fmt.Errorf("event visibility %q is invalid", c.Publication.EventVisibility)
	}
	if c.Publication.AggregateUse == AggregatePrivate && c.Publication.EventVisibility != EventsPrivate {
		return c, fmt.Errorf("public event visibility requires public aggregate use")
	}
	if c.Publication.RawExport == "" {
		c.Publication.RawExport = RawExportDeny
	}
	switch c.Publication.RawExport {
	case RawExportDeny, RawExportNamedPeers, RawExportPublic:
	default:
		return c, fmt.Errorf("raw export %q is invalid", c.Publication.RawExport)
	}
	seenPeers := make(map[string]bool, len(c.Publication.FederationPeers))
	for _, peer := range c.Publication.FederationPeers {
		if !ValidScopeID(peer) {
			return c, fmt.Errorf("federation peer id %q is invalid", peer)
		}
		if seenPeers[peer] {
			return c, fmt.Errorf("federation peer id %q is duplicated", peer)
		}
		seenPeers[peer] = true
	}
	sort.Strings(c.Publication.FederationPeers)
	if c.Publication.RawExport == RawExportNamedPeers && len(c.Publication.FederationPeers) == 0 {
		return c, fmt.Errorf("named_peers raw export requires at least one federation peer")
	}
	seenSignals := make(map[Signal]bool, len(c.Publication.Signals))
	for _, signal := range c.Publication.Signals {
		if signal.GnssID < 0 || signal.GnssID > 7 || signal.SigID < 0 || signal.SigID > 255 {
			return c, fmt.Errorf("publication signal %d:%d is outside the wire domain", signal.GnssID, signal.SigID)
		}
		if seenSignals[signal] {
			return c, fmt.Errorf("publication signal %d:%d is duplicated", signal.GnssID, signal.SigID)
		}
		seenSignals[signal] = true
	}
	sort.Slice(c.Publication.Signals, func(i, j int) bool {
		if c.Publication.Signals[i].GnssID != c.Publication.Signals[j].GnssID {
			return c.Publication.Signals[i].GnssID < c.Publication.Signals[j].GnssID
		}
		return c.Publication.Signals[i].SigID < c.Publication.Signals[j].SigID
	})
	if c.Publication.Revision == "" {
		c.Publication.Revision = "config-private-v1"
	}
	if !ValidScopeID(c.Publication.Revision) {
		return c, fmt.Errorf("policy revision %q is invalid", c.Publication.Revision)
	}
	return c, nil
}

// PublicEligible reports whether this observation may affect public state.
func (c ObserverContext) PublicEligible() bool {
	return c.Publication.AggregateUse == AggregatePublicAnonymous ||
		c.Publication.AggregateUse == AggregatePublicAttributed
}

// PublicAttributed reports whether a public view may retain this observer id.
func (c ObserverContext) PublicAttributed() bool {
	return c.Publication.AggregateUse == AggregatePublicAttributed
}

// WithSessionEvidence returns a copy stamped with what the collector verified
// from this session's hardware evidence. fingerprint is the lowercase SHA-256 of
// the verified commissioning record and must be empty exactly when trust is
// none; the result is normalized so a malformed pair can never reach a receipt.
func (c ObserverContext) WithSessionEvidence(trust HardwareTrust, manufacturerAuthorityID, fingerprint string) (ObserverContext, error) {
	c.HardwareTrust, c.ManufacturerAuthorityID, c.CommissioningFingerprint = trust, manufacturerAuthorityID, fingerprint
	return c.Normalize()
}

// WithCredentialTier returns a copy stamped with the session's actual proof tier.
func (c ObserverContext) WithCredentialTier(tier CredentialTier) ObserverContext {
	c.CredentialTier = tier
	return c
}

// AuthorizationEqual compares every server-resolved authorization/evidence
// field. Active sessions close when this becomes false so changed ownership,
// membership, credentials, attestation, or publication takes effect within the
// documented cache/recheck bound.
//
// HardwareTrust, ManufacturerAuthorityID and CommissioningFingerprint are deliberately not compared.
// They are session evidence, not authorization: no authorization source ever
// resolves them, so including them would make every periodic recheck of a
// commissioned device look like a policy change, and would let one observer's
// sessions with different evidence (a second feed, or a reconnect after a
// registry update) cancel each other and reset the observer's audiences.
// Neither value selects an audience or a publication rule, so a difference
// needs no scope barrier; each receipt simply carries what its own session
// proved. WithSessionEvidence is the only way they enter a context.
func (c ObserverContext) AuthorizationEqual(other ObserverContext) bool {
	return c.ObserverID == other.ObserverID && c.OrganizationID == other.OrganizationID &&
		c.EnrollmentID == other.EnrollmentID && c.CollectorInstanceID == other.CollectorInstanceID &&
		c.CredentialTier == other.CredentialTier && c.CredentialFingerprint == other.CredentialFingerprint &&
		c.AttestationTier == other.AttestationTier &&
		c.Publication.AggregateUse == other.Publication.AggregateUse &&
		c.Publication.StationMetadata == other.Publication.StationMetadata &&
		c.Publication.EventVisibility == other.Publication.EventVisibility &&
		c.Publication.RawExport == other.Publication.RawExport &&
		c.Publication.Revision == other.Publication.Revision &&
		equalStrings(c.CollectionIDs, other.CollectionIDs) &&
		equalStrings(c.FeedGrants, other.FeedGrants) &&
		equalSignals(c.DeclaredCapabilities, other.DeclaredCapabilities) &&
		equalStrings(c.Publication.FederationPeers, other.Publication.FederationPeers) &&
		equalSignals(c.Publication.Signals, other.Publication.Signals)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalSignals(a, b []Signal) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// AllowsSignal reports whether this policy permits the signal. Empty means all
// supported signals, matching the normative publication contract.
func (p PublicationPolicy) AllowsSignal(gnssID, sigID int) bool {
	if len(p.Signals) == 0 {
		return true
	}
	for _, signal := range p.Signals {
		if signal.GnssID == gnssID && signal.SigID == sigID {
			return true
		}
	}
	return false
}

// NamesFederationPeer checks the explicit receipt/current peer allow-list.
func (p PublicationPolicy) NamesFederationPeer(peer string) bool {
	for _, allowed := range p.FederationPeers {
		if allowed == peer {
			return true
		}
	}
	return false
}
