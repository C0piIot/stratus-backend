package lookup

import (
	"context"
	"sync"
	"time"
)

// defaultPace is how far apart two requests to one service are kept. One a
// second is MusicBrainz's published limit per address, and the other two are
// given the same courtesy rather than a number of their own.
const defaultPace = time.Second

// pacer keeps requests to each service a fixed interval apart.
//
// Not a token bucket: there is no burst to allow, since a pass is a loop that
// would spend any allowance in the first second and then wait anyway. The
// next allowed time is claimed under the lock and slept towards outside it,
// so two goroutines asking at once are spaced rather than both let through.
type pacer struct {
	interval time.Duration
	mu       sync.Mutex
	next     map[string]time.Time
	// sleep is replaceable so a test can see what would have been waited
	// without waiting it.
	sleep func(ctx context.Context, d time.Duration) error
	now   func() time.Time
}

func newPacer(interval time.Duration) *pacer {
	return &pacer{interval: interval, next: make(map[string]time.Time), sleep: sleepFor, now: time.Now}
}

// wait blocks until a request to service may be made, or ctx ends.
func (p *pacer) wait(ctx context.Context, service string) error {
	if p.interval <= 0 {
		return nil
	}
	p.mu.Lock()
	now := p.now()
	at := p.next[service]
	if at.Before(now) {
		at = now
	}
	p.next[service] = at.Add(p.interval)
	p.mu.Unlock()
	return p.sleep(ctx, at.Sub(now))
}

func sleepFor(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
