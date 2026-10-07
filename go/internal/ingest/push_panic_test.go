package ingest

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/navlistener/internal/commissioning"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/metrics"
)

type panickingPending struct{ ReceptionCoordinator }

func (panickingPending) Pending(identity.ObserverContext, string, time.Time) ([]byte, []byte, []byte) {
	panic("pending failure")
}

type panickingAuth struct{}

func (panickingAuth) Authenticate(context.Context, string, string, string) (identity.ObserverContext, bool) {
	panic("authorization failure")
}

func TestPushCoordinatorPanicClosesConnection(t *testing.T) {
	for _, component := range []string{"control", "authorization"} {
		t.Run(component, func(t *testing.T) {
			server, client := net.Pipe()
			defer server.Close()
			defer client.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			observer := identity.NewPrivateContext("panic-"+component, identity.CredentialToken)
			p := &PushServer{out: make(chan *RawFrame), ackInterval: time.Millisecond, reauthorizeEvery: time.Millisecond,
				log: slog.New(slog.NewTextHandler(io.Discard, nil)), reception: panickingPending{}, auth: panickingAuth{}}
			counter := metrics.PushErrorsTotal.WithLabelValues(observer.ObserverID, "panic")
			before := testutil.ToFloat64(counter)
			done := make(chan struct{})
			go func() {
				defer close(done)
				if component == "control" {
					ctx = context.WithValue(ctx, receptionContextKey{}, uint8(1))
					p.stream(ctx, server, &connWriter{c: server}, observer, "ubx", "boot")
				} else {
					p.watchAuthorization(ctx, ctx, server, "token", observer.ObserverID, "ubx", observer, commissioning.Result{}, nil)
				}
			}()
			_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, err := client.Read(make([]byte, 1)); err != io.EOF {
				t.Fatalf("read = %v, want EOF", err)
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("coordinator did not exit")
			}
			if got := testutil.ToFloat64(counter) - before; got != 1 {
				t.Fatalf("panic count = %v, want 1", got)
			}
		})
	}
}
