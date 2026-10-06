package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/identity"
)

func testEvidencePolicy() EvidencePolicy {
	return EvidencePolicy{PreRoll: 10 * time.Minute, PostRoll: time.Minute, Horizon: 6 * 24 * time.Hour, Batch: 50, MaxSamples: 100}
}

func TestEvidencePolicyValidate(t *testing.T) {
	if err := testEvidencePolicy().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*EvidencePolicy){
		"no pre-roll":       func(p *EvidencePolicy) { p.PreRoll = 0 },
		"negative post":     func(p *EvidencePolicy) { p.PostRoll = -time.Second },
		"horizon too short": func(p *EvidencePolicy) { p.Horizon = p.PostRoll },
		"no batch":          func(p *EvidencePolicy) { p.Batch = 0 },
		"no sample bound":   func(p *EvidencePolicy) { p.MaxSamples = 0 },
	} {
		p := testEvidencePolicy()
		mutate(&p)
		if p.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestEvidenceSampleColumnsMatchSchema(t *testing.T) {
	if len(evidenceSampleColumns) != 4+len(rfColumns) || evidenceSampleColumns[4] != "ts" {
		t.Fatalf("evidence sample columns = %v", evidenceSampleColumns)
	}
}

func TestEvidenceQueryValidate(t *testing.T) {
	op := identity.Audience{Kind: identity.AudienceOperator, ID: "c1"}
	for name, q := range map[string]EvidenceQuery{
		"public":       {Audience: identity.Audience{Kind: identity.AudiencePublic}, Seq: 1, Limit: 10},
		"zero id":      {Audience: op, Seq: 0, Limit: 10},
		"limit":        {Audience: op, Seq: 1, Limit: EvidenceMaxLimit + 1},
		"offset":       {Audience: op, Seq: 1, Limit: 1, Offset: -1},
		"bad audience": {Audience: identity.Audience{Kind: "galaxy", ID: "x"}, Seq: 1, Limit: 1},
	} {
		if q.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := (EvidenceQuery{Audience: op, Seq: 1, Limit: 1}).Validate(); err != nil {
		t.Fatal(err)
	}
}

// TestIntegrationEventEvidenceCapture stores RF, solution and board samples for two
// organizations' views of one station, confirms events in an operator and an
// organization audience, and checks that capture copies each audience's own window,
// runs once, honors the sample bound and serves the bundle back.
func TestIntegrationEventEvidenceCapture(t *testing.T) {
	ctx := context.Background()
	s, err := New(ctx, config.Store{DSN: testDSN(t)}, integrationLog())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Microsecond)
	collector := fmt.Sprintf("evidence-c%d", now.UnixNano())
	station := fmt.Sprintf("evidence-station-%d", now.UnixNano())
	eventTime := now.Add(-2 * time.Minute)

	var frames []*NavFrame
	seq := uint64(0)
	add := func(at time.Time, org string, rf *RFSample, board *BoardSample) {
		seq++
		frames = append(frames, &NavFrame{Ts: at, ReceivedAt: at, SourceID: station, OrganizationID: org,
			CollectorInstanceID: collector, Session: "boot", SourceSeq: seq, HasSourceSeq: true,
			Raw: []byte{1, byte(seq)}, RF: rf, Board: board})
	}
	for i := 0; i < 5; i++ {
		at := eventTime.Add(time.Duration(-i) * time.Minute)
		add(at, "org-a", &RFSample{Kind: "solution", Data: []byte(`{"pvt":{"tow_ms":1}}`)}, nil)
		add(at, "org-b", &RFSample{Kind: "jamming", Data: []byte(`{"Bands":[]}`)}, nil)
		stamp := at
		add(at, "org-a", nil, &BoardSample{Kind: "timing", SampleTime: &stamp, Data: []byte(`{"uptime_ms":1}`)})
	}
	// Outside the window on both sides.
	add(eventTime.Add(-11*time.Minute), "org-a", &RFSample{Kind: "solution", Data: []byte(`{}`)}, nil)
	add(eventTime.Add(2*time.Minute), "org-a", &RFSample{Kind: "solution", Data: []byte(`{}`)}, nil)
	if n, err := s.persistAtomicOnce(ctx, frames); err != nil || n != int64(len(frames)) {
		t.Fatalf("persist samples: %d %v", n, err)
	}

	operator := identity.Audience{Kind: identity.AudienceOperator, ID: collector}
	orgA := identity.Audience{Kind: identity.AudienceOrganization, ID: "org-a"}
	write := func(a identity.Audience, typ string) int64 {
		id, err := s.WriteEvent(ctx, EventRow{Audience: a.Key(), Time: eventTime, SV: station, Type: typ,
			OldValue: "ok", NewValue: "suspected", Severity: 2, Raw: []byte(`{"station":"` + station + `"}`),
			DedupeKey: fmt.Sprintf("%s-%s-%d", typ, a.Key(), now.UnixNano()), CollectorInstanceID: collector})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	opSeq := write(operator, "spoofing_suspected")
	orgSeq := write(orgA, "station_assurance")
	write(operator, "orbit_disco") // not a station evidence type

	p := testEvidencePolicy()
	n, err := s.CaptureEventEvidence(ctx, collector, now, p)
	if err != nil || n != 2 {
		t.Fatalf("capture: %d %v", n, err)
	}
	if n, err := s.CaptureEventEvidence(ctx, collector, now, p); err != nil || n != 0 {
		t.Fatalf("second capture: %d %v", n, err)
	}
	if n, err := s.CaptureEventEvidence(ctx, "evidence-other-collector", now, p); err != nil || n != 0 {
		t.Fatalf("another collector captured this collector's events: %d %v", n, err)
	}

	op, err := s.QueryEventEvidence(ctx, EvidenceQuery{Audience: operator, Seq: opSeq, Limit: EvidenceMaxLimit})
	if err != nil {
		t.Fatal(err)
	}
	// The operator sees both organizations' samples in the window: 5 solution + 5
	// jamming RF rows and 5 board rows.
	if op.Event.Type != "spoofing_suspected" || op.RFSamples != 10 || op.BoardSamples != 5 || op.Truncated || len(op.Samples) != 15 {
		t.Fatalf("operator bundle: type %s rf %d board %d truncated %v samples %d", op.Event.Type, op.RFSamples, op.BoardSamples, op.Truncated, len(op.Samples))
	}
	if op.Samples[0].Origin != "rf" || op.Samples[len(op.Samples)-1].Origin != "board" || op.Samples[0].Sequence == nil || len(op.Samples[0].Raw) != 2 {
		t.Fatalf("sample order or content: first %+v last %+v", op.Samples[0], op.Samples[len(op.Samples)-1])
	}
	if !op.WindowStart.Equal(eventTime.Add(-p.PreRoll)) || !op.WindowEnd.Equal(eventTime.Add(p.PostRoll)) {
		t.Fatalf("window %v..%v", op.WindowStart, op.WindowEnd)
	}
	org, err := s.QueryEventEvidence(ctx, EvidenceQuery{Audience: orgA, Seq: orgSeq, Limit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if org.RFSamples != 5 || org.BoardSamples != 5 || len(org.Samples) != 4 || !org.HasMore {
		t.Fatalf("organization bundle: rf %d board %d page %d more %v", org.RFSamples, org.BoardSamples, len(org.Samples), org.HasMore)
	}
	for _, sample := range org.Samples {
		if sample.Kind == "jamming" {
			t.Fatal("organization bundle holds another organization's sample")
		}
	}
	// A bundle is keyed by its event's audience: the organization's page is its own
	// event, and a sequence the audience never issued has no evidence.
	if org.Event.Type != "station_assurance" || org.Event.SV != station || org.Event.ID != orgSeq {
		t.Fatalf("organization bundle is for %+v", org.Event)
	}
	if _, err := s.QueryEventEvidence(ctx, EvidenceQuery{Audience: operator, Seq: opSeq + 1000, Limit: 10}); !errors.Is(err, ErrNoEvidence) {
		t.Fatalf("unknown event: %v", err)
	}

	// A bound smaller than the window marks the bundle truncated.
	tight := p
	tight.MaxSamples = 3
	trSeq := write(operator, "jamming_detected")
	if n, err := s.CaptureEventEvidence(ctx, collector, now, tight); err != nil || n != 1 {
		t.Fatalf("tight capture: %d %v", n, err)
	}
	tr, err := s.QueryEventEvidence(ctx, EvidenceQuery{Audience: operator, Seq: trSeq, Limit: EvidenceMaxLimit})
	if err != nil || !tr.Truncated || tr.RFSamples != 3 || tr.BoardSamples != 3 {
		t.Fatalf("truncated bundle: %+v %v", tr, err)
	}
}
