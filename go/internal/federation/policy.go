// Package federation contains the transport-independent outbound authorization
// gate. No peer transport may send an observation unless EvaluateExport allows
// it. Inbound trust and outbound licensing are deliberately separate edges.
package federation

import (
	"fmt"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
)

type DataClass string

const (
	DataAggregate     DataClass = "aggregate"
	DataDecoded       DataClass = "decoded"
	DataRaw           DataClass = "raw"
	DataSignedRaw     DataClass = "signed_raw"
	DataCertDirectory DataClass = "cert_directory"
)

type Attribution string

const (
	AttributionAnonymous      Attribution = "anonymous"
	AttributionOriginID       Attribution = "origin_id"
	AttributionFullProvenance Attribution = "full_provenance"
)

// ReceivedTrust is an inbound classification only. It can quarantine/restrict a
// relayed observation, but even Trusted never creates an outbound grant.
type ReceivedTrust string

const (
	TrustLocal    ReceivedTrust = "local"
	TrustTrusted  ReceivedTrust = "trusted"
	TrustReadOnly ReceivedTrust = "read_only"
	TrustPending  ReceivedTrust = "pending"
	TrustRevoked  ReceivedTrust = "revoked"
)

// Observation is the immutable receipt plus relay provenance being considered.
type Observation struct {
	Context                  identity.ObserverContext
	LocalCollectorInstanceID string
	OriginPeerID             string
	ReceivedTrust            ReceivedTrust
	Path                     []string
	GnssID                   int
	SigID                    int
	EndToEndObserverSigned   bool
}

// ExportGrant is one explicit directed licensing edge. At least one source
// selector and one purpose are required; absence is not a wildcard.
type ExportGrant struct {
	SourceCollectorInstanceID string
	DestinationPeerID         string
	OrganizationIDs           []string
	CollectionIDs             []string
	ObserverIDs               []string
	Signals                   []identity.Signal // empty = all signals allowed by source policy
	DataClasses               []DataClass
	MaxAttribution            Attribution
	MaxRetention              time.Duration
	Purposes                  []string
	ValidFrom                 time.Time
	ValidUntil                time.Time
	ApprovedBy                string
	Revision                  string
	Enabled                   bool
}

// ExportRequest is the peer subscription after it has already been narrowed to
// one data class/signal/purpose. A request can never widen the grant.
type ExportRequest struct {
	DestinationPeerID string
	DataClass         DataClass
	Attribution       Attribution
	Retention         time.Duration
	Purpose           string
}

type Decision struct {
	Allowed                    bool
	Reason                     string
	EffectiveAttribution       Attribution
	PreservesEndToEndSignature bool
	HardwareAuthenticated      bool
}

// EvaluateExport applies the receipt-policy ∩ current-policy ∩ destination-grant
// rule. Current policy can narrow old data; it can never widen a private receipt.
func EvaluateExport(now time.Time, observation Observation, current identity.PublicationPolicy, grant ExportGrant, request ExportRequest) Decision {
	deny := func(reason string) Decision { return Decision{Reason: reason} }

	receipt, err := observation.Context.Normalize()
	if err != nil {
		return deny("invalid receipt context: " + err.Error())
	}
	currentContext := receipt
	currentContext.Publication = current
	currentContext, err = currentContext.Normalize()
	if err != nil {
		return deny("invalid current policy: " + err.Error())
	}
	if request.DestinationPeerID == "" || request.DestinationPeerID != grant.DestinationPeerID {
		return deny("destination has no matching export grant")
	}
	localCollector := observation.LocalCollectorInstanceID
	if localCollector == "" {
		localCollector = receipt.CollectorInstanceID
	}
	if grant.SourceCollectorInstanceID != localCollector {
		return deny("grant belongs to a different source collector")
	}
	if !grant.Enabled {
		return deny("export grant is disabled")
	}
	if !grant.ValidFrom.IsZero() && now.Before(grant.ValidFrom) {
		return deny("export grant is not yet valid")
	}
	if !grant.ValidUntil.IsZero() && !now.Before(grant.ValidUntil) {
		return deny("export grant has expired")
	}
	if grant.Revision == "" || grant.ApprovedBy == "" {
		return deny("export grant lacks approval provenance")
	}
	switch observation.ReceivedTrust {
	case TrustLocal, TrustTrusted, TrustReadOnly:
	case TrustPending, TrustRevoked:
		return deny("observation is quarantined by inbound trust")
	default:
		return deny("observation has unknown inbound provenance")
	}
	for _, hop := range observation.Path {
		if hop == request.DestinationPeerID {
			return deny("federation path would loop to destination")
		}
	}
	if !matchesSource(grant, receipt) {
		return deny("grant does not select this organization, collection, or observer")
	}
	if !containsDataClass(grant.DataClasses, request.DataClass) {
		return deny("requested data class is not granted")
	}
	if !allowsSignal(grant.Signals, observation.GnssID, observation.SigID) {
		return deny("signal is not granted")
	}
	if !receipt.Publication.AllowsSignal(observation.GnssID, observation.SigID) ||
		!currentContext.Publication.AllowsSignal(observation.GnssID, observation.SigID) {
		return deny("signal is excluded by receipt or current source policy")
	}
	if !containsString(grant.Purposes, request.Purpose) || request.Purpose == "" {
		return deny("purpose is not granted")
	}
	if request.Retention <= 0 || grant.MaxRetention <= 0 || request.Retention > grant.MaxRetention {
		return deny("retention exceeds the destination grant")
	}
	if attributionRank(request.Attribution) < 0 || attributionRank(request.Attribution) > attributionRank(grant.MaxAttribution) {
		return deny("requested attribution exceeds the destination grant")
	}
	if !policyAllows(receipt.Publication, request, request.DestinationPeerID) ||
		!policyAllows(currentContext.Publication, request, request.DestinationPeerID) {
		return deny("receipt or current source policy denies this export")
	}
	if request.DataClass == DataSignedRaw && request.Attribution != AttributionFullProvenance {
		return deny("signed_raw requires full provenance; signatures cannot be anonymized honestly")
	}
	if request.DataClass == DataSignedRaw && !observation.EndToEndObserverSigned {
		return deny("signed_raw requested without an end-to-end observer signature")
	}
	if request.DataClass == DataCertDirectory && request.Attribution != AttributionFullProvenance {
		return deny("certificate directory export requires full provenance")
	}

	preserveSignature := request.DataClass == DataSignedRaw && observation.EndToEndObserverSigned
	hardware := preserveSignature && receipt.CredentialTier == identity.CredentialHardwareMTLS &&
		receipt.AttestationTier != identity.AttestationNone
	return Decision{
		Allowed:                    true,
		Reason:                     "receipt policy, current policy, and destination grant intersect",
		EffectiveAttribution:       request.Attribution,
		PreservesEndToEndSignature: preserveSignature,
		HardwareAuthenticated:      hardware,
	}
}

func policyAllows(policy identity.PublicationPolicy, request ExportRequest, peer string) bool {
	switch request.DataClass {
	case DataAggregate:
		if policy.AggregateUse == identity.AggregatePrivate && !policy.NamesFederationPeer(peer) {
			return false
		}
		if policy.AggregateUse == identity.AggregatePublicAnonymous && request.Attribution != AttributionAnonymous {
			return false
		}
		return true
	case DataDecoded, DataRaw, DataSignedRaw, DataCertDirectory:
		switch policy.RawExport {
		case identity.RawExportPublic:
			return true
		case identity.RawExportNamedPeers:
			return policy.NamesFederationPeer(peer)
		default:
			return false
		}
	default:
		return false
	}
}

func matchesSource(grant ExportGrant, c identity.ObserverContext) bool {
	if len(grant.OrganizationIDs)+len(grant.CollectionIDs)+len(grant.ObserverIDs) == 0 {
		return false
	}
	if containsString(grant.OrganizationIDs, c.OrganizationID) || containsString(grant.ObserverIDs, c.ObserverID) {
		return true
	}
	for _, collection := range c.CollectionIDs {
		if containsString(grant.CollectionIDs, collection) {
			return true
		}
	}
	return false
}

func allowsSignal(signals []identity.Signal, gnssID, sigID int) bool {
	if len(signals) == 0 {
		return true
	}
	for _, signal := range signals {
		if signal.GnssID == gnssID && signal.SigID == sigID {
			return true
		}
	}
	return false
}

func containsDataClass(haystack []DataClass, needle DataClass) bool {
	for _, value := range haystack {
		if value == needle {
			return true
		}
	}
	return false
}

func containsString(haystack []string, needle string) bool {
	for _, value := range haystack {
		if value == needle {
			return true
		}
	}
	return false
}

func attributionRank(a Attribution) int {
	switch a {
	case AttributionAnonymous:
		return 0
	case AttributionOriginID:
		return 1
	case AttributionFullProvenance:
		return 2
	default:
		return -1
	}
}

// ValidateGrant catches unsafe or nonsensical control-plane rows before they
// can enter an evaluator cache.
func ValidateGrant(g ExportGrant) error {
	for name, value := range map[string]string{
		"source collector": g.SourceCollectorInstanceID,
		"destination peer": g.DestinationPeerID,
		"approved by":      g.ApprovedBy,
		"revision":         g.Revision,
	} {
		if !identity.ValidScopeID(value) {
			return fmt.Errorf("%s id %q is invalid", name, value)
		}
	}
	if len(g.OrganizationIDs)+len(g.CollectionIDs)+len(g.ObserverIDs) == 0 {
		return fmt.Errorf("at least one organization, collection, or observer selector is required")
	}
	if g.SourceCollectorInstanceID == g.DestinationPeerID {
		return fmt.Errorf("source collector and destination peer must differ")
	}
	for label, values := range map[string][]string{
		"organization": g.OrganizationIDs,
		"collection":   g.CollectionIDs,
		"observer":     g.ObserverIDs,
		"purpose":      g.Purposes,
	} {
		seen := map[string]bool{}
		for _, value := range values {
			if !identity.ValidScopeID(value) {
				return fmt.Errorf("%s selector %q is invalid", label, value)
			}
			if seen[value] {
				return fmt.Errorf("%s selector %q is duplicated", label, value)
			}
			seen[value] = true
		}
	}
	if len(g.DataClasses) == 0 {
		return fmt.Errorf("at least one data class is required")
	}
	seenClasses := map[DataClass]bool{}
	for _, class := range g.DataClasses {
		if !containsDataClass([]DataClass{DataAggregate, DataDecoded, DataRaw, DataSignedRaw, DataCertDirectory}, class) {
			return fmt.Errorf("data class %q is invalid", class)
		}
		if seenClasses[class] {
			return fmt.Errorf("data class %q is duplicated", class)
		}
		seenClasses[class] = true
	}
	if attributionRank(g.MaxAttribution) < 0 {
		return fmt.Errorf("attribution %q is invalid", g.MaxAttribution)
	}
	if g.MaxRetention <= 0 {
		return fmt.Errorf("max retention must be positive")
	}
	if len(g.Purposes) == 0 {
		return fmt.Errorf("at least one purpose is required")
	}
	if !g.ValidUntil.IsZero() && !g.ValidFrom.IsZero() && !g.ValidUntil.After(g.ValidFrom) {
		return fmt.Errorf("valid_until must be after valid_from")
	}
	seenSignals := map[identity.Signal]bool{}
	for _, signal := range g.Signals {
		if signal.GnssID < 0 || signal.GnssID > 7 || signal.SigID < 0 || signal.SigID > 255 {
			return fmt.Errorf("signal %d:%d is outside the wire domain", signal.GnssID, signal.SigID)
		}
		if seenSignals[signal] {
			return fmt.Errorf("signal %d:%d is duplicated", signal.GnssID, signal.SigID)
		}
		seenSignals[signal] = true
	}
	return nil
}
