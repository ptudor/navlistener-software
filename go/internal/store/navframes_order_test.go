package store

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/frame"
	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/state"
)

func orderingGloRaw(number, slot int) []byte {
	b := make([]byte, 16)
	put := func(start, n int, v uint64) {
		for i := 0; i < n; i++ {
			if v>>uint(n-1-i)&1 != 0 {
				p := start + i
				b[p/8] |= 1 << uint(7-p%8)
			}
		}
	}
	put(1, 4, uint64(number))
	if number == 5 {
		put(5, 11, 615)
	}
	if slot != 0 {
		put(8, 5, uint64(slot))
	}
	w := make([]uint32, 4)
	for i := range w {
		w[i] = binary.BigEndian.Uint32(b[i*4:])
	}
	frame.StampGLONASSHamming(w)
	for i := range w {
		binary.BigEndian.PutUint32(b[i*4:], w[i])
	}
	return b
}

func TestIntegrationRawOrderMigrationPlansCompression(t *testing.T) {
	ctx := context.Background()
	baseDSN := testDSN(t)
	admin, err := pgxpool.New(ctx, baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("raw_order_%d", time.Now().UnixNano())
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
	var actualSchema string
	if err = legacyPool.QueryRow(ctx, "SELECT current_schema()").Scan(&actualSchema); err != nil || actualSchema != schema {
		t.Fatalf("fixture schema %q want %q: %v", actualSchema, schema, err)
	}
	// Apply the preceding schema and compress old rows BEFORE the additive
	// migration; the new order must remain unknown on already compressed data.
	a := strings.Index(schemaSQL, "-- Receipt-order migration:")
	b := strings.Index(schemaSQL[a:], "-- Query paths:") + a
	if a < 0 || b < a {
		t.Fatal("migration marker missing")
	}
	oldSchema := schemaSQL[:a] + schemaSQL[b:]
	if _, err = legacyPool.Exec(ctx, oldSchema); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Hour)
	for _, raw := range [][]byte{{2, 0, 0, 0}, {1, 0, 0, 0}, {1, 0, 0, 0}} {
		_, err = legacyPool.Exec(ctx, `INSERT INTO nav_frames(ts,received_at,source_id,gnssid,svid,sigid,freqid,msg_type,raw)
   VALUES($1,$1,'legacy',6,12,0,7,64,$2)`, at, raw)
		if err != nil {
			t.Fatal(err)
		}
	}
	compress := func(pool *pgxpool.Pool) {
		t.Helper()
		rows, e := pool.Query(ctx, `SELECT compress_chunk(c,true)::text FROM show_chunks('nav_frames') c`)
		if e != nil {
			t.Fatal(e)
		}
		for rows.Next() {
			var c string
			if e = rows.Scan(&c); e != nil {
				t.Fatal(e)
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			t.Fatal(e)
		}
	}
	compress(legacyPool)
	legacyPool.Close()
	s, err := New(ctx, config.Store{DSN: dsn, RawRetention: "7 days", CompressAfter: "1 day"}, integrationLog())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var oldCount int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM nav_frames WHERE receipt_order IS NULL AND source_session IS NULL AND source_seq IS NULL`).Scan(&oldCount); err != nil || oldCount != 3 {
		t.Fatalf("legacy changed: count=%d %v", oldCount, err)
	}
	raw := [][]byte{orderingGloRaw(5, 0), orderingGloRaw(6, 7), orderingGloRaw(7, 0), orderingGloRaw(6, 9), orderingGloRaw(7, 0)}
	var batch []*NavFrame
	for i, r := range raw {
		recv := at.Add(12 * time.Hour)
		if i >= 3 {
			recv = recv.Add(-time.Hour)
		} // receiver rollback remains observable
		batch = append(batch, &NavFrame{Ts: at.Add(time.Duration([]int{4, 2, 6, 3, 5}[i]) * time.Hour), ReceivedAt: recv,
			SourceID: "ordered", Session: "boot-a", SourceSeq: uint64(90 + i), HasSourceSeq: true,
			GnssID: 6, SvID: 12, SigID: 0, FreqID: 7, MsgType: 64, Raw: r})
	}
	// COPY order defines first-storage admission, independent of both timestamps.
	if n, e := s.persistAtomicOnce(ctx, batch); e != nil || n != 5 {
		t.Fatalf("persist %d %v", n, e)
	}
	replay := *batch[0]
	replay.Ts = at.Add(20 * time.Hour)
	if n, e := s.persistAtomicOnce(ctx, []*NavFrame{&replay}); e != nil || n != 0 {
		t.Fatalf("reconnect replay %d %v", n, e)
	}
	// A new boot reusing a sequence is distinct. All uint64 sequence bits survive.
	boot := *batch[0]
	boot.Session = "boot-b"
	boot.SourceSeq = ^uint64(0)
	boot.Ts = at.Add(21 * time.Hour)
	if n, e := s.persistAtomicOnce(ctx, []*NavFrame{&boot}); e != nil || n != 1 {
		t.Fatalf("new boot %d %v", n, e)
	}
	var baseline []StoredNavFrame
	var baselineDiagnostics []string
	for compressed := 0; compressed < 2; compressed++ {
		if compressed == 1 {
			compress(s.pool)
		}
		for _, seqScan := range []string{"on", "off"} {
			for attempt := 0; attempt < 3; attempt++ {

				readDSN := orderTestDSN(dsn, "enable_seqscan", seqScan)
				other := map[string]string{"on": "off", "off": "on"}[seqScan]
				readDSN = orderTestDSN(readDSN, "enable_indexscan", other)
				readDSN = orderTestDSN(readDSN, "enable_bitmapscan", other)
				reader, e := OpenReader(ctx, readDSN)
				if e != nil {
					t.Fatal(e)
				}
				var got []StoredNavFrame
				e = reader.QueryNavFrames(ctx, NavFrameQuery{Since: at, Until: at.Add(24 * time.Hour)}, func(f StoredNavFrame) error { got = append(got, f); return nil })
				reader.Close()
				if e != nil {
					t.Fatal(e)
				}
				if len(got) != 9 {
					t.Fatalf("got %d rows", len(got))
				}
				for i := 0; i < 3; i++ {
					if got[i].ReceiptOrder != nil || got[i].HasSourceSeq {
						t.Fatal("legacy order invented")
					}
				}
				for i := 3; i < len(got); i++ {
					if got[i].ReceiptOrder == nil || !got[i].HasSourceSeq {
						t.Fatal("new receipt metadata missing")
					}
					if i > 3 && *got[i].ReceiptOrder <= *got[i-1].ReceiptOrder {
						t.Fatal("admission sequence reordered")
					}
					want := uint64(90 + i - 3)
					if i == 8 {
						want = ^uint64(0)
					}
					if got[i].SourceSeq != want {
						t.Fatalf("source seq %d want %d", got[i].SourceSeq, want)
					}
				}
				live := state.New(4)
				var diagnostics []string
				for i, f := range got[3:] {
					words := make([]uint32, len(f.Raw)/4)
					for j := range words {
						words[j] = binary.BigEndian.Uint32(f.Raw[j*4:])
					}
					local := at.Add(24*time.Hour + time.Duration(i)*time.Second)
					live.Apply(&ingest.RawFrame{Source: f.SourceID, Session: f.Session, Recv: f.ReceivedAt, RecvLocal: local,
						GnssID: gnss.GLONASS, SvID: f.SvID, SigID: f.SigID, FreqID: f.FreqID, Words: words})
					snapshot, e := json.Marshal(live.FeedAlmanac(local))
					if e != nil {
						t.Fatal(e)
					}
					diagnostics = append(diagnostics, string(snapshot))
				}
				if baseline == nil {
					baseline = got
					baselineDiagnostics = diagnostics
					if diagnostics[0] != "{}" || diagnostics[1] != "{}" || !strings.Contains(diagnostics[2], "R07") || !strings.Contains(diagnostics[4], "R09") {
						t.Fatalf("fixture did not discriminate assembly order: %v", diagnostics)
					}
				} else if !reflect.DeepEqual(got, baseline) || !reflect.DeepEqual(diagnostics, baselineDiagnostics) {
					t.Fatal("replay/assembly changed across plan/compression")
				}
			}
		}
	}
}

// pgx ConnString deliberately returns the original string, ignoring mutations
// to parsed RuntimeParams. Build each fixture DSN explicitly instead.
func orderTestDSN(base, key, value string) string {
	if strings.HasPrefix(base, "postgres://") || strings.HasPrefix(base, "postgresql://") {
		u, err := url.Parse(base)
		if err != nil {
			panic(err)
		}
		q := u.Query()
		q.Set(key, value)
		u.RawQuery = q.Encode()
		return u.String()
	}
	value = strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), "'", `\'`)
	return base + " " + key + "='" + value + "'"
}
