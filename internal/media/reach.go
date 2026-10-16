package media

import (
	"errors"
	"io"
	"time"
)

// Telling a failure to understand some bytes from a failure to reach them.
//
// The indexer used to write both down the same way: a row with the reason on
// it, which the queue counts as done. That is right for a file nothing can
// parse and wrong for everything else -- thirty Seconds of a bucket not
// answering marked a whole batch as unreadable for good, and only a version
// bump would have looked at it again (#157).
//
// So a failure on the way to the bytes is not a verdict. The row is still
// written, because a file with no row sits at the head of a queue ordered by id
// and nothing behind it would ever be read, but it carries a time to come back
// at instead of a full stop.

// ErrUnreachable marks a failure that says nothing about the file. It is a
// wrapper rather than a value: what a person reading /status needs is the
// reason underneath it.
var ErrUnreachable = errors.New("could not be read")

// retryAfter is how long a deferred file waits. Flat, and an hour: an outage
// lasting a day costs a file twenty-four attempts, which is nothing, and a
// counter with an escalating delay would be arithmetic nobody has needed yet.
const retryAfter = time.Hour

// Unreachable reports whether err was about getting to the bytes.
func Unreachable(err error) bool { return errors.Is(err, ErrUnreachable) }

// StoreReader remembers the first failure the store gave, because by the time
// an error reaches the indexer it may no longer say where it came from: the
// readers in mkv.go and mp4.go turn any failed read into their own "not mine",
// and the extractors we did not write do the same in their own words.
type StoreReader struct {
	io.ReadSeekCloser
	Err error
}

func (s *StoreReader) Read(p []byte) (int, error) {
	n, err := s.ReadSeekCloser.Read(p)
	// The end of the file is not a failure to reach it.
	if err != nil && !errors.Is(err, io.EOF) {
		s.keep(err)
	}
	return n, err
}

// Seek is Read's twin: a seek the store refuses is a failure to reach the
// bytes, not a verdict on them.
func (s *StoreReader) Seek(offset int64, whence int) (int64, error) {
	at, err := s.ReadSeekCloser.Seek(offset, whence)
	if err != nil {
		s.keep(err)
	}
	return at, err
}

// keep holds the first one. A later failure is usually the first one repeating
// itself through another layer.
func (s *StoreReader) keep(err error) {
	if s.Err == nil {
		s.Err = err
	}
}
