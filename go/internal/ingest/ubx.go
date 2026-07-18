package ingest

import (
	"bufio"
	"encoding/binary"
	"io"
	"math"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/metrics"
)

// UBX protocol constants (u-blox interface description). SFRBX carries raw nav
// subframe words per gnssId/sigId (docs/CONSTELLATIONS.md §2.1).
const (
	ubxSync1      = 0xB5
	ubxSync2      = 0x62
	ubxClassNAV   = 0x01
	ubxIDNAVSAT   = 0x35
	ubxClassRXM   = 0x02
	ubxIDSFRBX    = 0x13
	ubxIDRAWX     = 0x15
	ubxClassMON   = 0x0A
	ubxIDMONHW    = 0x09
	ubxIDMONRF    = 0x38
	ubxMaxPayload = 1 << 14 // guard against a corrupt length prefix
)

// scanUBX reads a UBX byte stream and calls emit for each UBX-RXM-SFRBX message,
// converted to a RawFrame. It resynchronises on the 0xB5 0x62 sync pattern and
// drops any message whose Fletcher checksum fails, so a mid-stream connect or a
// corrupt frame never derails the reader. It returns when the reader errors (EOF /
// connection drop), which the connector treats as a reconnect trigger.
func scanUBX(r io.Reader, source string, now func() time.Time, emit func(*RawFrame), onErr func(kind string)) error {
	br := bufio.NewReaderSize(r, 1<<16)
	for {
		// Resynchronise to the sync pattern.
		if err := syncTo(br, ubxSync1, ubxSync2); err != nil {
			return err
		}
		// peek the header+payload+checksum without consuming it. On a
		// length or checksum failure, nothing here is Discarded, so the next
		// syncTo call resumes scanning byte-by-byte from right after the sync
		// pattern instead of skipping the whole claimed (possibly bogus) extent
		// (up to ubxMaxPayload+2 = 16386 bytes), which may contain a real frame.
		hdr, err := br.Peek(4) // class, id, length(2, LE)
		if err != nil {
			return err
		}
		length := int(binary.LittleEndian.Uint16(hdr[2:]))
		if length > ubxMaxPayload {
			onErr("ubx_length") // implausible length: skip and resync
			continue
		}
		total := 4 + length + 2 // header + payload + 2 checksum bytes
		peeked, err := br.Peek(total)
		if err != nil {
			return err
		}
		body := peeked[4 : 4+length]
		ckA, ckB := fletcher8(peeked[0], peeked[1], peeked[2], peeked[3], body)
		if ckA != peeked[total-2] || ckB != peeked[total-1] {
			onErr("ubx_checksum")
			continue
		}
		// Copy what's needed before Discard invalidates br's buffer view (peeked
		// aliases it and is only valid until the next read/Discard).
		cls, id := peeked[0], peeked[1]
		body = append([]byte(nil), body...)
		if _, err := br.Discard(total); err != nil {
			return err
		}
		switch {
		case cls == ubxClassRXM && id == ubxIDSFRBX:
			if f := parseSFRBX(body, source, now()); f != nil {
				emit(f)
			} else {
				onErr("ubx_sfrbx")
			}
		case cls == ubxClassRXM && id == ubxIDRAWX:
			// only STRUCTURAL malformation is a parse error. A well-formed
			// RAWX with nothing usable — cold-start week==0/invalid rcvTow, or every
			// measurement's PR-valid bit clear — is a normal receiver start-up state,
			// already accounted per-field by RawObsInvalidTotal; counting it here too
			// inflated IngestErrorsTotal{ubx_rawx} through every cold/warm start (and
			// double-counted the week==0 case).
			if _, ok := parseRAWX(body, source, now(), emit); !ok {
				onErr("ubx_rawx")
			}
		case cls == ubxClassMON && id == ubxIDMONRF:
			if f := parseMONRF(body, source, now()); f != nil {
				emit(f)
			} else {
				onErr("ubx_monrf")
			}
		case cls == ubxClassMON && id == ubxIDMONHW:
			if f := parseMONHW(body, source, now()); f != nil {
				emit(f)
			} else {
				onErr("ubx_monhw")
			}
		case cls == ubxClassNAV && id == ubxIDNAVSAT:
			if f := parseNAVSAT(body, source, now()); f != nil {
				emit(f)
			} else {
				onErr("ubx_navsat")
			}
		}
	}
}

// parseRAWX converts a UBX-RXM-RAWX payload into one observation RawFrame per
// measurement (u-blox interface description: 16-byte header — rcvTow R8 in
// seconds of week, week U2, leapS I1, numMeas U1, recStat X1 — then 32 bytes per
// measurement). These are the dual-frequency observables feeding the measured
// ionosphere (docs/MATH.md §7.4); they are telemetry, not nav frames, so they
// carry Obs instead of Words. Returns the number of measurements emitted and
// whether the payload was structurally well-formed : ok=false only for
// bytes that cannot carry a valid RAWX (short header, numMeas overrunning the
// payload) — a checksum-valid message shaped like that means the receiver/link
// really did corrupt something. A structurally-valid payload with no usable
// measurements returns (0, true).
func parseRAWX(p []byte, source string, recv time.Time, emit func(*RawFrame)) (int, bool) {
	if len(p) < 16 {
		return 0, false
	}
	rcvTow := math.Float64frombits(binary.LittleEndian.Uint64(p[0:]))
	week := int(binary.LittleEndian.Uint16(p[8:]))
	numMeas := int(p[11])
	if 16+numMeas*32 > len(p) {
		return 0, false
	}
	if !finiteFloat(rcvTow) || rcvTow < 0 || rcvTow >= 604800 || week == 0 {
		metrics.RawObsInvalidTotal.WithLabelValues(source, "time").Inc()
		return 0, true
	}
	emitted := 0
	for i := 0; i < numMeas; i++ {
		m := p[16+i*32:]
		prM := math.Float64frombits(binary.LittleEndian.Uint64(m[0:]))
		cpCyc := math.Float64frombits(binary.LittleEndian.Uint64(m[8:]))
		doHz := math.Float32frombits(binary.LittleEndian.Uint32(m[16:]))
		trkStat := m[30]
		// trkStat bit 0 = pseudorange valid, bit 1 = carrier phase valid.
		if trkStat&0x01 == 0 {
			continue
		}
		if !finiteFloat(prM) || prM <= 0 || prM > 1e9 {
			metrics.RawObsInvalidTotal.WithLabelValues(source, "pseudorange").Inc()
			continue
		}
		if !finiteFloat(float64(doHz)) || math.Abs(float64(doHz)) > 1e6 {
			metrics.RawObsInvalidTotal.WithLabelValues(source, "doppler").Inc()
			continue
		}
		cpValid := trkStat&0x02 != 0
		arcBreak := false
		if cpValid && (!finiteFloat(cpCyc) || math.Abs(cpCyc) > 1e10) {
			metrics.RawObsInvalidTotal.WithLabelValues(source, "carrier").Inc()
			cpValid, arcBreak = false, true
			cpCyc = 0
		}
		emit(&RawFrame{
			Recv:   recv,
			Source: source,
			GnssID: gnss.GNSSID(m[20]),
			SvID:   int(m[21]),
			SigID:  int(m[22]),
			FreqID: int(m[23]),
			Obs: &RawObs{
				RcvTow:     rcvTow,
				Week:       week,
				PrM:        prM,
				CpCyc:      cpCyc,
				DoHz:       float64(doHz),
				LockTimeMs: int(binary.LittleEndian.Uint16(m[24:])),
				Cn0:        int(m[26]),
				CpValid:    cpValid,
				CycleSlip:  trkStat&0x08 != 0,
				ArcBreak:   arcBreak,
			},
		})
		emitted++
	}
	return emitted, true
}

func finiteFloat(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// parseSFRBX converts a UBX-RXM-SFRBX payload to a RawFrame. Layout (F9/M9
// generation): gnssId, svId, sigId, freqId, numWords, reserved, version,
// reserved, then numWords little-endian 32-bit dwrds each holding one native nav
// word right-aligned. Returns nil on a malformed/short payload.
func parseSFRBX(p []byte, source string, recv time.Time) *RawFrame {
	if len(p) < 8 {
		return nil
	}
	gnssID := gnss.GNSSID(p[0])
	svID := int(p[1])
	sigID := int(p[2])
	freqID := int(p[3])
	numWords := int(p[4])
	if numWords == 0 || 8+numWords*4 > len(p) {
		return nil
	}
	words := make([]uint32, numWords)
	for i := 0; i < numWords; i++ {
		words[i] = binary.LittleEndian.Uint32(p[8+i*4:])
	}
	return &RawFrame{
		Recv:   recv,
		Source: source,
		GnssID: gnssID,
		SvID:   svID,
		SigID:  sigID,
		FreqID: freqID,
		Words:  words,
	}
}

// parseMONRF converts a UBX-MON-RF payload (F9+ RF-front-end telemetry) into a
// station-scoped RF sample (docs/DEFENSE-PNT.md §1). Layout: version U1, nBlocks U1,
// reserved U1[2], then nBlocks × 24-byte blocks — blockId U1, flags X1 (bits 0-1 =
// jammingState), antStatus U1, antPower U1, postStatus U4, reserved U1[4], noisePerMS U2,
// agcCnt U2, jamInd U1, … Every length is bounds-checked. Returns nil on a short frame.
func parseMONRF(p []byte, source string, recv time.Time) *RawFrame {
	if len(p) < 4 {
		return nil
	}
	nBlocks := int(p[1])
	if nBlocks == 0 || 4+nBlocks*24 > len(p) {
		return nil
	}
	rf := &RawRF{Bands: make([]RFBand, 0, nBlocks)}
	for i := 0; i < nBlocks; i++ {
		b := p[4+i*24:]
		rf.Bands = append(rf.Bands, RFBand{
			Block:     int(b[0]),
			JamState:  int(b[1] & 0x03),
			AntStatus: int(b[2]),
			// postStatus U4 sits at +4, reserved U1[4] at +8 (12 bytes total,
			// matching the doc comment above), then noisePerMS U2 @+12, agcCnt U2
			// @+14, jamInd U1 @+16 — the previous +14/+16/+20 offsets read agcCnt as
			// noise, jamInd|ofsI<<8 as AGC, and magQ as CWSuppress.
			NoiseLevel: int(binary.LittleEndian.Uint16(b[12:])),
			AGC:        int(binary.LittleEndian.Uint16(b[14:])),
			CWSuppress: int(b[16]), // jamInd (CW-jamming indicator, 0..255)
		})
	}
	return &RawFrame{Source: source, Recv: recv, RF: rf}
}

// parseMONHW converts a legacy UBX-MON-HW payload (60 bytes) into a single-band RF
// sample: noisePerMS U2 @16, agcCnt U2 @18, aStatus U1 @20, flags X1 @22 (jammingState in
// bits 2-3), jamInd U1 @45 (docs/DEFENSE-PNT.md §1). Returns nil on a short frame.
func parseMONHW(p []byte, source string, recv time.Time) *RawFrame {
	if len(p) < 60 {
		return nil
	}
	band := RFBand{
		Block:      0,
		NoiseLevel: int(binary.LittleEndian.Uint16(p[16:])),
		AGC:        int(binary.LittleEndian.Uint16(p[18:])),
		AntStatus:  int(p[20]),
		JamState:   int((p[22] >> 2) & 0x03),
		CWSuppress: int(p[45]),
	}
	return &RawFrame{Source: source, Recv: recv, RF: &RawRF{Bands: []RFBand{band}}}
}

// parseNAVSAT converts a UBX-NAV-SAT payload into per-SV C/N₀ + elevation for the
// C/N₀-vs-elevation spoofing gate (docs/DEFENSE-PNT.md §3). Layout: iTOW U4, version U1,
// numSvs U1, reserved U1[2], then numSvs × 12-byte blocks — gnssId U1, svId U1, cno U1
// (dB-Hz), elev I1 (deg), azim I2, prRes I2, flags X4 (bit 3 = svUsed). Bounds-checked.
func parseNAVSAT(p []byte, source string, recv time.Time) *RawFrame {
	if len(p) < 8 {
		return nil
	}
	numSvs := int(p[5])
	if numSvs == 0 || 8+numSvs*12 > len(p) {
		return nil
	}
	rf := &RawRF{Sats: make([]SatCN0, 0, numSvs)}
	for i := 0; i < numSvs; i++ {
		s := p[8+i*12:]
		flags := binary.LittleEndian.Uint32(s[8:])
		rf.Sats = append(rf.Sats, SatCN0{
			GnssID:  int(s[0]),
			SvID:    int(s[1]),
			Cn0:     int(s[2]),
			ElevDeg: int(int8(s[3])),
			Used:    flags&0x08 != 0,
		})
	}
	return &RawFrame{Source: source, Recv: recv, RF: rf}
}

// syncTo advances the reader until the two-byte sync pattern s1,s2 is consumed.
func syncTo(br *bufio.Reader, s1, s2 byte) error {
	for {
		b, err := br.ReadByte()
		if err != nil {
			return err
		}
		if b != s1 {
			continue
		}
		b2, err := br.ReadByte()
		if err != nil {
			return err
		}
		if b2 == s2 {
			return nil
		}
		// Not the pattern; b2 might itself be s1 — push it back to re-examine.
		if b2 == s1 {
			_ = br.UnreadByte()
		}
	}
}

// fletcher8 computes the UBX 8-bit Fletcher checksum over class, id, the two
// length bytes, and the payload.
func fletcher8(cls, id, lenLo, lenHi byte, body []byte) (ckA, ckB byte) {
	add := func(x byte) {
		ckA += x
		ckB += ckA
	}
	add(cls)
	add(id)
	add(lenLo)
	add(lenHi)
	for _, x := range body {
		add(x)
	}
	return ckA, ckB
}
