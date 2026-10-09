package media

import (
	"errors"
	"testing"
	"time"
)

// TestThumbnailsAreRefusedRatherThanQueuedForever is the bound a photo grid on
// a small machine runs into. Without it the fortieth tile stands in line for
// however long the thirty-nine in front of it take, holding a connection while
// the only CPU is busy, and the whole UI behind it is what looks stuck.
func TestThumbnailsAreRefusedRatherThanQueuedForever(t *testing.T) {
	t.Parallel()
	th, _, _ := thumbs(t)
	// One decoder with room for one waiting behind it, so the third ask is the
	// one over the line.
	th.decoding = make(chan struct{}, 1)
	th.queue = 2

	decoding, release := make(chan struct{}), make(chan struct{})
	go func() {
		_, _, _ = th.cached(t.Context(), "held", func() ([]byte, error) {
			close(decoding)
			<-release
			return []byte("a thumbnail"), nil
		})
	}()
	<-decoding

	queued := make(chan error, 1)
	go func() {
		_, _, err := th.cached(t.Context(), "waiting", func() ([]byte, error) {
			return []byte("another"), nil
		})
		queued <- err
	}()
	waitFor(t, th, 2)

	if _, _, err := th.cached(t.Context(), "refused", func() ([]byte, error) {
		t.Error("a refused request decoded anyway")
		return nil, nil
	}); !errors.Is(err, ErrBusy) {
		t.Errorf("with the queue full, err = %v, want ErrBusy", err)
	}

	// And the one that was waiting is served rather than collateral.
	close(release)
	if err := <-queued; err != nil {
		t.Errorf("the request that had a place in the queue: %v", err)
	}
}

// TestAQueuedThumbnailGivesUp is the other half of the bound: a queue short
// enough to stand in is still a wait nobody should take, when what is in front
// is a 4K frame on a shared CPU.
func TestAQueuedThumbnailGivesUp(t *testing.T) {
	t.Parallel()
	th, _, _ := thumbs(t)
	th.decoding = make(chan struct{}, 1)
	th.queue = 8
	th.maxWait = 20 * time.Millisecond

	decoding, release := make(chan struct{}), make(chan struct{})
	go func() {
		_, _, _ = th.cached(t.Context(), "held", func() ([]byte, error) {
			close(decoding)
			<-release
			return []byte("a thumbnail"), nil
		})
	}()
	<-decoding
	defer close(release)

	if _, _, err := th.cached(t.Context(), "gives up", func() ([]byte, error) {
		t.Error("a request that gave up decoded anyway")
		return nil, nil
	}); !errors.Is(err, ErrBusy) {
		t.Errorf("after the deadline, err = %v, want ErrBusy", err)
	}
}

// waitFor blocks until n requests are in the queue, so that a test does not
// race the goroutine it just started.
func waitFor(t *testing.T, th *Thumbs, n int64) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if th.inQueue.Load() == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("only %d requests reached the queue, want %d", th.inQueue.Load(), n)
}
