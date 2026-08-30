package storage

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Limits applied everywhere, so that a key which works on one backend works on
// all of them: S3's object key limit, and the NAME_MAX of every filesystem
// Stratus is likely to sit on.
const (
	MaxKeyLen     = 1024
	MaxSegmentLen = 255
)

// ValidateKey reports whether key is usable on every backend.
//
// It rejects rather than sanitises. Silently rewriting a key would make two
// different keys collide on one object, which is the aliasing bug this
// chokepoint exists to prevent -- and it would do it invisibly.
//
// The accepted grammar is slash-separated, non-empty segments: no leading or
// trailing slash, no empty segment, no "." or ".." segment, no backslash, no
// control characters, valid UTF-8, at most MaxKeyLen bytes.
//
// Two rules are concessions to the disk backend: no segment may exceed
// MaxSegmentLen bytes, since that is a filesystem's NAME_MAX, and the first
// segment may not begin with a dot, since the disk backend reserves a dot
// directory under its root for partial writes. Both are stated here, in the
// shared validator, precisely so that the two backends accept exactly the same
// set of keys instead of diverging quietly on a key that only S3 can hold.
func ValidateKey(key string) error {
	switch {
	case key == "":
		return fmt.Errorf("%w: empty", ErrInvalidKey)
	case len(key) > MaxKeyLen:
		return fmt.Errorf("%w: %d bytes exceeds the %d byte limit", ErrInvalidKey, len(key), MaxKeyLen)
	case !utf8.ValidString(key):
		return fmt.Errorf("%w: not valid UTF-8", ErrInvalidKey)
	case strings.HasPrefix(key, "/"), strings.HasSuffix(key, "/"):
		return fmt.Errorf("%w: %q has a leading or trailing slash", ErrInvalidKey, key)
	case strings.Contains(key, `\`):
		return fmt.Errorf("%w: %q contains a backslash", ErrInvalidKey, key)
	}

	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: %q contains a control character", ErrInvalidKey, key)
		}
	}

	first := true
	for seg := range strings.SplitSeq(key, "/") {
		switch {
		case seg == "":
			return fmt.Errorf("%w: %q has an empty segment", ErrInvalidKey, key)
		case seg == "." || seg == "..":
			return fmt.Errorf("%w: %q has a %q segment", ErrInvalidKey, key, seg)
		case len(seg) > MaxSegmentLen:
			return fmt.Errorf("%w: %q has a %d byte segment, over the %d byte limit", ErrInvalidKey, key, len(seg), MaxSegmentLen)
		case first && seg[0] == '.':
			return fmt.Errorf("%w: %q starts with a dot segment, which the disk backend reserves", ErrInvalidKey, key)
		}
		first = false
	}
	return nil
}
