package frame

import "github.com/ptudor/gnss/gnsstime"

// sowDelta returns the shortest signed a−b difference on the GNSS week ring
// (gnsstime.SOWDelta, regression fix), rejecting inputs outside the transmitted
// seconds-of-week domain [0, 604800). The decoders do not range-check SOW
// fields, so an out-of-domain value from a corrupt frame must fail here
// rather than be normalized into an apparently broadcast-adjacent delta.
func sowDelta(a, b int) (int, bool) {
	const week = int(gnsstime.WeekSeconds)
	if a < 0 || a >= week || b < 0 || b >= week {
		return 0, false
	}
	return gnsstime.SOWDelta(a, b), true
}
