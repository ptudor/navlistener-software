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

func scan(in io.Reader, visit func(record) error) error {
	s := bufio.NewScanner(in)
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
		if err := visit(r); err != nil {
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

func run(args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 {
		enc := json.NewEncoder(out)
		return scan(in, func(r record) error { return enc.Encode(r) })
	}
	if args[0] == "compare" && (len(args) == 3 || len(args) == 4) {
		return compare(args[1:], out)
	}
	return fmt.Errorf("usage: rfsurvey < console.log | rfsurvey compare original.log moved.log [returned.log]")
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
