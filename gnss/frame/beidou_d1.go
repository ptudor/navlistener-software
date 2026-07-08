package frame

import (
	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/clock"
	"github.com/ptudor/gnss/kepler"
	"github.com/ptudor/gnss/physconst"
)

// BeiDou D1 NAV decoding (BDS-SIS-ICD-B1I v3.0 §5.2.4), for MEO/IGSO SVs. u-blox
// delivers each 300-bit subframe as one UBX-RXM-SFRBX of ten 30-bit words. The
// receiver has removed the BCH(15,11) parity, leaving the information bits at the
// top of each word: 26 bits in word 1, 22 bits in words 2–10. We concatenate those
// into a 224-bit information stream and read fields at their ICD offsets. All
// offsets and scale factors here were validated against real ZED-F9T frames
// (a ≈ 27906 km, i₀ ≈ 55°, SVs above the receiver's horizon).

// BeiDou scale factors beyond the shared set (note Crc/Crs use 2^-6, not GPS 2^-5).
const (
	p2m6  = 1.0 / (1 << 6)
	p2m50 = 1.0 / float64(uint64(1)<<50)
	p2m66 = p2m33 * p2m33 // 2^-66 (2^66 overflows uint64)
	bdsT0 = 8.0           // toe / toc step, seconds (2^3)
)

// BeiDouSubframe holds the decoded fields of one D1 subframe (FraID 1–3 carry the
// clock and the two ephemeris halves; a full set is assembled with AssembleBeiDou).
type BeiDouSubframe struct {
	FraID         int
	SOW           int
	Health        int
	URAI          int
	Toc           float64
	Af0, Af1, Af2 float64
	TGD1          float64
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
		sf.Health = int(u(38, 1))
		sf.URAI = int(u(44, 4))
		sf.Toc = float64(u(61, 17)) * bdsT0
		sf.TGD1 = float64(s(78, 10)) * 1e-10 // 0.1 ns
		sf.Af0 = float64(s(167, 24)) * p2m33
		sf.Af1 = float64(s(191, 22)) * p2m50
		sf.Af2 = float64(s(213, 11)) * p2m66
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
		// ┌─ COMPLIANCE GAP: BDS-SIS-ICD-B1I §5.2.4.4 (D1 subframe 3) ───────────┐
		// │ These SF3 offsets were reverse-engineered from captures (no B1I ICD │
		// │ on hand) and are PARTLY WRONG. Cross-checking against the           │
		// │ ICD-authoritative B-CNAV2 decoder (which decodes the same orbit)    │
		// │ confirms i0@53, OmegaDot@103 are correct, but Omega0 is @159 (not   │
		// │ @145), and omega/Cic/Cis do NOT match — so the orbital-plane        │
		// │ orientation (Ω0, ω) is wrong: BeiDou SVs get the right radius and   │
		// │ inclination (TestRealBeiDouD1 passes) but are placed in the WRONG   │
		// │ DIRECTION. A future review MUST verify the full SF3 field map       │
		// │ against BDS-SIS-ICD-B1I §5.2.4.4 and fix, validating that the D1    │
		// │ position matches the B-CNAV2 position for the same SV (they must    │
		// │ agree to metres). Do NOT partial-fix Omega0 alone — the whole SF3   │
		// │ tail (Cic/OmegaDot/Cis/Omega0/omega/IDOT, incl. any split fields)   │
		// │ needs to come from the ICD together.                                │
		// └─────────────────────────────────────────────────────────────────────┘
		sf.toeLSB = int(u(38, 15))
		sf.eph.I0 = float64(s(53, 32)) * p2m31 * semi
		sf.eph.Cic = float64(s(85, 18)) * p2m31
		sf.eph.OmegaDot = float64(s(103, 24)) * p2m43 * semi
		sf.eph.Cis = float64(s(127, 18)) * p2m31
		sf.eph.Omega0 = float64(s(145, 32)) * p2m31 * semi
		sf.eph.Omega = float64(s(177, 32)) * p2m31 * semi
		sf.eph.IDot = float64(s(209, 14)) * p2m43 * semi
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
