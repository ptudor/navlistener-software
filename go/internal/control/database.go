package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/ptudor/navlistener/internal/authority"
	"github.com/ptudor/navlistener/internal/commissioning"
	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/identity"
)

// Initialize installs the fresh schema and publishes the configured authority
// registrations atomically. Historical key ownership cannot be reassigned.
func (s *Service) Initialize(ctx context.Context, cfg *config.Config) error {
	conn, err := s.DB.Acquire(ctx)
	if err != nil {
		return err
	}
	_, err = conn.Conn().PgConn().Exec(ctx, schema).ReadAll()
	conn.Release()
	if err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(719252602)`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE navl_authorities SET enabled=false`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM navl_authority_pairings`); err != nil {
		return err
	}
	register := func(kind, id string, enabled bool, value any) error {
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO navl_authorities VALUES($1,$2,$3,$4) ON CONFLICT(kind,id) DO UPDATE SET enabled=EXCLUDED.enabled,configuration=EXCLUDED.configuration`, kind, id, enabled, data)
		return err
	}
	key := func(pin, kind, id, role string) error {
		var oldKind, oldID, oldRole string
		err := tx.QueryRow(ctx, `SELECT kind,authority_id,role FROM navl_authority_keys WHERE spki=$1`, pin).Scan(&oldKind, &oldID, &oldRole)
		if err == nil {
			if oldKind != kind || oldID != id || oldRole != role {
				return errors.New("key already belongs to another authority or role")
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO navl_authority_keys VALUES($1,$2,$3,$4)`, pin, kind, id, role)
		return err
	}
	for _, m := range cfg.ManufacturerAuthorities {
		if err := register("manufacturer", m.ManufacturerAuthorityID, m.Active, m); err != nil {
			return err
		}
		for role, files := range map[string][]string{"manufacturer": m.ManufacturerKeys, "registry": m.RegistryKeys} {
			if len(files) == 0 {
				continue
			}
			keys, err := commissioning.LoadKeySet(files)
			if err != nil {
				return err
			}
			for _, pub := range keys.PublicKeys() {
				if err := key(commissioning.KeyFingerprint(pub), "manufacturer", m.ManufacturerAuthorityID, role); err != nil {
					return err
				}
			}
		}
	}
	for _, op := range cfg.OperationalAuthorities {
		if err := register("operational", op.ID, op.Enabled, op); err != nil {
			return err
		}
		for _, path := range op.Issuers {
			certs, err := authority.Certificates(path)
			if err != nil {
				return err
			}
			for _, cert := range certs {
				if err := key(authority.Fingerprint(cert.RawSubjectPublicKeyInfo), "operational", op.ID, "issuing"); err != nil {
					return err
				}
			}
		}
		for _, m := range op.Manufacturers {
			if _, err := tx.Exec(ctx, `INSERT INTO navl_authority_pairings VALUES($1,$2)`, op.ID, m); err != nil {
				return err
			}
		}
	}
	if _, err = tx.Exec(ctx, `NOTIFY navlistener_authorization_changed`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) Revoke(ctx context.Context, operator, id string) error {
	if !identity.ValidScopeID(operator) || !identity.ValidScopeID(id) {
		return errors.New("operator and enrollment ids are required")
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var observer string
	err = tx.QueryRow(ctx, `UPDATE navl_enrollments SET active=false,revoked_at=now() WHERE id=$1 AND active RETURNING observer_id`, id).Scan(&observer)
	if err != nil {
		return fmt.Errorf("active enrollment not found: %w", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO navl_service_events(observer_id,enrollment_id,kind,operator_id,detail) VALUES($1,$2,'revoke',$3,'{}')`, observer, id, operator); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `NOTIFY navlistener_authorization_changed`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
