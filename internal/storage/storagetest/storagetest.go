// Package storagetest is the conformance suite every storage backend has to
// pass.
//
// It exists so that "the disk backend does it this way" never becomes the
// contract by accident. The rules it follows are as important as the cases it
// runs: it never assumes an order in a listing, never assumes a resolution for
// ModTime, and never assumes anything about how a key turns into a path or an
// object name.
//
// It also stays inside what every backend can represent: no case here ever
// stores an object and a prefix under the same name. S3 is happy to hold both
// "a/b" and "a/b/c", a filesystem cannot, and the contract is the
// intersection.
package storagetest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/storage"
)

// Run executes the suite against the backend built by newStore.
//
// newStore is called once per case and must return an empty store that no other
// case can observe, so the cases can run in parallel.
func Run(t *testing.T, newStore func(t *testing.T) storage.Storage) {
	t.Helper()

	cases := []struct {
		name string
		fn   func(t *testing.T, s storage.Storage)
	}{
		{"put and get round trip", putGetRoundTrip},
		{"put overwrites atomically", putOverwrites},
		{"put accepts an unknown size", putUnknownSize},
		{"put rejects a size that does not match", putSizeMismatch},
		{"empty objects are objects", emptyObject},
		{"large objects stream", largeObject},
		{"missing objects report ErrNotFound", missingObject},
		{"delete is idempotent", deleteIdempotent},
		{"ranges", ranges},
		{"invalid ranges are rejected", invalidRanges},
		{"invalid keys are rejected", invalidKeys},
		{"keys are opaque unicode", unicodeKeys},
		{"list matches a string prefix", listPrefix},
		{"list with no prefix lists everything", listEverything},
		{"list reflects deletes", listAfterDelete},
		{"list can be abandoned early", listBreak},
		{"a cancelled context is honoured", cancelledContext},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.fn(t, newStore(t))
		})
	}
}

func putGetRoundTrip(t *testing.T, s storage.Storage) {
	const key = "photos/2024/06/IMG_0001.jpg"
	body := []byte("not really a jpeg")

	info := put(t, s, key, body)
	if info.Key != key {
		t.Errorf("Put returned key %q, want %q", info.Key, key)
	}
	if info.Size != int64(len(body)) {
		t.Errorf("Put returned size %d, want %d", info.Size, len(body))
	}
	// Deliberately loose: a backend may report seconds, or the clock of another
	// machine.
	if d := time.Since(info.ModTime); d > time.Hour || d < -time.Hour {
		t.Errorf("ModTime %v is %v away from now", info.ModTime, d)
	}

	if got := get(t, s, key, storage.All()); !bytes.Equal(got, body) {
		t.Errorf("Get = %q, want %q", got, body)
	}

	stat, err := s.Stat(t.Context(), key)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if stat.Size != info.Size || stat.Key != key {
		t.Errorf("Stat = %+v, want key %q size %d", stat, key, info.Size)
	}
}

func putOverwrites(t *testing.T, s storage.Storage) {
	const key = "notes.txt"
	put(t, s, key, []byte("the first, longer version"))
	put(t, s, key, []byte("the second"))

	if got := get(t, s, key, storage.All()); string(got) != "the second" {
		t.Errorf("Get = %q, want the second write", got)
	}
	if list := keys(t, s, ""); len(list) != 1 {
		t.Errorf("keys = %v, want one object", list)
	}
}

func putUnknownSize(t *testing.T, s storage.Storage) {
	const key = "unknown-length"
	body := []byte("streamed without a content-length")

	info, err := s.Put(t.Context(), key, bytes.NewReader(body), -1)
	if err != nil {
		t.Fatalf("Put with size -1: %v", err)
	}
	if info.Size != int64(len(body)) {
		t.Errorf("Size = %d, want %d", info.Size, len(body))
	}
}

func putSizeMismatch(t *testing.T, s storage.Storage) {
	const key = "truncated"
	body := []byte("only twenty-four bytes..")

	_, err := s.Put(t.Context(), key, bytes.NewReader(body), int64(len(body)+10))
	if !errors.Is(err, storage.ErrSizeMismatch) {
		t.Fatalf("Put with a short body = %v, want ErrSizeMismatch", err)
	}
	if _, err := s.Stat(t.Context(), key); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat after a failed Put = %v, want ErrNotFound: nothing should have been published", err)
	}

	// And a failed overwrite must leave the previous object alone.
	put(t, s, key, body)
	if _, err := s.Put(t.Context(), key, bytes.NewReader(body), 1); !errors.Is(err, storage.ErrSizeMismatch) {
		t.Fatalf("overwrite with a bad size = %v, want ErrSizeMismatch", err)
	}
	if got := get(t, s, key, storage.All()); !bytes.Equal(got, body) {
		t.Errorf("Get after a failed overwrite = %q, want the original %q", got, body)
	}
}

func emptyObject(t *testing.T, s storage.Storage) {
	const key = "empty"
	info := put(t, s, key, nil)
	if info.Size != 0 {
		t.Errorf("Size = %d, want 0", info.Size)
	}
	if got := get(t, s, key, storage.All()); len(got) != 0 {
		t.Errorf("Get = %q, want empty", got)
	}
	if list := keys(t, s, ""); !slices.Equal(list, []string{key}) {
		t.Errorf("keys = %v, want [%q]: an empty object is still an object", list, key)
	}
}

func largeObject(t *testing.T, s storage.Storage) {
	const key = "music/album/track.flac"
	body := make([]byte, 3<<20)
	for i := range body {
		body[i] = byte(i)
	}

	put(t, s, key, body)

	if got := get(t, s, key, storage.All()); !bytes.Equal(got, body) {
		t.Errorf("Get returned %d bytes, want the %d written", len(got), len(body))
	}
	// A range in the middle of a multi-megabyte object is the streaming case
	// that matters: it is what seeking in a media player becomes.
	if got := get(t, s, key, storage.Slice(1<<20, 16)); !bytes.Equal(got, body[1<<20:1<<20+16]) {
		t.Errorf("Get(Slice) = %v, want %v", got, body[1<<20:1<<20+16])
	}
}

func missingObject(t *testing.T, s storage.Storage) {
	const key = "nothing/here"

	if _, err := s.Stat(t.Context(), key); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat = %v, want ErrNotFound", err)
	}
	if _, _, err := s.Get(t.Context(), key, storage.All()); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Get = %v, want ErrNotFound", err)
	}

	// A key whose parent is an object, not a container, is equally absent.
	put(t, s, "a", []byte("x"))
	if _, err := s.Stat(t.Context(), "a/b"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat under an object = %v, want ErrNotFound", err)
	}
}

func deleteIdempotent(t *testing.T, s storage.Storage) {
	const key = "calendar/personal.ics"

	if err := s.Delete(t.Context(), key); err != nil {
		t.Errorf("Delete of a missing key = %v, want nil", err)
	}

	put(t, s, key, []byte("BEGIN:VCALENDAR"))
	if err := s.Delete(t.Context(), key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Stat(t.Context(), key); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat after Delete = %v, want ErrNotFound", err)
	}
	if err := s.Delete(t.Context(), key); err != nil {
		t.Errorf("second Delete = %v, want nil", err)
	}
}

func ranges(t *testing.T, s storage.Storage) {
	const key = "alphabet"
	const body = "abcdefghijklmnopqrstuvwxyz"
	put(t, s, key, []byte(body))

	tests := []struct {
		name string
		rng  storage.Range
		want string
	}{
		{"all", storage.All(), body},
		{"zero value is all", storage.Range{}, body},
		{"from the start", storage.From(0), body},
		{"from an offset", storage.From(20), "uvwxyz"},
		{"from the very end", storage.From(int64(len(body))), ""},
		{"a slice", storage.Slice(2, 3), "cde"},
		{"a slice past the end is truncated", storage.Slice(24, 100), "yz"},
		{"an empty slice", storage.Slice(5, 0), ""},
		{"a suffix", storage.Suffix(3), "xyz"},
		{"a suffix longer than the object", storage.Suffix(100), body},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := get(t, s, key, tt.rng); string(got) != tt.want {
				t.Errorf("Get = %q, want %q", got, tt.want)
			}
		})
	}
}

func invalidRanges(t *testing.T, s storage.Storage) {
	const key = "alphabet"
	put(t, s, key, []byte("abcdefghijklmnopqrstuvwxyz"))

	tests := []struct {
		name string
		rng  storage.Range
	}{
		{"offset past the end", storage.From(27)},
		{"slice past the end", storage.Slice(99, 1)},
		{"zero length suffix", storage.Suffix(0)},
		{"negative suffix", storage.Suffix(-1)},
		{"negative offset", storage.From(-1)},
		{"negative length", storage.Slice(0, -1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r, _, err := s.Get(t.Context(), key, tt.rng)
			if err == nil {
				_ = r.Close()
			}
			if !errors.Is(err, storage.ErrInvalidRange) {
				t.Errorf("Get = %v, want ErrInvalidRange", err)
			}
		})
	}
}

func invalidKeys(t *testing.T, s storage.Storage) {
	bad := []string{
		"",
		"/leading",
		"trailing/",
		"double//slash",
		"dot/./segment",
		"dot/../segment",
		"..",
		".hidden/photo.jpg",
		`back\slash`,
		"nul\x00byte",
		"newline\nin\nkey",
		strings.Repeat("x/", storage.MaxKeyLen) + "x",
		strings.Repeat("x", storage.MaxSegmentLen+1),
	}
	for _, key := range bad {
		t.Run(strconv.Quote(key), func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()

			if _, err := s.Put(ctx, key, strings.NewReader("x"), 1); !errors.Is(err, storage.ErrInvalidKey) {
				t.Errorf("Put = %v, want ErrInvalidKey", err)
			}
			if _, _, err := s.Get(ctx, key, storage.All()); !errors.Is(err, storage.ErrInvalidKey) {
				t.Errorf("Get = %v, want ErrInvalidKey", err)
			}
			if _, err := s.Stat(ctx, key); !errors.Is(err, storage.ErrInvalidKey) {
				t.Errorf("Stat = %v, want ErrInvalidKey", err)
			}
			if err := s.Delete(ctx, key); !errors.Is(err, storage.ErrInvalidKey) {
				t.Errorf("Delete = %v, want ErrInvalidKey", err)
			}
		})
	}
}

func unicodeKeys(t *testing.T, s storage.Storage) {
	const key = "fotos/verano/ñandú en la playa 🌞.jpg"
	body := []byte("bytes")

	put(t, s, key, body)
	if got := get(t, s, key, storage.All()); !bytes.Equal(got, body) {
		t.Errorf("Get = %q, want %q", got, body)
	}
	if list := keys(t, s, "fotos/"); !slices.Equal(list, []string{key}) {
		t.Errorf("keys = %v, want [%q]", list, key)
	}
}

func listPrefix(t *testing.T, s storage.Storage) {
	for _, key := range []string{"a/b", "a/bc", "a/c/d", "ab", "b/x"} {
		put(t, s, key, []byte(key))
	}

	tests := []struct {
		prefix string
		want   []string
	}{
		// A string prefix, not a directory: "a/b" matches "a/bc" too.
		{"a/b", []string{"a/b", "a/bc"}},
		{"a/", []string{"a/b", "a/bc", "a/c/d"}},
		{"a", []string{"a/b", "a/bc", "a/c/d", "ab"}},
		{"b/", []string{"b/x"}},
		{"zzz", nil},
	}
	for _, tt := range tests {
		t.Run(tt.prefix, func(t *testing.T) {
			t.Parallel()
			if got := keys(t, s, tt.prefix); !slices.Equal(got, tt.want) {
				t.Errorf("keys(%q) = %v, want %v", tt.prefix, got, tt.want)
			}
		})
	}
}

func listEverything(t *testing.T, s storage.Storage) {
	if got := keys(t, s, ""); len(got) != 0 {
		t.Fatalf("a fresh store lists %v, want nothing", got)
	}

	want := []string{"deep/nested/path/file", "top", "x/y"}
	for _, key := range want {
		put(t, s, key, []byte(key))
	}
	if got := keys(t, s, ""); !slices.Equal(got, want) {
		t.Errorf("keys = %v, want %v", got, want)
	}
}

func listAfterDelete(t *testing.T, s storage.Storage) {
	for _, key := range []string{"one/a", "one/b", "two/c"} {
		put(t, s, key, []byte(key))
	}
	if err := s.Delete(t.Context(), "one/a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := keys(t, s, ""); !slices.Equal(got, []string{"one/b", "two/c"}) {
		t.Errorf("keys = %v, want the two survivors", got)
	}

	// Emptying a prefix must leave nothing behind that a listing can see.
	if err := s.Delete(t.Context(), "one/b"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := keys(t, s, "one/"); len(got) != 0 {
		t.Errorf("keys under an emptied prefix = %v, want nothing", got)
	}
}

func listBreak(t *testing.T, s storage.Storage) {
	for _, key := range []string{"a", "b", "c", "d"} {
		put(t, s, key, []byte(key))
	}

	seen := 0
	for _, err := range s.List(t.Context(), "") {
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		seen++
		break
	}
	if seen != 1 {
		t.Errorf("saw %d objects, want to have stopped after 1", seen)
	}
}

func cancelledContext(t *testing.T, s storage.Storage) {
	put(t, s, "key", []byte("body"))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := s.Put(ctx, "key", strings.NewReader("x"), 1); !errors.Is(err, context.Canceled) {
		t.Errorf("Put = %v, want context.Canceled", err)
	}
	if _, _, err := s.Get(ctx, "key", storage.All()); !errors.Is(err, context.Canceled) {
		t.Errorf("Get = %v, want context.Canceled", err)
	}
	if _, err := s.Stat(ctx, "key"); !errors.Is(err, context.Canceled) {
		t.Errorf("Stat = %v, want context.Canceled", err)
	}
	if err := s.Delete(ctx, "key"); !errors.Is(err, context.Canceled) {
		t.Errorf("Delete = %v, want context.Canceled", err)
	}

	var listErr error
	for _, err := range s.List(ctx, "") {
		if err != nil {
			listErr = err
			break
		}
	}
	if !errors.Is(listErr, context.Canceled) {
		t.Errorf("List = %v, want context.Canceled", listErr)
	}
}

// put writes body and fails the test if it cannot.
func put(t *testing.T, s storage.Storage, key string, body []byte) storage.ObjectInfo {
	t.Helper()
	info, err := s.Put(t.Context(), key, bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("Put(%q): %v", key, err)
	}
	return info
}

// get reads a range in full and fails the test if it cannot.
func get(t *testing.T, s storage.Storage, key string, rng storage.Range) []byte {
	t.Helper()
	r, _, err := s.Get(t.Context(), key, rng)
	if err != nil {
		t.Fatalf("Get(%q): %v", key, err)
	}
	defer func() {
		if cerr := r.Close(); cerr != nil {
			t.Errorf("Close(%q): %v", key, cerr)
		}
	}()
	body, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read %q: %v", key, err)
	}
	return body
}

// keys collects a listing and sorts it. The sort is the point: the port does
// not promise an order, so a suite that compared listings as they arrived would
// pin whichever order the first backend happened to produce.
func keys(t *testing.T, s storage.Storage, prefix string) []string {
	t.Helper()
	var out []string
	for info, err := range s.List(t.Context(), prefix) {
		if err != nil {
			t.Fatalf("List(%q): %v", prefix, err)
		}
		out = append(out, info.Key)
	}
	slices.Sort(out)
	return out
}
