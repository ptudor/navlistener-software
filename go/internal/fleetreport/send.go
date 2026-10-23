package fleetreport

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ptudor/navlistener/internal/metrics"
)

const (
	startDelay     = 5 * time.Minute  // let boards reconnect after a restart before reporting them
	sampleEvery    = 10 * time.Minute // well inside the hour after which the collector forgets a board
	requestTimeout = 30 * time.Second
	firstRetry     = time.Minute
	maxRetry       = time.Hour
	maxDetailBytes = 200
)

// outcome is how one attempt ended, as docs/FLEET-REPORTS.md tells the
// collector to treat each response.
type outcome string

const (
	accepted  outcome = "accepted"
	duplicate outcome = "duplicate"
	refused   outcome = "refused" // not retried: the next interval sends a new report
	failed    outcome = "failed"  // retried with the same report ID
)

// Reporter samples the boards the collector holds, sends one report per
// interval and retries it until it is answered or the next one replaces it.
//
// The collector forgets a board after an hour of silence, so the reporter
// keeps its own record of every board sampled since it started: a board that
// is offline when a report is built is still listed, with the hour it was
// last seen.
type Reporter struct {
	cfg      Config
	source   Source
	instance string
	client   *http.Client
	log      *slog.Logger
	seen     map[string]Board

	startDelay, sampleEvery, firstRetry, maxRetry time.Duration
	now                                           func() time.Time
}

// New builds a Reporter for an enabled, validated Config.
func New(cfg Config, source Source, instance string, log *slog.Logger) *Reporter {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return newReporter(cfg, source, instance, log, &http.Client{Transport: transport})
}

func newReporter(cfg Config, source Source, instance string, log *slog.Logger, client *http.Client) *Reporter {
	client.Timeout = requestTimeout
	// The token goes to the configured URL only: a redirect is an answer, not
	// an instruction to send the report elsewhere.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Reporter{
		cfg: cfg, source: source, instance: instance, client: client, log: log.With("component", "fleet_report"),
		seen:       make(map[string]Board),
		startDelay: startDelay, sampleEvery: sampleEvery, firstRetry: firstRetry, maxRetry: maxRetry, now: time.Now,
	}
}

// sample merges the boards the collector holds now into the reporter's record,
// keeping each board's latest receipt. At MaxBoards the longest-silent board
// makes room for a new one.
func (r *Reporter) sample() {
	for _, b := range r.source.FleetBoards(r.cfg.ManufacturerAuthorityID) {
		if kept, ok := r.seen[b.ObserverID]; ok && !b.LastReceived.After(kept.LastReceived) {
			continue
		}
		if _, ok := r.seen[b.ObserverID]; !ok && len(r.seen) >= MaxBoards {
			oldest := ""
			for id, kept := range r.seen {
				if oldest == "" || kept.LastReceived.Before(r.seen[oldest].LastReceived) {
					oldest = id
				}
			}
			delete(r.seen, oldest)
		}
		r.seen[b.ObserverID] = b
	}
}

func (r *Reporter) build() (Built, bool) {
	id, err := NewReportID()
	if err == nil {
		boards := make([]Board, 0, len(r.seen))
		for _, b := range r.seen {
			boards = append(boards, b)
		}
		var built Built
		if built, err = Build(id, r.cfg.ManufacturerAuthorityID, r.instance, r.now(), boards); err == nil {
			metrics.FleetReportBoards.Set(float64(built.Boards))
			if built.Omitted > 0 {
				r.log.Warn("fleet report leaves out boards it cannot represent exactly", "omitted", built.Omitted)
			}
			return built, true
		}
	}
	r.log.Error("fleet report not built", "error", err)
	metrics.FleetReportsTotal.WithLabelValues(string(failed)).Inc()
	return Built{}, false
}

// Run samples and reports until ctx ends: the first report after startDelay,
// then one per interval. A report not yet answered is retried with the same
// ID, backing off from firstRetry to maxRetry, until the next replaces it.
func (r *Reporter) Run(ctx context.Context) {
	r.log.Info("fleet reporting enabled", "manufacturer_authority_id", r.cfg.ManufacturerAuthorityID,
		"interval", r.cfg.Interval.String())
	r.sample()
	sampling := time.NewTicker(r.sampleEvery)
	defer sampling.Stop()
	next := time.NewTimer(r.startDelay + jitter(r.startDelay))
	defer next.Stop()
	// Stopped until a delivery fails. Timers are synchronous (go 1.23+): after
	// Stop or Reset no stale fire is delivered.
	retry := time.NewTimer(time.Hour)
	retry.Stop()
	defer retry.Stop()
	var pending *Built
	var backoff time.Duration
	var due time.Time // when the next report replaces the pending one

	deliver := func() {
		result, detail := r.attempt(ctx, *pending)
		metrics.FleetReportsTotal.WithLabelValues(string(result)).Inc()
		switch result {
		case accepted, duplicate:
			r.log.Info("fleet report delivered", "report_id", pending.ID, "boards", pending.Boards, "result", string(result))
			pending = nil
		case refused:
			r.log.Warn("fleet report refused; the next interval sends a new one", "report_id", pending.ID, "detail", detail)
			pending = nil
		default:
			if ctx.Err() != nil {
				return
			}
			if r.now().Add(backoff).After(due) {
				r.log.Warn("fleet report undelivered; the next interval replaces it", "report_id", pending.ID, "detail", detail)
				pending = nil
				return
			}
			r.log.Warn("fleet report not delivered; retrying", "report_id", pending.ID, "detail", detail, "retry_in", backoff.String())
			retry.Reset(backoff)
			backoff = min(2*backoff, r.maxRetry)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-sampling.C:
			r.sample()
		case <-next.C:
			r.sample()
			due = r.now().Add(r.cfg.Interval)
			next.Reset(r.cfg.Interval)
			retry.Stop()
			if built, ok := r.build(); ok {
				pending, backoff = &built, r.firstRetry
				deliver()
			}
		case <-retry.C:
			if pending != nil {
				deliver()
			}
		}
	}
}

// attempt sends the report once and classifies the answer.
func (r *Reporter) attempt(ctx context.Context, built Built) (outcome, string) {
	token, err := ReadToken(r.cfg.TokenFile)
	if err != nil {
		return refused, "token: " + err.Error()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.cfg.URL, bytes.NewReader(built.Body))
	if err != nil {
		return refused, err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		var urlErr interface{ Timeout() bool }
		if errors.As(err, &urlErr) && urlErr.Timeout() {
			return failed, "timeout"
		}
		return failed, redact(err.Error(), token)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	detail := fmt.Sprintf("HTTP %d %s", resp.StatusCode, sanitize(string(body)))
	switch {
	case resp.StatusCode == http.StatusCreated:
		return accepted, detail
	case resp.StatusCode == http.StatusOK:
		return duplicate, detail
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return failed, detail
	default:
		return refused, detail
	}
}

// sanitize keeps a response body fit for one log field: printable, short.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= maxDetailBytes {
			b.WriteString("...")
			break
		}
		if r >= 0x20 && r < 0x7f {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	return strings.TrimSpace(b.String())
}

func redact(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "[token]")
}
