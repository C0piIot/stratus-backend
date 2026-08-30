package storage_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/storage"
)

func TestValidateKeyAccepts(t *testing.T) {
	t.Parallel()
	keys := []string{
		"file",
		"photos/2024/06/IMG_0001.jpg",
		"music/Björk/Homogenic/01 Hunter.flac",
		"fotos/ñandú 🌞.jpg",
		"spaces are fine.txt",
		"a-file.with.dots",
		"not/the/first/.hidden",
		"blobs/ab/cd/" + strings.Repeat("f", 64),
		strings.Repeat("x", storage.MaxSegmentLen),
		strings.Repeat("d/", 300) + "leaf",
	}
	for _, key := range keys {
		t.Run(strconv.Quote(key), func(t *testing.T) {
			t.Parallel()
			if err := storage.ValidateKey(key); err != nil {
				t.Errorf("ValidateKey(%q) = %v, want nil", key, err)
			}
		})
	}
}

func TestValidateKeyRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"leading slash", "/etc/passwd"},
		{"trailing slash", "photos/"},
		{"empty segment", "photos//img.jpg"},
		{"dot segment", "photos/./img.jpg"},
		{"parent segment", "photos/../../etc/passwd"},
		{"bare parent", ".."},
		{"bare dot", "."},
		{"leading dot segment", ".tmp/stolen"},
		{"leading dot file", ".env"},
		{"backslash", `photos\img.jpg`},
		{"nul byte", "photos/img\x00.jpg"},
		{"newline", "photos/img\n.jpg"},
		{"del", "photos/img\x7f.jpg"},
		{"invalid utf-8", "photos/\xff.jpg"},
		{"too long", strings.Repeat("x/", storage.MaxKeyLen/2) + "x"},
		{"segment too long", strings.Repeat("x", storage.MaxSegmentLen+1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := storage.ValidateKey(tt.key)
			if !errors.Is(err, storage.ErrInvalidKey) {
				t.Errorf("ValidateKey(%q) = %v, want ErrInvalidKey", tt.key, err)
			}
			// The message has to name the culprit: this error reaches a log
			// line, and "invalid key" alone is unactionable.
			if err != nil && !strings.Contains(err.Error(), "storage: invalid key") {
				t.Errorf("error %q does not read as an invalid key", err)
			}
		})
	}
}
