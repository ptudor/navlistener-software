package store

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/identity"
)

// receiverStatusBlock is a whole Septentrio ReceiverStatus block (4014, revision 1),
// CRC'd independently; the historian keeps such a block whole in rf_samples.raw.
const receiverStatusBlock = "24401a91ae2f440000ccbf198d090500b1020000300000000000000009041d7f" +
	"05246000062e640103336801043266000c32600002336b0100286601012c64000b296400"

func receiverStatusFrame(t *testing.T, source, collector string, at time.Time, seq uint64) *NavFrame {
	t.Helper()
	raw, err := hex.DecodeString(receiverStatusBlock)
	if err != nil {
		t.Fatal(err)
	}
	stamp := at.Add(-200 * time.Millisecond)
	return &NavFrame{Ts: at, ReceivedAt: stamp, SourceID: source, OrganizationID: "test-org", CollectorInstanceID: collector,
		Session: "boot-a", SourceSeq: seq, HasSourceSeq: true, MsgType: 4014, Raw: raw,
		RF: &RFSample{Kind: "receiver_status", Data: []byte(`{"uptime_s":689,"front_ends":[{"id":0,"gain_db":40}]}`),
			LocalReceivedAt: at, WallClockStamp: &stamp}}
}

// TestIntegrationReceiverStatusRows: a receiver_status row is stored once with the whole
// block and its clocks, comes back to a station replay as the live checks received it,
// and is copied into the evidence of a station event raised from it.
func TestIntegrationReceiverStatusRows(t *testing.T) {
	ctx := context.Background()
	dsn := testDSN(t)
	s, err := New(ctx, config.Store{DSN: dsn}, integrationLog())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Microsecond)
	source := fmt.Sprintf("receiver-status-%d", now.UnixNano())
	collector := fmt.Sprintf("receiver-status-c%d", now.UnixNano())
	eventTime := now.Add(-2 * time.Minute)
	f := receiverStatusFrame(t, source, collector, eventTime, 1)
	if n, err := s.persistAtomicOnce(ctx, []*NavFrame{f}); err != nil || n != 1 {
		t.Fatalf("write: %d %v", n, err)
	}
	if n, err := s.persistAtomicOnce(ctx, []*NavFrame{f}); err != nil || n != 0 {
		t.Fatalf("replay: %d %v", n, err)
	}
	var navRows int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM nav_frames WHERE source_id=$1`, source).Scan(&navRows); err != nil || navRows != 0 {
		t.Fatalf("a receiver_status row also landed in nav_frames: %d %v", navRows, err)
	}

	reader, err := OpenReader(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var inputs []StationInput
	err = reader.QueryStationInputs(ctx, StationInputQuery{CollectorID: collector, Station: source,
		Since: eventTime.Add(-time.Minute), Until: eventTime.Add(time.Minute)}, func(in StationInput) error {
		inputs = append(inputs, in)
		return nil
	})
	if err != nil || len(inputs) != 1 {
		t.Fatalf("station inputs: %d %v", len(inputs), err)
	}
	in := inputs[0]
	if in.Origin != "rf" || in.Kind != "receiver_status" || !bytes.Equal(in.Raw, f.Raw) || in.Approximate ||
		!in.Local.Equal(eventTime) || in.WallClock == nil || !in.WallClock.Equal(*f.RF.WallClockStamp) ||
		in.Session != "boot-a" || !in.HasSeq || in.Seq != 1 {
		t.Fatalf("station input = %+v", in)
	}

	operator := identity.Audience{Kind: identity.AudienceOperator, ID: collector}
	seq, err := s.WriteEvent(ctx, EventRow{Audience: operator.Key(), Time: eventTime, SV: source, Type: "station_rf_degraded",
		OldValue: "ok", NewValue: "degraded", Severity: 1, Raw: []byte(`{"station":"` + source + `"}`),
		DedupeKey: fmt.Sprintf("receiver-status-%d", now.UnixNano()), CollectorInstanceID: collector})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.CaptureEventEvidence(ctx, collector, now, testEvidencePolicy()); err != nil || n != 1 {
		t.Fatalf("capture: %d %v", n, err)
	}
	bundle, err := s.QueryEventEvidence(ctx, EvidenceQuery{Audience: operator, Seq: seq, Limit: EvidenceMaxLimit})
	if err != nil {
		t.Fatal(err)
	}
	if bundle.RFSamples != 1 || len(bundle.Samples) != 1 || bundle.Samples[0].Kind != "receiver_status" || !bytes.Equal(bundle.Samples[0].Raw, f.Raw) {
		t.Fatalf("evidence bundle: rf %d samples %+v", bundle.RFSamples, bundle.Samples)
	}
}

// TestIntegrationReceiverStatusKindMigration: a database whose rf_samples constraint
// predates the kind is widened at startup, once, and then accepts it.
func TestIntegrationReceiverStatusKindMigration(t *testing.T) {
	dsn := isolatedDSN(t)
	ctx := context.Background()
	cfg := config.Store{DSN: dsn, BatchSize: 100, BatchEvery: 50 * time.Millisecond, RawRetention: "7 days", CompressAfter: "1 day"}
	open := func() {
		t.Helper()
		s, err := New(ctx, cfg, integrationLog())
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
	open()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err = pool.Exec(ctx, `DELETE FROM rf_samples WHERE source_id='status-migration'`); err != nil {
		t.Fatal(err)
	}
	// Recreate the constraint as the previous build left it, after a chunk exists.
	_, err = pool.Exec(ctx, `INSERT INTO rf_samples(ts,received_at,source_id,collector_instance_id,sample_time,kind,raw,data)
 VALUES(now(),now(),'status-migration','collector-a',now(),'solution','\x0101','{}');
 ALTER TABLE rf_samples DROP CONSTRAINT rf_samples_kind_check;
 ALTER TABLE rf_samples ADD CONSTRAINT rf_samples_kind_check CHECK(kind IN ('reception','jamming','combined','solution'));`)
	if err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO rf_samples(ts,received_at,source_id,collector_instance_id,sample_time,kind,raw,data)
 VALUES(now(),now(),'status-migration','collector-a',now(),'receiver_status','\x2440','{"uptime_s":1}')`
	if _, err = pool.Exec(ctx, insert); err == nil {
		t.Fatal("the previous constraint admitted receiver_status; the migration is not exercised")
	}
	open()
	var firstOID, secondOID uint32
	var definition string
	query := `SELECT oid, pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='rf_samples'::regclass AND conname='rf_samples_kind_check'`
	if err = pool.QueryRow(ctx, query).Scan(&firstOID, &definition); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"reception", "jamming", "combined", "solution", "receiver_status"} {
		if !strings.Contains(definition, "'"+kind+"'") {
			t.Fatalf("migrated constraint %q lacks %s", definition, kind)
		}
	}
	if _, err = pool.Exec(ctx, insert); err != nil {
		t.Fatal("migrated constraint rejects receiver_status: ", err)
	}
	open()
	if err = pool.QueryRow(ctx, query).Scan(&secondOID, &definition); err != nil || firstOID != secondOID {
		t.Fatal("migration repeats on restart: ", err)
	}
	if _, err = pool.Exec(ctx, `DELETE FROM rf_samples WHERE source_id='status-migration'`); err != nil {
		t.Fatal(err)
	}
}
