package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ptudor/navlistener/internal/config"
)

const testCommissioningFingerprint = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
const testManufacturerAuthority = "test-manufacturer"

// TestUnverifiedHardwareDefaultsAreExplicit: a frame from a source that presented
// no evidence is stored as an explicit "none", never an empty string a reader
// might treat as unknown.
func TestUnverifiedHardwareDefaultsAreExplicit(t *testing.T) {
	r := navFrameToRow(&NavFrame{SourceID: "dial", Raw: []byte{1, 2, 3, 4}})
	for i, col := range copyColumns {
		switch col {
		case "hardware_trust":
			if r[i] != "none" {
				t.Errorf("hardware_trust default = %v, want none", r[i])
			}
		case "manufacturer_authority_id":
			if r[i] != nil {
				t.Errorf("software manufacturer = %v, want NULL", r[i])
			}
		case "commissioning_fingerprint":
			if r[i] != "" {
				t.Errorf("%s default = %v, want empty", col, r[i])
			}
		}
	}
}

// TestBoardRowSharesReceiptProvenance pins the observer_samples layout to the
// nav_frames one by name. Board rows reuse the leading provenance columns and
// the trailing replay key of a nav row positionally, so a column added to the
// provenance block must move both ends together — and every receipt column a
// nav row carries, hardware evidence included, must reach the board row.
func TestBoardRowSharesReceiptProvenance(t *testing.T) {
	if copyColumns[provenanceColumns-1] != "policy_revision" || copyColumns[provenanceColumns] != "gnssid" {
		t.Fatalf("provenanceColumns = %d does not end the receipt block: %q | %q",
			provenanceColumns, copyColumns[provenanceColumns-1], copyColumns[provenanceColumns])
	}
	sampled := time.Unix(1_700_000_100, 0)
	f := &NavFrame{
		Ts: time.Unix(1_700_000_000, 0), ReceivedAt: time.Unix(1_700_000_001, 0), SourceID: "obs1",
		OrganizationID: "customer-a", AttestationTier: "verified_v1_core",
		HardwareTrust: "trusted", ManufacturerAuthorityID: testManufacturerAuthority, CommissioningFingerprint: testCommissioningFingerprint,
		PolicyRevision: "policy-7", Raw: []byte{9, 8}, DecoderVer: "v1",
		Session: "boot-a", SourceSeq: 77, HasSourceSeq: true,
		Board: &BoardSample{Kind: "timing", SampleTime: &sampled, Data: []byte(`{"uptime_ms":1}`)},
	}
	row := boardFrameToRow(f)
	if len(row) != len(boardColumns) {
		t.Fatalf("board row has %d cols, want %d", len(row), len(boardColumns))
	}
	want := map[string]any{
		"source_id": "obs1", "organization_id": "customer-a", "attestation_tier": "verified_v1_core",
		"hardware_trust": "trusted", "manufacturer_authority_id": testManufacturerAuthority,
		"commissioning_fingerprint": testCommissioningFingerprint,
		"policy_revision":           "policy-7", "kind": "timing", "data": `{"uptime_ms":1}`, "decoder_ver": "v1",
		"source_session": "boot-a", "source_seq": int64(77),
	}
	for i, col := range boardColumns {
		if w, ok := want[col]; ok && row[i] != w {
			t.Errorf("board column %q (index %d) = %v, want %v", col, i, row[i], w)
		}
	}
}

// Both receipt tables use the first v1 schema. Pre-authority deployments gain
// missing columns; incompatible prototype authority layouts are not converted.
func TestSchemaDeclaresHardwareEvidenceEverywhere(t *testing.T) {
	for _, declaration := range []string{"manufacturer_authority_id TEXT,", "operational_authority_id TEXT NOT NULL", "authority_evidence JSONB NOT NULL"} {
		if strings.Count(strings.ToLower(schemaSQL), "\n    "+strings.ToLower(declaration)) != 2 {
			t.Errorf("missing receipt declaration %s", declaration)
		}
	}
	if !strings.Contains(navFrameSelect, "COALESCE(manufacturer_authority_id,'')") {
		t.Fatal("replay does not handle nullable manufacturer")
	}
}

func TestIntegrationPreAuthorityReceiptMigration(t *testing.T) {
	baseDSN := testDSN(t)
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := fmt.Sprintf("receipt_upgrade_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{name}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
	}()
	dsn := orderTestDSN(baseDSN, "search_path", name+",public")
	old, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close(ctx)
	// Recreate the production layout before hardware and authority evidence.
	var lines []string
	for _, line := range strings.Split(schemaSQL, "\n") {
		if strings.Contains(line, "hardware_trust") || strings.Contains(line, "manufacturer_authority_id") ||
			strings.Contains(line, "operational_authority_id") || strings.Contains(line, "authority_evidence") ||
			strings.Contains(line, "commissioning_fingerprint") {
			continue
		}
		lines = append(lines, line)
	}
	if _, err := old.PgConn().Exec(ctx, strings.Join(lines, "\n")).ReadAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(ctx, `INSERT INTO nav_frames
		(ts,received_at,source_id,gnssid,svid,sigid,freqid,msg_type,raw)
		VALUES (now()-interval '2 days',now()-interval '2 days','legacy',0,1,0,0,1,'\x01020304');
		INSERT INTO observer_samples (ts,received_at,source_id,kind,raw,data)
		VALUES (now()-interval '2 days',now()-interval '2 days','legacy','environment','\x01020304','{}')`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"nav_frames", "observer_samples"} {
		var chunks int
		if err := old.QueryRow(ctx, "SELECT count(compress_chunk(c)) FROM show_chunks($1::regclass) c", table).Scan(&chunks); err != nil || chunks != 1 {
			t.Fatalf("compress %s: chunks=%d, %v", table, chunks, err)
		}
	}
	s, err := New(ctx, config.Store{DSN: dsn, RawRetention: "7 days", CompressAfter: "1 day"}, integrationLog())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, table := range []string{"nav_frames", "observer_samples"} {
		var preserved bool
		if err := s.pool.QueryRow(ctx, "SELECT hardware_trust='none' AND manufacturer_authority_id IS NULL AND operational_authority_id='' AND authority_evidence='{}'::jsonb AND commissioning_fingerprint='' AND raw='\\x01020304'::bytea FROM "+pgx.Identifier{table}.Sanitize()+" WHERE source_id='legacy'").Scan(&preserved); err != nil || !preserved {
			t.Fatalf("legacy receipt changed in %s: %v", table, err)
		}
	}
	now := time.Now().UTC()
	nav := &NavFrame{Ts: now, ReceivedAt: now, SourceID: "new", Raw: []byte{1, 2, 3, 4}, OperationalAuthorityID: "customer"}
	board := *nav
	board.Board = &BoardSample{Kind: "environment", Data: []byte(`{}`)}
	if n, err := s.persistAtomicOnce(ctx, []*NavFrame{nav, &board}); err != nil || n != 2 {
		t.Fatalf("write after upgrade: rows=%d, %v", n, err)
	}
	if err := applySchema(ctx, s.pool); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
}

func TestIntegrationHardwareEvidenceReceipts(t *testing.T) {
	baseDSN := testDSN(t)
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := fmt.Sprintf("hardware_evidence_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{name}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
	}()
	s, err := New(ctx, config.Store{DSN: orderTestDSN(baseDSN, "search_path", name+",public"), RawRetention: "7 days", CompressAfter: "1 day"}, integrationLog())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, source := range []string{"software", "hardware"} {
		for i := 0; i < 2; i++ {
			f := &NavFrame{Ts: now, ReceivedAt: now, SourceID: source, Session: "boot", SourceSeq: uint64(i + 1), HasSourceSeq: true, OperationalAuthorityID: "customer", GnssID: 0, SvID: 12, MsgType: 1, Raw: []byte{1, 2, 3, 4}}
			if source == "hardware" {
				f.HardwareTrust = "trusted"
				f.ManufacturerAuthorityID = "ab"
				f.CommissioningFingerprint = testCommissioningFingerprint
				f.AuthorityEvidence = `{"core_signer_spki":"recorded"}`
			}
			if i == 1 {
				f.Board = &BoardSample{Kind: "environment", Data: []byte(`{}`)}
			}
			if n, err := s.persistAtomicOnce(ctx, []*NavFrame{f}); err != nil || n != 1 {
				t.Fatalf("persist %d %v", n, err)
			}
		}
	}
	for _, table := range []string{"nav_frames", "observer_samples"} {
		var absent bool
		if err := s.pool.QueryRow(ctx, "SELECT manufacturer_authority_id IS NULL FROM "+pgx.Identifier{table}.Sanitize()+" WHERE source_id='software'").Scan(&absent); err != nil || !absent {
			t.Fatalf("software provenance: %v", err)
		}
	}
	var frames []StoredNavFrame
	if err := s.QueryNavFrames(ctx, NavFrameQuery{Since: now.Add(-time.Second)}, func(f StoredNavFrame) error { frames = append(frames, f); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 {
		t.Fatalf("frames %d", len(frames))
	}
	for _, f := range frames {
		if f.OperationalAuthorityID != "customer" {
			t.Fatal("lost operational authority")
		}
		if f.SourceID == "hardware" && (f.ManufacturerAuthorityID != "ab" || f.CommissioningFingerprint != testCommissioningFingerprint || !strings.Contains(f.AuthorityEvidence, "recorded")) {
			t.Fatal("lost immutable receipt evidence")
		}
	}
}
