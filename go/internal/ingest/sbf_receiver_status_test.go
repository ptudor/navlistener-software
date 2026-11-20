package ingest

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/wire"
)

// agcState is one AGCState sub-block as the receiver sends it: FrontEndID, Gain (i1),
// SampleVar and BlankingStat.
type agcState struct {
	id                uint8
	gain              int8
	variance, blanked uint8
}

// sbfStatusBlock builds a CRC-valid ReceiverStatus block field by field from the
// reference guide's layout. size is the AGCState sub-block length; a size above four
// pads each sub-block, as a later revision that extends it would.
func sbfStatusBlock(revision uint16, tow uint32, wnc uint16, cpu, extError uint8, uptime, rxState, rxError uint32,
	cmdCount, temperature uint8, size int, ends []agcState) []byte {
	body := make([]byte, sbfStatusCountsLen+len(ends)*size)
	binary.LittleEndian.PutUint32(body[0:], tow)
	binary.LittleEndian.PutUint16(body[4:], wnc)
	body[6], body[7] = cpu, extError
	binary.LittleEndian.PutUint32(body[8:], uptime)
	binary.LittleEndian.PutUint32(body[12:], rxState)
	binary.LittleEndian.PutUint32(body[16:], rxError)
	body[20], body[21], body[22], body[23] = uint8(len(ends)), uint8(size), cmdCount, temperature
	for i, e := range ends {
		sub := body[sbfStatusCountsLen+i*size:]
		sub[0], sub[1], sub[2], sub[3] = e.id, uint8(e.gain), e.variance, e.blanked
	}
	return sbfWrap(revision<<13|SBFReceiverStatusBlock, body)
}

// sbfWrap frames a body as a CRC-valid SBF block, padded to a multiple of four.
func sbfWrap(id uint16, body []byte) []byte {
	for len(body)%4 != 0 {
		body = append(body, 0)
	}
	block := make([]byte, 8, 8+len(body))
	block[0], block[1] = sbfSync1, sbfSync2
	binary.LittleEndian.PutUint16(block[4:], id)
	binary.LittleEndian.PutUint16(block[6:], uint16(8+len(body)))
	block = append(block, body...)
	binary.LittleEndian.PutUint16(block[2:], crc16ccitt(block[4:8], block[8:]))
	return block
}

// benchFrontEnds are the nine AGCState sub-blocks a mosaic-G5 P3 on firmware 1.0.0
// reported with an L1/L5 antenna and a fix, in the order it sent them. The observer
// logged the decoded fields, not the bytes, so the block is rebuilt from them here;
// its TOW and WNc were not logged and are this test's own.
var benchFrontEnds = []agcState{
	{5, 36, 96, 0}, {6, 46, 100, 1}, {3, 51, 104, 1}, {4, 50, 102, 0}, {12, 50, 96, 0},
	{2, 51, 107, 1}, {0, 40, 102, 1}, {1, 44, 100, 0}, {11, 41, 100, 0},
}

func benchStatusBlock() []byte {
	// Revision 1, 68 bytes, CPU 5 %, up 689 s, RxState WNSET|TOWSET, no error, command
	// count 29, 27 °C, nine four-byte sub-blocks: as logged.
	return sbfStatusBlock(1, 432_000_000, 2445, 5, 0, 689, 0x30, 0, 29, 127, 4, benchFrontEnds)
}

// sbfFixtureBenchStatus is the same block packed and CRC'd independently, with Python's
// struct and binascii.crc_hqx over ID, Length and body, so neither this file's builder
// nor crc16ccitt checks itself.
const sbfFixtureBenchStatus = "24401a91ae2f440000ccbf198d090500b1020000300000000000000009041d7f" +
	"05246000062e640103336801043266000c32600002336b0100286601012c64000b296400"

func TestBenchStatusBlockMatchesIndependentFixture(t *testing.T) {
	if got := benchStatusBlock(); !bytes.Equal(got, sbfFixture(t, sbfFixtureBenchStatus)) {
		t.Fatalf("built block %x differs from the independent fixture", got)
	}
}

func intPtr(v int) *int { return &v }

// TestDecodeSBFReceiverStatus decodes the block a real mosaic-G5 reported, field for
// field.
func TestDecodeSBFReceiverStatus(t *testing.T) {
	block := benchStatusBlock()
	if len(block) != 68 {
		t.Fatalf("block is %d bytes, the receiver sent 68", len(block))
	}
	number, _, body, err := splitSBFBlock(block)
	if err != nil || number != SBFReceiverStatusBlock {
		t.Fatalf("split: block %d, %v", number, err)
	}
	got, err := DecodeSBFReceiverStatus(body)
	if err != nil {
		t.Fatal(err)
	}
	tow, wnc := uint32(432_000_000), uint16(2445)
	want := &SBFReceiverStatus{
		TOWMS: &tow, WNc: &wnc, CPULoadPercent: intPtr(5), UptimeS: 689, RxState: 0x30,
		CmdCount: intPtr(29), TemperatureC: intPtr(27),
		FrontEnds: []SBFFrontEnd{
			{ID: 5, Code: 5, Locked: true, GainDB: intPtr(36), SampleVariance: intPtr(96)},
			{ID: 6, Code: 6, Locked: true, GainDB: intPtr(46), SampleVariance: intPtr(100), BlankedPercent: 1},
			{ID: 3, Code: 3, Locked: true, GainDB: intPtr(51), SampleVariance: intPtr(104), BlankedPercent: 1},
			{ID: 4, Code: 4, Locked: true, GainDB: intPtr(50), SampleVariance: intPtr(102)},
			{ID: 12, Code: 12, Locked: true, GainDB: intPtr(50), SampleVariance: intPtr(96)},
			{ID: 2, Code: 2, Locked: true, GainDB: intPtr(51), SampleVariance: intPtr(107), BlankedPercent: 1},
			{ID: 0, Code: 0, Locked: true, GainDB: intPtr(40), SampleVariance: intPtr(102), BlankedPercent: 1},
			{ID: 1, Code: 1, Locked: true, GainDB: intPtr(44), SampleVariance: intPtr(100)},
			{ID: 11, Code: 11, Locked: true, GainDB: intPtr(41), SampleVariance: intPtr(100)},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded\n %+v\nwant\n %+v", got, want)
	}
	if got.RxState&SBFRxStateActiveAntenna != 0 || got.RxError&SBFRxErrorAntenna != 0 {
		t.Fatalf("state %#x error %#x", got.RxState, got.RxError)
	}
}

// TestDecodeSBFReceiverStatusDoNotUse: every Do-Not-Use value decodes as absent, never
// as a number: an unknown time stamp, load, command count and temperature, a front end
// whose PLL is not locked (gain -128) and one with no sample variance.
func TestDecodeSBFReceiverStatusDoNotUse(t *testing.T) {
	block := sbfStatusBlock(1, 0xFFFFFFFF, 0xFFFF, 0xFF, 0x08, 7, 0, SBFRxErrorSoftware, 0, 0, 4,
		[]agcState{{0, -128, 0, 0}, {1, -127, 1, 100}, {5, 127, 255, 3}})
	got, err := DecodeSBFReceiverStatus(block[8:])
	if err != nil {
		t.Fatal(err)
	}
	if got.TOWMS != nil || got.WNc != nil || got.CPULoadPercent != nil || got.CmdCount != nil || got.TemperatureC != nil {
		t.Fatalf("Do-Not-Use fields decoded as values: %+v", got)
	}
	if got.ExtError != 0x08 || got.UptimeS != 7 || got.RxError != SBFRxErrorSoftware {
		t.Fatalf("bit fields and uptime = %+v", got)
	}
	want := []SBFFrontEnd{
		{ID: 0, Code: 0},
		{ID: 1, Code: 1, Locked: true, GainDB: intPtr(-127), SampleVariance: intPtr(1), BlankedPercent: 100},
		{ID: 5, Code: 5, Locked: true, GainDB: intPtr(127), SampleVariance: intPtr(255), BlankedPercent: 3},
	}
	if !reflect.DeepEqual(got.FrontEnds, want) {
		t.Fatalf("front ends = %+v\nwant %+v", got.FrontEnds, want)
	}
}

// TestDecodeSBFReceiverStatusTemperature: the field carries an offset of 100, so it
// spans -99 to 155 °C.
func TestDecodeSBFReceiverStatusTemperature(t *testing.T) {
	for raw, want := range map[uint8]int{1: -99, 60: -40, 100: 0, 125: 25, 185: 85, 255: 155} {
		got, err := DecodeSBFReceiverStatus(sbfStatusBlock(1, 0, 0, 0, 0, 0, 0, 0, 1, raw, 4, nil)[8:])
		if err != nil || got.TemperatureC == nil || *got.TemperatureC != want {
			t.Fatalf("raw %d decoded %v (%v), want %d", raw, got.TemperatureC, err, want)
		}
	}
}

// TestDecodeSBFReceiverStatusShapes: the shapes a block may legitimately take, and the
// ones that contradict themselves.
func TestDecodeSBFReceiverStatusShapes(t *testing.T) {
	full := benchStatusBlock()[8:]

	// A body that ends after RxError, as a receiver without the AGC fields sends it.
	short, err := DecodeSBFReceiverStatus(full[:sbfStatusFixedLen])
	if err != nil || short.UptimeS != 689 || short.FrontEnds != nil || short.TemperatureC != nil || short.CmdCount != nil {
		t.Fatalf("20-byte body = %+v, %v", short, err)
	}
	// Anything shorter has lost fixed fields.
	for n := 0; n < sbfStatusFixedLen; n++ {
		if _, err := DecodeSBFReceiverStatus(full[:n]); !errors.Is(err, ErrBadSBFReceiverStatus) {
			t.Fatalf("%d-byte body: err = %v", n, err)
		}
	}
	// No sub-blocks at all.
	none, err := DecodeSBFReceiverStatus(sbfStatusBlock(1, 0, 0, 3, 0, 9, 0, 0, 1, 125, 4, nil)[8:])
	if err != nil || len(none.FrontEnds) != 0 || none.TemperatureC == nil || *none.TemperatureC != 25 {
		t.Fatalf("no sub-blocks = %+v, %v", none, err)
	}
	// A longer sub-block: the four defined bytes are read and the rest skipped.
	wide, err := DecodeSBFReceiverStatus(sbfStatusBlock(2, 0, 0, 3, 0, 9, 0, 0, 1, 125, 8, []agcState{{0, 40, 100, 0}, {5, 38, 99, 2}})[8:])
	if err != nil || len(wide.FrontEnds) != 2 || *wide.FrontEnds[1].GainDB != 38 || wide.FrontEnds[1].BlankedPercent != 2 {
		t.Fatalf("8-byte sub-blocks = %+v, %v", wide, err)
	}
	// More sub-blocks than the block holds, and a sub-block too short for its fields.
	overstated := append([]byte(nil), full...)
	overstated[20] = 10
	if _, err := DecodeSBFReceiverStatus(overstated); !errors.Is(err, ErrBadSBFReceiverStatus) {
		t.Fatalf("N beyond the block: err = %v", err)
	}
	narrow := append([]byte(nil), full...)
	narrow[21] = 3
	if _, err := DecodeSBFReceiverStatus(narrow); !errors.Is(err, ErrBadSBFReceiverStatus) {
		t.Fatalf("SBLength 3: err = %v", err)
	}
	// Padding after the last sub-block is not a sub-block.
	padded, err := DecodeSBFReceiverStatus(append(append([]byte(nil), full...), 0, 0, 0, 0))
	if err != nil || len(padded.FrontEnds) != 9 {
		t.Fatalf("padded body = %+v, %v", padded, err)
	}
}

// TestSBFFrontEndNames: the reference guide's codes, the auxiliary antennas, and codes
// and antennas the guide does not define, which keep a stable name of their own.
func TestSBFFrontEndNames(t *testing.T) {
	for id, want := range map[uint8]string{
		0: "GPSL1/E1", 1: "GLOL1", 2: "E6", 3: "GPSL2", 4: "GLOL2", 5: "L5/E5a/B2a", 6: "E5b/B2b", 7: "E5",
		8: "L1 combined", 9: "L2 combined", 10: "MSS/L-band", 11: "B1", 12: "B3", 13: "S-band", 14: "B3/E6 combined",
		15: "fe15", 31: "fe31", 1<<5 | 0: "aux1 GPSL1/E1", 2<<5 | 5: "aux2 L5/E5a/B2a", 3<<5 | 11: "ant3 B1", 7<<5 | 31: "ant7 fe31",
	} {
		if got := SBFFrontEndLabel(id); got != want {
			t.Errorf("front end %#02x = %q, want %q", id, got, want)
		}
	}
	block := sbfStatusBlock(1, 0, 0, 1, 0, 1, 0, 0, 1, 125, 4, []agcState{{1<<5 | 3, 30, 100, 0}})
	got, err := DecodeSBFReceiverStatus(block[8:])
	if err != nil || got.FrontEnds[0].ID != 0x23 || got.FrontEnds[0].Code != 3 || got.FrontEnds[0].Antenna != 1 {
		t.Fatalf("aux front end = %+v, %v", got, err)
	}
}

// TestScanSBFDecodesReceiverStatus: on a dial capture a ReceiverStatus block carries
// its decoded status as an RF sample and still rebuilds byte for byte; any other block
// is captured only; and a ReceiverStatus that passes its CRC but contradicts itself is
// counted and still captured.
func TestScanSBFDecodesReceiverStatus(t *testing.T) {
	status := benchStatusBlock()
	other := sbfTestBlock(4007, 96) // a PVTGeodetic-sized block
	broken := sbfWrap(1<<13|SBFReceiverStatusBlock, append(append([]byte(nil), status[8:8+sbfStatusCountsLen]...), 1, 2, 3, 4))
	stream := append(append(append([]byte(nil), status...), other...), broken...)

	var frames []*RawFrame
	var errs []string
	at := time.Unix(1_800_000_000, 0)
	err := scanSBF(bytes.NewReader(stream), "p3", func() time.Time { return at },
		func(f *RawFrame) { frames = append(frames, f) }, func(kind string) { errs = append(errs, kind) })
	if !errors.Is(err, io.EOF) {
		t.Fatalf("scan ended with %v", err)
	}
	if len(frames) != 3 {
		t.Fatalf("%d frames, want 3", len(frames))
	}
	assertSBFFrame(t, frames[0], status)
	if frames[0].RF == nil || frames[0].RF.Status == nil || len(frames[0].RF.Status.FrontEnds) != 9 ||
		frames[0].RF.Bands != nil || frames[0].RF.Sats != nil {
		t.Fatalf("ReceiverStatus frame RF = %+v", frames[0].RF)
	}
	if !frames[0].Recv.Equal(at) || frames[0].Source != "p3" {
		t.Fatalf("frame stamp %v source %q", frames[0].Recv, frames[0].Source)
	}
	assertSBFFrame(t, frames[1], other)
	if frames[1].RF != nil {
		t.Fatalf("block 4007 carries RF %+v", frames[1].RF)
	}
	assertSBFFrame(t, frames[2], broken)
	if frames[2].RF != nil {
		t.Fatalf("a self-contradicting ReceiverStatus carries RF %+v", frames[2].RF)
	}
	if !reflect.DeepEqual(errs, []string{"sbf_receiver_status"}) {
		t.Fatalf("errors = %v, want one sbf_receiver_status", errs)
	}
}

// TestPushSBFDecodesReceiverStatus: an sbf push record gives the frame a dial capture
// gives, decoded status included, with the observer's reception stamp.
func TestPushSBFDecodesReceiverStatus(t *testing.T) {
	status := benchStatusBlock()
	f := recordToFrame(sbfRecord(status), "sbf", "p3")
	if f == nil {
		t.Fatal("record dropped")
	}
	assertSBFFrame(t, f, status)
	if f.RF == nil || f.RF.Status == nil || *f.RF.Status.TemperatureC != 27 || !f.RecvStamped || f.RecvLocal.IsZero() {
		t.Fatalf("pushed ReceiverStatus frame = %+v (RF %+v)", f, f.RF)
	}
	if other := recordToFrame(sbfRecord(sbfTestBlock(4027, 20)), "sbf", "p3"); other == nil || other.RF != nil {
		t.Fatalf("a MeasEpoch block: %+v", other)
	}
	// The same bytes on another feed are not an SBF block at all.
	if g := recordToFrame(wire.RawRecord{FrameType: wire.SBFBlockRecord, Raw: status}, "ubx", "p3"); g != nil && g.RF != nil {
		t.Fatalf("a ubx record decoded as ReceiverStatus: %+v", g.RF)
	}
}

// TestStoredReceiverStatusReplays: a receiver_status row holds the whole block, and a
// replay rebuilds the frame live state applied; anything else in such a row is refused.
func TestStoredReceiverStatusReplays(t *testing.T) {
	status := benchStatusBlock()
	local := time.Unix(1_800_000_000, 0)
	stamp := local.Add(-300 * time.Millisecond)
	f, err := StoredFrame(StoredInput{Origin: "rf", Kind: "receiver_status", Raw: status, Source: "p3",
		Local: local, WallClock: &stamp, Session: "boot-1", Seq: 42, HasSeq: true})
	if err != nil {
		t.Fatal(err)
	}
	assertSBFFrame(t, f, status)
	if f.RF == nil || f.RF.Status == nil || len(f.RF.Status.FrontEnds) != 9 {
		t.Fatalf("replayed RF = %+v", f.RF)
	}
	if !f.Recv.Equal(stamp) || !f.LocalRecv().Equal(local) || !f.RecvStamped || f.Session != "boot-1" || f.Seq != 42 || !f.HasSeq {
		t.Fatalf("replayed frame clocks and identity = %+v", f)
	}
	live := recordToFrame(sbfRecord(status), "sbf", "p3")
	if !reflect.DeepEqual(f.RF, live.RF) || !bytes.Equal(f.Bytes, live.Bytes) || !bytes.Equal(f.SBFHeader, live.SBFHeader) || f.MsgType != live.MsgType {
		t.Fatal("the replayed frame differs from the live one")
	}
	for name, raw := range map[string][]byte{
		"another block":  sbfTestBlock(4007, 96),
		"a body alone":   status[8:],
		"a damaged CRC":  append([]byte{status[0], status[1], status[2] ^ 1}, status[3:]...),
		"a short status": sbfWrap(1<<13|SBFReceiverStatusBlock, make([]byte, 16)),
	} {
		if g, err := StoredFrame(StoredInput{Origin: "rf", Kind: "receiver_status", Raw: raw, Local: local}); err == nil {
			t.Errorf("%s replayed as %+v", name, g)
		}
	}
}
