package cli

// eofReader and nopWriteCloser are copied from the template server's
// internal/cli/stdio.go: the proxy is a nested Go module and cannot import
// the template's internal packages.

import (
	"errors"
	"io"
	"sync/atomic"
)

// eofReader wraps an [io.Reader] and records whether it has reached [io.EOF], so
// the caller can recognize the client closing the input stream as a clean
// shutdown even when the SDK surfaces it as a non-EOF "server is closing" error.
//
// The flag is atomic so the Read (on the SDK's read goroutine) and the sawEOF
// check (after the server's Run returns) are race-free; Run returns only after
// that goroutine has finished, which establishes the happens-before that makes
// the store visible to sawEOF.
type eofReader struct {
	reader io.Reader
	eof    atomic.Bool
}

func (r *eofReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if errors.Is(err, io.EOF) {
		r.eof.Store(true)
	}
	return n, err
}

func (r *eofReader) sawEOF() bool { return r.eof.Load() }

// nopWriteCloser adapts an [io.Writer] to an [io.WriteCloser] with a no-op
// Close. It is the write-side counterpart to [io.NopCloser].
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
