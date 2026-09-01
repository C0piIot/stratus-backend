package auth

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrTooManyAttempts means the queue of failures waiting to be answered is
// already longer than anyone should wait, so this one is refused outright.
var ErrTooManyAttempts = errors.New("auth: too many attempts")

// Verifier is what a protocol adapter authenticates against. Credentials is one;
// Throttle wraps another.
type Verifier interface {
	Verify(ctx context.Context, username, password string) error
}

// Throttle caps how fast credentials can be guessed, for everyone at once
// rather than per client.
//
// Global on purpose. Per-IP limiting is close to useless here: behind a reverse
// proxy every request shares one address, and X-Forwarded-For is set by whoever
// is calling. A single-user server has one password, so what has to be limited
// is guesses at it, wherever they come from.
//
// It exists because dropping bcrypt (#39) removed the limiter nobody had
// noticed was there: 65 ms a verification put a ceiling of about fifteen
// guesses a second per core. A SHA-256 comparison has no such ceiling.
//
// Only failures are queued. A correct password is answered immediately, even
// while an attack is in progress, so a client syncing hundreds of files never
// waits behind somebody else's guesses.
type Throttle struct {
	verifier Verifier
	cfg      ThrottleConfig

	mu sync.Mutex
	// next is the earliest moment the next failure may be answered.
	next time.Time
}

// ThrottleConfig is the shape of the limit. The zero value is not useful; see
// DefaultThrottle.
type ThrottleConfig struct {
	// Every is the spacing between two answered failures.
	Every time.Duration
	// Burst is how many failures are answered without waiting, so that a
	// mistyped password is not punished.
	Burst int
	// MaxWait is how long a failure may be held before it is refused with
	// ErrTooManyAttempts instead. It is what stops a flood from queueing
	// somebody into a wait measured in minutes.
	MaxWait time.Duration
}

// DefaultThrottle is one guess a second after three free ones, with anything
// that would wait more than two seconds refused instead.
//
// A second between guesses turns an exhaustive search into something that
// outlives the hardware; the numbers are otherwise unremarkable and are meant to
// be invisible to anyone typing their own password.
var DefaultThrottle = ThrottleConfig{
	Every:   time.Second,
	Burst:   3,
	MaxWait: 2 * time.Second,
}

// NewThrottle wraps v so that failed verifications are rate limited.
func NewThrottle(v Verifier, cfg ThrottleConfig) *Throttle {
	return &Throttle{verifier: v, cfg: cfg}
}

// Verify implements Verifier.
func (t *Throttle) Verify(ctx context.Context, username, password string) error {
	err := t.verifier.Verify(ctx, username, password)
	if err == nil {
		return nil
	}

	wait, ok := t.reserve(time.Now())
	if !ok {
		return ErrTooManyAttempts
	}
	// Slept outside the lock, so concurrent guesses queue up rather than
	// serialise on a mutex -- the spacing is already decided by then.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
	}
	return err
}

// reserve takes the next slot in the queue and reports how long its holder must
// wait, or false when that would be longer than MaxWait.
func (t *Throttle) reserve(now time.Time) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// An idle period earns back at most Burst slots, which is what keeps a typo
	// on Monday from being paid for on Tuesday. Burst-1 because the slot taken
	// below is itself one of them.
	if earliest := now.Add(-time.Duration(t.cfg.Burst-1) * t.cfg.Every); t.next.Before(earliest) {
		t.next = earliest
	}

	wait := t.next.Sub(now)
	if wait > t.cfg.MaxWait {
		return 0, false
	}
	t.next = t.next.Add(t.cfg.Every)
	if wait < 0 {
		wait = 0
	}
	return wait, true
}
