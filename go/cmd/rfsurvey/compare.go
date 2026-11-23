package main

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
)

type count struct{ flagged, assessed int }

func (c count) String() string {
	if c.assessed == 0 {
		return "unassessed"
	}
	return fmt.Sprintf("%d/%d", c.flagged, c.assessed)
}

// A capture supports a steady value when the center was assessed in every
// report, the detector was operating, and repeated reports agreed.
func (c count) steady(total int) bool {
	return total >= 2 && c.assessed == total && (c.flagged == 0 || c.flagged == total)
}

type capture struct {
	reports, known, warnings int
	centers                  map[uint32]count
	// Multiple gains within a capture are meaningful. Include the tuning in
	// the row key so a receiver retune never joins two different RF paths.
	gains       map[string]map[byte]bool
	sensitivity map[bool]bool
}

func readCapture(path string) (*capture, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	c := &capture{centers: make(map[uint32]count), gains: make(map[string]map[byte]bool), sensitivity: make(map[bool]bool)}
	err = scan(f, func(r record) error {
		if h := r.RF.HighSensitivity; h != nil {
			c.sensitivity[*h] = true
		}
		if s := r.RF.Spectrum; s != nil {
			key := fmt.Sprintf("Path %d PGA (dB), %.6f MHz center, %d Hz span, %d Hz bins",
				s.Path, float64(s.CenterHz)/1e6, s.SpanHz, s.ResolutionHz)
			if c.gains[key] == nil {
				c.gains[key] = make(map[byte]bool)
			}
			c.gains[key][s.PGADB] = true
		}
		if s := r.RF.SignalSecurity; s != nil {
			c.reports++
			if !s.JamEnabled || (s.JamState != 1 && s.JamState != 2) {
				return nil
			}
			c.known++
			if s.JamState == 2 {
				c.warnings++
			}
			for _, center := range s.Centers {
				n := c.centers[center.KHz]
				n.assessed++
				if center.Jammed {
					n.flagged++
				}
				c.centers[center.KHz] = n
			}
		}
		return nil
	})
	return c, err
}

func (c *capture) sensitivityText() string {
	switch len(c.sensitivity) {
	case 0:
		return "unknown"
	case 2:
		return "changed"
	}
	if c.sensitivity[true] {
		return "on"
	}
	return "off"
}

func compare(paths []string, out io.Writer) error {
	var captures []*capture
	centers := make(map[uint32]count)
	settings := make(map[string]map[byte]bool)
	for _, path := range paths {
		c, err := readCapture(path)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		captures = append(captures, c)
		maps.Copy(centers, c.centers)
		maps.Copy(settings, c.gains)
	}
	sensitivity := captures[0].sensitivityText()
	sameSensitivity := sensitivity == "on" || sensitivity == "off"
	for _, c := range captures[1:] {
		sameSensitivity = sameSensitivity && c.sensitivityText() == sensitivity
	}
	var report bytes.Buffer
	t := tabwriter.NewWriter(&report, 0, 4, 2, ' ', 0)
	labels := []string{"Original", "Moved", "Returned"}[:len(paths)]
	fmt.Fprintln(t, "Measurement\t"+strings.Join(labels, "\t"))
	row := func(label string, value func(*capture) string) {
		fmt.Fprint(t, label)
		for _, c := range captures {
			fmt.Fprintf(t, "\t%s", value(c))
		}
		fmt.Fprintln(t)
	}
	row("SEC-SIG reports (known/total)", func(c *capture) string { return fmt.Sprintf("%d/%d", c.known, c.reports) })
	row("Overall warnings/known", func(c *capture) string { return fmt.Sprintf("%d/%d", c.warnings, c.known) })
	row("High sensitivity", (*capture).sensitivityText)
	var reversed []string
	for _, khz := range slices.Sorted(maps.Keys(centers)) {
		mhz := fmt.Sprintf("%.3f MHz", float64(khz)/1000)
		row(mhz+" flagged/assessed", func(c *capture) string { return c.centers[khz].String() })
		if len(captures) == 3 {
			a, b, back := captures[0].centers[khz], captures[1].centers[khz], captures[2].centers[khz]
			if a.steady(captures[0].reports) && b.steady(captures[1].reports) && back.steady(captures[2].reports) &&
				(a.flagged == 0) != (b.flagged == 0) && (a.flagged == 0) == (back.flagged == 0) {
				reversed = append(reversed, mhz)
			}
		}
	}
	for _, key := range slices.Sorted(maps.Keys(settings)) {
		row(key, func(c *capture) string {
			var gains []string
			for _, g := range slices.Sorted(maps.Keys(c.gains[key])) {
				gains = append(gains, strconv.Itoa(int(g)))
			}
			if len(gains) == 0 {
				return "unavailable"
			}
			return strings.Join(gains, ", ")
		})
	}
	t.Flush() // writes only to the in-memory buffer
	fmt.Fprintln(&report)
	switch {
	case len(centers) == 0:
		fmt.Fprintln(&report, "Capture rf survey at each resting position until the receiver assesses frequencies.")
	case len(captures) == 2:
		fmt.Fprintln(&report, "Return the connected antenna to its original position and capture a third window to check repeatability.")
	case len(reversed) > 0:
		fmt.Fprintf(&report, "%s changed at the second position and reverted on return.\n", strings.Join(reversed, ", "))
		if sameSensitivity {
			fmt.Fprintln(&report, "Test antenna placement and cable routing separately to narrow the pickup path.")
		} else {
			fmt.Fprintln(&report, "Verify the same detector sensitivity at each position before attributing the change to placement.")
		}
	default:
		fmt.Fprintln(&report, "The captures do not show a consistent per-center reversal across all three windows. Check for mixed readings and changes in gain.")
		fmt.Fprintln(&report, "If the warning persists, try a known-good antenna or cable at a fixed position, then compare a second receiver on the same feed.")
	}
	_, err := io.Copy(out, &report)
	return err
}
