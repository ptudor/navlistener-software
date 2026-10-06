package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/config"
)

func TestRFRowSharesReceiptProvenance(t *testing.T) {
	at := time.Unix(1_800_000_000, 0)
	f := &NavFrame{
		Ts: at.Add(time.Second), ReceivedAt: at, SourceID: "edge", OrganizationID: "org",
		PolicyRevision: "policy-9", HardwareTrust: "trusted", ManufacturerAuthorityID: "maker",
		Raw: []byte{1, 0, 1, 0, 12, 43, 51, 1}, DecoderVer: "test",
		Session: "boot", SourceSeq: 9, HasSourceSeq: true,
		RF: &RFSample{Kind: "reception", Data: []byte(`{"Sats":[{"Cn0":43}]}`)},
	}
	row := rfFrameToRow(f)
	if len(row) != len(rfColumns) {
		t.Fatalf("RF row has %d columns, want %d", len(row), len(rfColumns))
	}
	want := map[string]any{
		"source_id": "edge", "organization_id": "org", "policy_revision": "policy-9",
		"hardware_trust": "trusted", "manufacturer_authority_id": "maker",
		"sample_time": at, "kind": "reception", "data": `{"Sats":[{"Cn0":43}]}`,
		"decoder_ver": "test", "source_session": "boot", "source_seq": int64(9),
	}
	for i, col := range rfColumns {
		if expected, ok := want[col]; ok && row[i] != expected {
			t.Errorf("RF column %q = %v, want %v", col, row[i], expected)
		}
	}
	if string(row[len(rfColumns)-5].([]byte)) != string(f.Raw) {
		t.Fatal("RF raw body changed")
	}
}

func TestIntegrationRFSamplesAtomicReplay(t *testing.T) {
	ctx := context.Background()
	s, err := New(ctx, config.Store{DSN: testDSN(t)}, integrationLog())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Microsecond)
	source := fmt.Sprintf("rf-history-test-%d", now.UnixNano())
	f := &NavFrame{
		Ts: now, ReceivedAt: now.Add(-time.Second), SourceID: source, OrganizationID: "test-org",
		Session: "boot-a", SourceSeq: 1, HasSourceSeq: true, Raw: []byte{1, 0, 0},
		RF: &RFSample{Kind: "reception", Data: []byte(`{"Sats":[]}`)},
	}
	if n, err := s.persistAtomicOnce(ctx, []*NavFrame{f}); err != nil || n != 1 {
		t.Fatalf("write: %d %v", n, err)
	}
	if n, err := s.persistAtomicOnce(ctx, []*NavFrame{f}); err != nil || n != 0 {
		t.Fatalf("replay: %d %v", n, err)
	}
	var kind, org string
	var raw []byte
	var sample time.Time
	if err := s.pool.QueryRow(ctx, `SELECT kind,organization_id,raw,sample_time FROM rf_samples WHERE source_id=$1`, source).
		Scan(&kind, &org, &raw, &sample); err != nil {
		t.Fatal(err)
	}
	if kind != "reception" || org != "test-org" || string(raw) != string(f.Raw) || !sample.Equal(f.ReceivedAt) {
		t.Fatal("RF evidence or receipt provenance changed")
	}
}
