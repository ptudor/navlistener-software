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
		case "commissioning_fingerprint":
			if r[i] != "" {
				t.Errorf("commissioning_fingerprint default = %v, want empty", r[i])
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
		OrganizationID: "customer-a", AttestationTier: "verified_v2_complete",
		HardwareTrust: "trusted", CommissioningFingerprint: testCommissioningFingerprint,
		PolicyRevision: "policy-7", Raw: []byte{9, 8}, DecoderVer: "v1",
		Session: "boot-a", SourceSeq: 77, HasSourceSeq: true,
		Board: &BoardSample{Kind: "timing", SampleTime: &sampled, Data: []byte(`{"uptime_ms":1}`)},
	}
	row := boardFrameToRow(f)
	if len(row) != len(boardColumns) {
		t.Fatalf("board row has %d cols, want %d", len(row), len(boardColumns))
	}
	want := map[string]any{
		"source_id": "obs1", "organization_id": "customer-a", "attestation_tier": "verified_v2_complete",
		"hardware_trust": "trusted", "commissioning_fingerprint": testCommissioningFingerprint,
		"policy_revision": "policy-7", "kind": "timing", "data": `{"uptime_ms":1}`, "decoder_ver": "v1",
		"source_session": "boot-a", "source_seq": int64(77),
	}
	for i, col := range boardColumns {
		if w, ok := want[col]; ok && row[i] != w {
			t.Errorf("board column %q (index %d) = %v, want %v", col, i, row[i], w)
		}
	}
}

// TestSchemaDeclaresHardwareEvidenceEverywhere: both receipt tables create the
// columns and both carry the additive migration, so a database created by any
// earlier schema and a fresh one end up identical.
func TestSchemaDeclaresHardwareEvidenceEverywhere(t *testing.T) {
	for _, table := range []string{"nav_frames", "observer_samples"} {
		for _, column := range []string{"hardware_trust", "commissioning_fingerprint"} {
			if !strings.Contains(schemaSQL, "ALTER TABLE "+table+" ADD COLUMN IF NOT EXISTS "+column+" ") {
				t.Errorf("schema has no additive migration for %s.%s", table, column)
			}
		}
	}
	if n := strings.Count(schemaSQL, "    hardware_trust        TEXT   NOT NULL DEFAULT 'none',"); n != 2 {
		t.Errorf("hardware_trust is declared in %d CREATE TABLE blocks, want 2", n)
	}
	if !strings.Contains(navFrameSelect, "hardware_trust, commissioning_fingerprint") {
		t.Error("replay SELECT does not read the hardware evidence columns")
	}
}

// precedingHardwareSchema is schema.sql as it stood before hardware evidence:
// every line naming either column is a whole declaration or a whole ALTER, so
// dropping those lines reconstructs the earlier schema exactly.
func precedingHardwareSchema(t *testing.T) string {
	t.Helper()
	var kept []string
	dropped := 0
	for _, line := range strings.Split(schemaSQL, "\n") {
		if strings.Contains(line, " hardware_trust ") || strings.Contains(line, "commissioning_fingerprint") {
			dropped++
			continue
		}
		kept = append(kept, line)
	}
	if dropped != 8 {
		t.Fatalf("dropped %d schema lines, want the 4 declarations and 4 migrations", dropped)
	}
	return strings.Join(kept, "\n")
}

// TestIntegrationHardwareEvidenceMigration applies the preceding schema,
// stores and compresses rows under it, then opens the store — which applies
// the current schema — and confirms the earlier rows read back as explicitly
// unverified while new rows keep what their session proved, in both tables.
func TestIntegrationHardwareEvidenceMigration(t *testing.T) {
	baseDSN := testDSN(t)
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("hardware_evidence_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, e := admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); e != nil {
			t.Error(e)
		}
	}()
	dsn := orderTestDSN(baseDSN, "search_path", schema+",public")
	legacyPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = legacyPool.Exec(ctx, precedingHardwareSchema(t)); err != nil {
		legacyPool.Close()
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Hour)
	if _, err = legacyPool.Exec(ctx, `INSERT INTO nav_frames(ts,received_at,source_id,gnssid,svid,sigid,freqid,msg_type,raw)
   VALUES($1,$1,'legacy',0,12,0,0,1,'\x01020304')`, at); err == nil {
		_, err = legacyPool.Exec(ctx, `INSERT INTO observer_samples(ts,received_at,source_id,kind,raw,data)
   VALUES($1,$1,'legacy','environment','\x0104','{}')`, at)
	}
	if err != nil {
		legacyPool.Close()
		t.Fatal(err)
	}
	for _, table := range []string{"nav_frames", "observer_samples"} {
		rows, e := legacyPool.Query(ctx, `SELECT compress_chunk(c,true)::text FROM show_chunks($1::regclass) c`, table)
		if e != nil {
			legacyPool.Close()
			t.Fatal(e)
		}
		for rows.Next() {
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			legacyPool.Close()
			t.Fatal(e)
		}
	}
	legacyPool.Close()

	s, err := New(ctx, config.Store{DSN: dsn, RawRetention: "7 days", CompressAfter: "1 day"}, integrationLog())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Microsecond)
	verified := func(seq uint64, board *BoardSample) *NavFrame {
		return &NavFrame{Ts: now, ReceivedAt: now, SourceID: "commissioned", Session: "boot-a", SourceSeq: seq, HasSourceSeq: true,
			HardwareTrust: "trusted", CommissioningFingerprint: testCommissioningFingerprint,
			GnssID: 0, SvID: 12, MsgType: 1, Raw: []byte{1, 2, 3, 4}, Board: board}
	}
	batch := []*NavFrame{verified(1, nil), verified(2, &BoardSample{Kind: "environment", Data: []byte(`{}`)})}
	if n, e := s.persistAtomicOnce(ctx, batch); e != nil || n != 2 {
		t.Fatalf("persist %d %v", n, e)
	}
	for _, table := range []string{"nav_frames", "observer_samples"} {
		for source, want := range map[string][2]string{"legacy": {"none", ""}, "commissioned": {"trusted", testCommissioningFingerprint}} {
			var trust, fingerprint string
			query := "SELECT hardware_trust, commissioning_fingerprint FROM " + pgx.Identifier{table}.Sanitize() + " WHERE source_id=$1"
			if err := s.pool.QueryRow(ctx, query, source).Scan(&trust, &fingerprint); err != nil {
				t.Fatalf("%s %s: %v", table, source, err)
			}
			if trust != want[0] || fingerprint != want[1] {
				t.Errorf("%s %s = %q/%q, want %q/%q", table, source, trust, fingerprint, want[0], want[1])
			}
		}
	}
	var replayed []StoredNavFrame
	if err := s.QueryNavFrames(ctx, NavFrameQuery{Since: at.Add(-time.Hour)}, func(f StoredNavFrame) error {
		replayed = append(replayed, f)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(replayed) != 2 || replayed[0].HardwareTrust != "none" || replayed[1].HardwareTrust != "trusted" ||
		replayed[1].CommissioningFingerprint != testCommissioningFingerprint {
		t.Fatalf("replay lost hardware evidence: %+v", replayed)
	}
}
