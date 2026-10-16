package stream

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// readFixture reads one of internal/media's fixtures from where that package
// keeps them rather than from a copy: a film is a film to both halves.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// stubFFmpeg is what the tests that never reach the binary are wired with: a
// script that fails. The toolchain container has no media tools, so a test that
// did reach it fails saying so rather than quietly finding a real one.
func stubFFmpeg(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffmpeg")
	script := "#!/bin/sh\necho 'this is the stub, not ffmpeg' >&2\nexit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// realFFmpeg is the binary itself, where there is one. There is not one in the
// toolchain container, which is why these skip: what runs it for real is
// scripts/smoke.sh, inside the image that carries it.
func realFFmpeg(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg on the PATH")
	}
	return path
}
