package federation

import (
	"strings"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
)

func fixture() (time.Time, Observation, identity.PublicationPolicy, ExportGrant, ExportRequest) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	c := identity.NewPrivateContext("observer-a", identity.CredentialHardwareMTLS)
	c.OrganizationID = "customer-a"
	c.EnrollmentID = "enrollment-a"
	c.CollectorInstanceID = "collector-a"
	c.CollectionIDs = []string{"fleet-a"}
	c.AttestationTier = identity.AttestationVerifiedV2
	c.Publication.AggregateUse = identity.AggregatePrivate
	c.Publication.RawExport = identity.RawExportNamedPeers
	c.Publication.FederationPeers = []string{"peer-b"}
	c.Publication.Revision = "receipt-v1"
	current := c.Publication
	current.Revision = "current-v2"
	grant := ExportGrant{
		SourceCollectorInstanceID: "collector-a", DestinationPeerID: "peer-b",
		OrganizationIDs: []string{"customer-a"}, DataClasses: []DataClass{DataAggregate, DataRaw, DataSignedRaw},
		MaxAttribution: AttributionFullProvenance, MaxRetention: 24 * time.Hour,
		Purposes: []string{"integrity-monitoring"}, ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour),
		ApprovedBy: "owner-a", Revision: "grant-v3", Enabled: true,
	}
	obs := Observation{Context: c, ReceivedTrust: TrustLocal, GnssID: 0, SigID: 0, EndToEndObserverSigned: true}
	req := ExportRequest{DestinationPeerID: "peer-b", DataClass: DataRaw, Attribution: AttributionOriginID,
		Retention: time.Hour, Purpose: "integrity-monitoring"}
	return now, obs, current, grant, req
}

func TestIntersectionAllowsExplicitNamedPeer(t *testing.T) {
	now, obs, current, grant, req := fixture()
	got := EvaluateExport(now, obs, current, grant, req)
	if !got.Allowed {
		t.Fatalf("explicit export denied: %+v", got)
	}
}

func TestInboundTrustNeverCreatesOutboundAuthority(t *testing.T) {
	now, obs, current, grant, req := fixture()
	obs.ReceivedTrust = TrustTrusted
	grant.Enabled = false
	got := EvaluateExport(now, obs, current, grant, req)
	if got.Allowed || !strings.Contains(got.Reason, "disabled") {
		t.Fatalf("inbound trust widened outbound access: %+v", got)
	}
}

func TestHistoryIntersectionOnlyNarrows(t *testing.T) {
	now, obs, current, grant, req := fixture()
	obs.Context.Publication.RawExport = identity.RawExportDeny
	current.RawExport = identity.RawExportPublic // later widening must not relabel history
	if got := EvaluateExport(now, obs, current, grant, req); got.Allowed {
		t.Fatalf("current widening exposed private receipt: %+v", got)
	}

	_, obs, current, grant, req = fixture()
	current.RawExport = identity.RawExportDeny // withdrawal narrows immediately
	if got := EvaluateExport(now, obs, current, grant, req); got.Allowed {
		t.Fatalf("current withdrawal did not narrow receipt: %+v", got)
	}
}

func TestGrantScopeSignalPurposeAndRetentionAllIntersect(t *testing.T) {
	now, obs, current, grant, req := fixture()
	grant.Signals = []identity.Signal{{GnssID: 2, SigID: 0}}
	if EvaluateExport(now, obs, current, grant, req).Allowed {
		t.Fatal("ungranted signal exported")
	}
	grant.Signals = nil
	req.Purpose = "advertising"
	if EvaluateExport(now, obs, current, grant, req).Allowed {
		t.Fatal("ungranted purpose exported")
	}
	req.Purpose = "integrity-monitoring"
	req.Retention = 48 * time.Hour
	if EvaluateExport(now, obs, current, grant, req).Allowed {
		t.Fatal("over-retention export allowed")
	}
	req.Retention = time.Hour
	grant.OrganizationIDs = []string{"other"}
	if EvaluateExport(now, obs, current, grant, req).Allowed {
		t.Fatal("out-of-scope organization exported")
	}
}

func TestAnonymousCannotMasqueradeAsSignedHardware(t *testing.T) {
	now, obs, current, grant, req := fixture()
	req.DataClass = DataSignedRaw
	req.Attribution = AttributionAnonymous
	if got := EvaluateExport(now, obs, current, grant, req); got.Allowed {
		t.Fatalf("anonymous signed_raw allowed: %+v", got)
	}
	req.Attribution = AttributionFullProvenance
	got := EvaluateExport(now, obs, current, grant, req)
	if !got.Allowed || !got.PreservesEndToEndSignature || !got.HardwareAuthenticated {
		t.Fatalf("fully attributed signed hardware denied/mislabeled: %+v", got)
	}
}

func TestQuarantineAndLoopPrevention(t *testing.T) {
	now, obs, current, grant, req := fixture()
	obs.ReceivedTrust = TrustPending
	if EvaluateExport(now, obs, current, grant, req).Allowed {
		t.Fatal("pending/quarantined observation relayed")
	}
	obs.ReceivedTrust = TrustTrusted
	obs.Path = []string{"origin", "peer-b"}
	if EvaluateExport(now, obs, current, grant, req).Allowed {
		t.Fatal("looping relay allowed")
	}
	obs.Path = nil
	obs.ReceivedTrust = ""
	if EvaluateExport(now, obs, current, grant, req).Allowed {
		t.Fatal("unknown inbound provenance allowed")
	}
}

func TestPublicPolicyStillNeedsDestinationGrant(t *testing.T) {
	now, obs, current, grant, req := fixture()
	obs.Context.Publication.AggregateUse = identity.AggregatePublicAnonymous
	obs.Context.Publication.RawExport = identity.RawExportPublic
	obs.Context.Publication.FederationPeers = nil
	current = obs.Context.Publication
	req.DataClass = DataAggregate
	req.Attribution = AttributionAnonymous
	grant.Enabled = false
	if EvaluateExport(now, obs, current, grant, req).Allowed {
		t.Fatal("public policy exported without an enabled destination grant")
	}
}

func TestPublicRawPolicyCanUseExplicitGrantWithoutPeerAllowList(t *testing.T) {
	now, obs, current, grant, req := fixture()
	obs.Context.Publication.RawExport = identity.RawExportPublic
	obs.Context.Publication.FederationPeers = nil
	current = obs.Context.Publication
	req.DataClass = DataSignedRaw
	req.Attribution = AttributionFullProvenance
	got := EvaluateExport(now, obs, current, grant, req)
	if !got.Allowed || !got.PreservesEndToEndSignature {
		t.Fatalf("public raw policy did not defer destination selection to explicit grant: %+v", got)
	}
}

func TestValidateGrantRejectsWildcardDefaults(t *testing.T) {
	_, _, _, grant, _ := fixture()
	if err := ValidateGrant(grant); err != nil {
		t.Fatalf("valid grant rejected: %v", err)
	}
	grant.OrganizationIDs = nil
	if err := ValidateGrant(grant); err == nil {
		t.Fatal("selector-free wildcard grant accepted")
	}
}
