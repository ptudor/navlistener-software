package frame

import (
	"encoding/binary"
	"testing"

	"github.com/ptudor/gnss"
)

// Every Kepler-family epoch field is bounded by its ICD to lie inside the week
// (GPS/QZSS ≤ 604 784, CNAV ≤ 604 500, Galileo ≤ 604 740, BDS ≤ 604 792) while the
// bit field codes values well past it. EphAge wraps once, so an out-of-domain
// reference would alias into a plausible age and kepler.Solve would return a
// finite, wrong position with no error; the decoders therefore reject a value at
// or past the week (errBadEpoch) and accept the ICD maximum.

func TestCNAVRejectsEpochOutsideWeek(t *testing.T) {
	if _, err := DecodeGPSCNAV(gnss.GPS, cnavMsg10Words(2288, 0, 2, 2047)); err != errBadEpoch {
		t.Errorf("MT10 toe raw 2047 (614 100 s): err = %v, want errBadEpoch", err)
	}
	if _, err := DecodeGPSCNAV(gnss.GPS, cnavMsg10Words(2288, 0, 2, 2016)); err != errBadEpoch {
		t.Errorf("MT10 toe raw 2016 (604 800 s): err = %v, want errBadEpoch", err)
	}
	if m, err := DecodeGPSCNAV(gnss.GPS, cnavMsg10Words(2288, 0, 2, 2015)); err != nil || m.eph.Toe != 604500 {
		t.Errorf("MT10 toe raw 2015: m=%v err=%v, want toe 604500 accepted", m, err)
	}
	if _, err := DecodeGPSCNAV(gnss.GPS, cnavMsg11Words(2047)); err != errBadEpoch {
		t.Errorf("MT11 toe raw 2047: err = %v, want errBadEpoch", err)
	}
	if _, err := DecodeGPSCNAV(gnss.GPS, cnavMsg30Words(2047, 0, 0, 0, 0, 0, 0, 0, 0)); err != errBadEpoch {
		t.Errorf("MT30 toc raw 2047: err = %v, want errBadEpoch", err)
	}
	if m, err := DecodeGPSCNAV(gnss.GPS, cnavMsg30Words(2015, 0, 0, 0, 0, 0, 0, 0, 0)); err != nil || m.clk.Toc != 604500 {
		t.Errorf("MT30 toc raw 2015: m=%v err=%v, want toc 604500 accepted", m, err)
	}
	// The 17-bit TOW count codes up to 786 426 s.
	buf := make([]byte, 40)
	setBits(buf, 8, 6, 5)
	setBits(buf, 14, 6, 10)
	setBits(buf, 20, 17, 131071)
	setBits(buf, 70, 11, 10)
	setCNAVPreambleAndCRC(buf)
	words := make([]uint32, 10)
	for i := range words {
		words[i] = binary.BigEndian.Uint32(buf[i*4:])
	}
	if _, err := DecodeGPSCNAV(gnss.GPS, words); err != errBadEpoch {
		t.Errorf("CNAV TOW count 131071 (786 426 s): err = %v, want errBadEpoch", err)
	}
}

func TestGalileoINAVRejectsEpochOutsideWeek(t *testing.T) {
	word1 := func(t0eRaw uint64) []uint32 {
		content := make([]byte, 16)
		setContentBits(content, 0, 1, 6)
		setContentBits(content, 6, 7, 10)
		setContentBits(content, 16, t0eRaw, 14)
		return buildGalileoINAVWords(content)
	}
	if _, err := DecodeGalileoINAV(word1(16383)); err != errBadEpoch {
		t.Errorf("word 1 t0e raw 16383 (983 040 s): err = %v, want errBadEpoch", err)
	}
	if _, err := DecodeGalileoINAV(word1(10080)); err != errBadEpoch {
		t.Errorf("word 1 t0e raw 10080 (604 800 s): err = %v, want errBadEpoch", err)
	}
	if w, err := DecodeGalileoINAV(word1(10079)); err != nil || w.eph.Toe != 604740 {
		t.Errorf("word 1 t0e raw 10079: w=%v err=%v, want t0e 604740 accepted", w, err)
	}
	word4 := func(t0cRaw uint64) []uint32 {
		content := make([]byte, 16)
		setContentBits(content, 0, 4, 6)
		setContentBits(content, 6, 7, 10)
		setContentBits(content, 16, 14, 6)
		setContentBits(content, 54, t0cRaw, 14)
		return buildGalileoINAVWords(content)
	}
	if _, err := DecodeGalileoINAV(word4(16383)); err != errBadEpoch {
		t.Errorf("word 4 t0c raw 16383: err = %v, want errBadEpoch", err)
	}
	if w, err := DecodeGalileoINAV(word4(10079)); err != nil || w.clk.Toc != 604740 {
		t.Errorf("word 4 t0c raw 10079: w=%v err=%v, want t0c 604740 accepted", w, err)
	}
	word5 := func(tow uint64) []uint32 {
		content := make([]byte, 16)
		setContentBits(content, 0, 5, 6)
		setContentBits(content, 73, 1264, 12)
		setContentBits(content, 85, tow, 20)
		return buildGalileoINAVWords(content)
	}
	if _, err := DecodeGalileoINAV(word5(1048575)); err != errBadEpoch {
		t.Errorf("word 5 TOW 1048575: err = %v, want errBadEpoch", err)
	}
	if w, err := DecodeGalileoINAV(word5(604799)); err != nil || w.TOW != 604799 {
		t.Errorf("word 5 TOW 604799: w=%v err=%v, want accepted", w, err)
	}
}

func TestGalileoFNAVRejectsEpochOutsideWeek(t *testing.T) {
	page := func(pageType int, fill func(buf []byte)) []uint32 {
		buf := make([]byte, 32)
		setFNAVBufBits(buf, 0, uint64(pageType), 6)
		if pageType == 1 {
			setFNAVBufBits(buf, 6, 14, 6)
			setFNAVBufBits(buf, 12, 7, 10)
		} else {
			setFNAVBufBits(buf, 6, 7, 10)
		}
		fill(buf)
		words := fnavBufToWords(buf)
		StampGalileoFNAVCRC(words)
		return words
	}
	if _, err := DecodeGalileoFNAV(page(1, func(b []byte) { setFNAVBufBits(b, 22, 16383, 14) })); err != errBadEpoch {
		t.Errorf("page 1 t0c raw 16383: err = %v, want errBadEpoch", err)
	}
	if w, err := DecodeGalileoFNAV(page(1, func(b []byte) { setFNAVBufBits(b, 22, 10079, 14) })); err != nil || w.clk.Toc != 604740 {
		t.Errorf("page 1 t0c raw 10079: w=%v err=%v, want t0c 604740 accepted", w, err)
	}
	if _, err := DecodeGalileoFNAV(page(1, func(b []byte) { setFNAVBufBits(b, 167, 1048575, 20) })); err != errBadEpoch {
		t.Errorf("page 1 TOW 1048575: err = %v, want errBadEpoch", err)
	}
	if _, err := DecodeGalileoFNAV(page(3, func(b []byte) { setFNAVBufBits(b, 160, 16383, 14) })); err != errBadEpoch {
		t.Errorf("page 3 t0e raw 16383: err = %v, want errBadEpoch", err)
	}
	if w, err := DecodeGalileoFNAV(page(3, func(b []byte) { setFNAVBufBits(b, 160, 10079, 14) })); err != nil || w.eph.Toe != 604740 {
		t.Errorf("page 3 t0e raw 10079: w=%v err=%v, want t0e 604740 accepted", w, err)
	}
}

// d1Subframe builds one BeiDou D1 subframe (info bits, then the delivered word
// layout and BCH stamps of d1TestWords) with the given SOW and extra fields.
func d1Subframe(fraID, sow int, fill func(info []byte)) []uint32 {
	info := make([]byte, 28)
	setBits(info, 15, 3, uint64(fraID))
	setBits(info, 18, 8, uint64(sow>>12))
	setBits(info, 26, 12, uint64(sow&0xFFF))
	if fill != nil {
		fill(info)
	}
	r := NewBitReaderN(info, 224)
	words := make([]uint32, 10)
	v, _ := r.Bits(0, 26)
	words[0] = uint32(v) << 4
	for i := 1; i < 10; i++ {
		v, _ = r.Bits(26+(i-1)*22, 22)
		words[i] = uint32(v) << 8
	}
	StampBeiDouD1BCH(words)
	return words
}

func TestBeiDouD1RejectsEpochOutsideWeek(t *testing.T) {
	if _, err := DecodeBeiDouD1(d1Subframe(1, 100, func(b []byte) { setBits(b, 61, 17, 131071) })); err != errBadEpoch {
		t.Errorf("sf1 toc raw 131071 (1 048 568 s): err = %v, want errBadEpoch", err)
	}
	if sf, err := DecodeBeiDouD1(d1Subframe(1, 100, func(b []byte) { setBits(b, 61, 17, 75599) })); err != nil || sf.Toc != 604792 {
		t.Errorf("sf1 toc raw 75599: sf=%v err=%v, want toc 604792 accepted", sf, err)
	}
	// toe is split across subframes 2 and 3 and bounded at assembly.
	assemble := func(toeRaw int) error {
		sf1, err := DecodeBeiDouD1(d1Subframe(1, 100, nil))
		if err != nil {
			t.Fatal(err)
		}
		sf2, err := DecodeBeiDouD1(d1Subframe(2, 106, func(b []byte) { setBits(b, 222, 2, uint64(toeRaw>>15)) }))
		if err != nil {
			t.Fatal(err)
		}
		sf3, err := DecodeBeiDouD1(d1Subframe(3, 112, func(b []byte) { setBits(b, 38, 15, uint64(toeRaw&0x7FFF)) }))
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = AssembleBeiDou(6, sf1, sf2, sf3)
		return err
	}
	if err := assemble(131071); err != errBadEpoch {
		t.Errorf("toe raw 131071: err = %v, want errBadEpoch", err)
	}
	if err := assemble(75599); err != nil {
		t.Errorf("toe raw 75599 (604 792 s): err = %v, want accepted", err)
	}
}

func TestBCNAV2RejectsEpochOutsideWeek(t *testing.T) {
	message := func(mesType int, fill func(buf []byte)) []uint32 {
		buf := make([]byte, 36)
		setBits(buf, 0, 6, 30)
		setBits(buf, 6, 6, uint64(mesType))
		setBits(buf, 12, 18, 100)
		if mesType == 10 {
			setBits(buf, 72, 2, 3) // SatType MEO
		}
		fill(buf)
		c := CRC24Q(buf[:33])
		buf[33], buf[34], buf[35] = byte(c>>16), byte(c>>8), byte(c)
		words := make([]uint32, 9)
		for i := range words {
			words[i] = binary.BigEndian.Uint32(buf[i*4:])
		}
		return words
	}
	if _, err := DecodeBeiDouBCNAV2(message(10, func(b []byte) { setBits(b, 61, 11, 2047) })); err != errBadEpoch {
		t.Errorf("MT10 toe raw 2047 (614 100 s): err = %v, want errBadEpoch", err)
	}
	if m, err := DecodeBeiDouBCNAV2(message(10, func(b []byte) { setBits(b, 61, 11, 2015) })); err != nil || m.eph.Toe != 604500 {
		t.Errorf("MT10 toe raw 2015: m=%v err=%v, want toe 604500 accepted", m, err)
	}
	if _, err := DecodeBeiDouBCNAV2(message(30, func(b []byte) { setBits(b, 42, 11, 2047) })); err != errBadEpoch {
		t.Errorf("MT30 toc raw 2047: err = %v, want errBadEpoch", err)
	}
	if _, err := DecodeBeiDouBCNAV2(message(34, func(b []byte) { setBits(b, 64, 11, 2047) })); err != errBadEpoch {
		t.Errorf("MT34 toc raw 2047: err = %v, want errBadEpoch", err)
	}
	if m, err := DecodeBeiDouBCNAV2(message(34, func(b []byte) { setBits(b, 64, 11, 2015) })); err != nil || m.clk.Toc != 604500 {
		t.Errorf("MT34 toc raw 2015: m=%v err=%v, want toc 604500 accepted", m, err)
	}
}
