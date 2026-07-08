// Package ingest reads raw broadcast nav frames from receivers we control and
// forwards them, undecoded, to the central decode+state stage. Each source is a
// thin connector (docs/DESIGN.md §1): read a local feed → frame → emit. All
// decoding and orbit math is central — the edge is dumb.
//
// This build implements dial-mode connectors (the collector opens a TCP
// connection to a known receiver on the LAN); the authenticated fleet push
// endpoint is a later pass.
package ingest

import (
	"time"

	"github.com/ptudor/gnss"
)

// RawFrame is one raw broadcast nav frame lifted off a receiver, tagged with just
// enough to dispatch it to the right decoder. Word-oriented frames (GPS/QZSS/
// BeiDou/GLONASS/SBAS from UBX or SBF) carry Words; byte-oriented content (RTCM
// messages, SBF blocks) carries Bytes. The decode stage picks by (GnssID, SigID)
// or message type.
type RawFrame struct {
	Recv    time.Time   // reception time (receiver/host clock)
	Source  string      // ingest source name
	GnssID  gnss.GNSSID // constellation (u-blox numbering)
	SvID    int         // PRN/slot within constellation
	SigID   int         // signal id (0 = primary civil signal)
	FreqID  int         // GLONASS FDMA channel carrier (k = FreqID − 7)
	MsgType int         // SBF block number / RTCM message number (byte-oriented sources)
	Words   []uint32    // 30-bit (or native) nav words, right-aligned
	Bytes   []byte      // raw frame bytes (for byte-oriented sources)
}
