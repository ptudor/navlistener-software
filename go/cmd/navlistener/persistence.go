package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/stationcontrol"
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
	authorityEvidence, _ := json.Marshal(map[string]any{
		"issuer_spki": observer.IssuerSPKI, "core_signer_spki": observer.CoreSignerSPKI,
		"core_attestation_fingerprint": observer.CoreAttestationFingerprint,
		"commissioning_signer_spki":    observer.CommissioningSignerSPKI,
		"registry_signer_spki":         observer.RegistrySignerSPKI,
		"product":                      observer.HardwareProduct, "revision": observer.HardwareRevision,
	})
	saved := &store.NavFrame{
		OperationalAuthorityID: observer.OperationalAuthorityID,
		AuthorityEvidence:      string(authorityEvidence),
		Ts:                     time.Now(),
		ReceivedAt:             f.Recv,
		SourceID:               f.Source,
		OrganizationID:         observer.OrganizationID,
		EnrollmentID:           observer.EnrollmentID,
		CollectorInstanceID:    observer.CollectorInstanceID,
		CollectionIDs:          append([]string(nil), observer.CollectionIDs...),
		FeedGrants:             append([]string(nil), observer.FeedGrants...),
		DeclaredCapabilities:   policySignalStrings(observer.DeclaredCapabilities),
		Provenance:             "local",
		CredentialTier:         string(observer.CredentialTier),
		CredentialFingerprint:  observer.CredentialFingerprint,
		AttestationTier:        string(observer.AttestationTier),
		// Verified from this session's hardware evidence at the handshake.
		HardwareTrust:            string(observer.HardwareTrust),
		ManufacturerAuthorityID:  observer.ManufacturerAuthorityID,
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
	if f.RF != nil {
		kind := "combined"
		switch {
		case f.MsgType == ingest.TelemReceptionData || len(f.RF.Sats) > 0 && len(f.RF.Bands) == 0:
			kind = "reception"
		case f.MsgType == ingest.TelemJammingStats || len(f.RF.Bands) > 0 && len(f.RF.Sats) == 0:
			kind = "jamming"
		}
		data, err := json.Marshal(f.RF)
		if err != nil {
			panic(err)
		}
		saved.RF = &store.RFSample{Kind: kind, Data: data}
		// Push frames retain their exact telemetry body in Bytes. Dial-mode
		// receiver parsers normalize the same values into that canonical body.
		if len(saved.Raw) == 0 {
			switch kind {
			case "reception":
				saved.Raw = ingest.EncodeReceptionData(f.RF.Sats)
			case "jamming":
				saved.Raw = ingest.EncodeJammingStats(f.RF.Bands)
			default:
				// No current receiver message combines these inputs. Preserve a
				// deterministic decoded projection if a future connector does.
				saved.Raw = data
			}
		}
	}
	return saved
}

func restoreReceptionPowerModels(ctx context.Context, historian *store.Store, manager *stationcontrol.Manager, log *slog.Logger) error {
	models, err := historian.LoadReceptionPowerModels(ctx)
	if err != nil {
		return err
	}
	for _, saved := range models {
		if _, err := manager.RestorePowerModel(saved.SourceID, saved.ModelID, saved.Data); err != nil {
			// Configuration/antenna epoch changes intentionally make the old
			// blob inapplicable. Keep it for forensics and start that station cold.
			log.Warn("stored reception power model is not applicable", "observer", saved.SourceID, "error", err)
		}
	}
	return nil
}

func saveReceptionPowerModels(ctx context.Context, historian *store.Store, manager *stationcontrol.Manager, dirtyOnly bool) error {
	models, err := manager.PowerModels(dirtyOnly)
	if err != nil {
		return err
	}
	var failures []error
	for _, model := range models {
		err := historian.SaveReceptionPowerModel(ctx, store.ReceptionPowerModel{
			SourceID: model.Observer, UpdatedAt: time.Now(), ModelID: model.ModelID, Data: model.Data,
		})
		if err != nil {
			failures = append(failures, errors.New(model.Observer+": "+err.Error()))
			continue
		}
		manager.MarkPowerModelSaved(model.Observer, model.Revision)
	}
	return errors.Join(failures...)
}

func receptionPowerModelLoop(ctx context.Context, historian *store.Store, manager *stationcontrol.Manager, log *slog.Logger) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			saveCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := saveReceptionPowerModels(saveCtx, historian, manager, true)
			cancel()
			if err != nil && ctx.Err() == nil {
				log.Error("reception power model checkpoint failed", "error", err)
			}
		}
	}
}
