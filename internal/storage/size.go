package storage

import (
	"errors"
	"fmt"
	"io"
)

// ExactReader wraps r so that it yields exactly size bytes, failing with
// ErrSizeMismatch if the stream is shorter or longer than that. A negative size
// means "unknown" and returns r untouched.
//
// This lives in the port because it is the only way the two backends can agree.
// A disk backend can count what it wrote and compare afterwards; an S3 client
// sends a Content-Length and stops reading there, so a body longer than the
// declared size is silently truncated into a short object that looks like a
// successful upload. Enforcing the length in the stream itself makes both
// backends fail the same way, at the same point.
func ExactReader(r io.Reader, size int64) io.Reader {
	if size < 0 {
		return r
	}
	return &exactReader{r: r, size: size, left: size}
}

type exactReader struct {
	r    io.Reader
	size int64
	left int64
}

func (e *exactReader) Read(p []byte) (int, error) {
	if e.left == 0 {
		return 0, e.checkEOF()
	}
	if int64(len(p)) > e.left {
		p = p[:e.left]
	}
	n, err := e.r.Read(p)
	e.left -= int64(n)
	if errors.Is(err, io.EOF) && e.left > 0 {
		return n, fmt.Errorf("%w: stream ended %d bytes short of the declared %d", ErrSizeMismatch, e.left, e.size)
	}
	return n, err
}

// checkEOF spends one byte proving the stream really is over. Without it a body
// longer than the declared size would read as a clean EOF.
func (e *exactReader) checkEOF() error {
	var probe [1]byte
	for {
		// A reader is allowed to return (0, nil), so this loops rather than
		// treating it as the end.
		switch n, err := e.r.Read(probe[:]); {
		case n > 0:
			return fmt.Errorf("%w: stream is longer than the declared %d bytes", ErrSizeMismatch, e.size)
		case errors.Is(err, io.EOF):
			return io.EOF
		case err != nil:
			return err
		}
	}
}
