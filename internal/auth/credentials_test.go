package auth_test

import (
	"errors"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/auth"
)

const (
	username = "edu"
	// "example" in the name and in the value: it is what GitGuardian's generic
	// password detector looks for to tell a fixture from a leaked credential.
	examplePassword = "example correct horse battery staple"
)

func credentials(t *testing.T) auth.Credentials {
	t.Helper()
	hash, err := auth.Hash(examplePassword)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	return auth.Credentials{Username: username, Hash: hash}
}

func TestVerify(t *testing.T) {
	t.Parallel()
	creds := credentials(t)

	if err := creds.Verify(username, examplePassword); err != nil {
		t.Errorf("Verify with the right credentials = %v, want nil", err)
	}

	tests := []struct {
		name     string
		username string
		password string
	}{
		{name: "wrong password", username: username, password: "not it"},
		{name: "wrong username", username: "someone", password: examplePassword},
		{name: "both wrong", username: "someone", password: "not it"},
		{name: "empty password", username: username},
		{name: "empty username", password: examplePassword},
		{name: "nothing at all"},
		// The username is compared in constant time, which must not turn into
		// a prefix match.
		{name: "username is a prefix", username: "ed", password: examplePassword},
		{name: "username has a suffix", username: username + "x", password: examplePassword},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := creds.Verify(tt.username, tt.password); !errors.Is(err, auth.ErrUnauthorized) {
				t.Errorf("Verify = %v, want ErrUnauthorized", err)
			}
		})
	}
}

// TestVerifyWithoutCredentials is the case that would be a disaster to get
// wrong: nothing is configured, so nobody is let in.
func TestVerifyWithoutCredentials(t *testing.T) {
	t.Parallel()
	for _, creds := range []auth.Credentials{
		{},
		{Username: username},
		{Hash: credentials(t).Hash},
	} {
		if err := creds.Verify(username, examplePassword); !errors.Is(err, auth.ErrUnauthorized) {
			t.Errorf("Verify against %+v = %v, want ErrUnauthorized", creds, err)
		}
		if creds.Configured() {
			t.Errorf("%+v reports itself configured", creds)
		}
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		creds   auth.Credentials
		wantErr bool
	}{
		{name: "nothing set is legal for now", creds: auth.Credentials{}},
		{name: "both set", creds: credentials(t)},
		{name: "a hash with no username", creds: auth.Credentials{Hash: credentials(t).Hash}, wantErr: true},
		{name: "a username with no hash", creds: auth.Credentials{Username: username}, wantErr: true},
		{name: "a hash bcrypt cannot read", creds: auth.Credentials{Username: username, Hash: "not a hash"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.creds.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && errors.Is(err, auth.ErrUnauthorized) {
				t.Error("a configuration error must not read as a failed login")
			}
		})
	}
}
