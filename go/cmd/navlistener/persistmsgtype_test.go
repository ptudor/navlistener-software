package main

import (
	"testing"

	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/ingest"
)

// TestPersistMsgTypeBackfillsFromNavType guards a dial connector (UBX)
// never populates RawFrame.MsgType, while the push path's feeder sets it via
// its own NavType()-mirroring frame_type() -- so the identical GPS LNAV
// subframe historian-persisted as msg_type=0 via one ingest mode and 0x10 via
// the other. persistMsgType must backfill a zero MsgType from NavType() for
// word-oriented frames, and must never touch an already-set MsgType (the push
// path) or a byte-oriented (SBF/RTCM) frame's own message number.
func TestPersistMsgTypeBackfillsFromNavType(t *testing.T) {
	// Dial connector: MsgType never set, word-oriented (UBX SFRBX) -- must
	// backfill to NavType()'s GpsLnav (0x10).
	dial := &ingest.RawFrame{GnssID: gnss.GPS, SvID: 5, SigID: 0, Words: []uint32{1, 2, 3}}
	if got := persistMsgType(dial); got != 0x10 {
		t.Errorf("dial (unset MsgType, word-oriented): persistMsgType = 0x%x, want 0x10", got)
	}

	// Push path: MsgType already set by the feeder -- must be left untouched,
	// even though it happens to be the same value.
	push := &ingest.RawFrame{GnssID: gnss.GPS, SvID: 5, SigID: 0, Words: []uint32{1, 2, 3}, MsgType: 0x10}
	if got := persistMsgType(push); got != 0x10 {
		t.Errorf("push (MsgType already set): persistMsgType = 0x%x, want 0x10 unchanged", got)
	}

	// Byte-oriented frame (SBF/RTCM): MsgType unset but Words is nil -- must
	// stay 0, never backfilled from NavType() (whose GnssID/SigID zero values
	// would otherwise alias to GpsLnav).
	byteOriented := &ingest.RawFrame{GnssID: gnss.GPS, SvID: 5, SigID: 0, Bytes: []byte{0xD3, 0x00, 0x01}}
	if got := persistMsgType(byteOriented); got != 0 {
		t.Errorf("byte-oriented (nil Words): persistMsgType = 0x%x, want 0 (SBF/RTCM's own number, unset here)", got)
	}

	// A byte-oriented frame that DOES carry its own message number must be
	// left alone too.
	rtcm := &ingest.RawFrame{GnssID: gnss.GPS, SvID: 5, SigID: 0, Bytes: []byte{0xD3, 0x00, 0x01}, MsgType: 1019}
	if got := persistMsgType(rtcm); got != 1019 {
		t.Errorf("rtcm (own MsgType set): persistMsgType = %d, want 1019 unchanged", got)
	}
}
