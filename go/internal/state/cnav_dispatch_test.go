package state

import (
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/frame"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/metrics"
)

func dispatchCNAVWords() []uint32 {
	buf := make([]byte, 40)
	buf[0] = 0x8B
	setAbsBits(buf, 14, 6, 10)
	setAbsBits(buf, 276, 24, uint64(frame.CRC24QBits(buf, 0, 276)))
	words := make([]uint32, 10)
	for i := range words {
		words[i] = binary.BigEndian.Uint32(buf[i*4:])
	}
	return words
}

// TestCNAVAndNavICStateDispatch guards hand-maintained asymmetric signal map at
// the actual Apply boundary, including metrics, capability evidence, and decode/discard.
func TestCNAVAndNavICStateDispatch(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	for _, tc := range []struct {
		id   gnss.GNSSID
		sigs []int
	}{
		{gnss.GPS, []int{3, 4, 6, 7}},
		{gnss.QZSS, []int{4, 5, 8, 9}},
	} {
		for _, sig := range tc.sigs {
			t.Run(fmt.Sprintf("%d_%d", tc.id, sig), func(t *testing.T) {
				s := New(2)
				counter := metrics.DecodeTotal.WithLabelValues(fmt.Sprint(int(tc.id)), "cnav")
				before := testutil.ToFloat64(counter)
				source := fmt.Sprintf("cnav-%d-%d", tc.id, sig)
				s.Apply(&ingest.RawFrame{Source: source, GnssID: tc.id, SvID: 1, SigID: sig, Recv: now, Words: dispatchCNAVWords()})
				if got := testutil.ToFloat64(counter) - before; got != 1 {
					t.Fatalf("DecodeTotal delta = %v, want 1", got)
				}
				caps := s.FeedStationCapabilities(now)[source]
				if len(caps) != 1 || caps[0].Gnss != int(tc.id) || caps[0].Sig != sig {
					t.Fatalf("capabilities = %+v, want (%d,%d)", caps, tc.id, sig)
				}
				if svs := s.FeedSVs(now); len(svs) != 0 {
					t.Fatalf("capability-only CNAV created SV state: %+v", svs)
				}
			})
		}
	}

	for _, tc := range []struct {
		id  gnss.GNSSID
		sig int
	}{{gnss.QZSS, 1}, {gnss.GPS, 5}} {
		s := New(2)
		counter := metrics.DecodeErrorsTotal.WithLabelValues(fmt.Sprint(int(tc.id)), "unsupported")
		before := testutil.ToFloat64(counter)
		s.Apply(&ingest.RawFrame{Source: "negative", GnssID: tc.id, SvID: 1, SigID: tc.sig, Recv: now, Words: dispatchCNAVWords()})
		if got := testutil.ToFloat64(counter) - before; got != 1 {
			t.Errorf("(%d,%d) unsupported delta = %v, want 1", tc.id, tc.sig, got)
		}
		if caps := s.FeedStationCapabilities(now)["negative"]; len(caps) != 0 {
			t.Errorf("unsupported (%d,%d) recorded capability: %+v", tc.id, tc.sig, caps)
		}
	}

	s := New(2)
	deferred := metrics.DecodeErrorsTotal.WithLabelValues(fmt.Sprint(int(gnss.NavIC)), "navic_deferred")
	before := testutil.ToFloat64(deferred)
	s.Apply(&ingest.RawFrame{Source: "navic", GnssID: gnss.NavIC, SvID: 1, SigID: 0, Recv: now, Words: []uint32{0}})
	if got := testutil.ToFloat64(deferred) - before; got != 1 {
		t.Fatalf("navic_deferred delta = %v, want 1", got)
	}
	if len(s.FeedSVs(now)) != 0 || len(s.FeedStationCapabilities(now)["navic"]) != 0 {
		t.Fatal("deferred NavIC frame mutated state or capability")
	}
}
