package storage

import "fmt"

// Range selects part of an object. The zero value reads the whole thing, which
// makes storage.Range{} and All() the same request.
//
// The fields are unexported on purpose: a Range can only be built by one of the
// constructors below, so "offset 5, length -3" is not a value that exists.
type Range struct {
	off int64
	n   int64
	// hasLen distinguishes "to the end of the object" from "zero bytes".
	hasLen bool
	// suffix reads the last n bytes, the bytes=-N form. Media needs it: ID3v1
	// tags and a faststart-less MP4 moov atom both live at the end of the file.
	suffix bool
}

// All returns the Range covering the whole object.
func All() Range { return Range{} }

// From returns the Range running from off to the end of the object.
func From(off int64) Range { return Range{off: off} }

// Slice returns the Range of n bytes starting at off. A length running past the
// end of the object is truncated, not an error.
func Slice(off, n int64) Range { return Range{off: off, n: n, hasLen: true} }

// Suffix returns the Range covering the last n bytes of the object.
func Suffix(n int64) Range { return Range{n: n, hasLen: true, suffix: true} }

// Resolve turns the Range into a concrete offset and length for an object of
// the given size.
//
// This lives in the port rather than in each adapter on purpose: it is exactly
// the place where two backends would otherwise quietly disagree about
// bytes=-500, about an offset at the end of the object, or about a length that
// runs off the end. An adapter translates the result; it does not reinterpret
// the request.
//
// An offset past the end of the object is ErrInvalidRange. An offset exactly at
// the end reads zero bytes: whether that deserves a 416 is an RFC 7233 decision
// for the protocol adapter, which has the size and the request in front of it.
func (r Range) Resolve(size int64) (off, n int64, err error) {
	if size < 0 {
		return 0, 0, fmt.Errorf("%w: negative object size %d", ErrInvalidRange, size)
	}

	if r.suffix {
		if r.n <= 0 {
			return 0, 0, fmt.Errorf("%w: suffix length %d must be positive", ErrInvalidRange, r.n)
		}
		if r.n >= size {
			return 0, size, nil
		}
		return size - r.n, r.n, nil
	}

	if r.off < 0 {
		return 0, 0, fmt.Errorf("%w: negative offset %d", ErrInvalidRange, r.off)
	}
	if r.off > size {
		return 0, 0, fmt.Errorf("%w: offset %d past the end of a %d byte object", ErrInvalidRange, r.off, size)
	}
	if !r.hasLen {
		return r.off, size - r.off, nil
	}
	if r.n < 0 {
		return 0, 0, fmt.Errorf("%w: negative length %d", ErrInvalidRange, r.n)
	}
	return r.off, min(r.n, size-r.off), nil
}
