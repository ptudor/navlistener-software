package main

import (
	"encoding/json"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/store"
	"github.com/ptudor/navlistener/internal/version"
)

// frameForPersistence preserves raw framing and immutable receipt provenance.
func frameForPersistence(f *ingest.RawFrame) *store.NavFrame {
	observer := f.Observer
	if observer.ObserverID == "" {
		// Legacy/programmatic RawFrames have no trusted context. Persistence
		// still records an explicit private/unassigned decision rather than NULL
		// fields that a future reader might accidentally interpret as public.
		observer = identity.NewPrivateContext(f.Source, identity.CredentialLocalDial)
	}
	saved := &store.NavFrame{
		Ts:                    time.Now(),
		ReceivedAt:            f.Recv,
		SourceID:              f.Source,
		OrganizationID:        observer.OrganizationID,
		EnrollmentID:          observer.EnrollmentID,
		CollectorInstanceID:   observer.CollectorInstanceID,
		CollectionIDs:         append([]string(nil), observer.CollectionIDs...),
		FeedGrants:            append([]string(nil), observer.FeedGrants...),
		DeclaredCapabilities:  policySignalStrings(observer.DeclaredCapabilities),
		Provenance:            "local",
		CredentialTier:        string(observer.CredentialTier),
		CredentialFingerprint: observer.CredentialFingerprint,
		AttestationTier:       string(observer.AttestationTier),
		// Verified from this session's hardware evidence at the handshake.
		HardwareTrust:            string(observer.HardwareTrust),
		CommissioningFingerprint: observer.CommissioningFingerprint,
		AggregateUse:             string(observer.Publication.AggregateUse),
		StationMetadata:          string(observer.Publication.StationMetadata),
		EventVisibility:          string(observer.Publication.EventVisibility),
		RawExport:                string(observer.Publication.RawExport),
		FederationPeers:          append([]string(nil), observer.Publication.FederationPeers...),
		PublishSignals:           policySignalStrings(observer.Publication.Signals),
		PolicyRevision:           observer.Publication.Revision,
		GnssID:                   int(f.GnssID),
		SvID:                     f.SvID,
		SigID:                    f.SigID,
		FreqID:                   f.FreqID,
		MsgType:                  persistMsgType(f),
		Raw:                      f.RawBytes(),
		SBFHeader:                append([]byte(nil), f.SBFHeader...),
		DecoderVer:               version.Identity(),
		SourceSeq:                f.Seq,
		HasSourceSeq:             f.HasSeq,
		Session:                  f.Session, // dedup-key third component
	}
	if f.Details != nil {
		kind := "environment"
		if f.Details.Timing != nil {
			kind = "timing"
		}
		data, err := json.Marshal(f.Details)
		if err != nil {
			panic(err)
		} // validated decoder numbers are finite
		saved.Board = &store.BoardSample{Kind: kind, Data: data}
		saved.ReceivedAt = f.LocalRecv()
		if f.BoardSampleStamped {
			stamp := f.Recv
			saved.Board.SampleTime = &stamp
		}
	}
	return saved

}
