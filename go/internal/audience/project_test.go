package audience

import (
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/ingest"
)

func contextFor(mode identity.AggregateUse, metadata identity.StationMetadata) identity.ObserverContext {
	c := identity.NewPrivateContext("eui64-observer", identity.CredentialToken)
	c.OrganizationID = "institution-c"
	c.Publication.AggregateUse = mode
	c.Publication.StationMetadata = metadata
	c.Publication.Revision = "policy-7"
	return c
}

func TestProjectPublicFailsClosedAndDoesNotMutateInput(t *testing.T) {
	rf := &ingest.RawFrame{Source: "private", Recv: time.Now(), RF: &ingest.RawRF{}}
	if got, ok := ProjectPublic(rf); ok || got != nil {
		t.Fatalf("private frame projected: %#v", got)
	}
	if rf.RF == nil || rf.Source != "private" {
		t.Fatal("projection mutated private input")
	}

	legacy := &ingest.RawFrame{Source: "legacy", Recv: time.Now()}
	if _, ok := ProjectPublic(legacy); ok {
		t.Fatal("zero/legacy context projected publicly")
	}
}

func TestProjectPublicAnonymousContributesWithoutStationEvidence(t *testing.T) {
	in := &ingest.RawFrame{
		Source: "hardware-id", Recv: time.Now(), GnssID: gnss.GPS, SvID: 5,
		Observer: contextFor(identity.AggregatePublicAnonymous, identity.MetadataNone),
		Obs:      &ingest.RawObs{PrM: 20_000_000}, RF: &ingest.RawRF{},
	}
	out, ok := ProjectPublic(in)
	if !ok {
		t.Fatal("anonymous public frame rejected")
	}
	if out.Source != AnonymousPublicSource || out.Obs != nil || out.RF != nil {
		t.Fatalf("anonymous projection leaks station evidence: %+v", out)
	}
	if in.Source != "hardware-id" || in.Obs == nil || in.RF == nil {
		t.Fatal("projection mutated authenticated input")
	}
}

func TestProjectPublicAttributedKeepsGeometryButNotRF(t *testing.T) {
	in := &ingest.RawFrame{
		Source: "presented-name", Recv: time.Now(),
		Observer: contextFor(identity.AggregatePublicAttributed, identity.MetadataCoarse),
		Obs:      &ingest.RawObs{PrM: 20_000_000}, RF: &ingest.RawRF{},
	}
	out, ok := ProjectPublic(in)
	if !ok {
		t.Fatal("attributed public frame rejected")
	}
	if out.Source != "eui64-observer" || out.Obs == nil || out.RF != nil {
		t.Fatalf("attributed projection wrong: %+v", out)
	}
}

func TestProjectPublicEventsIsIndependentlyGatedAndRedacted(t *testing.T) {
	in := &ingest.RawFrame{
		Source: "hardware-id", Recv: time.Now(),
		Observer: contextFor(identity.AggregatePublicAttributed, identity.MetadataFull),
		Obs:      &ingest.RawObs{PrM: 20_000_000},
	}
	if _, ok := ProjectPublicEvents(in); ok {
		t.Fatal("default private event policy entered public detector state")
	}

	in.Observer.Publication.EventVisibility = identity.EventsPublicRedacted
	redacted, ok := ProjectPublicEvents(in)
	if !ok || redacted.Source != AnonymousPublicSource || redacted.Obs != nil {
		t.Fatalf("redacted event projection wrong: %+v, %v", redacted, ok)
	}

	in.Observer.Publication.EventVisibility = identity.EventsPublic
	public, ok := ProjectPublicEvents(in)
	if !ok || public.Source != "eui64-observer" || public.Obs == nil {
		t.Fatalf("public event projection wrong: %+v, %v", public, ok)
	}
}

func TestPublicSourcesExposeOnlyAttributedPresentation(t *testing.T) {
	private := config.Source{Name: "private", Remark: "secret site"}
	coarse := config.Source{Name: "coarse", Remark: "exact rooftop", ObserverContext: contextFor(identity.AggregatePublicAttributed, identity.MetadataCoarse)}
	full := config.Source{Name: "full", Remark: "approved site", ObserverContext: contextFor(identity.AggregatePublicAttributed, identity.MetadataFull)}
	anon := config.Source{Name: "anon", ObserverContext: contextFor(identity.AggregatePublicAnonymous, identity.MetadataNone)}

	got := PublicSources([]config.Source{private, coarse, full, anon})
	if len(got) != 2 {
		t.Fatalf("got %d public sources, want 2: %+v", len(got), got)
	}
	if got[0].Remark != "" || got[1].Remark != "approved site" {
		t.Fatalf("remark redaction wrong: %+v", got)
	}
}
