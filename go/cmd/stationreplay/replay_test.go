package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/detect"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/integrity"
	"github.com/ptudor/navlistener/internal/state"
	"github.com/ptudor/navlistener/internal/store"
)

var replayT0 = time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)

const replayStation = "replay-station"

func fixedInstallation() integrity.StationProfile {
	return integrity.StationProfile{Mode: integrity.ModeFixed, Position: &integrity.Surveyed{LatDeg: 37.4219, LonDeg: -122.0841, HeightM: 12.5}}
}

// takeoverFrames is a fixed station's push stream: healthy for healthy seconds, then
// a takeover that moves the solution 2 km north and steps the receiver clock by 3 µs.
func takeoverFrames(healthy, takeover int) []*ingest.RawFrame {
	var frames []*ingest.RawFrame
	for i := 0; i < healthy+takeover; i++ {
		local := replayT0.Add(time.Duration(i) * time.Second)
		stamp := local.Add(-200 * time.Millisecond)
		utc := local.Add(-450 * time.Millisecond)
		tow := uint32(345_600_000 + i*1000)
		lat, bias := int32(374219000), int32(1000+25*i)
		if i >= healthy {
			lat += 180000 // about 2 km
			bias += 3000
		}
		sol := &ingest.ReceiverSolution{
			PVT: &ingest.SolutionPVT{TOWMS: tow, Year: uint16(utc.Year()), Month: uint8(utc.Month()), Day: uint8(utc.Day()),
				Hour: uint8(utc.Hour()), Minute: uint8(utc.Minute()), Second: uint8(utc.Second()), NanoNS: int32(utc.Nanosecond()),
				UTCValid: 7, TAccNS: 25, FixType: 3, FixFlags: ingest.FixFlagOK, NumSV: 14,
				LatE7: lat, LonE7: -1220841000, HeightMM: 12500, HAccMM: 1500, VAccMM: 2500, SAccMMS: 90},
			Clock:  &ingest.SolutionClock{TOWMS: tow, BiasNS: bias, DriftNSS: 25, TAccNS: 25},
			Status: &ingest.SolutionStatus{TOWMS: tow, FixType: 3, SpoofState: 1, SinceStart: uint32(9_000_000 + i*1000)},
		}
		frames = append(frames, &ingest.RawFrame{Source: replayStation, Recv: stamp, RecvLocal: local, RecvStamped: true,
			MsgType: ingest.TelemReceiverSolution, Solution: sol, Bytes: ingest.EncodeReceiverSolution(sol)})
	}
	return frames
}

func replayAll(t *testing.T, frames []*ingest.RawFrame, until time.Time) ([]record, *replayer) {
	t.Helper()
	cfg, err := state.NewIntegrityConfig(integrity.DefaultProfile(), map[string]integrity.StationProfile{replayStation: fixedInstallation()})
	if err != nil {
		t.Fatal(err)
	}
	var out []record
	r := newReplayer(cfg, 15*time.Second, func(rec record) { out = append(out, rec) })
	for _, f := range frames {
		r.apply(f)
	}
	r.finish(until)
	return out, r
}

func TestReplayTakeoverRaisesSpoofing(t *testing.T) {
	frames := takeoverFrames(300, 120)
	out, r := replayAll(t, frames, replayT0.Add(420*time.Second))
	var spoofing, assured bool
	for _, rec := range out {
		if rec.Kind == "event" && rec.Type == "spoofing_suspected" && rec.NewValue == "suspected" {
			spoofing = true
		}
		if rec.Kind == "assessment" && rec.State == string(integrity.Assured) {
			assured = true
		}
	}
	if !assured || !spoofing {
		t.Fatalf("assured before takeover %v, spoofing after %v; timeline %+v", assured, spoofing, out)
	}
	if r.inputs != len(frames) {
		t.Fatalf("applied %d of %d inputs", r.inputs, len(frames))
	}
}

func TestReplayDeterministic(t *testing.T) {
	encode := func() []byte {
		out, _ := replayAll(t, takeoverFrames(200, 60), replayT0.Add(260*time.Second))
		b, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if a, b := encode(), encode(); !bytes.Equal(a, b) {
		t.Fatal("two replays of the same inputs differ")
	}
}

func TestCompareEvents(t *testing.T) {
	at := replayT0.Add(20 * time.Minute)
	replayed := []detect.Event{
		{Time: at, SV: "s", Type: "spoofing_suspected", NewValue: "suspected"},
		{Time: at.Add(10 * time.Second), SV: "s", Type: "station_assurance", NewValue: "unassured"},
		{Time: replayT0.Add(time.Minute), SV: "s", Type: "jamming_detected", NewValue: "crit"}, // inside warm-up
		{Time: at, SV: "other", Type: "spoofing_suspected", NewValue: "suspected"},
	}
	stored := []store.StoredStationEvent{
		{Time: at.Add(20 * time.Second), Type: "spoofing_suspected", NewValue: "suspected"},
		{Time: at.Add(5 * time.Minute), Type: "antenna_fault", NewValue: "fault"},
	}
	c := compareEvents(replayed, stored, "s", replayT0.Add(10*time.Minute), 30*time.Second)
	if c.Matched != 1 || len(c.MissingReplay) != 1 || c.MissingReplay[0].Type != "antenna_fault" ||
		len(c.MissingInStore) != 1 || c.MissingInStore[0].Type != "station_assurance" {
		t.Fatalf("comparison = %+v", c)
	}
	dup := []store.StoredStationEvent{{Time: at, Type: "x", NewValue: "y", Audience: "operator:a"}, {Time: at, Type: "x", NewValue: "y", Audience: "organization:b"}}
	if got := dedupeStored(dup); len(got) != 1 {
		t.Fatalf("dedupe kept %d", len(got))
	}
}

func TestInstallationOverride(t *testing.T) {
	configured := fixedInstallation()
	if p, err := installation(options{}, configured); err != nil || p.Position == nil {
		t.Fatalf("configured profile not kept: %+v %v", p, err)
	}
	p, err := installation(options{mode: "fixed", position: "1.5, 2.5, 30"}, configured)
	if err != nil || p.Position.LatDeg != 1.5 || p.Position.HeightM != 30 {
		t.Fatalf("override: %+v %v", p, err)
	}
	for _, o := range []options{{position: "1,2,3"}, {mode: "fixed", position: "1,2"}, {mode: "mobile", position: "1,2,3"}, {mode: "fixed", position: "x,2,3"}, {mode: "fixed", maxSpeed: 3}} {
		if _, err := installation(o, configured); err == nil {
			t.Errorf("%+v accepted", o)
		}
	}
}

func TestRedactDSN(t *testing.T) {
	for _, dsn := range []string{
		"postgres://app:s3cret@db.invalid:5432/nav",
		"postgres://app@db.invalid/nav?password=s3cret",
		"host=db.invalid dbname=nav user=app password=a@b",
		"postgres://app:s3cret@db.invalid/nav?sslmode=require",
	} {
		got := redactDSN(dsn)
		if got != `db.invalid:5432/nav (user "app")` {
			t.Errorf("redactDSN returned %q; want only host, port, database and user", got)
		}
	}
	if got := redactDSN("postgres://app:s3cret@db.invalid:bad/nav"); got != "(DSN not shown)" {
		t.Errorf("malformed DSN returned %q", got)
	}
}

// TestIntegrationReplayMatchesLive stores a station's inputs with their receipt clocks
// and the events a live evaluation of them confirmed, then replays them from the
// historian and requires the same events.
func TestIntegrationReplayMatchesLive(t *testing.T) {
	dsn := os.Getenv("NAVLISTENER_TEST_DSN")
	if dsn == "" {
		t.Skip("NAVLISTENER_TEST_DSN not set; skipping live-TimescaleDB integration test")
	}
	ctx := context.Background()
	historian, err := store.New(ctx, config.Store{DSN: dsn}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer historian.Close()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	frames := takeoverFrames(300, 180)
	until := replayT0.Add(480 * time.Second)
	_, live := replayAll(t, frames, until)
	if len(live.events) == 0 {
		t.Fatal("the live evaluation confirmed no event to compare")
	}
	collector := fmt.Sprintf("replay-c%d", time.Now().UnixNano())
	station := replayStation
	if _, err := pool.Exec(ctx, `DELETE FROM rf_samples WHERE source_id = $1 AND sample_time >= $2 AND sample_time <= $3`, station, replayT0, until); err != nil {
		t.Fatal(err)
	}
	for i, f := range frames {
		stamp, _ := f.WallClockStamp()
		if _, err := pool.Exec(ctx, `INSERT INTO rf_samples (ts, received_at, source_id, collector_instance_id, sample_time,
				kind, raw, data, source_session, source_seq, local_received_at, wall_clock_stamp)
			VALUES ($1, $2, $3, $4, $2, 'solution', $5, '{}', 'boot', $6, $7, $8)`,
			f.LocalRecv().Add(50*time.Millisecond), f.Recv, station, collector, f.Bytes, i, f.LocalRecv(), stamp); err != nil {
			t.Fatal(err)
		}
	}
	audience := "operator:" + collector
	for i, e := range live.events {
		if _, err := historian.WriteEvent(ctx, store.EventRow{Audience: audience, Time: e.Time, SV: e.SV, Type: e.Type,
			OldValue: e.OldValue, NewValue: e.NewValue, Severity: e.Severity, Message: e.Message,
			DedupeKey: fmt.Sprintf("%s-%d", collector, i), CollectorInstanceID: collector}); err != nil {
			t.Fatal(err)
		}
	}
	installationMode := "fixed"
	var stdout, stderr bytes.Buffer
	code, err := run(ctx, options{dsn: dsn, collector: collector, station: station,
		since: replayT0.Format(time.RFC3339), until: until.Format(time.RFC3339),
		mode: installationMode, position: "37.4219,-122.0841,12.5",
		interval: 15 * time.Second, warmup: time.Minute, compare: true}, &stdout, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("replay exit %d %v\n%s", code, err, stderr.String())
	}
	// The window selects rows by their observer stamp: the first frame was stamped
	// 200 ms before it.
	if !strings.Contains(stderr.String(), fmt.Sprintf("%d replayed, 0 skipped, 0 on approximate", len(frames)-1)) ||
		!strings.Contains(stderr.String(), "0 stored but not replayed, 0 replayed but not stored") {
		t.Fatalf("summary:\n%s", stderr.String())
	}
	if !strings.Contains(stdout.String(), `"type":"spoofing_suspected"`) {
		t.Fatalf("timeline lacks the spoofing event:\n%s", stdout.String())
	}

	// A replay config may name private daemon files the analyst cannot read.
	// The replay must still load the same installations and historian scope.
	configPath := filepath.Join(t.TempDir(), "replay.toml")
	body := fmt.Sprintf(`[collector]
instance_id = %q
[store]
dsn = %q
[push]
addr = "127.0.0.1:4443"
tls_cert = %q
tls_key = %q
[[integrity.station]]
observer = %q
mode = "fixed"
position = [37.4219, -122.0841, 12.5]
`, collector, dsn, filepath.Join(t.TempDir(), "unavailable-cert.pem"), filepath.Join(t.TempDir(), "unavailable-key.pem"), station)
	if err := os.WriteFile(configPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(configPath); err == nil {
		t.Fatal("daemon accepted unavailable TLS credentials")
	}
	var configuredOut, configuredErr bytes.Buffer
	code, err = run(ctx, options{configPath: configPath, station: station,
		since: replayT0.Format(time.RFC3339), until: until.Format(time.RFC3339),
		interval: 15 * time.Second, warmup: time.Minute, compare: true}, &configuredOut, &configuredErr)
	if err != nil || code != 0 {
		t.Fatalf("configured replay exit %d %v\n%s", code, err, configuredErr.String())
	}
	if configuredOut.String() != stdout.String() {
		t.Fatal("configured replay differs from explicit station installation")
	}

	// The same events replay from their captured evidence, which outlives raw retention.
	policy := store.EvidencePolicy{PreRoll: 10 * time.Minute, PostRoll: time.Minute, Horizon: 30 * 24 * time.Hour, Batch: 50, MaxSamples: 20000}
	if n, err := historian.CaptureEventEvidence(ctx, collector, time.Now(), policy); err != nil || n != len(live.events) {
		t.Fatalf("captured %d bundles, want %d: %v", n, len(live.events), err)
	}
	var seq int64
	if err := pool.QueryRow(ctx, `SELECT audience_seq FROM gnss_events WHERE audience = $1 AND event_type = 'spoofing_suspected'`, audience).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	code, err = run(ctx, options{dsn: dsn, audience: audience, id: seq, mode: installationMode, position: "37.4219,-122.0841,12.5",
		interval: 15 * time.Second, warmup: time.Minute, compare: true}, &stdout, &stderr)
	if err != nil || code != 0 || !strings.Contains(stdout.String(), `"type":"spoofing_suspected"`) {
		t.Fatalf("evidence replay exit %d %v\n%s\n%s", code, err, stderr.String(), stdout.String())
	}
}
