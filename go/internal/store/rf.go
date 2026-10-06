package store

import "time"

// RFSample is private receiver RF evidence. Raw is the versioned telemetry body
// stored on NavFrame; Data is its decoded projection. Keeping this separate
// from broadcast navigation frames makes replay and retention unambiguous.
// LocalReceivedAt and WallClockStamp are the clocks the live station checks used
// (state/integrity.go): the collector-local receipt instant, and the independent
// wall-clock stamp, nil when the frame had none.
type RFSample struct {
	Kind            string
	Data            []byte
	LocalReceivedAt time.Time
	WallClockStamp  *time.Time
}

// sampleColumns is the layout rf_samples shares with observer_samples.
var sampleColumns = append(append([]string(nil), copyColumns[:provenanceColumns]...),
	"sample_time", "kind", "raw", "data", "decoder_ver", "source_session", "source_seq")

// rfColumns adds the two receipt clocks only receiver RF rows carry.
var rfColumns = append(append([]string(nil), sampleColumns...), "local_received_at", "wall_clock_stamp")

func rfFrameToRow(f *NavFrame) []any {
	row := navFrameToRow(f)
	var local any
	if !f.RF.LocalReceivedAt.IsZero() {
		local = f.RF.LocalReceivedAt
	}
	var stamp any
	if f.RF.WallClockStamp != nil {
		stamp = *f.RF.WallClockStamp
	}
	return append(row[:provenanceColumns:provenanceColumns], f.ReceivedAt, f.RF.Kind, f.Raw,
		string(f.RF.Data), nilIfEmpty(f.DecoderVer), row[len(row)-2], row[len(row)-1], local, stamp)
}
