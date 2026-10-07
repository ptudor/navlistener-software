package state

import (
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/identity"
)

func BenchmarkApplyDeclaredSource(b *testing.B) {
	s := New(4)
	f := cnavStateFrame(gnss.GPS, 5, 3, cnavMT10(5, 2288, 0, 2, 400), time.Unix(1700000000, 0))
	f.Observer = identity.NewPrivateContext("test", identity.CredentialToken)
	f.Observer.DeclaredCapabilities = []identity.Signal{{GnssID: 0, SigID: 0}, {GnssID: 0, SigID: 3}, {GnssID: 2, SigID: 0}, {GnssID: 3, SigID: 0}}
	s.Apply(f)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		s.Apply(f)
	}
}
