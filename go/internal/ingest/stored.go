package ingest

import (
	"fmt"
	"time"
)

// StoredInput is one stored station input, as rf_samples and observer_samples keep it:
// the exact telemetry body with the clocks it was received under.
type StoredInput struct {
	// Origin is "rf" (rf_samples) or "board" (observer_samples); Kind is the row kind.
	Origin, Kind string
	Raw          []byte
	Source       string
	// Local is the collector-local receipt instant the live checks used.
	Local time.Time
	// WallClock is the independent wall-clock stamp of the reception, nil when the
	// frame had none.
	WallClock *time.Time
	Session   string
	Seq       uint64
	HasSeq    bool
}

// StoredFrame rebuilds the live frame a stored station input came from, with the same
// decoders the push path uses, so a replay applies exactly what live state applied.
// The frame's Recv is the wall-clock stamp when one exists and the local receipt
// otherwise; RecvLocal is the local receipt.
func StoredFrame(in StoredInput) (*RawFrame, error) {
	f := &RawFrame{Source: in.Source, Recv: in.Local, RecvLocal: in.Local, Session: in.Session, Seq: in.Seq, HasSeq: in.HasSeq,
		Bytes: append([]byte(nil), in.Raw...)}
	if in.WallClock != nil {
		f.Recv, f.RecvStamped, f.BoardSampleStamped = *in.WallClock, true, true
	}
	var err error
	switch {
	case in.Origin == "rf" && in.Kind == "reception":
		f.MsgType = TelemReceptionData
		f.RF = &RawRF{}
		f.RF.Sats, err = decodeReceptionData(in.Raw)
	case in.Origin == "rf" && in.Kind == "jamming":
		f.MsgType = TelemJammingStats
		f.RF = &RawRF{}
		f.RF.Bands, err = decodeJammingStats(in.Raw)
	case in.Origin == "rf" && in.Kind == "solution":
		f.MsgType = TelemReceiverSolution
		f.Solution, err = decodeReceiverSolution(in.Raw)
	case in.Origin == "board":
		f.MsgType = TelemObserverDetails
		f.Details, err = decodeObserverDetails(in.Raw)
	default:
		return nil, fmt.Errorf("stored input %s/%s cannot be replayed", in.Origin, in.Kind)
	}
	if err != nil {
		return nil, fmt.Errorf("stored input %s/%s: %w", in.Origin, in.Kind, err)
	}
	return f, nil
}
