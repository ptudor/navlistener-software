package state

import (
	"encoding/binary"
	"os"
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/frame"
	"github.com/ptudor/navlistener/internal/ingest"
)

// glonassCaptureFrames extracts the GLONASS UBX-RXM-SFRBX frames from a
// real-receiver capture, in stream order, all stamped at. It re-implements only
// the fixed UBX framing (sync / length / Fletcher checksum) and the SFRBX header
// layout: the production scanner (internal/ingest ubx.go) is unexported and
// state imports ingest, so an in-package state test cannot reach it; the
// scanner itself is validated against these same captures by ingest's own tests.
func glonassCaptureFrames(t *testing.T, path string, at time.Time) []*ingest.RawFrame {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("no capture fixture: %v", err)
	}
	var out []*ingest.RawFrame
	for i := 0; i+8 <= len(data); {
		if data[i] != 0xB5 || data[i+1] != 0x62 {
			i++
			continue
		}
		cls, id := data[i+2], data[i+3]
		n := int(binary.LittleEndian.Uint16(data[i+4:]))
		end := i + 6 + n + 2
		if end > len(data) {
			break
		}
		var ckA, ckB byte
		for _, b := range data[i+2 : i+6+n] {
			ckA += b
			ckB += ckA
		}
		if ckA != data[end-2] || ckB != data[end-1] {
			i++ // corrupt extent: resync byte-by-byte, as the production scanner does
			continue
		}
		if cls == 0x02 && id == 0x13 && n >= 8 { // UBX-RXM-SFRBX
			p := data[i+6 : i+6+n]
			words := int(p[4])
			if gnss.GNSSID(p[0]) == gnss.GLONASS && words > 0 && 8+words*4 <= n {
				f := &ingest.RawFrame{
					Recv: at, Source: "cap",
					GnssID: gnss.GLONASS, SvID: int(p[1]), SigID: int(p[2]), FreqID: int(p[3]),
					Words: make([]uint32, words),
				}
				for w := 0; w < words; w++ {
					f.Words[w] = binary.LittleEndian.Uint32(p[8+w*4:])
				}
				out = append(out, f)
			}
		}
		i = end
	}
	return out
}

// TestGLONASSFrame5RealSuperframe validates the regression fix frame-5 heuristic against
// a real full-superframe fleet capture (independent validation) — the finding's
// own verification step, previously done only with synthetic frames. The fixture
// is 250 s of raw UBX cat'd off capture-station's receiver on 2026-07-12 (GLONASS
// L1OF, 9 SVs, ~1.7 superframes — the earlier f9p_capture.ubx ended ~20 s before
// frame 5's almanac section ever aired).
//
// Ground truth is reconstructed from the broadcast content itself, independently
// of the heuristic under test: within one frame, the almanac pair on strings
// (m, m+1) carries subject slot b+(m−6)/2, so every decoded pair on strings 6–13
// implies the frame's base slot b — b ∈ {1,6,11,16} are frames 1–4 (whose strings
// 14/15 are a legitimate almanac pair), b = 21 is frame 5 (whose strings 14/15
// carry B1/B2/KP UT1 data instead, ICD Ed. 5.1 §4.5). The capture is then replayed
// through the real Store.Apply path and three things are asserted:
//
//  1. every legitimate frames-1–4 string-14/15 almanac (after NA was known) IS
//     stored — i.e. A3's stale-base blind spot (a lost string 6 wrongly skipping
//     a real almanac) did not fire on real data;
//  2. at every replay step every stored slot holds a value actually broadcast as
//     almanac (the regression fix signature was a slot flip-flopping between genuine and
//     garbage every superframe);
//  3. no frame-5 string-14/15 pair landed in the almanac store (the regression fix bug).
//     This capture proves the hazard is live, not theoretical: its frame-5
//     14/15 pairs decode to an IN-RANGE garbage slot — B1's bits misread as
//     "slot 1" — so without the heuristic they would overwrite the genuine
//     slot-1 almanac (broadcast ~10 s later by frame 1) once per superframe,
//     exactly the flip-flop regression fix predicted.
func TestGLONASSFrame5RealSuperframe(t *testing.T) {
	at := time.Unix(1_700_000_000, 0)
	frames := glonassCaptureFrames(t, "../ingest/testdata/glo_superframe_capture.ubx", at)
	if len(frames) < 200 {
		t.Fatalf("only %d GLONASS SFRBX frames in the capture; need a full ~2.5 min superframe", len(frames))
	}

	// --- Ground-truth pass -------------------------------------------------
	// na: the store refuses almanac pairs until string 5 has delivered NA, so
	// note the stream index where that happens and use the same NA for ground-
	// truth decodes.
	na, naIdx := 0, -1
	for i, f := range frames {
		if s, err := frame.DecodeGLONASSString(f.Words); err == nil && s.Number == 5 {
			if n, err := frame.DecodeGLONASSFrameNA(f.Words); err == nil {
				na, naIdx = n, i
				break
			}
		}
	}
	if naIdx < 0 {
		t.Fatal("capture has no decodable string 5 (NA); cannot ground-truth almanac storage")
	}

	type pair1415 struct {
		svid int
		idx  int // stream index of the odd string (when the store would pair it)
		ent  frame.GLONASSAlmanacEntry
	}
	var legit, frame5, unclassified []pair1415

	// broadcast collects, per slot, every DISTINCT almanac copy the capture
	// actually aired (strings 6-13 pairs always; 14/15 pairs except frame 5's).
	// Real GLONASS SVs carry slightly different copies of the same slot's almanac
	// — different upload generations diverge at the LSB in tλ/ΔT/τ and even MnA —
	// so "stored elements never change" is NOT an invariant of correct behaviour.
	// The regression fix signature is storing a value that was never broadcast as an
	// almanac at all; membership in this set is the honest invariant.
	broadcast := map[int][]frame.GLONASSAlmanacEntry{}
	wasBroadcast := func(ent frame.GLONASSAlmanacEntry) bool {
		for _, e := range broadcast[ent.Alm.Slot] {
			if e == ent {
				return true
			}
		}
		return false
	}
	addBroadcast := func(ent frame.GLONASSAlmanacEntry) {
		if !wasBroadcast(ent) {
			broadcast[ent.Alm.Slot] = append(broadcast[ent.Alm.Slot], ent)
		}
	}

	type svTrack struct {
		lastNum  int
		evenNum  int
		even     []uint32
		base     int // implied base slot consensus for the current frame; -1 = conflict
		basePair bool
	}
	// Ground truth is tracked per (SV, signal): each signal's stream has strictly
	// increasing string numbers within a frame, so frame boundaries are clean.
	// (The state machine under test merges L1OF/L2OF per SV — its pairing survives
	// the interleave because an odd string only pairs with a buffered even mate —
	// but the merged stream's duplicated string numbers would wreck this simple
	// boundary tracker, and per-signal truth is truth all the same.)
	trk := map[[2]int]*svTrack{}
	for i, f := range frames {
		if f.SigID != 0 && f.SigID != 2 {
			continue // applyGLONASS only consumes L1OF/L2OF
		}
		s, err := frame.DecodeGLONASSString(f.Words)
		if err != nil {
			continue
		}
		key := [2]int{f.SvID, f.SigID}
		tr := trk[key]
		if tr == nil {
			tr = &svTrack{}
			trk[key] = tr
		}
		if s.Number <= tr.lastNum { // string numbers only increase within a frame
			tr.even, tr.base, tr.basePair = nil, 0, false
		}
		tr.lastNum = s.Number
		switch {
		case s.Number >= 6 && s.Number <= 14 && s.Number%2 == 0:
			tr.even = append([]uint32(nil), f.Words...)
			tr.evenNum = s.Number
		case s.Number >= 7 && s.Number <= 15 && s.Number%2 == 1:
			if tr.even == nil || s.Number != tr.evenNum+1 {
				tr.even = nil
				continue
			}
			ent, err := frame.DecodeGLONASSAlmanac(tr.even, f.Words, na)
			tr.even = nil
			if err != nil {
				continue
			}
			if s.Number <= 13 {
				// Strings 6-13 are always almanac; each pair implies the frame base.
				addBroadcast(ent)
				b := ent.Alm.Slot - (tr.evenNum-6)/2
				switch {
				case !tr.basePair:
					tr.base, tr.basePair = b, true
				case tr.base != b:
					tr.base = -1 // inconsistent decodes: refuse to classify this frame
				}
				continue
			}
			p := pair1415{svid: f.SvID, idx: i, ent: ent}
			switch {
			case !tr.basePair || tr.base == -1:
				// Unknown frame: the heuristic's conservative fallback stores these,
				// so they count as broadcast almanac for the membership check.
				addBroadcast(ent)
				unclassified = append(unclassified, p)
			case tr.base == 21:
				frame5 = append(frame5, p) // B1/B2/KP — deliberately NOT in broadcast
			case tr.base == 1 || tr.base == 6 || tr.base == 11 || tr.base == 16:
				addBroadcast(ent)
				legit = append(legit, p)
			default:
				addBroadcast(ent)
				unclassified = append(unclassified, p)
			}
		}
	}
	if len(legit) == 0 {
		t.Fatal("capture contains no frames-1..4 string-14/15 almanac pair; cannot check the stale-base blind spot")
	}
	if len(frame5) == 0 {
		t.Fatal("capture contains no frame-5 string-14/15 pair; a fixture regression — it must span a full superframe to validate regression fix")
	}

	// --- Whole-stream replay through the real state path -------------------
	// Invariant: at EVERY point of the replay, every stored slot holds a value
	// that was actually broadcast as almanac. Frame-5 B1/B2/KP misreads (the
	// regression fix bug) are excluded from `broadcast`, so a heuristic failure — even one
	// transiently overwritten later — fails this. Cross-SV rewrites between
	// genuine divergent copies are legal and only counted for the log.
	st := New(4)
	prev := map[int]frame.GLONASSAlmanacEntry{}
	crossCopyRewrites := 0
	for _, f := range frames {
		st.Apply(f)
		st.gloAlmMu.Lock()
		for slot, got := range st.gloAlmanac {
			p, seen := prev[slot]
			if !seen || p != got.entry {
				if !wasBroadcast(got.entry) {
					t.Errorf("slot %d stored a value never broadcast as almanac (regression fix class): %+v",
						slot, got.entry)
				}
				if seen {
					crossCopyRewrites++
				}
				prev[slot] = got.entry
			}
		}
		st.gloAlmMu.Unlock()
	}
	divergent := 0
	for _, copies := range broadcast {
		if len(copies) > 1 {
			divergent++
		}
	}

	// --- Per-SV replays: the stale-base blind-spot check -------------------
	// Replayed alone, one SV's stream has a single writer per slot, so every
	// legitimate frames-1..4 strings-14/15 almanac it broadcast after its first
	// NA must be present with exactly its own elements. A missing or different
	// slot means the heuristic skipped a real almanac (A3's blind spot: string 6
	// lost → gloFrameBaseSlot stale at ≥ 21) — grounds to replace it with a real
	// frame counter.
	checked := 0
	svFrames := map[int][]*ingest.RawFrame{}
	svNaIdx := map[int]int{}
	for i, f := range frames {
		svFrames[f.SvID] = append(svFrames[f.SvID], f)
		if _, ok := svNaIdx[f.SvID]; !ok {
			if s, err := frame.DecodeGLONASSString(f.Words); err == nil && s.Number == 5 {
				if _, err := frame.DecodeGLONASSFrameNA(f.Words); err == nil {
					svNaIdx[f.SvID] = i
				}
			}
		}
	}
	for svid, fs := range svFrames {
		naAt, ok := svNaIdx[svid]
		if !ok {
			continue // SV never delivered a string 5; its pairs are refused by design 
		}
		solo := New(1)
		for _, f := range fs {
			solo.Apply(f)
		}
		solo.gloAlmMu.Lock()
		for _, p := range legit {
			if p.svid != svid || p.idx <= naAt {
				continue
			}
			checked++
			got, ok := solo.gloAlmanac[p.ent.Alm.Slot]
			switch {
			case !ok:
				t.Errorf("SV %d solo replay: its own frames-1..4 strings-14/15 almanac (slot %d) is missing — stale-base skip fired on real data",
					svid, p.ent.Alm.Slot)
			case got.entry != p.ent:
				t.Errorf("SV %d solo replay: slot %d holds %+v, not the SV's own 14/15 broadcast %+v",
					svid, p.ent.Alm.Slot, got.entry, p.ent)
			}
		}
		solo.gloAlmMu.Unlock()
	}
	if checked == 0 {
		t.Fatal("no legitimate string-14/15 pair fell after its SV's first NA; blind-spot check is vacuous")
	}

	// Frame-5 pairs, when the fixture carries them (see the doc comment), must
	// never appear in the store; the membership invariant above already enforces
	// it, so here just surface how many in-range garbage slots were at stake.
	inRange := 0
	for _, p := range frame5 {
		if p.ent.Alm.Slot >= 1 && p.ent.Alm.Slot <= 24 {
			inRange++
		}
	}

	t.Logf("real capture: %d GLONASS frames, %d (SV,sig) streams; 14/15 pairs: %d legit (%d blind-spot-checked), %d frame-5 (%d with in-range garbage slot), %d unclassified; %d slots stored, %d slots with divergent inter-SV copies, %d legal cross-copy rewrites",
		len(frames), len(trk), len(legit), checked, len(frame5), inRange, len(unclassified), len(prev), divergent, crossCopyRewrites)
}
