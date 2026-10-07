package identity

import "sort"

// CanonicalSigID maps a receiver-reported u-blox sigId onto the canonical
// primary sigId of the same signal. A receiver tags each tracked component
// separately — Galileo E1-B is sigId 1 on the wire while the configuration
// vocabulary (docs/CONSTELLATIONS.md §2.1, navlistener.toml.example) names the
// signal once by its primary component, "2:0" — and both components carry the
// same navigation message, so they are one capability and one publication
// grant. Every selector that is compared against a frame's sigId (declared
// capabilities, publish_signals, federation signal grants, the demonstrated
// capability fingerprint) is keyed in this canonical space; the per-SV feed
// keys (E##@3 for E5a F/NAV) are deliberately not, because they name the
// decoded message stream rather than the grant.
//
// GLONASS L1OF (6:0) and L2OF (6:2) are different bands with independent
// receiver support and are never merged. Unknown pairs map to themselves.
func CanonicalSigID(gnssID, sigID int) int {
	switch gnssID {
	case 0: // GPS
		switch sigID {
		case 4: // L2 CM → L2 CL
			return 3
		case 7: // L5 Q → L5 I
			return 6
		}
	case 2: // Galileo
		switch sigID {
		case 1: // E1-B → E1-C
			return 0
		case 4: // E5a-Q → E5a-I
			return 3
		case 6: // E5b-Q → E5b-I
			return 5
		}
	case 3: // BeiDou
		switch sigID {
		case 1: // B1I D2 → B1I D1
			return 0
		case 3: // B2I D2 → B2I D1
			return 2
		case 6: // B1 Cd → B1 Cp
			return 5
		case 7: // B2 ap → B2 ad
			return 8
		}
	case 5: // QZSS
		switch sigID {
		case 5: // L2 CL → L2 CM
			return 4
		case 9: // L5 Q → L5 I
			return 8
		}
	}
	return sigID
}

// Canonical returns the selector in the canonical signal space.
func (s Signal) Canonical() Signal {
	return Signal{GnssID: s.GnssID, SigID: CanonicalSigID(s.GnssID, s.SigID)}
}

// canonicalSignals maps every selector through CanonicalSigID, collapses the
// aliases that then coincide (a list naming both "2:1" and "2:0" means one
// grant), and sorts the result by (gnss, sig). nil in, nil out.
func canonicalSignals(signals []Signal) []Signal {
	if signals == nil {
		return nil
	}
	out := make([]Signal, 0, len(signals))
	seen := make(map[Signal]bool, len(signals))
	for _, signal := range signals {
		c := signal.Canonical()
		if seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GnssID != out[j].GnssID {
			return out[i].GnssID < out[j].GnssID
		}
		return out[i].SigID < out[j].SigID
	})
	return out
}
