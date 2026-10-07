// Package control owns NavListen enrollment independently of any web framework.
// Operators provide controlled bench validation; CA private keys remain in hardware.
package control

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ptudor/navlistener/internal/attestation"
	"github.com/ptudor/navlistener/internal/authority"
	"github.com/ptudor/navlistener/internal/boardid"
	"github.com/ptudor/navlistener/internal/commissioning"
	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/identity"
)

//go:embed schema.sql
var schema string

type Service struct {
	DB            *pgxpool.Pool
	Authorities   *authority.Set
	Manufacturers config.ManufacturerAuthorities
	Now           func() time.Time
	// Log receives the detail of failures the API answers only in summary, such
	// as a key file or registry it could not read; nil selects slog.Default.
	Log *slog.Logger
}

// Request is accepted only from an authenticated enrollment operator. Hardware
// validation identifies the controlled live-read/CSR check in the bench ledger;
// it is an operator assertion, never a device's proof of non-extractability.
type Request struct {
	ObserverID              string                     `json:"observer_id"`
	OperationalAuthorityID  string                     `json:"operational_authority_id"`
	ManufacturerAuthorityID string                     `json:"manufacturer_authority_id"`
	OrganizationID          string                     `json:"organization_id"`
	CollectorInstanceID     string                     `json:"collector_instance_id"`
	ReplaceEnrollmentID     string                     `json:"replace_enrollment_id"`
	ServiceAction           string                     `json:"service_action"`
	ServiceApproval         string                     `json:"service_approval"`
	HardwareValidation      string                     `json:"hardware_validation"`
	Product                 uint16                     `json:"product"`
	Revision                uint16                     `json:"revision"`
	BoardUIDKind            string                     `json:"board_uid_kind"`
	BoardUID                string                     `json:"board_uid"`
	ATECCSerial             string                     `json:"atecc_serial"`
	CoreRecord              string                     `json:"core_record"`
	CommissioningRecord     string                     `json:"commissioning_record"`
	CSRPEM                  string                     `json:"csr_pem"`
	CertificatePEM          string                     `json:"certificate_pem"`
	FeedGrants              []string                   `json:"feed_grants"`
	CollectionIDs           []string                   `json:"collection_ids"`
	DeclaredCapabilities    []identity.Signal          `json:"declared_capabilities"`
	Publication             identity.PublicationPolicy `json:"publication"`
}

type Validated struct {
	Context            identity.ObserverContext
	Core               []byte
	Commission         []byte
	Statement          commissioning.Statement
	CertificateExpires *time.Time
	RegistrySequence   uint64
	RegistrySignerSPKI string
}

func decodeHex(value string, dst []byte) error {
	b, err := hex.DecodeString(value)
	if err != nil || len(b) != len(dst) || hex.EncodeToString(b) != value {
		return errors.New("invalid lowercase hexadecimal field")
	}
	copy(dst, b)
	return nil
}

// Validate checks a request without touching the database. A failure the
// operator can correct is an invalidRequest carrying its reason; trust
// material of the control plane's own that cannot be read or verified is
// reported as unavailable with its detail logged, never with its path.
func (s *Service) Validate(r Request) (Validated, error) {
	var out Validated
	if r.ServiceAction != "" && r.ServiceAction != "replace_atecc" {
		return out, invalid("unknown service action")
	}
	if (r.ServiceAction == "replace_atecc") != (r.ServiceApproval != "") || (r.ServiceApproval != "" && !identity.ValidScopeID(r.ServiceApproval)) {
		return out, invalid("ATECC replacement requires an explicit service approval reference")
	}
	if r.ServiceAction != "" && (r.ReplaceEnrollmentID == "" || r.ManufacturerAuthorityID == "") {
		return out, invalid("ATECC replacement requires a previous hardware enrollment")
	}
	if err := s.Authorities.Allows(r.OperationalAuthorityID, r.ManufacturerAuthorityID); err != nil {
		return out, invalid("%v", err)
	}
	if !identity.ValidScopeID(r.OrganizationID) || !identity.ValidScopeID(r.CollectorInstanceID) {
		return out, invalid("organization and collector ids are required")
	}
	c := identity.NewPrivateContext(r.ObserverID, identity.CredentialToken)
	c.OperationalAuthorityID, c.ManufacturerAuthorityID = r.OperationalAuthorityID, r.ManufacturerAuthorityID
	c.OrganizationID, c.CollectorInstanceID = r.OrganizationID, r.CollectorInstanceID
	c.FeedGrants, c.CollectionIDs, c.DeclaredCapabilities, c.Publication = r.FeedGrants, r.CollectionIDs, r.DeclaredCapabilities, r.Publication
	for _, f := range r.FeedGrants {
		if f != "ubx" && f != "rtcm" {
			return out, invalid("unsupported feed grant")
		}
	}
	if r.ManufacturerAuthorityID == "" {
		if r.CoreRecord != "" || r.CommissioningRecord != "" || r.BoardUID != "" || r.BoardUIDKind != "" || r.ATECCSerial != "" || r.Product != 0 || r.Revision != 0 || r.HardwareValidation != "" {
			return out, invalid("software enrollment cannot claim hardware evidence")
		}
		// A board's name is derived from its factory serial and belongs to that
		// board. A software station holding it would present as the board and block
		// the board's own enrollment.
		if boardid.ReservedObserverID(r.ObserverID) {
			return out, invalid("observer ids beginning %q are reserved for hardware enrollment", boardid.ObserverPrefix)
		}
	} else {
		var h *config.HardwareTrust
		for i := range s.Manufacturers {
			if s.Manufacturers[i].ManufacturerAuthorityID == r.ManufacturerAuthorityID && s.Manufacturers[i].Enabled() {
				h = &s.Manufacturers[i]
			}
		}
		if h == nil {
			return out, invalid("unknown or disabled manufacturer authority")
		}
		if !identity.ValidScopeID(r.HardwareValidation) {
			return out, invalid("controlled bench validation reference is required")
		}
		coreID := attestation.HardwareIdentity{Product: r.Product, BoardRevision: r.Revision}
		uid, err := boardid.Parse(r.BoardUIDKind, r.BoardUID)
		if err != nil {
			return out, invalid("%v", err)
		}
		coreID.BoardUID = uid
		if err := decodeHex(r.ATECCSerial, coreID.ATECCSerial[:]); err != nil {
			return out, invalid("atecc_serial: %v", err)
		}
		if commissioning.ObserverID(coreID.BoardUID) != r.ObserverID {
			return out, invalid("hardware observer id must equal the typed board UID")
		}
		var core attestation.Record
		if err := decodeHex(r.CoreRecord, core[:]); err != nil {
			return out, invalid("core_record: %v", err)
		}
		keys, err := commissioning.LoadKeySet(h.ManufacturerKeys)
		if err != nil {
			return out, s.unavailable("manufacturer keys", "could not be loaded", err)
		}
		verified, signer, err := keys.VerifyCore(core, coreID)
		if err != nil {
			return out, invalid("%v", err)
		}
		var record commissioning.Record
		if err := decodeHex(r.CommissioningRecord, record[:]); err != nil {
			return out, invalid("commissioning_record: %v", err)
		}
		statement, err := keys.Verify(record)
		if err != nil {
			return out, invalid("%v", err)
		}
		if statement.Product != commissioning.Product(r.Product) || statement.BoardRevision != r.Revision || statement.BoardUID != coreID.BoardUID || statement.ATECCSerial != coreID.ATECCSerial || statement.Attestation != verified.RecordFingerprint {
			return out, invalid("commissioning does not bind the exact verified core")
		}
		allowed := false
		for _, p := range h.Products {
			allowed = allowed || p.Allows(statement)
		}
		if !allowed {
			return out, invalid("assembly is outside manufacturer's product/revision policy")
		}
		if h.Registry != "" {
			v, err := h.NewVerifier()
			if err != nil {
				return out, s.unavailable("registry keys", "could not be loaded", err)
			}
			if h.RegistryState != "" {
				floor, err := commissioning.ReadRegistryState(h.RegistryState, h.ManufacturerAuthorityID)
				if err != nil {
					return out, s.unavailable("registry state", "could not be read", err)
				}
				v.SetRegistryFloor(floor)
			}
			data, err := os.ReadFile(h.Registry)
			if err != nil {
				return out, s.unavailable("registry", "could not be read", err)
			}
			registry, err := v.LoadRegistry(data)
			if err != nil {
				return out, s.unavailable("registry", "did not verify or is older than the recorded floor", err)
			}
			if err := v.Recheck(commissioning.Result{ManufacturerAuthorityID: h.ManufacturerAuthorityID, Trust: identity.HardwareTrustOpen, Statement: statement, Fingerprint: record.Fingerprint()}); err != nil {
				return out, invalid("%v", err)
			}
			out.RegistrySequence, out.RegistrySignerSPKI = registry.Sequence, registry.SignerSPKI
		}
		c.HardwareProduct, c.HardwareRevision, c.AttestationTier = r.Product, r.Revision, verified.Tier
		c.CoreSignerSPKI, c.CoreAttestationFingerprint = signer, hex.EncodeToString(verified.RecordFingerprint[:])
		out.Core, out.Commission, out.Statement = core[:], record[:], statement
		c.CommissioningSignerSPKI = keys.SignerFingerprint(record.KeyID())
	}
	if (r.CSRPEM == "") != (r.CertificatePEM == "") {
		return out, invalid("CSR and issued certificate must be supplied together")
	}
	if r.CertificatePEM != "" {
		csrBlock, rest := pem.Decode([]byte(r.CSRPEM))
		if csrBlock == nil || len(bytes.TrimSpace(rest)) != 0 || csrBlock.Type != "CERTIFICATE REQUEST" {
			return out, invalid("invalid CSR PEM")
		}
		csr, err := x509.ParseCertificateRequest(csrBlock.Bytes)
		if err != nil || csr.CheckSignature() != nil {
			return out, invalid("CSR proof of possession is invalid")
		}
		if len(csr.DNSNames) != 1 || csr.DNSNames[0] != r.ObserverID || len(csr.IPAddresses)+len(csr.EmailAddresses)+len(csr.URIs) != 0 {
			return out, invalid("CSR must request only the observer DNS SAN")
		}
		if r.ManufacturerAuthorityID != "" {
			key, ok := csr.PublicKey.(*ecdsa.PublicKey)
			if !ok || key.Curve != elliptic.P256() {
				return out, invalid("hardware operational key must be ECDSA P-256")
			}
		}
		certBlock, rest := pem.Decode([]byte(r.CertificatePEM))
		if certBlock == nil || certBlock.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
			return out, invalid("expected one issued leaf certificate")
		}
		cert, err := x509.ParseCertificate(certBlock.Bytes)
		if err != nil {
			return out, invalid("certificate: %v", err)
		}
		if !bytes.Equal(cert.RawSubjectPublicKeyInfo, csr.RawSubjectPublicKeyInfo) || cert.IsCA || cert.KeyUsage != x509.KeyUsageDigitalSignature || len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth || len(cert.UnknownExtKeyUsage) != 0 || len(cert.DNSNames) != 1 || cert.DNSNames[0] != r.ObserverID || len(cert.IPAddresses)+len(cert.EmailAddresses)+len(cert.URIs) != 0 {
			return out, invalid("leaf does not bind the requested key and sole observer DNS SAN")
		}
		chains, err := cert.Verify(x509.VerifyOptions{Roots: s.Authorities.ClientPool(), CurrentTime: s.now(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
		if err != nil {
			return out, invalid("%v", err)
		}
		c.IssuerSPKI, err = s.Authorities.MatchIssuer(chains, r.OperationalAuthorityID)
		if err != nil {
			return out, invalid("%v", err)
		}
		c.CredentialFingerprint = authority.Fingerprint(cert.Raw)
		c.CredentialTier = identity.CredentialSoftwareMTLS
		if c.ManufacturerAuthorityID != "" {
			c.CredentialTier = identity.CredentialHardwareMTLS
		}
		out.CertificateExpires = &cert.NotAfter
	}
	var err error
	if c, err = c.Normalize(); err != nil {
		return out, invalid("%v", err)
	}
	out.Context = c
	return out, nil
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (s *Service) Enroll(ctx context.Context, operator string, r Request) (string, string, error) {
	v, err := s.Validate(r)
	if err != nil {
		return "", "", err
	}
	if !identity.ValidScopeID(operator) {
		return "", "", invalid("operator id is required")
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", "", err
	}
	token := base64.RawURLEncoding.EncodeToString(secret[:])
	digest := sha256.Sum256([]byte(token))
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return "", "", err
	}
	id := hex.EncodeToString(idBytes)
	v.Context.EnrollmentID = id
	snapshot, err := json.Marshal(v.Context)
	if err != nil {
		return "", "", err
	}
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(ctx)
	// Serialize first enrollment and service operations for the global station id.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, r.ObserverID); err != nil {
		return "", "", err
	}
	var prior, oldManufacturer, oldSerial string
	var oldGeneration int64
	var oldCommission, oldCore []byte
	err = tx.QueryRow(ctx, `SELECT COALESCE(manufacturer_authority_id,''),commissioning_generation,commissioning_record,core_record,current_enrollment_id,COALESCE(atecc_serial,'') FROM navl_devices WHERE observer_id=$1 FOR UPDATE`, r.ObserverID).Scan(&oldManufacturer, &oldGeneration, &oldCommission, &oldCore, &prior, &oldSerial)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", "", err
	}
	if err == nil {
		coreChanged := oldManufacturer != r.ManufacturerAuthorityID || !bytes.Equal(oldCore, v.Core)
		if coreChanged != (r.ServiceAction == "replace_atecc") || (coreChanged && (oldManufacturer == "" || oldSerial == r.ATECCSerial)) {
			return "", "", invalid("new permanent core requires approved replacement of the secure element; provenance cannot be relabeled")
		}
		if !bytes.Equal(oldCommission, v.Commission) && uint64(v.Statement.Generation) <= uint64(oldGeneration) {
			return "", "", invalid("replacement commissioning generation must increase")
		}
		if prior == "" || r.ReplaceEnrollmentID != prior {
			return "", "", invalid("existing station requires its current enrollment for an explicit service transition")
		}
	} else if r.ReplaceEnrollmentID != "" {
		return "", "", invalid("replacement enrollment does not exist")
	}
	if v.RegistrySequence > 0 {
		var floor uint64
		err := tx.QueryRow(ctx, `SELECT sequence FROM navl_registry_floors WHERE manufacturer_authority_id=$1 FOR UPDATE`, r.ManufacturerAuthorityID).Scan(&floor)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", "", err
		}
		if v.RegistrySequence < floor {
			return "", "", s.unavailable("registry", "copy is older than the floor this control plane has recorded",
				fmt.Errorf("registry sequence %d is below the recorded floor %d for %s", v.RegistrySequence, floor, r.ManufacturerAuthorityID))
		}
		if _, err := tx.Exec(ctx, `INSERT INTO navl_registry_floors VALUES($1,$2) ON CONFLICT(manufacturer_authority_id) DO UPDATE SET sequence=GREATEST(navl_registry_floors.sequence,EXCLUDED.sequence)`, r.ManufacturerAuthorityID, v.RegistrySequence); err != nil {
			return "", "", err
		}
	}
	var rtc any
	if v.Statement.IdentityFlags&commissioning.IdentityRTCEUIRecorded != 0 {
		rtc = hex.EncodeToString(v.Statement.RTCEUI64[:])
	}
	_, err = tx.Exec(ctx, `INSERT INTO navl_devices(observer_id,manufacturer_authority_id,board_uid,atecc_serial,rtc_eui64,rtc_model_id,hardware_product,hardware_revision,core_record,core_attestation_fingerprint,core_signer_spki,commissioning_record,commissioning_generation,commissioning_signer_spki,current_enrollment_id,board_uid_kind)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
 ON CONFLICT(observer_id) DO UPDATE SET manufacturer_authority_id=EXCLUDED.manufacturer_authority_id,atecc_serial=EXCLUDED.atecc_serial,hardware_product=EXCLUDED.hardware_product,hardware_revision=EXCLUDED.hardware_revision,core_record=EXCLUDED.core_record,core_attestation_fingerprint=EXCLUDED.core_attestation_fingerprint,core_signer_spki=EXCLUDED.core_signer_spki,rtc_eui64=EXCLUDED.rtc_eui64,rtc_model_id=EXCLUDED.rtc_model_id,commissioning_record=EXCLUDED.commissioning_record,commissioning_generation=EXCLUDED.commissioning_generation,commissioning_signer_spki=EXCLUDED.commissioning_signer_spki,current_enrollment_id=EXCLUDED.current_enrollment_id`,
		r.ObserverID, nullable(r.ManufacturerAuthorityID), nullable(r.BoardUID), nullable(r.ATECCSerial), rtc, v.Statement.RTCModel, r.Product, r.Revision, v.Core, v.Context.CoreAttestationFingerprint, v.Context.CoreSignerSPKI, v.Commission, v.Statement.Generation, v.Context.CommissioningSignerSPKI, id, nullable(r.BoardUIDKind))
	if err != nil {
		return "", "", err
	}
	if _, err = tx.Exec(ctx, `UPDATE navl_enrollments SET active=false,revoked_at=now() WHERE observer_id=$1 AND active`, r.ObserverID); err != nil {
		return "", "", err
	}
	c := v.Context
	_, err = tx.Exec(ctx, `INSERT INTO navl_enrollments(id,observer_id,operational_authority_id,manufacturer_authority_id,organization_id,collector_instance_id,token_sha256,issuer_spki,credential_fingerprint,credential_tier,certificate_pem,certificate_expires,attestation_tier,snapshot,feed_grants,collection_ids,declared_capabilities,aggregate_use,station_metadata,event_visibility,raw_export,federation_peers,publish_signals,policy_revision,core_record,commissioning_record,registry_sequence,registry_signer_spki)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28)`,
		id, r.ObserverID, r.OperationalAuthorityID, nullable(r.ManufacturerAuthorityID), r.OrganizationID, r.CollectorInstanceID, hex.EncodeToString(digest[:]), c.IssuerSPKI, c.CredentialFingerprint, c.CredentialTier, r.CertificatePEM, v.CertificateExpires, c.AttestationTier, snapshot, c.FeedGrants, nonNil(c.CollectionIDs), signalStrings(c.DeclaredCapabilities), c.Publication.AggregateUse, c.Publication.StationMetadata, c.Publication.EventVisibility, c.Publication.RawExport, nonNil(c.Publication.FederationPeers), signalStrings(c.Publication.Signals), c.Publication.Revision, v.Core, v.Commission, v.RegistrySequence, v.RegistrySignerSPKI)
	if err != nil {
		return "", "", err
	}
	detail, _ := json.Marshal(map[string]any{"previous_enrollment": prior, "hardware_validation": r.HardwareValidation, "service_action": r.ServiceAction, "service_approval": r.ServiceApproval, "snapshot": v.Context, "registry_sequence": v.RegistrySequence, "registry_signer_spki": v.RegistrySignerSPKI})
	if _, err = tx.Exec(ctx, `INSERT INTO navl_service_events(observer_id,enrollment_id,kind,operator_id,detail) VALUES($1,$2,'enroll',$3,$4)`, r.ObserverID, id, operator, detail); err != nil {
		return "", "", err
	}
	if _, err = tx.Exec(ctx, `NOTIFY navlistener_authorization_changed`); err != nil {
		return "", "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", "", err
	}
	return id, token, nil
}

// CurrentEnrollment is the enrollment a device currently names, for an
// operator whose activation response was lost: the token is returned once and
// never stored, so recovery is a replacement enrollment naming this id, not a
// second copy of the token.
type CurrentEnrollment struct {
	ObserverID   string     `json:"observer_id"`
	EnrollmentID string     `json:"enrollment_id"`
	Active       bool       `json:"active"`
	CreatedAt    time.Time  `json:"created_at"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
}

// CurrentEnrollment reads the enrollment id recorded for observerID. It
// changes nothing and returns no credential.
func (s *Service) CurrentEnrollment(ctx context.Context, observerID string) (CurrentEnrollment, error) {
	var current CurrentEnrollment
	if observerID == "" || len(observerID) > 256 {
		return current, invalid("observer_id is required")
	}
	err := s.DB.QueryRow(ctx, `SELECT d.observer_id,d.current_enrollment_id,e.active,e.created_at,e.revoked_at
 FROM navl_devices d JOIN navl_enrollments e ON e.id=d.current_enrollment_id WHERE d.observer_id=$1`, observerID).
		Scan(&current.ObserverID, &current.EnrollmentID, &current.Active, &current.CreatedAt, &current.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CurrentEnrollment{}, invalid("observer %q has no enrollment", observerID)
	}
	if err != nil {
		return CurrentEnrollment{}, err
	}
	return current, nil
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
