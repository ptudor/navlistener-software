package ingest

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/navlistener/internal/metrics"
	"github.com/ptudor/navlistener/internal/wire"
)

// SBF record format 1 (wire.SBFFormat) on the push path. The two short blocks are
// the independently generated CRC fixtures the dial capture tests use
// (cmd/navlistener/sbf_capture_test.go: Python's binascii.crc_hqx over ID, Length
// and body); longer blocks are built here with crc16ccitt, which
// TestCRC16CCITTGoldenVectors pins. None is a receiver capture.
const (
	sbfFixtureRev1 = "24408b7ab12f1800000102030405060708090a0b0c0d0e0f" // block 4017, revision 1
	sbfFixtureRev6 = "24408c49b1cf1800000102030405060708090a0b0c0d0e0f" // block 4017, revision 6
)

func sbfFixture(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// sbfTestBlock builds a CRC-valid block of the given ID and total length.
func sbfTestBlock(id uint16, length int) []byte {
	b := make([]byte, length)
	b[0], b[1] = '$', '@'
	binary.LittleEndian.PutUint16(b[4:], id)
	binary.LittleEndian.PutUint16(b[6:], uint16(length))
	for i := 8; i < length; i++ {
		b[i] = byte(i * 7)
	}
	binary.LittleEndian.PutUint16(b[2:], crc16ccitt(b[4:8], b[8:]))
	return b
}

func sbfRecord(block []byte) wire.RawRecord {
	return wire.RawRecord{RecvUnixNs: time.Now().UnixNano(), FrameType: wire.SBFBlockRecord, Raw: block}
}

// assertSBFFrame checks that a pushed block became the frame a dial capture gives:
// the block number, the eight original header bytes and the body, rebuilding the
// exact block.
func assertSBFFrame(t *testing.T, f *RawFrame, block []byte) {
	t.Helper()
	if want := int(binary.LittleEndian.Uint16(block[4:]) & 0x1fff); f.MsgType != want {
		t.Fatalf("MsgType = %d, want block number %d", f.MsgType, want)
	}
	if !bytes.Equal(f.SBFHeader, block[:8]) || !bytes.Equal(f.Bytes, block[8:]) || f.Words != nil {
		t.Fatalf("frame header %x body %d bytes words %v, want the block split at 8", f.SBFHeader, len(f.Bytes), f.Words)
	}
	rev, known := f.SBFRevision()
	if !known || rev != uint8(binary.LittleEndian.Uint16(block[4:])>>13) {
		t.Fatalf("revision %d known=%v", rev, known)
	}
	rebuilt, err := f.SBFWire()
	if err != nil || !bytes.Equal(rebuilt, block) {
		t.Fatalf("rebuilt block differs: %v", err)
	}
}

// An sbf session must name format 1, and only an sbf session may name a format.
// The WELCOME confirms the format in the compact spelling the edge feeders read.
func TestPushSBFHandshake(t *testing.T) {
	for _, tc := range []struct {
		name  string
		feed  string
		sbf   uint8
		error string
	}{
		{"format 1", "sbf", wire.SBFFormat, ""},
		{"no format", "sbf", 0, "unsupported sbf format"},
		{"unknown format", "sbf", 2, "unsupported sbf format"},
		{"format on ubx", "ubx", wire.SBFFormat, "unsupported sbf format"},
		{"ubx", "ubx", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			addr, out := startPushServer(t, ctx, tokenAuth("observer16", "s3cret", tc.feed))
			conn := dialPush(t, addr)
			defer conn.Close()
			if err := wire.WriteHello(conn, wire.HelloMsg{Token: "s3cret", Station: "observer16", Feed: tc.feed,
				Session: "boot-sbf", SBF: tc.sbf}); err != nil {
				t.Fatal(err)
			}
			ft, payload, err := wire.ReadFrame(conn)
			if err != nil || ft != wire.Welcome {
				t.Fatalf("welcome frame: ft=%d err=%v", ft, err)
			}
			wmsg, err := parseWelcome(payload)
			if err != nil {
				t.Fatal(err)
			}
			if tc.error != "" {
				if wmsg.OK || wmsg.Error != tc.error || wmsg.SBF != 0 {
					t.Fatalf("welcome = %+v, want refusal %q", wmsg, tc.error)
				}
				return
			}
			if !wmsg.OK || wmsg.SBF != tc.sbf || strings.Contains(string(payload), "\"sbf\":") != (tc.sbf != 0) {
				t.Fatalf("welcome %s, want ok with sbf %d", payload, tc.sbf)
			}
			if tc.sbf != 0 && !strings.Contains(string(payload), "\"sbf\":1") {
				t.Fatalf("welcome %s lacks the compact \"sbf\":1 the feeders match", payload)
			}
			if tc.feed != "sbf" {
				return
			}
			block := sbfFixture(t, sbfFixtureRev1)
			if err := wire.WriteFrame(conn, wire.Data, wire.EncodeData(1, sbfRecord(block))); err != nil {
				t.Fatal(err)
			}
			select {
			case f := <-out:
				if f.Source != "observer16" || f.Session != "boot-sbf" || f.Seq != 1 {
					t.Fatalf("frame %s/%s/%d, want observer16/boot-sbf/1", f.Source, f.Session, f.Seq)
				}
				assertSBFFrame(t, f, block)
			case <-time.After(2 * time.Second):
				t.Fatal("the SBF block did not reach the decode channel")
			}
		})
	}
}

// Every SBF block reaches the decode stage as the frame a dial capture gives,
// whatever its revision or length up to the format's own bound, beside the
// telemetry an sbf session also carries. All of them are persistable: the
// watermark waits for the historian.
func TestPushSBFRecordsSplitLikeCaptures(t *testing.T) {
	tr := NewDurableTracker()
	cli, out, _ := streamOnPipe(t, "sbf", tr)
	blocks := [][]byte{
		sbfFixture(t, sbfFixtureRev1),
		sbfFixture(t, sbfFixtureRev6),
		sbfTestBlock(4027|1<<13, 8192),
		sbfTestBlock(4023, wire.MaxSBFBlock),
	}
	for i, b := range blocks {
		if err := wire.WriteFrame(cli, wire.Data, wire.EncodeData(uint64(i+1), sbfRecord(b))); err != nil {
			t.Fatal(err)
		}
	}
	telem := wire.RawRecord{RecvUnixNs: time.Now().UnixNano(), FrameType: TelemJammingStats,
		Raw: EncodeJammingStats([]RFBand{{Block: 0, AGC: 1200, CWSuppress: 3, JamState: 1, AntStatus: 2}})}
	if err := wire.WriteFrame(cli, wire.Data, wire.EncodeData(5, telem)); err != nil {
		t.Fatal(err)
	}
	for i, b := range blocks {
		select {
		case f := <-out:
			if f.Seq != uint64(i+1) || !f.RecvStamped {
				t.Fatalf("frame seq %d stamped=%v, want %d stamped", f.Seq, f.RecvStamped, i+1)
			}
			assertSBFFrame(t, f, b)
		case <-time.After(2 * time.Second):
			t.Fatalf("block %d never reached the decode stage", i+1)
		}
	}
	select {
	case f := <-out:
		if f.RF == nil || len(f.RF.Bands) != 1 || f.SBFHeader != nil {
			t.Fatalf("telemetry on an sbf session gave %+v", f)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("telemetry on an sbf session never reached the decode stage")
	}
	if got := tr.Watermark("obs1", "boot-a"); got != 0 {
		t.Fatalf("watermark = %d before the historian resolved anything, want 0", got)
	}
	for seq := uint64(1); seq <= 5; seq++ {
		tr.Resolved("obs1", "boot-a", seq)
	}
	if got := tr.Watermark("obs1", "boot-a"); got != 5 {
		t.Fatalf("watermark = %d after resolving every record, want 5", got)
	}
}

// A record on an sbf session that is not one whole, CRC-valid SBF block under the
// SBF record type is dropped under its own reason and acked as permanently
// malformed, so it never wedges the valid records behind it.
func TestPushSBFRejectsMalformedRecords(t *testing.T) {
	good := sbfFixture(t, sbfFixtureRev1)
	badCRC := append([]byte(nil), good...)
	badCRC[len(badCRC)-1] ^= 1
	badSync := append([]byte(nil), good...)
	badSync[1] = '#'
	longer := sbfTestBlock(4017, 28)
	binary.LittleEndian.PutUint16(longer[6:], 32) // Length claims more than the record holds
	binary.LittleEndian.PutUint16(longer[2:], crc16ccitt(longer[4:8], longer[8:]))
	unaligned := sbfTestBlock(4017, 24)
	unaligned = append(unaligned, 0, 0)
	binary.LittleEndian.PutUint16(unaligned[6:], 26)
	binary.LittleEndian.PutUint16(unaligned[2:], crc16ccitt(unaligned[4:8], unaligned[8:]))
	navType := sbfRecord(good)
	navType.FrameType = 0x10
	galileo := sbfRecord(good)
	galileo.GnssID = 2
	satellite := sbfRecord(good)
	satellite.SvID = 11
	cases := []wire.RawRecord{
		sbfRecord(badCRC), sbfRecord(badSync), sbfRecord(longer), sbfRecord(unaligned),
		sbfRecord(good[:6]), navType, galileo, satellite,
	}

	tr := NewDurableTracker()
	cli, out, _ := streamOnPipe(t, "sbf", tr)
	malformed := metrics.PushErrorsTotal.WithLabelValues("obs1", "sbf_block")
	oversize := metrics.PushErrorsTotal.WithLabelValues("obs1", "record_oversize")
	beforeMalformed, beforeOversize := testutil.ToFloat64(malformed), testutil.ToFloat64(oversize)
	seq := uint64(0)
	for _, rec := range cases {
		seq++
		if err := wire.WriteFrame(cli, wire.Data, wire.EncodeData(seq, rec)); err != nil {
			t.Fatal(err)
		}
	}
	seq++
	if err := wire.WriteFrame(cli, wire.Data, wire.EncodeData(seq, sbfRecord(make([]byte, wire.MaxSBFBlock+4)))); err != nil {
		t.Fatal(err)
	}
	seq++
	if err := wire.WriteFrame(cli, wire.Data, wire.EncodeData(seq, sbfRecord(good))); err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-out:
		if f.Seq != seq {
			t.Fatalf("forwarded seq %d, want only the valid block %d", f.Seq, seq)
		}
		assertSBFFrame(t, f, good)
	case <-time.After(2 * time.Second):
		t.Fatal("the valid block behind the malformed ones never reached the decode stage")
	}
	expectNoFrame(t, out, "malformed SBF records")
	if got := testutil.ToFloat64(malformed) - beforeMalformed; got != float64(len(cases)) {
		t.Fatalf("push_errors_total{sbf_block} rose by %v, want %d", got, len(cases))
	}
	if got := testutil.ToFloat64(oversize) - beforeOversize; got != 1 {
		t.Fatalf("push_errors_total{record_oversize} rose by %v, want 1", got)
	}
	tr.Resolved("obs1", "boot-a", seq)
	if got := tr.Watermark("obs1", "boot-a"); got != seq {
		t.Fatalf("watermark = %d after resolving the valid block, want %d", got, seq)
	}
}

// On a ubx or rtcm session the SBF record type means nothing special: those feeds
// keep their own record rules.
func TestSBFRecordWellFormedOnlyOnSBF(t *testing.T) {
	junk := wire.RawRecord{FrameType: wire.SBFBlockRecord, Raw: []byte{1, 2, 3, 4}}
	for _, feed := range []string{"ubx", "rtcm"} {
		if !sbfRecordWellFormed(junk, feed) {
			t.Errorf("sbf gate applied to a %s record", feed)
		}
	}
	if sbfRecordWellFormed(junk, "sbf") {
		t.Error("a four-byte body passed as an SBF block")
	}
	telem := wire.RawRecord{FrameType: TelemObserverDetails, Raw: []byte{1}}
	if !sbfRecordWellFormed(telem, "sbf") {
		t.Error("telemetry on an sbf session was held to the SBF block rules")
	}
}
