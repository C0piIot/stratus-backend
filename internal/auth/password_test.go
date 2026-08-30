package auth_test

import (
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/C0piIot/stratus-backend/internal/auth"
)

func TestHashRoundTrip(t *testing.T) {
	t.Parallel()
	const password = "correct horse battery staple"

	hash, err := auth.Hash(password)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if err := auth.ValidateHash(hash); err != nil {
		t.Errorf("ValidateHash on a fresh hash: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		t.Errorf("the hash does not verify against its own password: %v", err)
	}
	if strings.Contains(hash, password) {
		t.Error("the hash contains the password")
	}
}

func TestHashIsSalted(t *testing.T) {
	t.Parallel()
	// Two hashes of the same password must differ, or the salt is not doing its
	// job and identical passwords would be visible as identical hashes.
	first, err := auth.Hash("same password")
	if err != nil {
		t.Fatal(err)
	}
	second, err := auth.Hash("same password")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Error("two hashes of the same password are identical, so it is unsalted")
	}
}

func TestHashRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		password string
	}{
		{"empty", ""},
		// bcrypt truncates past 72 bytes, so accepting this would make two
		// different passphrases the same password.
		{"too long for bcrypt", strings.Repeat("x", auth.MaxPasswordLen+1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := auth.Hash(tt.password); err == nil {
				t.Error("Hash = nil, want an error")
			}
		})
	}
}

func TestValidateHashRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		hash string
	}{
		{"empty", ""},
		{"plaintext", "hunter2"},
		{"truncated bcrypt", "$2a$10$N9qo8uLOickgx2ZMRZoMye"},
		{"wrong algorithm", "$argon2id$v=19$m=65536,t=3,p=4$c29tZXNhbHQ$hash"},
		{"an md5 crypt hash", "$1$salt$qJH7.N4xYta3aEG/dfqo/0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := auth.ValidateHash(tt.hash); err == nil {
				t.Errorf("ValidateHash(%q) = nil, want an error", tt.hash)
			}
		})
	}
}
