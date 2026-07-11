package frame

const secondsPerWeek = 604800

// sowDelta returns the shortest signed a-b difference on a GNSS week ring.
// Inputs outside the transmitted seconds-of-week domain are rejected rather
// than normalized into an apparently fresh value.
func sowDelta(a, b int) (int, bool) {
	if a < 0 || a >= secondsPerWeek || b < 0 || b >= secondsPerWeek {
		return 0, false
	}
	d := a - b
	if d > secondsPerWeek/2 {
		d -= secondsPerWeek
	}
	if d < -secondsPerWeek/2 {
		d += secondsPerWeek
	}
	return d, true
}
