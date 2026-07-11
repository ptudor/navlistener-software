package frame

import (
	"errors"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/clock"
	"github.com/ptudor/gnss/kepler"
	"github.com/ptudor/gnss/physconst"
)

// errBeiDouSOWGap is returned when D1 subframes 1/2/3 are not broadcast-adjacent
//  — the only coherence guard available, since D1 has no IODE/IODC-style tag.
var errBeiDouSOWGap = errors.New("frame: BeiDou D1 subframes not broadcast-adjacent (SOW gap)")

// errBadFraID  is returned for a length-valid D1 frame with an out-of-range
// FraID (not 1..5) — a mis-tagged or corrupt frame.
var errBadFraID = errors.New("frame: BeiDou D1 FraID out of range (1..5)")

// BeiDou D1 NAV decoding (BDS-SIS-ICD-B1I v3.0 §5.2.4), for MEO/IGSO SVs. u-blox
// delivers each 300-bit subframe as one UBX-RXM-SFRBX of ten 30-bit words. The
// receiver has removed the BCH(15,11) parity, leaving the information bits at the
// top of each word: 26 bits in word 1, 22 bits in words 2–10. We concatenate those
// into a 224-bit information stream and read fields at their ICD offsets. Offsets
// and scale factors follow BDS-SIS-ICD-B1I v3.0 Figures 5-8…5-10 and Tables
// 5-5…5-10, and the decode cross-validates against the B-CNAV2 (B2a) decode of
// the same SVs on real captured frames (frame test).

// BeiDou scale factors beyond the shared set (note Crc/Crs use 2^-6, not GPS 2^-5).
const (
	p2m6  = 1.0 / (1 << 6)
	p2m24 = 1.0 / (1 << 24)
	p2m27 = 1.0 / (1 << 27)
	p2m50 = 1.0 / float64(uint64(1)<<50)
	p2m66 = p2m33 * p2m33 // 2^-66 (2^66 overflows uint64)
	bdsT0 = 8.0           // toe / toc step, seconds (2^3)
)

// BeiDouSubframe holds the decoded fields of one D1 subframe (FraID 1–3 carry the
// clock and the two ephemeris halves; a full set is assembled with AssembleBeiDou).
type BeiDouSubframe struct {
	FraID         int
	SOW           int
	Health        int // SatH1
	AODC          int // age of clock data (ICD Table 5-6)
	URAI          int
	WN            int // BDT week number
	Toc           float64
	Af0, Af1, Af2 float64
	TGD1, TGD2    float64    // B1I / B2I equipment group delays, seconds
	Alpha, Beta   [4]float64 // Klobuchar coefficients (ICD Table 5-5)
	AODE          int        // age of ephemeris data (ICD Table 5-8)
	toeMSB        int
	toeLSB        int
	eph           kepler.Ephemeris
}

// beidouInfo builds the 224-bit information stream from the ten words: the top 26
// bits of word 1 then the top 22 bits of words 2–10 (the BCH parity, already used
// by the receiver, occupies the low bits).
func beidouInfo(words []uint32) []byte {
	buf := make([]byte, 28) // 224 bits
	pos := 0
	put := func(v uint32, n int) {
		for i := 0; i < n; i++ {
			if v&(1<<uint(n-1-i)) != 0 {
				buf[pos>>3] |= 1 << uint(7-(pos&7))
			}
			pos++
		}
	}
	put((words[0]>>4)&0x3FFFFFF, 26)
	for i := 1; i < 10; i++ {
		put((words[i]>>8)&0x3FFFFF, 22)
	}
	return buf
}

// DecodeBeiDouD1 decodes one D1 subframe from ten words.
func DecodeBeiDouD1(words []uint32) (*BeiDouSubframe, error) {
	if len(words) < 10 {
		return nil, ErrShortFrame
	}
	r := NewBitReaderN(beidouInfo(words), 224)
	u := func(p, n int) uint64 { v, _ := r.Bits(p, n); return v }
	s := func(p, n int) int64 { v, _ := r.Signed(p, n); return v }

	fra := u(15, 3)
	sf := &BeiDouSubframe{
		FraID: int(fra),
		SOW:   int(u(18, 8)<<12 | u(26, 12)),
	}
	semi := physconst.Pi
	switch fra {
	case 1:
		// Figure 5-8. Note the clock order quirk: B1I broadcasts a2 BEFORE
		// a0 and a1 (a2@162, a0@173, a1@197), and the Klobuchar α/β set
		// rides in subframe 1 (@98–161), unlike GPS (subframe 4).
		sf.Health = int(u(38, 1))
		sf.AODC = int(u(39, 5))
		sf.URAI = int(u(44, 4))
		sf.WN = int(u(48, 13))
		sf.Toc = float64(u(61, 17)) * bdsT0
		sf.TGD1 = float64(s(78, 10)) * 1e-10 // 0.1 ns
		sf.TGD2 = float64(s(88, 10)) * 1e-10
		sf.Alpha[0] = float64(s(98, 8)) * p2m30
		sf.Alpha[1] = float64(s(106, 8)) * p2m27
		sf.Alpha[2] = float64(s(114, 8)) * p2m24
		sf.Alpha[3] = float64(s(122, 8)) * p2m24
		sf.Beta[0] = float64(s(130, 8)) * (1 << 11)
		sf.Beta[1] = float64(s(138, 8)) * (1 << 14)
		sf.Beta[2] = float64(s(146, 8)) * (1 << 16)
		sf.Beta[3] = float64(s(154, 8)) * (1 << 16)
		sf.Af2 = float64(s(162, 11)) * p2m66
		sf.Af0 = float64(s(173, 24)) * p2m33
		sf.Af1 = float64(s(197, 22)) * p2m50
		sf.AODE = int(u(219, 5))
	case 2:
		sf.eph.DeltaN = float64(s(38, 16)) * p2m43 * semi
		sf.eph.Cuc = float64(s(54, 18)) * p2m31
		sf.eph.M0 = float64(s(72, 32)) * p2m31 * semi
		sf.eph.Ecc = float64(u(104, 32)) * p2m33
		sf.eph.Cus = float64(s(136, 18)) * p2m31
		sf.eph.Crc = float64(s(154, 18)) * p2m6
		sf.eph.Crs = float64(s(172, 18)) * p2m6
		sf.eph.SqrtA = float64(u(190, 32)) * p2m19
		sf.toeMSB = int(u(222, 2))
	case 3:
		// Figure 5-10 field order: toe(15 LSB), i0, Cic, Ω̇, Cis, IDOT, Ω0, ω —
		// note IDOT sits BETWEEN Cis and Ω0 (the word-split fields are
		// contiguous in the parity-stripped information stream). Verified
		// against BDS-SIS-ICD-B1I v3.0 and cross-validated against the
		// ICD-authoritative B-CNAV2 decode of the same SVs (frame test).
		sf.toeLSB = int(u(38, 15))
		sf.eph.I0 = float64(s(53, 32)) * p2m31 * semi
		sf.eph.Cic = float64(s(85, 18)) * p2m31
		sf.eph.OmegaDot = float64(s(103, 24)) * p2m43 * semi
		sf.eph.Cis = float64(s(127, 18)) * p2m31
		sf.eph.IDot = float64(s(145, 14)) * p2m43 * semi
		sf.eph.Omega0 = float64(s(159, 32)) * p2m31 * semi
		sf.eph.Omega = float64(s(191, 32)) * p2m31 * semi
	case 4, 5:
		// Almanac/integrity pages: a structurally valid FraID, not decoded here.
	default:
		// FraID must be 1..5 (BDS-SIS-ICD-B1I §5.2). A length-valid frame with an
		// out-of-range FraID (0/6/7) is mis-tagged or corrupt — reject so the caller counts
		// a decode error and records no capability off garbage.
		return nil, errBadFraID
	}
	return sf, nil
}

// AssembleBeiDou combines D1 subframes 1/2/3 into the ephemeris and clock model.
// toe is the 17-bit value split across subframes 2 (2 MSB) and 3 (15 LSB). svid
// tags the constellation; kepler applies the GEO rotation for C01–C05/C59–C63.
func AssembleBeiDou(svid int, sf1, sf2, sf3 *BeiDouSubframe) (kepler.Ephemeris, clock.Model, error) {
	if sf1 == nil || sf2 == nil || sf3 == nil {
		return kepler.Ephemeris{}, clock.Model{}, ErrShortFrame
	}
	// D1 carries no AODE-style pairing tag across subframes (unlike GPS
	// IODE/IODC, Galileo IODnav, or B-CNAV2's SOW-adjacency check on m10/m11), so
	// broadcast adjacency — sf1/sf2/sf3 are each exactly 6s apart within one 30s
	// D1 frame — is the only valid rule. Without it, a stale sf2 (e.g. from
	// before an hourly changeover, after a subframe-2 loss) can pair with a
	// fresh sf1/sf3: toe is split across sf2/sf3, so this splices a toe
	// belonging to neither, fabricating a garbage ephemeris that would
	// otherwise pass the toe-based IOD gate in state.go and fire a false
	// critical orbit-disco event. The delta wraps mod 604800  so the one
	// legitimate frame per week that straddles the BDT rollover (604794 → 0 → 6)
	// still assembles; the exact +6 rule is unchanged everywhere else, and an
	// out-of-domain SOW fails the check rather than being normalized (sowDelta).
	d21, ok21 := sowDelta(sf2.SOW, sf1.SOW)
	d32, ok32 := sowDelta(sf3.SOW, sf2.SOW)
	if !ok21 || !ok32 || d21 != 6 || d32 != 6 {
		return kepler.Ephemeris{}, clock.Model{}, errBeiDouSOWGap
	}
	eph := sf2.eph
	eph.I0, eph.Cic, eph.OmegaDot = sf3.eph.I0, sf3.eph.Cic, sf3.eph.OmegaDot
	eph.Cis, eph.Omega0, eph.Omega, eph.IDot = sf3.eph.Cis, sf3.eph.Omega0, sf3.eph.Omega, sf3.eph.IDot
	eph.Toe = float64(sf2.toeMSB<<15|sf3.toeLSB) * bdsT0
	eph.ID = gnss.BeiDou
	eph.SVID = svid
	clk := clock.Model{
		ID:  gnss.BeiDou,
		Toc: sf1.Toc, Af0: sf1.Af0, Af1: sf1.Af1, Af2: sf1.Af2, TGD: sf1.TGD1,
	}
	return eph, clk, nil
}
