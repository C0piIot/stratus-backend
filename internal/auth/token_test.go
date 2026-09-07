package auth_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/auth"
)

// The specification publishes this vector, so it is documentation rather than a
// credential: password "sesame" with salt "c19b2d" digests to the token below.
const (
	vectorPassword = "sesame"
	vectorSalt     = "c19b2d"
	vectorToken    = "26719a1196d2a940705a59634eb18eab"
)

func TestVerifyToken(t *testing.T) {
	t.Parallel()
	creds := auth.Credentials{Username: username, Password: vectorPassword}

	tests := []struct {
		name              string
		user, token, salt string
		want              error
	}{
		{name: "the specification's own vector", user: username, token: vectorToken, salt: vectorSalt},
		{
			// Lower case is what the specification asks for, but a client that
			// shouts is still right: case is not part of the secret.
			name: "an upper case token is the same token",
			user: username, token: strings.ToUpper(vectorToken), salt: vectorSalt,
		},
		{name: "a wrong token", user: username, token: "00000000000000000000000000000000", salt: vectorSalt, want: auth.ErrUnauthorized},
		{name: "the right token for another salt", user: username, token: vectorToken, salt: "different", want: auth.ErrUnauthorized},
		{name: "a wrong username", user: "someone-else", token: vectorToken, salt: vectorSalt, want: auth.ErrUnauthorized},
		{
			// A digest over no salt is a constant, and a constant is a
			// password that can be replayed.
			name: "no salt at all",
			user: username, token: vectorToken, salt: "", want: auth.ErrUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := creds.VerifyToken(t.Context(), tt.user, tt.token, tt.salt)
			if !errors.Is(err, tt.want) {
				t.Errorf("VerifyToken = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestVerifyTokenWithoutCredentials(t *testing.T) {
	t.Parallel()

	var none auth.Credentials
	if err := none.VerifyToken(t.Context(), username, vectorToken, vectorSalt); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("VerifyToken with nothing configured = %v, want ErrUnauthorized", err)
	}
}

func TestThrottleVerifiesTokens(t *testing.T) {
	t.Parallel()
	throttle := auth.NewThrottle(auth.Credentials{Username: username, Password: vectorPassword}, fast)

	if err := throttle.VerifyToken(t.Context(), username, vectorToken, vectorSalt); err != nil {
		t.Errorf("VerifyToken with the right token = %v, want nil", err)
	}
	if err := throttle.VerifyToken(t.Context(), username, "nope", vectorSalt); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("VerifyToken with a wrong token = %v, want ErrUnauthorized", err)
	}
}

// TestThrottleSharesOneBucketBetweenSchemes is the property that makes routing
// tokens through the throttle worth doing: alternating schemes must not double
// the budget.
func TestThrottleSharesOneBucketBetweenSchemes(t *testing.T) {
	t.Parallel()
	throttle := auth.NewThrottle(auth.Credentials{Username: username, Password: vectorPassword},
		auth.ThrottleConfig{Every: time.Hour, Burst: 1, MaxWait: 0})

	// The one free failure, spent on the password scheme.
	if err := throttle.Verify(t.Context(), username, "not it"); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("the first failure = %v, want it answered", err)
	}
	// The token scheme finds the bucket already empty.
	if err := throttle.VerifyToken(t.Context(), username, "nope", vectorSalt); !errors.Is(err, auth.ErrTooManyAttempts) {
		t.Errorf("a token guess after a password guess = %v, want ErrTooManyAttempts", err)
	}
}

// passwordOnly is a verifier that holds no recoverable secret, which is the
// case the protocol has its own error for.
type passwordOnly struct{}

func (passwordOnly) Verify(context.Context, string, string) error { return auth.ErrUnauthorized }

func TestThrottleRefusesTokensAVerifierCannotAnswer(t *testing.T) {
	t.Parallel()
	throttle := auth.NewThrottle(passwordOnly{}, fast)

	err := throttle.VerifyToken(t.Context(), username, vectorToken, vectorSalt)
	if !errors.Is(err, auth.ErrTokenUnsupported) {
		t.Errorf("VerifyToken through a password-only verifier = %v, want ErrTokenUnsupported", err)
	}
}
