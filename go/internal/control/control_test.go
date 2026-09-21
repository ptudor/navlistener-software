package control

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"github.com/ptudor/navlistener/internal/boardid"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ptudor/navlistener/internal/attestation"
	"github.com/ptudor/navlistener/internal/authority"
	"github.com/ptudor/navlistener/internal/commissioning"
	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/testauthority"
)

type bench struct {
	ab, cd  *testauthority.Pair
	service *Service
	config  *config.Config
}

func newBench(t *testing.T) bench {
	t.Helper()
	ab, cd := testauthority.New(t, "navlisten", "ab"), testauthority.New(t, "customer", "ab", "cd")
	ops := []authority.Operational{ab.Config, cd.Config}
	set, err := authority.New(ops, map[string]bool{"ab": true, "cd": true}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	manufacturers := config.ManufacturerAuthorities{}
	for i, p := range []*testauthority.Pair{ab, cd} {
		id := []string{"ab", "cd"}[i]
		manufacturers = append(manufacturers, config.HardwareTrust{Active: true, ManufacturerAuthorityID: id, ManufacturerKeys: p.ManufacturerPaths, Products: []commissioning.ProductPolicy{{Product: 1, Revision: 258, RTCModels: []uint16{0, 1, 2}}}})
	}
	cfg := &config.Config{Authorities: set, OperationalAuthorities: ops, ManufacturerAuthorities: manufacturers}
	return bench{ab: ab, cd: cd, service: &Service{Authorities: set, Manufacturers: manufacturers}, config: cfg}
}

func (b bench) request(t *testing.T, coreKey, commissionKey *ecdsa.PrivateKey) Request {
	t.Helper()
	h := attestation.HardwareIdentity{Product: 1, BoardRevision: 258, BoardUID: boardid.EEPROM([8]byte{0, 4, 0xa3, 0xaa, 0xbb, 0xcc, 0xdd, 0xee}), ATECCSerial: [9]byte{1, 2, 3, 4, 5, 6, 7, 8, 9}}
	core, err := attestation.Sign(1, h, coreKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	statement := commissioning.Statement{Profile: commissioning.ProfileOpen, MCUFamily: commissioning.MCUESP32S3, Product: 1, BoardRevision: 258, Generation: 1, CommissionedAt: 1789646400, BoardUID: h.BoardUID, ATECCSerial: h.ATECCSerial, MCUMAC: [6]byte{1, 2, 3, 4, 5, 6}, Attestation: sha256.Sum256(core[:])}
	signer, err := commissioning.NewKeySigner(commissionKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	record, err := commissioning.Sign(statement, signer)
	if err != nil {
		t.Fatal(err)
	}
	return Request{ObserverID: statement.ObserverID(), OperationalAuthorityID: "navlisten", ManufacturerAuthorityID: "ab", OrganizationID: "owner", CollectorInstanceID: "collector", HardwareValidation: "bench-live-read-1", Product: 1, Revision: 258, BoardUIDKind: h.BoardUID.KindName(), BoardUID: h.BoardUID.Hex(), ATECCSerial: hex.EncodeToString(h.ATECCSerial[:]), CoreRecord: hex.EncodeToString(core[:]), CommissioningRecord: hex.EncodeToString(record[:]), FeedGrants: []string{"ubx"}}
}

func leaf(t *testing.T, r Request, p *testauthority.Pair) Request {
	cert, _, csr := p.Leaf(t, r.ObserverID)
	r.CertificatePEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}))
	r.CSRPEM = csr
	return r
}

func TestAuthorityScopedEnrollment(t *testing.T) {
	b := newBench(t)
	for core := 0; core < 2; core++ {
		for commission := 0; commission < 2; commission++ {
			t.Run(fmt.Sprintf("AB-%d-%d", core, commission), func(t *testing.T) {
				r := b.request(t, b.ab.ManufacturerKeys[core], b.ab.ManufacturerKeys[commission])
				v, err := b.service.Validate(leaf(t, r, b.ab))
				if err != nil {
					t.Fatal(err)
				}
				if v.Context.CoreSignerSPKI != commissioning.KeyFingerprint(&b.ab.ManufacturerKeys[core].PublicKey) || v.Context.CommissioningSignerSPKI != commissioning.KeyFingerprint(&b.ab.ManufacturerKeys[commission].PublicKey) {
					t.Fatal("signer snapshots do not identify the independent keys")
				}
				r.OperationalAuthorityID = "customer"
				v, err = b.service.Validate(leaf(t, r, b.cd))
				if err != nil {
					t.Fatal(err)
				}
				if v.Context.OperationalAuthorityID != "customer" || v.Context.ManufacturerAuthorityID != "ab" {
					t.Fatal("customer credential relabeled manufacturer provenance")
				}
			})
			t.Run(fmt.Sprintf("CD-%d-%d", core, commission), func(t *testing.T) {
				r := b.request(t, b.cd.ManufacturerKeys[core], b.cd.ManufacturerKeys[commission])
				r.ManufacturerAuthorityID = "cd"
				r.OperationalAuthorityID = "customer"
				if _, err := b.service.Validate(leaf(t, r, b.cd)); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
	base := b.request(t, b.ab.ManufacturerKeys[0], b.ab.ManufacturerKeys[1])
	for name, mutate := range map[string]func(Request) Request{
		"customer leaf for NavListen enrollment":     func(r Request) Request { return leaf(t, r, b.cd) },
		"unknown authority":                          func(r Request) Request { r.OperationalAuthorityID = "claimed"; return r },
		"unapproved pairing":                         func(r Request) Request { r.ManufacturerAuthorityID = "cd"; return r },
		"RTC named station":                          func(r Request) Request { r.ObserverID = "00-04-a3-12-34-56-78-90"; return r },
		"wrong product":                              func(r Request) Request { r.Product = 2; return r },
		"wrong revision":                             func(r Request) Request { r.Revision++; return r },
		"missing live validation":                    func(r Request) Request { r.HardwareValidation = ""; return r },
		"core signed by A commissioning signed by C": func(r Request) Request { return b.request(t, b.ab.ManufacturerKeys[0], b.cd.ManufacturerKeys[0]) },
		"C evidence under AB":                        func(r Request) Request { return b.request(t, b.cd.ManufacturerKeys[0], b.cd.ManufacturerKeys[1]) },
		"root CA key instead of slot 5":              func(r Request) Request { return b.request(t, b.ab.RootKeys[0], b.ab.ManufacturerKeys[0]) },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := b.service.Validate(mutate(base)); err == nil {
				t.Fatal("invalid enrollment accepted")
			}
		})
	}
	// Numeric product 1 in another authority does not inherit AB's policy.
	b.service.Manufacturers[1].Products[0].Revision = 99
	r := b.request(t, b.cd.ManufacturerKeys[0], b.cd.ManufacturerKeys[1])
	r.ManufacturerAuthorityID = "cd"
	r.OperationalAuthorityID = "customer"
	if _, err := b.service.Validate(r); err == nil {
		t.Fatal("product policy crossed manufacturer namespace")
	}
}

func TestSoftwareEnrollmentAndNoSelfReportedHardware(t *testing.T) {
	b := newBench(t)
	r := Request{ObserverID: "software-receiver", OperationalAuthorityID: "customer", OrganizationID: "owner", CollectorInstanceID: "collector", FeedGrants: []string{"ubx"}}
	v, err := b.service.Validate(r)
	if err != nil {
		t.Fatal(err)
	}
	if v.Context.ManufacturerAuthorityID != "" || v.Context.HardwareTrust != identity.HardwareTrustNone {
		t.Fatal("software credential manufactured hardware trust")
	}
	r.CoreRecord = "00"
	if _, err := b.service.Validate(r); err == nil {
		t.Fatal("software evidence accepted")
	}
}

func testDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("NAVLISTENER_CONTROL_TEST_DSN")
	if dsn == "" {
		t.Skip("NAVLISTENER_CONTROL_TEST_DSN is not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("navl_control_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = name
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{name}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	return db
}

func TestTransactionalEnrollmentAndService(t *testing.T) {
	db := testDatabase(t)
	b := newBench(t)
	b.service.DB = db
	ctx := context.Background()
	if err := b.service.Initialize(ctx, b.config); err != nil {
		t.Fatal(err)
	}
	r := b.request(t, b.ab.ManufacturerKeys[0], b.ab.ManufacturerKeys[1])
	id, token, err := b.service.Enroll(ctx, "operator", r)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) < 32 {
		t.Fatal("short credential")
	}
	var operational, manufacturer string
	if err := db.QueryRow(ctx, `SELECT operational_authority_id,manufacturer_authority_id FROM navlistener_observer_authorization_v3 WHERE enrollment_id=$1`, id).Scan(&operational, &manufacturer); err != nil {
		t.Fatal(err)
	}
	if operational != "navlisten" || manufacturer != "ab" {
		t.Fatal("wrong authority snapshot")
	}
	if _, _, err := b.service.Enroll(ctx, "operator", r); err == nil {
		t.Fatal("duplicate observer silently replaced")
	}
	r.ReplaceEnrollmentID = id
	r.OperationalAuthorityID = "customer"
	next, _, err := b.service.Enroll(ctx, "operator", leaf(t, r, b.cd))
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM navlistener_observer_authorization_v3 WHERE enrollment_id=$1`, id).Scan(&n); err != nil || n != 0 {
		t.Fatalf("old credential active: %d %v", n, err)
	}
	if err := db.QueryRow(ctx, `SELECT operational_authority_id,manufacturer_authority_id FROM navl_enrollments WHERE id=$1`, id).Scan(&operational, &manufacturer); err != nil || operational != "navlisten" || manufacturer != "ab" {
		t.Fatal("historical receipt was relabeled")
	}
	if err := b.service.Revoke(ctx, "operator", next); err != nil {
		t.Fatal(err)
	}
	software := Request{ObserverID: "software", OperationalAuthorityID: "customer", OrganizationID: "owner", CollectorInstanceID: "collector", FeedGrants: []string{"ubx"}}
	id, _, err = b.service.Enroll(ctx, "operator", software)
	if err != nil {
		t.Fatal(err)
	}
	var isNull bool
	if err := db.QueryRow(ctx, `SELECT manufacturer_authority_id IS NULL FROM navl_enrollments WHERE id=$1`, id).Scan(&isNull); err != nil || !isNull {
		t.Fatalf("software authority not null: %v", err)
	}
	// Persistent ownership of pins survives authority disable/re-registration.
	b.config.OperationalAuthorities[0].ID = "replacement"
	if err := b.service.Initialize(ctx, b.config); err == nil || !strings.Contains(err.Error(), "key already belongs") {
		t.Fatalf("key reassigned: %v", err)
	}
}
