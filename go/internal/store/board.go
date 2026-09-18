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

// The leading provenanceColumns are the same immutable receipt provenance as
// nav_frames; source_session and source_seq are the last two of both layouts.
var boardColumns = append(append([]string(nil), copyColumns[:provenanceColumns]...),
	"sample_time", "kind", "raw", "data", "decoder_ver", "source_session", "source_seq")

func boardFrameToRow(f *NavFrame) []any {
	row := navFrameToRow(f)
	return append(row[:provenanceColumns:provenanceColumns], f.Board.SampleTime, f.Board.Kind, f.Raw,
		string(f.Board.Data), nilIfEmpty(f.DecoderVer), row[len(row)-2], row[len(row)-1])
}
