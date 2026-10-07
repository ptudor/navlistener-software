// Command stationreplay replays a station's stored inputs through the collector's
// station integrity checks and station detectors, and prints the resulting timeline
// (docs/proposals/STATION-ASSURANCE.md §7, item 2.3).
//
// Two sources, same replay:
//
//	stationreplay -config navlistener.toml -station obs -since 2026-10-05T10:00:00Z -until 2026-10-05T11:00:00Z
//	stationreplay -config navlistener.toml -audience operator:collector-a -id 42
//
// The first reads the station's rf_samples (NAV-SAT, MON-RF, receiver solutions) and
// observer_samples (board timing and environment) within the window, so it works while
// raw retention still holds them. The second reads one station event's captured
// evidence, which never expires. Inputs are applied to a fresh live state in collector
// receipt order with the clocks the live checks used, and the detectors run every
// -interval, as in the collector.
//
// Output is one JSON object per line on stdout: "assessment" records when a station's
// fused state or any check state changes, and "event" records for confirmed station
// events. The summary goes to stderr. With -compare, the replayed events are matched
// against the stored ones (same type and new value within two intervals, after
// -warmup) and a mismatch exits with status 2.
//
// The replay starts cold. Checks warm up and the AGC baseline is learned again, so the
// first minutes may differ from the live collector, which carried state from earlier;
// -warmup excludes them from the comparison. A station's baseline check needs its
// partner's solutions, which a single-station replay does not read, so it stays
// unavailable. Rows stored before the receipt clocks
// were recorded are replayed on their persistence time without a wall-clock stamp, and
// the summary counts them.
//
// The installation profiles come from -config ([[integrity.station]] and
// [[reception.station]]), or from -mode, -position and -max-speed, which override
// the configured one. Config parsing is strict, but replay loads only station
// installations and does not read the daemon's TLS keys or authority files.
// Thresholds are this build's (integrity.DefaultProfile), so a replay of old inputs
// under a newer build shows what the newer checks conclude; the timeline's
// config_hash names the profile used.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/integrity"
	"github.com/ptudor/navlistener/internal/state"
	"github.com/ptudor/navlistener/internal/store"
)

type options struct {
	configPath, dsn, collector string
	station, since, until      string
	audience                   string
	id                         int64
	mode, position             string
	maxSpeed                   float64
	interval, warmup           time.Duration
	compare                    bool
}

func main() {
	var o options
	flag.StringVar(&o.configPath, "config", "", "collector configuration: historian DSN, collector id and station installations")
	flag.StringVar(&o.dsn, "from-store", "", "historian DSN (default: [store].dsn from -config)")
	flag.StringVar(&o.collector, "collector", "", "read only this collector's rows (default: [collector].instance_id from -config)")
	flag.StringVar(&o.station, "station", "", "station (observer) id to replay")
	flag.StringVar(&o.since, "since", "", "window start, RFC3339")
	flag.StringVar(&o.until, "until", "", "window end, RFC3339")
	flag.StringVar(&o.audience, "audience", "", "replay a captured event's evidence: the event's audience key")
	flag.Int64Var(&o.id, "id", 0, "replay a captured event's evidence: the event's id in -audience")
	flag.StringVar(&o.mode, "mode", "", "installation override: fixed or mobile")
	flag.StringVar(&o.position, "position", "", "installation override: surveyed latitude,longitude,height for a fixed station")
	flag.Float64Var(&o.maxSpeed, "max-speed", 0, "installation override: a mobile station's maximum speed, m/s")
	flag.DurationVar(&o.interval, "interval", 15*time.Second, "detector cadence")
	flag.DurationVar(&o.warmup, "warmup", 10*time.Minute, "-compare ignores events this soon after the window starts")
	flag.BoolVar(&o.compare, "compare", false, "compare replayed station events with the stored ones; exit 2 on a mismatch")
	flag.Parse()
	code, err := run(context.Background(), o, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "stationreplay:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func run(ctx context.Context, o options, stdout, stderr io.Writer) (int, error) {
	stations := map[string]integrity.StationProfile{}
	if o.configPath != "" {
		cfg, err := config.LoadForReplay(o.configPath)
		if err != nil {
			return 0, err
		}
		stations = cfg.IntegrityStations()
		if o.dsn == "" {
			o.dsn = cfg.Store.DSN
		}
		if o.collector == "" {
			o.collector = cfg.Collector.InstanceID
		}
	}
	if o.dsn == "" {
		return 0, errors.New("no historian: pass -from-store or a -config with [store].dsn")
	}
	if o.interval <= 0 {
		return 0, errors.New("-interval must be positive")
	}
	evidence := o.audience != "" || o.id != 0
	if evidence == (o.station != "" || o.since != "" || o.until != "") {
		return 0, errors.New("replay either -station with -since and -until, or -audience with -id")
	}

	reader, err := store.OpenReader(ctx, o.dsn)
	if err != nil {
		return 0, err
	}
	defer reader.Close()

	var since, until time.Time
	var inputs []store.StationInput
	collect := func(in store.StationInput) error { inputs = append(inputs, in); return nil }
	if evidence {
		if o.audience == "" || o.id < 1 {
			return 0, errors.New("-audience and a positive -id are both required")
		}
		w, err := reader.QueryEvidenceInputs(ctx, o.audience, o.id, collect)
		if err != nil {
			return 0, err
		}
		o.station, since, until = w.Station, w.Start, w.End
	} else {
		if o.station == "" {
			return 0, errors.New("-station is required")
		}
		if since, err = time.Parse(time.RFC3339, o.since); err != nil {
			return 0, fmt.Errorf("-since: %w", err)
		}
		if until, err = time.Parse(time.RFC3339, o.until); err != nil {
			return 0, fmt.Errorf("-until: %w", err)
		}
		if err := reader.QueryStationInputs(ctx, store.StationInputQuery{CollectorID: o.collector, Station: o.station, Since: since, Until: until}, collect); err != nil {
			return 0, err
		}
	}

	profile, err := installation(o, stations[o.station])
	if err != nil {
		return 0, err
	}
	cfg, err := state.NewIntegrityConfig(integrity.DefaultProfile(), map[string]integrity.StationProfile{o.station: profile})
	if err != nil {
		return 0, err
	}
	enc := json.NewEncoder(stdout)
	var writeErr error
	r := newReplayer(cfg, o.interval, func(rec record) {
		if writeErr == nil {
			writeErr = enc.Encode(rec)
		}
	})
	approximate, skipped := 0, 0
	for _, in := range inputs {
		if in.Approximate {
			approximate++
		}
		f, err := ingest.StoredFrame(ingest.StoredInput{Origin: in.Origin, Kind: in.Kind, Raw: in.Raw, Source: o.station,
			Local: in.Local, WallClock: in.WallClock, Session: in.Session, Seq: in.Seq, HasSeq: in.HasSeq})
		if err != nil {
			skipped++
			fmt.Fprintf(stderr, "skipped input received %s: %v\n", in.ReceivedAt.Format(time.RFC3339Nano), err)
			continue
		}
		r.apply(f)
	}
	r.finish(until)
	if writeErr != nil {
		return 0, fmt.Errorf("write timeline: %w", writeErr)
	}

	fmt.Fprintf(stderr, "source:        historian %s\n", redactDSN(o.dsn))
	fmt.Fprintf(stderr, "station:       %s (%s)\n", o.station, profile)
	fmt.Fprintf(stderr, "window:        %s .. %s\n", since.Format(time.RFC3339), until.Format(time.RFC3339))
	fmt.Fprintf(stderr, "inputs:        %d replayed, %d skipped, %d on approximate receipt clocks\n", r.inputs, skipped, approximate)
	fmt.Fprintf(stderr, "events:        %d replayed\n", len(r.events))
	hash, err := cfg.StationConfigHash(o.station)
	if err != nil {
		return 0, err
	}
	fmt.Fprintf(stderr, "configuration: %s (engine %d)\n", hash, integrity.EngineVersion)
	if !o.compare {
		return 0, nil
	}
	audience := o.audience
	if audience == "" && o.collector != "" {
		audience = "operator:" + o.collector
	}
	stored, err := reader.QueryStationEvents(ctx, o.station, audience, store.EvidenceEventTypes, since, until)
	if err != nil {
		return 0, err
	}
	c := compareEvents(r.events, dedupeStored(stored), o.station, since.Add(o.warmup), 2*o.interval)
	fmt.Fprintf(stderr, "comparison:    %d matched, %d stored but not replayed, %d replayed but not stored\n",
		c.Matched, len(c.MissingReplay), len(c.MissingInStore))
	for _, s := range c.MissingReplay {
		fmt.Fprintf(stderr, "  stored only:   %s %s %s -> %s\n", s.Time.Format(time.RFC3339), s.Type, s.OldValue, s.NewValue)
	}
	for _, e := range c.MissingInStore {
		fmt.Fprintf(stderr, "  replayed only: %s %s %s -> %s\n", e.Time.Format(time.RFC3339), e.Type, e.OldValue, e.NewValue)
	}
	if len(c.MissingReplay) > 0 || len(c.MissingInStore) > 0 {
		return 2, nil
	}
	return 0, nil
}

// installation applies the -mode, -position and -max-speed overrides to the
// configured profile.
func installation(o options, configured integrity.StationProfile) (integrity.StationProfile, error) {
	if o.mode == "" {
		if o.position != "" || o.maxSpeed != 0 {
			return configured, errors.New("-position and -max-speed need -mode")
		}
		return configured, nil
	}
	p := integrity.StationProfile{Mode: integrity.Mode(o.mode), MaxSpeedMPS: o.maxSpeed}
	if o.position != "" {
		parts := strings.Split(o.position, ",")
		if len(parts) != 3 {
			return p, errors.New("-position needs latitude,longitude,height")
		}
		var v [3]float64
		for i, s := range parts {
			f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
			if err != nil {
				return p, fmt.Errorf("-position: %w", err)
			}
			v[i] = f
		}
		p.Position = &integrity.Surveyed{LatDeg: v[0], LonDeg: v[1], HeightM: v[2]}
	}
	return p, p.Validate()
}

// redactDSN hides a DSN's password for the summary.
func redactDSN(dsn string) string {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return "(DSN not shown)"
	}
	return fmt.Sprintf("%s/%s (user %q)", net.JoinHostPort(cfg.Host, strconv.Itoa(int(cfg.Port))), cfg.Database, cfg.User)
}
