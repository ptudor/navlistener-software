// rfsurvey reads observer USB console logs and decodes the RF evidence records
// with the collector's historian decoder. It also accepts a live log on stdin.
package main

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ptudor/navlistener/internal/ingest"
)

var uptime = regexp.MustCompile(`\(([0-9]+)\) receiver: $`)

type record struct {
	Line       int           `json:"line"`
	CapturedAt *time.Time    `json:"captured_at,omitempty"`
	UptimeMS   *uint64       `json:"uptime_ms,omitempty"`
	RF         *ingest.RawRF `json:"rf"`
}

func decode(in io.Reader, out io.Writer) error {
	s := bufio.NewScanner(in)
	enc := json.NewEncoder(out)
	count := 0
	for line := 1; s.Scan(); line++ {
		before, body, found := strings.Cut(s.Text(), "RF evidence=")
		if !found {
			continue
		}
		// ESP-IDF may end its log line with the ANSI color reset.
		body, _, _ = strings.Cut(body, "\x1b")
		raw, err := hex.DecodeString(strings.TrimSpace(body))
		if err != nil {
			return fmt.Errorf("line %d: %w", line, err)
		}
		f, err := ingest.StoredFrame(ingest.StoredInput{Origin: "rf", Kind: "jamming", Raw: raw})
		if err != nil {
			return fmt.Errorf("line %d: %w", line, err)
		}
		r := record{Line: line, RF: f.RF}
		if m := uptime.FindStringSubmatch(before); m != nil {
			if n, err := strconv.ParseUint(m[1], 10, 64); err == nil {
				r.UptimeMS = &n
			}
		}
		if first, _, ok := strings.Cut(before, " "); ok {
			if t, err := time.Parse(time.RFC3339Nano, first); err == nil {
				r.CapturedAt = &t
			}
		}
		if err := enc.Encode(r); err != nil {
			return err
		}
		count++
	}
	if err := s.Err(); err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("no RF evidence records in the console log")
	}
	return nil
}

func main() {
	if err := decode(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
