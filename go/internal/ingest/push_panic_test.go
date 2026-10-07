package ingest

import (
	"bytes"
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
	"github.com/ptudor/navlistener/internal/reception"
	"github.com/ptudor/navlistener/internal/wire"
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

type panickingCheck struct {
	ReceptionCoordinator
	remaining int
}

func (p *panickingCheck) Check(identity.ObserverContext, string, reception.Sample, time.Time) *reception.Check {
	if p.remaining > 0 {
		p.remaining--
		panic("check failure")
	}
	return nil
}

func TestPushReceptionPanicPreservesReceiptUntilDurable(t *testing.T) {
	for _, panicCount := range []int{1, 2} {
		tr := NewDurableTracker()
		checks := &panickingCheck{remaining: panicCount}
		body := receptionDetails(17, (reception.Sample{ExpectationID: 7, Unix: 1800000000, UptimeMS: 1000, Valid: 1}).Encode())
		for attempt := 0; attempt < 2; attempt++ {
			server, client := net.Pipe()
			out := make(chan *RawFrame, 1)
			ctx, cancel := context.WithCancel(context.Background())
			var logs bytes.Buffer
			p := &PushServer{out: out, durable: tr, reception: checks, ackInterval: time.Millisecond,
				log: slog.New(slog.NewTextHandler(&logs, nil))}
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer server.Close()
				p.stream(ctx, server, &connWriter{c: server}, identity.NewPrivateContext("poison", identity.CredentialToken), "ubx", "boot")
			}()
			if err := wire.WriteFrame(client, wire.Data, wire.EncodeData(1, wire.RawRecord{FrameType: TelemObserverDetails, Raw: body})); err != nil {
				t.Fatal(err)
			}
			var f *RawFrame
			select {
			case f = <-out:
			case <-time.After(time.Second):
				t.Fatal("receipt not handed to historian")
			}
			poisoned := attempt < panicCount
			if (f.QuarantineReason != "") != poisoned || !bytes.Equal(f.RawBytes(), body) || f.Session != "boot" || f.Seq != 1 || f.MsgType != TelemObserverDetails {
				t.Fatalf("attempt %d receipt = %+v", attempt, f)
			}
			if got := tr.Watermark("poison", "boot"); got != 0 {
				t.Fatalf("pre-commit watermark = %d, want 0", got)
			}
			_ = client.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
			if _, _, err := wire.ReadFrame(client); err == nil {
				t.Fatal("ACK sent before durable notification")
			}
			// This models the historian callback; the persistence integration test
			// independently checks that its callback follows the row and ledger commit.
			tr.Resolved("poison", "boot", 1)
			_ = client.SetReadDeadline(time.Now().Add(time.Second))
			ft, payload, err := wire.ReadFrame(client)
			seq, decodeErr := wire.DecodeAck(payload)
			if err != nil || decodeErr != nil || ft != wire.Ack || seq != 1 {
				t.Fatalf("durable ACK: type=%d seq=%d err=%v/%v", ft, seq, err, decodeErr)
			}
			if poisoned {
				if _, _, err := wire.ReadFrame(client); err != io.EOF {
					t.Fatalf("quarantine connection not closed: %v", err)
				}
			}
			cancel()
			client.Close()
			<-done
			if poisoned && (!bytes.Contains(logs.Bytes(), []byte("session=boot")) || !bytes.Contains(logs.Bytes(), []byte("sequence=1"))) {
				t.Fatalf("quarantine identity missing from log: %s", logs.String())
			}
		}
	}
}

func TestWaitForDurableTimeoutAndCancellationLeaveReceiptReplayable(t *testing.T) {
	tr := NewDurableTracker()
	if !tr.Received("poison", "boot", 1, true) {
		t.Fatal("receipt rejected")
	}
	if waitForDurable(context.Background(), tr, "poison", "boot", 1, time.Millisecond) {
		t.Fatal("uncommitted receipt resolved on timeout")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if waitForDurable(ctx, tr, "poison", "boot", 1, time.Hour) {
		t.Fatal("canceled wait resolved receipt")
	}
	if got := tr.Watermark("poison", "boot"); got != 0 {
		t.Fatalf("watermark=%d after timeout/cancellation", got)
	}
	if !tr.Received("poison", "boot", 1, true) {
		t.Fatal("replay rejected")
	}
	tr.Resolved("poison", "boot", 1)
	if !waitForDurable(context.Background(), tr, "poison", "boot", 1, time.Millisecond) {
		t.Fatal("committed replay stayed blocked")
	}
}

type panickingUpdateReport struct{ UpdateCoordinator }

func (panickingUpdateReport) Pending(identity.ObserverContext) []byte { return nil }
func (panickingUpdateReport) Report(identity.ObserverContext, string, uint64, wire.UpdateStatus) error {
	panic("update report failure")
}

func TestPushUpdatePanicWaitsForAlreadyHandedOffReceipt(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr := NewDurableTracker()
	out := make(chan *RawFrame, 2)
	p := &PushServer{out: out, durable: tr, updates: panickingUpdateReport{}, ackInterval: time.Millisecond,
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer server.Close()
		p.stream(ctx, server, &connWriter{c: server}, identity.NewPrivateContext("poison-update", identity.CredentialToken), "ubx", "boot")
	}()
	body := tlv(observerGolden(t), 9, updateStatus(nil))
	if err := wire.WriteFrame(client, wire.Data, wire.EncodeData(1, wire.RawRecord{FrameType: TelemObserverDetails, Raw: body})); err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-out:
		if f.QuarantineReason != "" || f.Details.Update == nil || !bytes.Equal(f.RawBytes(), body) {
			t.Fatalf("original receipt changed: %+v", f)
		}
	case <-time.After(time.Second):
		t.Fatal("original receipt not forwarded")
	}
	_ = client.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	if _, _, err := wire.ReadFrame(client); err == nil {
		t.Fatal("ACK before durable notification")
	}
	if tr.Watermark("poison-update", "boot") != 0 {
		t.Fatal("receipt resolved before persistence")
	}
	tr.Resolved("poison-update", "boot", 1)
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	ft, payload, err := wire.ReadFrame(client)
	seq, decodeErr := wire.DecodeAck(payload)
	if err != nil || decodeErr != nil || ft != wire.Ack || seq != 1 {
		t.Fatalf("durable ACK: type=%d seq=%d errors=%v/%v", ft, seq, err, decodeErr)
	}
	if _, _, err := wire.ReadFrame(client); err != io.EOF {
		t.Fatalf("connection did not close: %v", err)
	}
	<-done
	select {
	case f := <-out:
		t.Fatalf("receipt handed off twice: %+v", f)
	default:
	}
}
