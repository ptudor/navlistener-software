package frame

import (
	"encoding/binary"
	"errors"
	"math"

	"github.com/ptudor/gnss/glonass"
)

// errGLONASSStringOrder is returned when AssembleGLONASS's arguments aren't
// strings 1/2/3 in that order (regression fix defense-in-depth).
var errGLONASSStringOrder = errors.New("frame: GLONASS strings not in 1/2/3 order")

// errBadStringNum  is returned for a length-valid GLONASS block with string
// number 0 (out of the 1..15 range) — a mis-tagged or corrupt frame.
var errBadStringNum = errors.New("frame: GLONASS string number out of range (1..15)")

// ErrGLONASSHamming  is returned when a string fails the ICD §4.7 Hamming check.
var ErrGLONASSHamming = errors.New("frame: GLONASS string Hamming check failed")

// errBadTb  is returned for a string 2 whose tb index is outside the ICD's
// effective range. tb is a 7-bit index of a 15-min interval within the current day
// (GLONASS ICD Ed. 5.1 §4.4), so the codespace (0..127 ≈ 31.75 h) exceeds a day;
// Table 4.5 bounds the effective range to 15…1425 minutes — index 1..95. An
// out-of-range tb decodes to a finite-but-garbage epoch that EphAgeDay's single
// ±43 200 s wrap then aliases into an in-domain RK4 propagation interval, defeating
// the regression fix domain guard and silently mis-epoching the served position — reject at
// the boundary instead (the regression fix idiom). The §4.7 Hamming check upstream is an
// 8-bit detect-only code, not a strong CRC, so this range gate is real defense.
var errBadTb = errors.New("frame: GLONASS tb index out of range (1..95)")

// errBadFreqCh  is returned when a frequency channel is outside the FDMA
// plan k ∈ [−7, +6] (GLO-ICD-5.1 §3.3.1.1 Table 3.1 and its note: all SVs launched
// after 2005 use K = −7…+6; the almanac word HnA encodes negatives as 25..31 per
// Table 4.10, so HnA 7..24 encodes no valid channel at all). An out-of-domain
// channel is corrupt identity metadata that would ride into FDMA frequency math
// and federation wire records — reject at the boundary (the regression fix idiom).
var errBadFreqCh = errors.New("frame: GLONASS frequency channel out of range (k -7..+6)")

// gloHammingRange builds the inclusive integer range [lo, hi].
func gloHammingRange(lo, hi int) []int {
	s := make([]int, 0, hi-lo+1)
	for i := lo; i <= hi; i++ {
		s = append(s, i)
	}
	return s
}

// gloHammingSets are the data-bit (ICD b_N) sets for check bits β1..β7 (index 0..6),
// GLONASS ICD Ed. 5.1 §4.7 / Table 4.13.
var gloHammingSets = func() [7][]int {
	cat := func(parts ...[]int) []int {
		var out []int
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	return [7][]int{
		{9, 10, 12, 13, 15, 17, 19, 20, 22, 24, 26, 28, 30, 32, 34, 35, 37, 39, 41, 43, 45, 47, 49, 51, 53, 55, 57, 59, 61, 63, 65, 66, 68, 70, 72, 74, 76, 78, 80, 82, 84},
		{9, 11, 12, 14, 15, 18, 19, 21, 22, 25, 26, 29, 30, 33, 34, 36, 37, 40, 41, 44, 45, 48, 49, 52, 53, 56, 57, 60, 61, 64, 65, 67, 68, 71, 72, 75, 76, 79, 80, 83, 84},
		cat(gloHammingRange(10, 12), gloHammingRange(16, 19), gloHammingRange(23, 26), gloHammingRange(31, 34), gloHammingRange(38, 41), gloHammingRange(46, 49), gloHammingRange(54, 57), gloHammingRange(62, 65), gloHammingRange(69, 72), gloHammingRange(77, 80), []int{85}),
		cat(gloHammingRange(13, 19), gloHammingRange(27, 34), gloHammingRange(42, 49), gloHammingRange(58, 65), gloHammingRange(73, 80)),
		cat(gloHammingRange(20, 34), gloHammingRange(50, 65), gloHammingRange(81, 85)),
		gloHammingRange(35, 65),
		gloHammingRange(66, 85),
	}
}()

// glonassHammingValid verifies the GLONASS ICD Ed. 5.1 §4.7 / Table 4.13 Hamming check
// bits over the 85-bit string. ICD bit b_N maps to block offset 85−N (the same
// convention DecodeGLONASSString's field offsets use): check bits β1..β8 are ICD bits 1..8,
// data bits b9..b85 are ICD bits 9..85. Each Cj = βj ⊕ (parity of a fixed data-bit set),
// and CΣ is the parity of the whole 85-bit string. The string carries no detected error
// (and is accepted) iff C1..C7 and CΣ are all zero; any nonzero checksum rejects it.
//
// DETECTION-ONLY BY DESIGN : ICD §4.7 rule (b) additionally defines
// single-bit-error CORRECTION from the syndrome, and the machinery here computes
// everything correction would need — it was considered and DECLINED, not
// overlooked. An (8,4)-class check cannot distinguish a correctable single-bit
// error from a miscorrectable multi-bit one, and for an integrity monitor a
// silently miscorrected string trusted into live state is strictly worse than a
// dropped one that re-broadcasts within 30 s (immediate) / 2.5 min (almanac).
// Cost: a slightly elevated reject rate on noisy push/federation links, visible
// per source via the nav_crc_fail metric's source label. Implement rule (b) only
// if a real feeder ever shows meaningful loss — and then only with a
// corrected-vs-rejected metric split.
func glonassHammingValid(r *BitReader) bool {
	bit := func(icd int) uint64 { v, _ := r.Bits(85-icd, 1); return v & 1 }
	var acc uint64
	for j := 0; j < 7; j++ {
		cj := bit(j + 1) // βj
		for _, i := range gloHammingSets[j] {
			cj ^= bit(i)
		}
		acc |= cj & 1
	}
	// CΣ = parity of β1..β8 (ICD 1..8) and b9..b85 (ICD 9..85) = parity of all 85 bits.
	var csum uint64
	for n := 1; n <= 85; n++ {
		csum ^= bit(n)
	}
	return acc|(csum&1) == 0
}

// StampGLONASSHamming computes and writes the 8 ICD §4.7 check bits (β1..β8) for the 85-bit
// string in words so it passes glonassHammingValid. Exposed for tests (and any GNF1
// re-framer that must synthesize a valid string), mirroring the exported CRC24Q used the
// same way. It mutates the first four words in place. Unlike the CRC stamping helpers,
// this function requires words to contain at least four 32-bit big-endian words and will
// panic on a shorter slice; production ingest never synthesizes strings and should call
// DecodeGLONASSString instead.
func StampGLONASSHamming(words []uint32) {
	buf := make([]byte, 16)
	for i := 0; i < 4; i++ {
		binary.BigEndian.PutUint32(buf[i*4:], words[i])
	}
	getBit := func(icd int) uint64 {
		o := 85 - icd
		return uint64(buf[o>>3]>>(7-uint(o&7))) & 1
	}
	setBit := func(icd int, v uint64) {
		o := 85 - icd
		mask := byte(1) << (7 - uint(o&7))
		if v&1 != 0 {
			buf[o>>3] |= mask
		} else {
			buf[o>>3] &^= mask
		}
	}
	// β1..β7 = parity of their data set (so Cj = 0).
	var betaParity uint64
	for j := 0; j < 7; j++ {
		var p uint64
		for _, i := range gloHammingSets[j] {
			p ^= getBit(i)
		}
		setBit(j+1, p)
		betaParity ^= p
	}
	// β8 = parity(β1..β7) ⊕ parity(b9..b85) so CΣ = 0 (β8 appears only in CΣ).
	var dataParity uint64
	for n := 9; n <= 85; n++ {
		dataParity ^= getBit(n)
	}
	setBit(8, betaParity^dataParity)
	for i := 0; i < 4; i++ {
		words[i] = binary.BigEndian.Uint32(buf[i*4:])
	}
}

// glonassBlock packs the four 32-bit words of a GLONASS string big-endian into a
// 128-bit block and returns a bit reader over it. The 85-bit ICD string maps into the
// block as block bit = 85 − (ICD bit number), so an ICD field spanning bits [lo..hi]
// (hi = MSB / sign) is read at block offset 85−hi, width hi−lo+1 (the offsets below
// are pre-computed from GLONASS ICD Ed. 5.1 Tables 4.11 that way).
func glonassBlock(words []uint32) *BitReader {
	buf := make([]byte, 16)
	for i := 0; i < 4; i++ {
		binary.BigEndian.PutUint32(buf[i*4:], words[i])
	}
	return NewBitReaderN(buf, 128)
}

// GLONASS L1OF/L2OF string decoding (GLONASS ICD Ed. 5.1 §4). u-blox delivers each
// 85-bit string as one UBX-RXM-SFRBX of four 32-bit words = a 128-bit block; the
// string number m sits at bits 1–4 and the string content follows (bit N of the
// 85-bit string maps to block bit 85−N). Strings 1–4 carry the immediate ephemeris
// as a PZ-90 Cartesian state; strings 1/2/3 hold the x/y/z components (with the
// same field offsets per axis), string 2 also the reference time tb and health.
//
// GLONASS fields are sign-magnitude, not two's complement. Offsets and scale
// factors validated against real ZED-F9T frames: √(x²+y²+z²) ≈ 25510 km for every
// SV, and the state stays on-shell when RK4-propagated.

// GLONASS scale factors (km, km/s, km/s²; clock terms dimensionless/seconds).
const (
	gloPos   = 1.0 / (1 << 11)      // 2^-11 km
	gloVel   = 1.0 / float64(1<<20) // 2^-20 km/s
	gloAccel = 1.0 / float64(1<<30) // 2^-30 km/s²
	gloTbSec = 900.0                // tb LSB = 15 min = 900 s

	// SV clock scale factors (regression fix; ICD Ed. 5.1 Table 4.5): γn(tb) LSB = 2^-40
	// (dimensionless), τn(tb) and Δτn LSB = 2^-30 s.
	gloGamma2m40 = 1.0 / float64(uint64(1)<<40)
	gloClk2m30   = 1.0 / float64(1<<30)
)

// GLONASSString holds the decoded fields of one nav string. Coord/Vel/Accel are
// this string's axis component (x for string 1, y for 2, z for 3).
type GLONASSString struct {
	Number int
	Coord  float64 // km
	Vel    float64 // km/s
	Accel  float64 // km/s²
	// Health is string 2's RAW 3-bit Bn word. regression fix — read this before using it:
	// only the MSB (mask 0x4) is the malfunction flag; the ICD says user equipment
	// "does not consider both second and third bits of this word" (GLO-ICD-5.1
	// §4.4), so the natural `Health != 0` test over-flags a healthy SV on a benign
	// low bit. Use Unhealthy() instead of interpreting this field directly.
	Health int
	Tb     float64 // string 2: reference time, seconds of day (index 1..95 × 900 s)

	// P1 is string 1's immediate-data updating flag, raw 2-bit value: the time
	// interval between adjacent tb values is 0 (=not announced) / 30 / 45 / 60
	// minutes for P1 = 0/1/2/3 (GLO-ICD-5.1 §4.4, Table 4.3; Table 4.6: string 1
	// bits 77–78). decoded because it is the broadcast input to any
	// adaptive validity window over tb (regression fix currently uses the fixed 60-min
	// ceiling, the table's maximum).
	P1 int

	// En is string 4's SV-declared age of the immediate data, whole days: the
	// time from the control-segment calculation (upload) of the current set to
	// tb, formed on board (GLO-ICD-5.1 §4.4; Table 4.6: string 4 bits 49–53).
	// a large En at a fresh tb is an upload anomaly — the daemon-side
	// En-vs-computed-age cross-check is the intended consumer.
	En int

	// SV clock terms (regression fix; ICD Ed. 5.1 Tables 4.5/4.6, sign-magnitude per
	// Table 4.5 Note 2): γn(tb) from string 3, τn(tb) and Δτn from string 4.
	GammaN    float64 // string 3: relative frequency deviation, dimensionless
	TauN      float64 // string 4: SV-time-to-GLONASS-time correction at tb, s
	DeltaTauN float64 // string 4: L2−L1 group-delay difference, s

	// Ln is the GLONASS-M ℓn fast malfunction flag : 0 = healthy, 1 =
	// malfunction (ICD Ed. 5.1 §4.4). It is the SV's LOW-LATENCY self-flag —
	// §5.3's note says ℓn exists precisely to cut the onboard malfunction-to-flag
	// delay from ≤1 min (Bn) to ≤10 s — and ICD Table 5.1 defines operability
	// over Bn(ℓn) jointly with the almanac Cn. Meaningful only when LnKnown:
	// ℓn rides strings 3, 5, 7, 9, 11, 13, 15 (Table 4.6: string 3 bit 65;
	// bit 9 in the odd strings 5–15), so 7 of the 15 strings refresh it.
	// Table 4.5 Remark (1) scopes ℓn to GLONASS-M navigation messages; on a
	// legacy-GLONASS message the position is reserved — callers deciding health
	// should still treat a set bit as a malfunction flag (the conservative
	// failure: a spurious not-ok worth investigating, never a silent wrong OK).
	Ln      int
	LnKnown bool
}

// DecodeGLONASSString decodes one string from its four words.
func DecodeGLONASSString(words []uint32) (*GLONASSString, error) {
	if len(words) < 4 {
		return nil, ErrShortFrame
	}
	r := glonassBlock(words)
	// verify the ICD §4.7 Hamming check bits before trusting any field. The receiver
	// is not a documented guarantee of pre-validated strings, and push-path frames arrive
	// from remote feeders; one flipped bit in string 2's tb/health (written straight into
	// live state) mis-epochs the RK4 or flips served health. A detected error rejects the
	// string, mirroring the regression fix/regression fix CRC hardening precedent.
	if !glonassHammingValid(r) {
		return nil, ErrGLONASSHamming
	}
	m, _ := r.Bits(1, 4)
	// string number must be 1..15 (GLONASS ICD Ed. 5.1 §4.1). A length-valid block
	// with string number 0 is mis-tagged or corrupt — reject so the caller counts a decode
	// error and records no capability off garbage.
	if m < 1 || m > 15 {
		return nil, errBadStringNum
	}
	s := &GLONASSString{Number: int(m)}

	// Strings 1–3 share the coordinate/velocity/acceleration field positions. gate
	// the reads on m ∈ 1..3 — for a string 4/5/…/15 those bit spans hold entirely different
	// fields (e.g. string 4's τn), so decoding them into Coord/Vel/Accel gives an external
	// library consumer plausible-looking garbage with no error. In-repo callers are safe
	// (AssembleGLONASS enforces 1/2/3), but the exported struct doc promises these are the
	// axis component; honor it.
	if m >= 1 && m <= 3 {
		coord, _ := r.SignMag(50, 27)
		vel, _ := r.SignMag(21, 24)
		acc, _ := r.SignMag(45, 5)
		s.Coord = float64(coord) * gloPos
		s.Vel = float64(vel) * gloVel
		s.Accel = float64(acc) * gloAccel
	}

	if m == 1 {
		p1, _ := r.Bits(7, 2) // P1 — ICD Table 4.6: string 1 bits 77–78 (85−78 = 7)
		s.P1 = int(p1)
	}

	if m == 2 {
		bn, _ := r.Bits(5, 3)
		tb, _ := r.Bits(9, 7)
		// tb must lie in the ICD's effective range 15…1425 min = index 1..95
		// (GLO-ICD-5.1 Table 4.5; the per-P1 grid of Table 4.3 is finer, but 1..95
		// holds regardless of P1). See errBadTb for why an out-of-day tb is dangerous.
		if tb < 1 || tb > 95 {
			return nil, errBadTb
		}
		s.Health = int(bn)
		s.Tb = float64(tb) * gloTbSec
	}
	if m == 3 {
		gamma, _ := r.SignMag(6, 11) // γn(tb) — ICD Table 4.6: string 3 bits 69–79 (85−79 = 6)
		s.GammaN = float64(gamma) * gloGamma2m40
	}
	// ℓn, the GLONASS-M fast malfunction flag — ICD Table 4.6: string 3
	// bit 65 (block offset 85−65 = 20) and bit 9 (offset 85−9 = 76) of the odd
	// strings 5,7,9,11,13,15. See the Ln field doc for the semantics.
	if m == 3 || (m >= 5 && m%2 == 1) {
		off := 20
		if m != 3 {
			off = 76
		}
		ln, _ := r.Bits(off, 1)
		s.Ln, s.LnKnown = int(ln), true
	}
	if m == 4 {
		tau, _ := r.SignMag(5, 22)  // τn(tb) — ICD Table 4.6: string 4 bits 59–80 (85−80 = 5)
		dtau, _ := r.SignMag(27, 5) // Δτn — ICD Table 4.6: string 4 bits 54–58 (85−58 = 27)
		en, _ := r.Bits(32, 5)      // En — ICD Table 4.6: string 4 bits 49–53 (85−53 = 32)
		s.TauN = float64(tau) * gloClk2m30
		s.DeltaTauN = float64(dtau) * gloClk2m30
		s.En = int(en)
	}
	// regression fix — deliberate deferrals (the NavIC-stub idiom: documented, not
	// forgotten). Words broadcast in these already-parsed strings and NOT decoded,
	// each awaiting a concrete consumer: tk (string 1 bits 65–76 — frame timestamp
	// within the day; a tk-vs-tb plausibility gate), P2 (string 2 bit 77 — tb
	// oddness flag), P4 (string 4 bit 34 — updated-ephemeris-ahead flag), M
	// (string 4 bits 9–10 — GLONASS/GLONASS-M satellite type; Table 4.5
	// Remark 1 scopes the GLONASS-M-only words — M itself plus ℓn, P4, FT,
	// NT, n, P — while En and P1 are legacy words carried by both
	// generations, their rows unmarked in Table 4.5; regression fix), n (string 4
	// bits 11–15 — the broadcast slot number; would enable svId-vs-n
	// cross-checks and unknown-slot recovery, regression fix), FT (string 4 bits
	// 30–33 — accuracy, tracked as regression fix).
	return s, nil
}

// Unhealthy reports the SV-broadcast malfunction state carried by THIS string
// : string 2's Bn MSB (the only Bn bit that means malfunction,
// GLO-ICD-5.1 §4.4) or a set ℓn fast flag on a string that carries one. It is a
// per-string view — a full picture combines Bn (string 2), ℓn (strings
// 3/5/7/9/11/13/15), and the almanac's ground-segment Cn per ICD Table 5.1, as
// the daemon's state layer does. Library consumers should call this rather than
// interpret the raw Health field (whose benign low bits over-flag).
func (s *GLONASSString) Unhealthy() bool {
	return s.Health&0x4 != 0 || (s.LnKnown && s.Ln != 0)
}

// GLONASS almanac scale factors (GLONASS ICD Ed. 5.1 Table 4.9). Angular words are in
// semicircles; ×π converts to radians.
const (
	gloAlm2m20 = 1.0 / (1 << 20) // λ, Δi (semicircle), ε (dimensionless)
	gloAlm2m15 = 1.0 / (1 << 15) // ω (semicircle)
	gloAlm2m9  = 1.0 / (1 << 9)  // ΔT (s)
	gloAlm2m14 = 1.0 / (1 << 14) // ΔT' (s)
	gloAlm2m5  = 1.0 / (1 << 5)  // tλ (s)
	gloAlm2m18 = 1.0 / (1 << 18) // τ (s)
)

// GLONASSAlmanacEntry is one satellite's decoded almanac (GLONASS ICD Ed. 5.1 §4.5),
// assembled from its two-string pair and the frame's NA day number (string 5). Alm is
// the SI-scaled element set the analytic propagator consumes; Cn is the generalized
// health flag broadcast at almanac upload (1 = operable).
type GLONASSAlmanacEntry struct {
	Alm     glonass.Almanac
	Cn      int     // generalized "unhealthy flag": 1 operable, 0 not (ICD §4.5)
	SatType int     // MnA: 0 = GLONASS, 1 = GLONASS-M
	TauNA   float64 // coarse clock correction to GLONASS time, s
}

// DecodeGLONASSAlmanac assembles one satellite's almanac from its two-string pair:
// first ∈ {6,8,10,12,14} carries slot/type/health/τ/λ/Δi/ε, second ∈ {7,9,11,13,15}
// carries ω/tλ/ΔT/ΔT'/H. na is the day number NA from string 5 (the day the almanac
// refers to). Fields are placed per ICD Ed. 5.1 Tables 4.9/4.11; sign-magnitude words
// (τ, λ, Δi, ΔT, ΔT', ω) use the ICD MSB-sign convention (Table 4.9 Note 2). Angles are
// converted from semicircles to radians. The block offsets are 85−(ICD hi-bit).
func DecodeGLONASSAlmanac(first, second []uint32, na int) (GLONASSAlmanacEntry, error) {
	if len(first) < 4 || len(second) < 4 {
		return GLONASSAlmanacEntry{}, ErrShortFrame
	}
	r1 := glonassBlock(first)
	r2 := glonassBlock(second)
	if !glonassHammingValid(r1) || !glonassHammingValid(r2) {
		return GLONASSAlmanacEntry{}, ErrGLONASSHamming
	}
	m1, _ := r1.Bits(1, 4)
	m2, _ := r2.Bits(1, 4)
	if m1 < 6 || m1 > 14 || m1%2 != 0 || m2 != m1+1 {
		return GLONASSAlmanacEntry{}, errBadStringNum
	}

	// First string of the pair (ICD Table 4.11): nA 73-77, MnA 78-79, τnA 63-72,
	// λnA 42-62, ΔinA 24-41, εnA 9-23, CnA 80.
	slot, _ := r1.Bits(8, 5)        // nA   (85−77 = 8)
	mType, _ := r1.Bits(6, 2)       // MnA  (85−79 = 6)
	cn, _ := r1.Bits(5, 1)          // CnA  (85−80 = 5)
	tau, _ := r1.SignMag(13, 10)    // τnA  (85−72 = 13), 2^-18 s
	lambda, _ := r1.SignMag(23, 21) // λnA  (85−62 = 23), 2^-20 sc
	deltaI, _ := r1.SignMag(44, 18) // ΔinA (85−41 = 44), 2^-20 sc
	ecc, _ := r1.Bits(62, 15)       // εnA  (85−23 = 62), 2^-20

	// Second string of the pair (ICD Table 4.11): HnA 10-14, tλnA 44-64, ΔTnA 22-43,
	// ΔT'nA 15-21, ωnA 65-80.
	omega, _ := r2.SignMag(5, 16)     // ωnA   (85−80 = 5),  2^-15 sc
	tLambda, _ := r2.Bits(21, 21)     // tλnA  (85−64 = 21), 2^-5 s
	deltaT, _ := r2.SignMag(42, 22)   // ΔTnA  (85−43 = 42), 2^-9 s
	deltaTdot, _ := r2.SignMag(64, 7) // ΔT'nA (85−21 = 64), 2^-14 s
	hn, _ := r2.Bits(71, 5)           // HnA   (85−14 = 71)

	// an HnA outside the frequency plan is corrupt string content the
	// 8-bit Hamming check can miss — reject the pair rather than store a channel
	// that cannot exist (see errBadFreqCh).
	freqCh, ok := gloHnToChannel(int(hn))
	if !ok {
		return GLONASSAlmanacEntry{}, errBadFreqCh
	}

	return GLONASSAlmanacEntry{
		Cn:      int(cn),
		SatType: int(mType),
		TauNA:   float64(tau) * gloAlm2m18,
		Alm: glonass.Almanac{
			NA:        na,
			Slot:      int(slot),
			FreqCh:    freqCh,
			Lambda:    float64(lambda) * gloAlm2m20 * math.Pi,
			DeltaI:    float64(deltaI) * gloAlm2m20 * math.Pi,
			Omega:     float64(omega) * gloAlm2m15 * math.Pi,
			Ecc:       float64(ecc) * gloAlm2m20,
			Tlambda:   float64(tLambda) * gloAlm2m5,
			DeltaT:    float64(deltaT) * gloAlm2m9,
			DeltaTdot: float64(deltaTdot) * gloAlm2m14,
		},
	}, nil
}

// gloHnToChannel maps the broadcast frequency-number word HnA to the FDMA channel k
// (GLONASS ICD Ed. 5.1 Table 4.10: values 25..31 encode channels −7..−1; 0..6 are
// the non-negative channels verbatim). HnA 7..24 encodes no valid channel
// under the post-2005 frequency plan (§3.3.1.1) — ok is false and the caller must
// reject rather than store an impossible k as identity metadata.
func gloHnToChannel(h int) (k int, ok bool) {
	switch {
	case h >= 25 && h <= 31:
		return h - 32, true
	case h >= 0 && h <= 6:
		return h, true
	default:
		return 0, false
	}
}

// DecodeGLONASSFrameNA reads the calendar day number NA from string 5 (ICD Ed. 5.1
// Table 4.11: NA at string bits 70-80). NA is common to every almanac in the frame —
// the day the almanac elements refer to.
func DecodeGLONASSFrameNA(words []uint32) (int, error) {
	if len(words) < 4 {
		return 0, ErrShortFrame
	}
	r := glonassBlock(words)
	if !glonassHammingValid(r) {
		return 0, ErrGLONASSHamming
	}
	m, _ := r.Bits(1, 4)
	if m != 5 {
		return 0, errBadStringNum
	}
	na, _ := r.Bits(5, 11) // NA (85−80 = 5), 11 bits
	return int(na), nil
}

// AssembleGLONASS combines strings 1/2/3 into the PZ-90 Cartesian ephemeris the
// RK4 propagator consumes. slot is the GLONASS slot number; freqID is the UBX
// freqId (the FDMA channel is freqID−7). tb and the health come from string 2;
// γn rides string 3. s4 is nil-tolerant : when present it must be a
// same-frame string 4 and contributes the SV clock terms τn/Δτn (ClockKnown);
// when nil the ephemeris assembles clockless — the caller enforces the
// same-frame temporal window for all strings, exactly as for strings 1–3.
func AssembleGLONASS(slot, freqID int, s1, s2, s3, s4 *GLONASSString) (glonass.Ephemeris, error) {
	if s1 == nil || s2 == nil || s3 == nil {
		return glonass.Ephemeris{}, ErrShortFrame
	}
	// regression fix (defense-in-depth): the caller is expected to pass strings in their
	// broadcast slots, but assert it rather than silently combine x/y/z components
	// from the wrong string numbers if a caller ever mis-wires the arguments.
	if s1.Number != 1 || s2.Number != 2 || s3.Number != 3 {
		return glonass.Ephemeris{}, errGLONASSStringOrder
	}
	if s4 != nil && s4.Number != 4 {
		return glonass.Ephemeris{}, errGLONASSStringOrder
	}
	// k = freqID − 7 must be a real FDMA channel −7..+6 (freqID 0..13,
	// GLO-ICD-5.1 §3.3.1.1) before it is stored as ephemeris identity metadata.
	// freqID is the RECEIVER's tag (u-blox freqId byte, 0..255 on the wire), not
	// Hamming-protected string content — an unvalidated corrupt byte would ride
	// into FDMA frequency math and federation records with no error.
	if freqID < 0 || freqID > 13 {
		return glonass.Ephemeris{}, errBadFreqCh
	}
	eph := glonass.Ephemeris{
		X: s1.Coord, Vx: s1.Vel, Ax: s1.Accel,
		Y: s2.Coord, Vy: s2.Vel, Ay: s2.Accel,
		Z: s3.Coord, Vz: s3.Vel, Az: s3.Accel,
		Tb:       s2.Tb,
		TodKnown: true, // string 2 carries tb; the day is anchored
		GammaN:   s3.GammaN,
		FreqCh:   freqID - 7,
		Slot:     slot,
	}
	if s4 != nil {
		eph.TauN, eph.DeltaTauN, eph.ClockKnown = s4.TauN, s4.DeltaTauN, true
	}
	return eph, nil
}
