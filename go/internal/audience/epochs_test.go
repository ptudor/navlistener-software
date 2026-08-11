package audience

import (
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
)

func TestPolicyEpochsStartFailClosedAndAdvanceIndependently(t *testing.T) {
	started := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	epochs := NewPolicyEpochs(started)
	if generation, visible := epochs.Current("public"); generation != 0 || !visible.Equal(started) {
		t.Fatalf("startup epoch = %d/%v", generation, visible)
	}
	at := started.Add(time.Hour)
	epochs.Advance([]identity.Audience{{Kind: identity.AudiencePublic}}, at)
	if generation, visible := epochs.Current("public"); generation != 1 || !visible.Equal(at) {
		t.Fatalf("advanced public epoch = %d/%v", generation, visible)
	}
	if generation, visible := epochs.Current("organization:customer-a"); generation != 0 || !visible.Equal(started) {
		t.Fatalf("unrelated epoch changed = %d/%v", generation, visible)
	}
}
