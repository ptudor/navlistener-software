package state

import (
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/ingest"
)

// setSignMag writes a sign-magnitude n-bit field (top bit sign, remaining n-1
// bits magnitude) at absolute bit offset start — the encoding gnss/frame's
// BitReader.SignMag decodes, used by GLONASS strings 1-3's coord/vel/accel.
func setSignMag(buf []byte, start, n int, value int64) {
	var sign, mag uint64
	if value < 0 {
		sign, mag = 1, uint64(-value)
	} else {
		mag = uint64(value)
	}
	v := (sign << uint(n-1)) | (mag & ((1 << uint(n-1)) - 1))
	setAbsBits(buf, start, n, v)
}

// glonassStringWords builds the 4-word raw form DecodeGLONASSString expects for
// one string (number 1/2/3), packing the same block-bit offsets the decoder
// reads (gnss/frame/glonass_string.go: string number @1, coord @50, vel @21,
// accel @45; string 2 additionally: health/Bn @5, tb @9). coord/vel/accel are
// raw quantized units (pre-scale-factor) — this test only needs distinct,
// round-trippable values, not physically realistic ones.
func glonassStringWords(number int, coord, vel, accel int64, health, tb int) []uint32 {
	buf := make([]byte, 16) // 128 bits
	setAbsBits(buf, 1, 4, uint64(number))
	setSignMag(buf, 50, 27, coord)
	setSignMag(buf, 21, 24, vel)
	setSignMag(buf, 45, 5, accel)
	if number == 2 {
		setAbsBits(buf, 5, 3, uint64(health))
		setAbsBits(buf, 9, 7, uint64(tb))
	}
	words := make([]uint32, 4)
	for i := 0; i < 4; i++ {
		words[i] = uint32(buf[i*4])<<24 | uint32(buf[i*4+1])<<16 | uint32(buf[i*4+2])<<8 | uint32(buf[i*4+3])
	}
	return words
}

func glonassStringFrame(svID, number int, coord, vel, accel int64, health, tb int, recv time.Time) *ingest.RawFrame {
	return &ingest.RawFrame{
		Recv: recv, Source: "test", GnssID: gnss.GLONASS, SvID: svID, SigID: 0, FreqID: 7,
		Words: glonassStringWords(number, coord, vel, accel, health, tb),
	}
}

// TestGLONASSChangeoverDoesNotMixEpochs guards strings 1/2/3 are coherent
// only within one ~30s frame; applyGLONASS previously reassembled unconditionally
// on every string arrival, so a fresh string arriving at a tb changeover paired
// with the other two strings still cached from up to ~30 minutes earlier —
// mixing X from the new epoch with Y/Z from the old. The reception-time window
//  must reject that and only accept a fully coherent (temporally close)
// triple.
func TestGLONASSChangeoverDoesNotMixEpochs(t *testing.T) {
	st := New(4)
	t0 := time.Unix(1_700_000_000, 0)

	// First, fully coherent triple, all within the same instant.
	st.Apply(glonassStringFrame(7, 1, 1000, 10, 1, 0, 0, t0))
	st.Apply(glonassStringFrame(7, 2, 2000, 20, 2, 0, 450, t0.Add(2*time.Second)))
	st.Apply(glonassStringFrame(7, 3, 3000, 30, 3, 0, 0, t0.Add(4*time.Second)))

	key := Key{G: gnss.GLONASS, Sv: 7, Sig: 0}
	sh := st.shardFor(key)
	sh.mu.Lock()
	firstEph := sh.m[key].gloEph
	haveEph := sh.m[key].haveGloEph
	sh.mu.Unlock()
	if !haveEph {
		t.Fatal("first coherent triple did not assemble")
	}
	if firstEph.X != 1000*gloPosScale {
		t.Fatalf("first eph.X = %v, want %v (sanity check on the encoder)", firstEph.X, 1000*gloPosScale)
	}

	// Changeover: a fresh string 1 (shifted coords, a new epoch) arrives ~30
	// minutes later, alone — strings 2/3 are still the stale cached ones from t0.
	tChangeover := t0.Add(30 * time.Minute)
	st.Apply(glonassStringFrame(7, 1, 9000, 10, 1, 0, 0, tChangeover))

	sh.mu.Lock()
	afterEph := sh.m[key].gloEph
	sh.mu.Unlock()
	if afterEph != firstEph {
		t.Errorf("gloEph changed from an incoherent triple (fresh string 1 + stale strings 2/3): %+v -> %+v",
			firstEph, afterEph)
	}

	// The coherent triple completes once strings 2/3 arrive within the same
	// window as the new string 1 — now the changeover must take effect.
	st.Apply(glonassStringFrame(7, 2, 9500, 20, 2, 0, 450, tChangeover.Add(2*time.Second)))
	st.Apply(glonassStringFrame(7, 3, 9800, 30, 3, 0, 0, tChangeover.Add(4*time.Second)))

	sh.mu.Lock()
	finalEph := sh.m[key].gloEph
	sh.mu.Unlock()
	if finalEph == firstEph {
		t.Error("coherent changeover triple did not update gloEph")
	}
	if finalEph.X != 9000*gloPosScale {
		t.Errorf("final eph.X = %v, want %v (the new epoch's X)", finalEph.X, 9000*gloPosScale)
	}
}

// gloPosScale mirrors gnss/frame's unexported gloPos (2^-11 km) scale factor —
// duplicated here (not imported; frame's constant is unexported) purely to
// translate this test's raw encoded units back to the decoded km for assertions.
const gloPosScale = 1.0 / (1 << 11)
