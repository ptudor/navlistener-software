package integrity

import (
	"math"
	"slices"
	"testing"
)

func res(check string, d Domain, s State) Result { return Result{Check: check, Domain: d, State: s} }

func lowerOnly(check string, d Domain, s State) Result {
	r := res(check, d, s)
	r.lowerOnly = true
	return r
}

func TestFuse(t *testing.T) {
	cases := []struct {
		name     string
		results  []Result
		weights  map[string]float64
		state    State
		score    float64 // NaN means no score
		domains  []Domain
		spoofing bool
	}{
		{name: "nothing", state: Unavailable, score: math.NaN()},
		{name: "all assured",
			results: []Result{res("a", DomainPosition, Assured), res("b", DomainReceiverClock, Assured)},
			state:   Assured, score: 1},
		{name: "lower-only assured cannot assure",
			results: []Result{lowerOnly("cn0", DomainSignalPower, Assured), lowerOnly("agc", DomainRFEnvironment, Assured)},
			state:   Unavailable, score: math.NaN()},
		{name: "lower-only inconsistent takes part",
			results: []Result{res("a", DomainPosition, Assured), lowerOnly("agc", DomainRFEnvironment, Inconsistent)},
			state:   Inconsistent, score: 0.5},
		{name: "half inconsistent is inconsistent",
			results: []Result{res("a", DomainPosition, Assured), res("b", DomainReceiverClock, Assured),
				res("c", DomainTimeReference, Inconsistent), res("d", DomainPosition, Inconsistent)},
			state: Inconsistent, score: 0.5},
		{name: "mostly assured",
			results: []Result{res("a", DomainPosition, Assured), res("b", DomainReceiverClock, Assured),
				res("c", DomainTimeReference, Assured), res("d", DomainPosition, Inconsistent)},
			state: Assured, score: 0.75},
		{name: "one physics domain caps at inconsistent",
			results: []Result{res("a", DomainPosition, Unassured), res("b", DomainReceiverClock, Assured),
				res("c", DomainTimeReference, Assured), res("d", DomainPosition, Assured)},
			state: Inconsistent, score: 0.5, domains: []Domain{DomainPosition}},
		{name: "two checks of one domain are one domain",
			results: []Result{res("static", DomainPosition, Unassured), res("velocity", DomainPosition, Unassured)},
			state:   Inconsistent, score: -1, domains: []Domain{DomainPosition}},
		{name: "two physics domains",
			results: []Result{res("a", DomainPosition, Unassured), res("b", DomainReceiverClock, Unassured),
				res("c", DomainTimeReference, Assured)},
			state: Unassured, score: -1.0 / 3, domains: []Domain{DomainPosition, DomainReceiverClock}, spoofing: true},
		{name: "physics with receiver verdict",
			results: []Result{res("a", DomainTimeReference, Unassured), lowerOnly("rx", DomainReceiverVerdict, Unassured),
				res("b", DomainPosition, Assured)},
			state: Unassured, score: -1.0 / 3, domains: []Domain{DomainReceiverVerdict, DomainTimeReference}, spoofing: true},
		{name: "receiver verdict alone",
			results: []Result{lowerOnly("rx", DomainReceiverVerdict, Unassured), res("b", DomainPosition, Assured)},
			state:   Inconsistent, score: 0, domains: []Domain{DomainReceiverVerdict}},
		{name: "physics with RF environment is unassured but not spoofing",
			results: []Result{res("a", DomainPosition, Unassured), lowerOnly("agc", DomainRFEnvironment, Unassured)},
			state:   Unassured, score: -1, domains: []Domain{DomainPosition, DomainRFEnvironment}},
		{name: "RF environment alone",
			results: []Result{lowerOnly("agc", DomainRFEnvironment, Unassured), res("a", DomainPosition, Assured)},
			state:   Inconsistent, score: 0, domains: []Domain{DomainRFEnvironment}},
		{name: "unassured with nothing assuring is inconsistent, not unavailable",
			results: []Result{lowerOnly("cn0", DomainSignalPower, Unassured)},
			state:   Inconsistent, score: -1, domains: []Domain{DomainSignalPower}},
		{name: "unavailable checks do not take part",
			results: []Result{res("a", DomainPosition, Assured), res("b", DomainReceiverClock, Unavailable)},
			state:   Assured, score: 1},
		{name: "zero weight excludes a check",
			results: []Result{res("a", DomainPosition, Assured), res("b", DomainReceiverClock, Inconsistent)},
			weights: map[string]float64{"b": 0}, state: Assured, score: 1},
		{name: "weights scale the mean",
			results: []Result{res("a", DomainPosition, Assured), res("b", DomainReceiverClock, Inconsistent)},
			weights: map[string]float64{"b": 3}, state: Inconsistent, score: 0.25},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := Fuse(tc.results, tc.weights)
			if f.State != tc.state {
				t.Errorf("state = %s, want %s", f.State, tc.state)
			}
			switch {
			case math.IsNaN(tc.score) && f.Score != nil:
				t.Errorf("score = %g, want none", *f.Score)
			case !math.IsNaN(tc.score) && (f.Score == nil || math.Abs(*f.Score-tc.score) > 1e-12):
				t.Errorf("score = %v, want %g", f.Score, tc.score)
			}
			if !slices.Equal(f.UnassuredDomains, tc.domains) {
				t.Errorf("unassured domains = %v, want %v", f.UnassuredDomains, tc.domains)
			}
			if f.SpoofingIndicated != tc.spoofing {
				t.Errorf("spoofing indicated = %v, want %v", f.SpoofingIndicated, tc.spoofing)
			}
		})
	}
}

func TestDomainPhysics(t *testing.T) {
	for _, d := range []Domain{DomainSignalPower, DomainPosition, DomainReceiverClock, DomainTimeReference} {
		if !d.Physics() {
			t.Errorf("%s should count toward the spoofing quorum", d)
		}
	}
	for _, d := range []Domain{DomainRFEnvironment, DomainReceiverVerdict} {
		if d.Physics() {
			t.Errorf("%s must not count toward the spoofing quorum", d)
		}
	}
}
