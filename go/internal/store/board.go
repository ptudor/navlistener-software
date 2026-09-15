package store

import "time"

// BoardSample is private environmental or timing evidence, sharing the bounded
// writer and atomic replay ledger with nav frames. It has its own table so nav
// replay cannot mistake sensor readings for satellite broadcast words.
type BoardSample struct {
	Kind       string
	SampleTime *time.Time
	Data       []byte
}

// The first 20 columns are the same immutable receipt provenance as nav_frames.
var boardColumns = append(append([]string(nil), copyColumns[:20]...),
	"sample_time", "kind", "raw", "data", "decoder_ver", "source_session", "source_seq")

func boardFrameToRow(f *NavFrame) []any {
	row := navFrameToRow(f)
	return append(row[:20:20], f.Board.SampleTime, f.Board.Kind, f.Raw,
		string(f.Board.Data), nilIfEmpty(f.DecoderVer), row[29], row[30])
}
