package frame

import "errors"

// NavIC (IRNSS) SPS navigation-message decoding — DEFERRED STUB.
//
// NavIC/IRNSS (u-blox gnssId 7, RINEX letter 'I') is a first-class target in the product plan
// (docs/CONSTELLATIONS.md), but its decoder is a deliberate, tracked deferral rather than an
// oversight. Two things set it apart from the constellations already decoded:
//
//  1. It is a from-scratch signal format. Unlike QZSS (whose L1 C/A LNAV and L2C/L5 CNAV reuse
//     the GPS decoders verbatim) or Galileo F/NAV (already decoded — the regression fix pass just wired
//     its dispatch), the IRNSS SPS L5/S NAV frame has its own subframe layout and word set:
//     real, multi-day ICD-citation engineering from the IRNSS SPS ICD.
//  2. We cannot test it. NavIC's satellites are below the horizon from our stations, so no
//     receiver in the fleet produces NavIC SFRBX; a real stream must be sourced in Asia (see
//     the NTRIP note in navlistener.toml.example). Implementing an ICD decoder with no real
//     frames to validate it against would be untestable guesswork — exactly what the
//     clean-room discipline forbids shipping unmarked.
//
// So this file exists as a signpost: NavIC is known, wanted, and unsupported. The collector's
// dispatch (go/internal/state/state.go) counts every NavIC SFRBX under the `navic_deferred`
// metric label and drops it — no decode happens. When a NavIC source exists, replace this stub
// with a real IRNSS SPS decoder (+ tests against captured frames), wire the gnss.NavIC dispatch
// case, and land it alongside the F/NAV / regression fix work per ordering note.

// ErrNavICDeferred marks the NavIC SPS decoder as an unimplemented, untestable deferral.
var ErrNavICDeferred = errors.New("frame: navic (IRNSS SPS) decoder deferred — from-scratch ICD format with no live stream to validate against ")

// DecodeNavICSPS is the placeholder entry point for the deferred NavIC decoder. It always
// returns ErrNavICDeferred. It is intentionally NOT wired into the collector's Apply dispatch
// (which counts NavIC frames under `navic_deferred` and drops them); it exists so the future
// decoder has a named home and so callers can detect the deferral explicitly rather than by a
// bare "unsupported".
func DecodeNavICSPS(_ []uint32) error {
	return ErrNavICDeferred
}
