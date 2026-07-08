package state

import (
	"math"
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/frame"
	"github.com/ptudor/navlistener/internal/ingest"
)

// --- synthetic LNAV builders (mirror the frame package's, using the exported
// GPSParity as the parity oracle) ---

func fieldOff(w, a int) int { return (w-1)*24 + (a - 1) }

func setField(buf []byte, w, a, n int, val int64) {
	start := fieldOff(w, a)
	u := uint64(val) & ((1 << uint(n)) - 1)
	for i := 0; i < n; i++ {
		if u&(1<<uint(n-1-i)) != 0 {
			pos := start + i
			buf[pos>>3] |= 1 << uint(7-(pos&7))
		}
	}
}

func setSplit32(buf []byte, wHi, aHi, wLo int, val int64) {
	u := uint64(uint32(val))
	setField(buf, wHi, aHi, 8, int64(u>>24))
	setField(buf, wLo, 1, 24, int64(u&0xFFFFFF))
}

func read24(buf []byte, wordIdx int) uint32 {
	var v uint32
	for i := 0; i < 24; i++ {
		pos := wordIdx*24 + i
		b := (buf[pos>>3] >> uint(7-(pos&7))) & 1
		v = (v << 1) | uint32(b)
	}
	return v
}

func findParity(data24, d29, d30 uint32) uint32 {
	for p := uint32(0); p < 64; p++ {
		if _, ok := frame.GPSParity((data24<<6)|p, d29, d30); ok {
			return p
		}
	}
	return 0
}

func packWords(buf []byte) []uint32 {
	words := make([]uint32, 10)
	var d29, d30 uint32
	for i := 0; i < 10; i++ {
		want := read24(buf, i)
		stored := want
		if d30 == 1 {
			stored = ^stored & 0xFFFFFF
		}
		p := findParity(stored, d29, d30)
		word := (stored << 6) | p
		words[i] = word
		d29 = (word >> 1) & 1
		d30 = word & 1
	}
	return words
}

func sf1Words(iodcLo int64) []uint32 {
	buf := make([]byte, 30)
	setField(buf, 2, 20, 3, 1)
	setField(buf, 3, 1, 10, 2200)
	setField(buf, 3, 13, 4, 4)
	setField(buf, 8, 1, 8, iodcLo)
	setField(buf, 8, 9, 16, 27000)
	setField(buf, 10, 1, 22, 214748)
	return packWords(buf)
}

func sf2Words(iode, m0 int64) []uint32 {
	buf := make([]byte, 30)
	setField(buf, 2, 20, 3, 2)
	setField(buf, 3, 1, 8, iode)
	setField(buf, 3, 9, 16, -640)
	setField(buf, 4, 1, 16, 12596)
	setSplit32(buf, 4, 17, 5, m0)
	setField(buf, 6, 1, 16, 537)
	setSplit32(buf, 6, 17, 7, 42949673)
	setField(buf, 8, 1, 16, 4295)
	setSplit32(buf, 8, 17, 9, 2702199603)
	setField(buf, 10, 1, 16, 27000)
	return packWords(buf)
}

func sf3Words(iode int64) []uint32 {
	buf := make([]byte, 30)
	setField(buf, 2, 20, 3, 3)
	setField(buf, 3, 1, 16, 54)
	setSplit32(buf, 3, 17, 4, -341830285)
	setField(buf, 5, 1, 16, -107)
	setSplit32(buf, 5, 17, 6, 656335797)
	setField(buf, 7, 1, 16, 8000)
	setSplit32(buf, 7, 17, 8, 478536844)
	setField(buf, 9, 1, 24, -22679)
	setField(buf, 10, 1, 8, iode)
	setField(buf, 10, 9, 14, 279)
	return packWords(buf)
}

func gpsFrame(words []uint32, recv time.Time) *ingest.RawFrame {
	return &ingest.RawFrame{Recv: recv, Source: "test", GnssID: gnss.GPS, SvID: 5, SigID: 0, Words: words}
}

func TestStoreEndToEnd(t *testing.T) {
	st := New(4)
	now := time.Unix(1_700_000_000, 0)
	st.Apply(gpsFrame(sf1Words(85), now))
	st.Apply(gpsFrame(sf2Words(85, 205075516), now))
	st.Apply(gpsFrame(sf3Words(85), now))
	st.Propagate(now)

	snap := st.Snapshot(now)
	e, ok := snap.SVs["G05@0"]
	if !ok {
		t.Fatalf("G05@0 not in snapshot: %+v", snap.SVs)
	}
	if e.XM == nil || e.YM == nil || e.ZM == nil {
		t.Fatal("G05@0 has no propagated position")
	}
	radius := math.Sqrt((*e.XM)*(*e.XM) + (*e.YM)*(*e.YM) + (*e.ZM)*(*e.ZM))
	if radius < 26.0e6 || radius > 27.2e6 {
		t.Errorf("propagated radius = %.0f m, want ~26560 km", radius)
	}
	if snap.LiveSVs != 1 {
		t.Errorf("live_svs = %d, want 1", snap.LiveSVs)
	}
}

func TestStoreDiscoOnIODChange(t *testing.T) {
	st := New(4)
	now := time.Unix(1_700_000_000, 0)
	// First data set (IODE 85).
	st.Apply(gpsFrame(sf1Words(85), now))
	st.Apply(gpsFrame(sf2Words(85, 205075516), now))
	st.Apply(gpsFrame(sf3Words(85), now))
	// A new data set (IODE 86) with a slightly shifted M0 → a small orbit-disco.
	st.Apply(gpsFrame(sf1Words(86), now))
	st.Apply(gpsFrame(sf2Words(86, 205075516+2000), now))
	st.Apply(gpsFrame(sf3Words(86), now))

	snap := st.Snapshot(now)
	e := snap.SVs["G05@0"]
	if e.OrbitDisco == nil {
		t.Fatal("orbit_disco not computed on IOD change")
	}
	if *e.OrbitDisco <= 0 || *e.OrbitDisco > 5000 {
		t.Errorf("orbit_disco = %v m, want a small positive jump", *e.OrbitDisco)
	}
	if e.TimeDisco == nil {
		t.Error("time_disco not computed on IOD change")
	}
}

func TestStoreExpire(t *testing.T) {
	st := New(2)
	now := time.Unix(1_700_000_000, 0)
	st.Apply(gpsFrame(sf1Words(85), now))
	st.Apply(gpsFrame(sf2Words(85, 205075516), now))
	st.Apply(gpsFrame(sf3Words(85), now))
	st.Expire(now.Add(3*time.Hour), 2*time.Hour)
	if len(st.Snapshot(now.Add(3*time.Hour)).SVs) != 0 {
		t.Error("stale SV should have been expired")
	}
}
