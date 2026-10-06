package store

// RFSample is private receiver RF evidence. Raw is the versioned telemetry body
// stored on NavFrame; Data is its decoded projection. Keeping this separate
// from broadcast navigation frames makes replay and retention unambiguous.
type RFSample struct {
	Kind string
	Data []byte
}

var rfColumns = append(append([]string(nil), copyColumns[:provenanceColumns]...),
	"sample_time", "kind", "raw", "data", "decoder_ver", "source_session", "source_seq")

func rfFrameToRow(f *NavFrame) []any {
	row := navFrameToRow(f)
	return append(row[:provenanceColumns:provenanceColumns], f.ReceivedAt, f.RF.Kind, f.Raw,
		string(f.RF.Data), nilIfEmpty(f.DecoderVer), row[len(row)-2], row[len(row)-1])
}
