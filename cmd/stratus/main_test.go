package main

import (
	"io"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/auth"
)

func TestHashPassword(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		stdin string
	}{
		{name: "piped with no newline", stdin: "hunter2"},
		{name: "typed, with a newline", stdin: "hunter2\n"},
		{name: "windows line ending", stdin: "hunter2\r\n"},
		{name: "only the first line is the password", stdin: "hunter2\nnot this\n"},
		// A password may legitimately end in a space; trimming it would hash
		// something the user never typed.
		{name: "a trailing space is part of it", stdin: "hunter2 \n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out strings.Builder
			if err := hashPassword(strings.NewReader(tt.stdin), &out, io.Discard); err != nil {
				t.Fatalf("hashPassword: %v", err)
			}

			hash := strings.TrimSpace(out.String())
			if err := auth.ValidateHash(hash); err != nil {
				t.Fatalf("output is not a bcrypt hash: %v", err)
			}
			if strings.Contains(hash, "hunter2") {
				t.Error("the hash contains the password")
			}
		})
	}
}

func TestHashPasswordRejectsEmptyInput(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	if err := hashPassword(strings.NewReader("\n"), &out, io.Discard); err == nil {
		t.Error("hashPassword on an empty line = nil, want an error")
	}
	if out.String() != "" {
		t.Errorf("wrote %q on failure, want nothing", out.String())
	}
}
