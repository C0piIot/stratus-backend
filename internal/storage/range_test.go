package storage_test

import (
	"errors"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/storage"
)

func TestRangeResolve(t *testing.T) {
	t.Parallel()
	const size = 100

	tests := []struct {
		name    string
		rng     storage.Range
		wantOff int64
		wantLen int64
	}{
		{"zero value is the whole object", storage.Range{}, 0, size},
		{"all", storage.All(), 0, size},
		{"from zero", storage.From(0), 0, size},
		{"from an offset", storage.From(90), 90, 10},
		{"from the end reads nothing", storage.From(size), size, 0},
		{"a slice", storage.Slice(10, 20), 10, 20},
		{"a slice is truncated at the end", storage.Slice(90, 50), 90, 10},
		{"an empty slice", storage.Slice(10, 0), 10, 0},
		{"a suffix", storage.Suffix(10), 90, 10},
		{"a suffix of the whole object", storage.Suffix(size), 0, size},
		{"a suffix longer than the object", storage.Suffix(size + 1), 0, size},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			off, n, err := tt.rng.Resolve(size)
			if err != nil {
				t.Fatalf("Resolve = %v, want nil", err)
			}
			if off != tt.wantOff || n != tt.wantLen {
				t.Errorf("Resolve = (%d, %d), want (%d, %d)", off, n, tt.wantOff, tt.wantLen)
			}
		})
	}
}

func TestRangeResolveEmptyObject(t *testing.T) {
	t.Parallel()
	// An empty object is legal, so every way of reading it must be too, a
	// suffix included: the last ten bytes of nothing are nothing. Answering 416
	// instead is an RFC 7233 decision, and it belongs to the protocol adapter.
	for _, rng := range []storage.Range{storage.All(), storage.From(0), storage.Slice(0, 10), storage.Suffix(10)} {
		off, n, err := rng.Resolve(0)
		if err != nil || off != 0 || n != 0 {
			t.Errorf("Resolve(0) = (%d, %d, %v), want (0, 0, nil)", off, n, err)
		}
	}
}

func TestRangeResolveRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		rng  storage.Range
		size int64
	}{
		{"offset past the end", storage.From(101), 100},
		{"slice past the end", storage.Slice(101, 1), 100},
		{"negative offset", storage.From(-1), 100},
		{"negative length", storage.Slice(0, -1), 100},
		{"zero length suffix", storage.Suffix(0), 100},
		{"negative suffix", storage.Suffix(-5), 100},
		{"negative size", storage.All(), -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := tt.rng.Resolve(tt.size); !errors.Is(err, storage.ErrInvalidRange) {
				t.Errorf("Resolve = %v, want ErrInvalidRange", err)
			}
		})
	}
}
