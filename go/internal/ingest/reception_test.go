package ingest

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/reception"
	"github.com/ptudor/navlistener/internal/stationcontrol"
	"github.com/ptudor/navlistener/internal/wire"
)

func receptionDetails(tag byte, value []byte) []byte {
	b := make([]byte, 27+len(value))
	b[0] = 1
	b[1] = 8
	b[9] = 1
	b[24] = tag
	binary.BigEndian.PutUint16(b[25:], uint16(len(value)))
	copy(b[27:], value)
	return b
}

func TestReceptionAssessmentTags(t *testing.T) {
	s := reception.Sample{ExpectationID: 7, Unix: 1_800_000_000, UptimeMS: 1000, Valid: 1, Alarm: 1, Boot: 1, Event: 2}
	for _, tag := range []byte{17, 18} {
		b := receptionDetails(tag, s.Encode())
		got, err := decodeObserverDetails(b)
		if err != nil {
			t.Fatal(err)
		}
		actual := got.Reception
		if tag == 18 {
			actual = got.ReceptionEvent
		}
		if actual == nil || *actual != s {
			t.Fatalf("tag %d: %+v", tag, got)
		}
		b[30] = 1 // reserved byte in the assessment
		if _, err := decodeObserverDetails(b); err == nil {
			t.Fatal("malformed assessment accepted")
		}
	}
	b := receptionDetails(19, []byte{1, 2, 3, 0, 0, 0, 0, 0, 0, 0, 0, 42})
	if got, err := decodeObserverDetails(b); err != nil || got.Snapshot.ID != 42 || got.Snapshot.Status != 2 {
		t.Fatalf("snapshot: %+v %v", got, err)
	}
	b[38] = 0
	if _, err := decodeObserverDetails(b); err == nil {
		t.Fatal("zero request ID accepted")
	}
}

func TestReceptionForecastRoundTripTLS(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := stationcontrol.New(reception.Config{Stations: []reception.Site{{Observer: "edge"}}}, func(c identity.ObserverContext, _ reception.Site, now time.Time) reception.Expectation {
		if c.ObserverID != "edge" {
			panic("wrong authenticated station")
		}
		e := reception.Expectation{ID: 7, Issued: now.Unix(), RadiusM: 1000, AlarmSeconds: 5, ClearSeconds: 5, MinExpected: 4, MinMissing: 3, MissingPercent: 50}
		for sv := uint8(1); sv <= 12; sv++ {
			e.Entries = append(e.Entries, reception.Entry{GNSS: 0, SV: sv, Signal: 255, Slots: 31})
		}
		return e
	})
	addr, out := startPushServer(t, ctx, tokenAuth("edge", "test-token", "ubx"), func(p *PushServer) { p.SetReception(m) })
	conn := dialPush(t, addr)
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if err := wire.WriteHello(conn, wire.HelloMsg{Token: "test-token", Station: "edge", Feed: "ubx", Session: "edge-boot", Reception: 1}); err != nil {
		t.Fatal(err)
	}
	if kind, _, err := wire.ReadFrame(conn); err != nil || kind != wire.Welcome {
		t.Fatalf("welcome: %v %v", kind, err)
	}
	var e reception.Expectation
	for {
		kind, b, err := wire.ReadFrame(conn)
		if err != nil {
			t.Fatal(err)
		}
		if kind == wire.ReceptionExpectation {
			e, err = reception.Decode(b)
			if err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	s := reception.Sample{ExpectationID: e.ID, Unix: e.Issued, UptimeMS: 1000, Valid: 1}
	s.Matched[0] = 3
	record := wire.RawRecord{FrameType: TelemObserverDetails, Raw: receptionDetails(17, s.Encode())}
	if err := wire.WriteFrame(conn, wire.Data, wire.EncodeData(1, record)); err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-out:
		if f.Source != "edge" || f.Details.Reception == nil || f.ReceptionCheck == nil || f.ReceptionCheck.Expected[0] != 12 || f.ReceptionCheck.Observed[0] != 2 {
			t.Fatalf("assessment lost: %+v", f)
		}
	case <-time.After(time.Second):
		t.Fatal("assessment did not reach collector")
	}
}
