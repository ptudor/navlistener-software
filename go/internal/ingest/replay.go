package ingest

import (
	"errors"
	"io"
	"time"
)

// ReplayUBX parses a captured UBX byte stream into RawFrames offline, without a
// receiver, a socket, or a Manager. It is the file-replay entry point behind the
// design rule that raw frames are stored so a decoder fix can be re-run over
// history (docs/DESIGN.md): the same
// scanner the live dial path uses, exposed for tools that read a capture instead
// of a stream.
//
// now supplies the Recv stamp for every emitted frame and is called once per
// frame, in stream order — a replay tool controls the synthetic receive clock by
// advancing it there. onErr receives the same parse-error kind strings the live
// path counts ("checksum", "resync", …); pass a no-op to ignore them.
//
// Returns the scanner's terminal error. Reaching the end of the capture is not
// an error here: scanUBX surfaces the reader's failure as its exit condition
// (the dial path's reconnect trigger), so a file replay ends in io.EOF, or in
// io.ErrUnexpectedEOF when the capture was cut mid-message — the normal shape of
// a stream recording stopped by a signal. Both are reported as nil; anything
// else is a real read failure and is returned.
func ReplayUBX(r io.Reader, source string, now func() time.Time, emit func(*RawFrame), onErr func(kind string)) error {
	err := scanUBX(r, source, now, emit, onErr)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return nil
	}
	return err
}
