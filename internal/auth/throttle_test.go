package auth_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/auth"
)

// fast is the same shape as DefaultThrottle with the clock turned down, so the
// tests measure behaviour rather than patience.
var fast = auth.ThrottleConfig{Every: 20 * time.Millisecond, Burst: 2, MaxWait: 50 * time.Millisecond}

func throttled(t *testing.T) *auth.Throttle {
	t.Helper()
	return auth.NewThrottle(credentials(t), fast)
}

// TestThrottleLetsTheRightPasswordThrough is the property that makes a global
// limit safe: a correct login is never queued behind somebody else's guesses,
// so an attack cannot stop the owner from using their own server.
func TestThrottleLetsTheRightPasswordThrough(t *testing.T) {
	t.Parallel()
	throttle := throttled(t)

	// Burn the burst and then some, so the queue is full of failures.
	for range 10 {
		_ = throttle.Verify(t.Context(), username, "not it")
	}

	start := time.Now()
	if err := throttle.Verify(t.Context(), username, examplePassword); err != nil {
		t.Fatalf("Verify with the right password = %v, want nil", err)
	}
	if waited := time.Since(start); waited > fast.Every {
		t.Errorf("the right password waited %v, want no delay at all", waited)
	}
}

func TestThrottleSpacesOutFailures(t *testing.T) {
	t.Parallel()
	throttle := throttled(t)

	// The burst is free.
	start := time.Now()
	for range fast.Burst {
		if err := throttle.Verify(t.Context(), username, "not it"); !errors.Is(err, auth.ErrUnauthorized) {
			t.Fatalf("Verify = %v, want ErrUnauthorized", err)
		}
	}
	if waited := time.Since(start); waited > fast.Every {
		t.Errorf("the burst took %v, want it answered immediately", waited)
	}

	// The next one is not.
	start = time.Now()
	if err := throttle.Verify(t.Context(), username, "not it"); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("Verify = %v, want ErrUnauthorized", err)
	}
	if waited := time.Since(start); waited < fast.Every/2 {
		t.Errorf("the failure after the burst waited %v, want about %v", waited, fast.Every)
	}
}

// TestThrottleRefusesAQueueTooLong is what keeps a flood from turning into a
// wait measured in minutes: past MaxWait the answer is immediate and is not a
// judgement on the credentials.
//
// Concurrent on purpose. A caller guessing one at a time is already spaced out
// by the wait it just served, so only parallel guesses can build a queue -- and
// parallel guesses are what an attack looks like.
func TestThrottleRefusesAQueueTooLong(t *testing.T) {
	t.Parallel()
	throttle := throttled(t)

	var wg sync.WaitGroup
	refused := make([]bool, 20)
	for i := range refused {
		wg.Add(1)
		go func() {
			defer wg.Done()
			refused[i] = errors.Is(throttle.Verify(t.Context(), username, "not it"), auth.ErrTooManyAttempts)
		}()
	}
	wg.Wait()

	if !slices.Contains(refused, true) {
		t.Fatal("twenty guesses at once and none was refused, so nothing is bounding the queue")
	}
	if !slices.Contains(refused, false) {
		t.Error("every guess was refused, so the queue is not being served at all")
	}
}

// TestThrottleHonoursCancellation keeps a queued failure from holding a
// connection open after the client has gone.
func TestThrottleHonoursCancellation(t *testing.T) {
	t.Parallel()
	throttle := auth.NewThrottle(credentials(t), auth.ThrottleConfig{
		Every: time.Hour, Burst: 0, MaxWait: 2 * time.Hour,
	})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	start := time.Now()
	if err := throttle.Verify(ctx, username, "not it"); !errors.Is(err, context.Canceled) {
		t.Errorf("Verify = %v, want context.Canceled", err)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("it waited %v after the context was cancelled", waited)
	}
}
