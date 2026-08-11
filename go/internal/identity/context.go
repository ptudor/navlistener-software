// Package identity contains the server-resolved administrative and publication
// context stamped on every observation. It deliberately contains no credentials:
// a feeder proves identity during authentication, then the collector carries this
// immutable result through decode, persistence, and audience selection.
package identity

import (
	"fmt"
	"regexp"
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
	AttestationNone              AttestationTier = "none"
	AttestationVerifiedV1Partial AttestationTier = "verified_v1_partial"
	AttestationVerifiedV2        AttestationTier = "verified_v2_complete"
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
	CredentialTier      CredentialTier
	// CredentialFingerprint is the lowercase SHA-256 of the exact operational
	// leaf certificate used for this session. It is empty for token/local
	// sessions and is resolved by the collector, never accepted from DATA.
	CredentialFingerprint string
	AttestationTier       AttestationTier
	Publication           PublicationPolicy
}

// scopeIDRe is deliberately narrower than arbitrary display text: these ids are
// persisted, logged, used in cache/audience keys, and may later appear in URLs.
var scopeIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,252}$`)
var certificateFingerprintRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidScopeID reports whether an administrative id is safe as an opaque key.
func ValidScopeID(s string) bool { return scopeIDRe.MatchString(s) }

// NewPrivateContext returns the fail-closed context for a local source.
func NewPrivateContext(observerID string, tier CredentialTier) ObserverContext {
	return ObserverContext{
		ObserverID:          observerID,
		OrganizationID:      UnassignedOrganization,
		EnrollmentID:        "config:" + observerID,
		CollectorInstanceID: LocalCollectorInstance,
		CredentialTier:      tier,
		AttestationTier:     AttestationNone,
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
	if c.ObserverID == "" {
		return c, fmt.Errorf("observer id is required")
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
	for _, id := range c.CollectionIDs {
		if !ValidScopeID(id) {
			return c, fmt.Errorf("collection id %q is invalid", id)
		}
		if seenCollections[id] {
			return c, fmt.Errorf("collection id %q is duplicated", id)
		}
		seenCollections[id] = true
	}
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
	case AttestationNone, AttestationVerifiedV1Partial, AttestationVerifiedV2:
	default:
		return c, fmt.Errorf("attestation tier %q is invalid", c.AttestationTier)
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

// WithCredentialTier returns a copy stamped with the session's actual proof tier.
func (c ObserverContext) WithCredentialTier(tier CredentialTier) ObserverContext {
	c.CredentialTier = tier
	return c
}

// AuthorizationEqual compares every server-resolved authorization/evidence
// field. Active sessions close when this becomes false so changed ownership,
// membership, credentials, attestation, or publication takes effect within the
// documented cache/recheck bound.
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
