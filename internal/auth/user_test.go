package auth_test

import (
	"context"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/auth"
)

func TestUser(t *testing.T) {
	t.Parallel()

	ctx := auth.WithUser(t.Context(), "edu")
	got, ok := auth.User(ctx)
	if !ok || got != "edu" {
		t.Errorf("User = %q, %v", got, ok)
	}
}

// TestUserWithout is the case that decides whether a handler serves somebody's
// files on a guess: a context nobody authenticated has to say so.
func TestUserWithout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ctx  context.Context
	}{
		{name: "nothing was ever set", ctx: context.Background()},
		// An empty username is what a badly wired middleware produces, and it
		// must not read as an authenticated request for the empty owner.
		{name: "an empty username", ctx: auth.WithUser(context.Background(), "")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got, ok := auth.User(tt.ctx); ok {
				t.Errorf("User = %q, %v; want no user", got, ok)
			}
		})
	}
}
