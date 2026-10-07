package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/ptudor/navlistener/internal/identity"
)

// invalidRequest marks an error caused by the operator's request rather than by
// the service, so the API can report its reason. Other errors stay generic.
type invalidRequest struct{ error }

func invalid(format string, args ...any) error {
	return invalidRequest{fmt.Errorf(format, args...)}
}

// unavailable marks a failure of the control plane's own trust material: a key
// file, registry or state file it could not read or verify. The operator gets
// a summary that names the material, never the server path; the detail is in
// the control plane's log.
type unavailable struct{ error }

func (s *Service) unavailable(material, reason string, err error) error {
	s.logger().Error("control plane trust material unavailable", "material", material, "error", err)
	return unavailable{fmt.Errorf("%s %s; see the control plane log", material, reason)}
}

func (s *Service) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// PolicyChange restates the operator-controlled policy of one active
// enrollment: its feed grants, collection memberships, declared capabilities and
// publication. Identity, ownership, authorities and evidence are not part of it;
// changing those is a service transition with a new credential.
type PolicyChange struct {
	EnrollmentID         string                     `json:"enrollment_id"`
	FeedGrants           []string                   `json:"feed_grants"`
	CollectionIDs        []string                   `json:"collection_ids"`
	DeclaredCapabilities []identity.Signal          `json:"declared_capabilities"`
	Publication          identity.PublicationPolicy `json:"publication"`
}

// ChangePolicy updates the active enrollment's policy in place and keeps its
// device credential, so adding a station to a collection or changing its
// publication needs no reprovisioning. The new policy must carry a new
// policy_revision: collector receipts stamp the revision they were received
// under, so earlier observations remain attributable to the earlier policy. The
// previous snapshot is kept in the service history.
func (s *Service) ChangePolicy(ctx context.Context, operator string, p PolicyChange) error {
	if !identity.ValidScopeID(operator) || !identity.ValidScopeID(p.EnrollmentID) {
		return invalid("operator and enrollment ids are required")
	}
	if len(p.FeedGrants) == 0 {
		return invalid("at least one feed grant is required")
	}
	for _, f := range p.FeedGrants {
		if f != "ubx" && f != "rtcm" {
			return invalid("unsupported feed grant %q", f)
		}
	}
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var observer string
	if err := tx.QueryRow(ctx, `SELECT observer_id FROM navl_enrollments WHERE id=$1 AND active`, p.EnrollmentID).Scan(&observer); errors.Is(err, pgx.ErrNoRows) {
		return invalid("active enrollment %s not found", p.EnrollmentID)
	} else if err != nil {
		return err
	}
	// The same per-station lock as enrollment and service transitions.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, observer); err != nil {
		return err
	}
	var previous []byte
	var revision string
	if err := tx.QueryRow(ctx, `SELECT snapshot,policy_revision FROM navl_enrollments WHERE id=$1 AND active FOR UPDATE`, p.EnrollmentID).Scan(&previous, &revision); errors.Is(err, pgx.ErrNoRows) {
		return invalid("active enrollment %s not found", p.EnrollmentID)
	} else if err != nil {
		return err
	}
	if p.Publication.Revision == "" || p.Publication.Revision == revision {
		return invalid("a policy change requires a policy_revision other than the current %q", revision)
	}
	// A revision names one policy interval for the receipts stamped under it.
	// Reusing one this enrollment has already carried, in its enrollment
	// snapshot or on either side of an earlier change, would make the stamp
	// span two intervals, so the service history is the record of what has
	// been used.
	var reused bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM navl_service_events WHERE enrollment_id=$1
 AND (detail->'snapshot'->'Publication'->>'policy_revision'=$2 OR detail->'previous'->'Publication'->>'policy_revision'=$2))`,
		p.EnrollmentID, p.Publication.Revision).Scan(&reused); err != nil {
		return err
	}
	if reused {
		return invalid("policy_revision %q has already been used by this enrollment; receipts stamped with it must name one policy interval", p.Publication.Revision)
	}
	var c identity.ObserverContext
	if err := json.Unmarshal(previous, &c); err != nil {
		return fmt.Errorf("stored enrollment snapshot: %w", err)
	}
	c.FeedGrants, c.CollectionIDs, c.DeclaredCapabilities, c.Publication = p.FeedGrants, p.CollectionIDs, p.DeclaredCapabilities, p.Publication
	if c, err = c.Normalize(); err != nil {
		return invalid("%v", err)
	}
	snapshot, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE navl_enrollments SET snapshot=$2,feed_grants=$3,collection_ids=$4,declared_capabilities=$5,aggregate_use=$6,station_metadata=$7,event_visibility=$8,raw_export=$9,federation_peers=$10,publish_signals=$11,policy_revision=$12 WHERE id=$1 AND active`,
		p.EnrollmentID, snapshot, c.FeedGrants, nonNil(c.CollectionIDs), signalStrings(c.DeclaredCapabilities), c.Publication.AggregateUse, c.Publication.StationMetadata, c.Publication.EventVisibility, c.Publication.RawExport, nonNil(c.Publication.FederationPeers), signalStrings(c.Publication.Signals), c.Publication.Revision)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("active enrollment changed concurrently")
	}
	detail, err := json.Marshal(map[string]json.RawMessage{"previous": previous, "snapshot": snapshot})
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO navl_service_events(observer_id,enrollment_id,kind,operator_id,detail) VALUES($1,$2,'policy',$3,$4)`, observer, p.EnrollmentID, operator, detail); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `NOTIFY navlistener_authorization_changed`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReadCredentialRequest names one read principal and its private audience
// grants, as the collector's navlistener_read_authorization_v1 view serves them.
type ReadCredentialRequest struct {
	PrincipalID    string   `json:"principal_id"`
	AudienceGrants []string `json:"audience_grants"`
	Revision       string   `json:"revision"`
}

// IssuedReadCredential is returned once. Only the digest is stored; the digest
// is what a later disable names, so rotation can overlap old and new tokens.
type IssuedReadCredential struct {
	PrincipalID string `json:"principal_id"`
	Token       string `json:"token"`
	TokenSHA256 string `json:"token_sha256"`
}

// CreateReadCredential issues an enabled read credential with the same grant
// rules the collector applies when it loads the view: a valid principal and
// revision, and at least one private audience, none public or repeated.
func (s *Service) CreateReadCredential(ctx context.Context, operator string, r ReadCredentialRequest) (IssuedReadCredential, error) {
	var issued IssuedReadCredential
	if !identity.ValidScopeID(operator) {
		return issued, invalid("operator id is required")
	}
	principal := identity.ReadPrincipal{ID: r.PrincipalID, Revision: r.Revision}
	for _, value := range r.AudienceGrants {
		audience, err := identity.ParseAudience(value)
		if err != nil {
			return issued, invalid("%v", err)
		}
		principal.AudienceGrants = append(principal.AudienceGrants, audience)
	}
	principal, err := identity.NormalizeReadPrincipal(principal)
	if err != nil {
		return issued, invalid("%v", err)
	}
	grants := make([]string, 0, len(principal.AudienceGrants))
	for _, grant := range principal.AudienceGrants {
		grants = append(grants, grant.Key())
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return issued, err
	}
	token := base64.RawURLEncoding.EncodeToString(secret[:])
	digest := sha256.Sum256([]byte(token))
	issued = IssuedReadCredential{PrincipalID: principal.ID, Token: token, TokenSHA256: hex.EncodeToString(digest[:])}
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return IssuedReadCredential{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO navl_read_credentials(token_sha256,principal_id,audience_grants,revision,enabled) VALUES($1,$2,$3,$4,true)`, issued.TokenSHA256, principal.ID, grants, principal.Revision); err != nil {
		return IssuedReadCredential{}, err
	}
	detail, err := json.Marshal(map[string]any{"audience_grants": grants, "revision": principal.Revision})
	if err != nil {
		return IssuedReadCredential{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO navl_read_credential_events(token_sha256,principal_id,kind,operator_id,detail) VALUES($1,$2,'create',$3,$4)`, issued.TokenSHA256, principal.ID, operator, detail); err != nil {
		return IssuedReadCredential{}, err
	}
	if _, err = tx.Exec(ctx, `NOTIFY navlistener_authorization_changed`); err != nil {
		return IssuedReadCredential{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return IssuedReadCredential{}, err
	}
	return issued, nil
}

// DisableReadCredential withdraws one read credential by its digest. The row is
// kept, disabled, so the digest can never be issued again.
func (s *Service) DisableReadCredential(ctx context.Context, operator, tokenSHA256 string) error {
	if !identity.ValidScopeID(operator) {
		return invalid("operator id is required")
	}
	if b, err := hex.DecodeString(tokenSHA256); err != nil || len(b) != sha256.Size || hex.EncodeToString(b) != tokenSHA256 {
		return invalid("token_sha256 must be 64 lowercase hex characters")
	}
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var principal string
	if err := tx.QueryRow(ctx, `UPDATE navl_read_credentials SET enabled=false WHERE token_sha256=$1 AND enabled RETURNING principal_id`, tokenSHA256).Scan(&principal); errors.Is(err, pgx.ErrNoRows) {
		return invalid("no enabled read credential has that digest")
	} else if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO navl_read_credential_events(token_sha256,principal_id,kind,operator_id,detail) VALUES($1,$2,'disable',$3,'{}')`, tokenSHA256, principal, operator); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `NOTIFY navlistener_authorization_changed`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func signalStrings(values []identity.Signal) []string {
	out := []string{}
	for _, v := range values {
		out = append(out, fmt.Sprintf("%d:%d", v.GnssID, v.SigID))
	}
	return out
}
