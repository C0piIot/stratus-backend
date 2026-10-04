package dav

import (
	"context"
	"errors"
	"os"
)

// What the generated mounts -- the photographs by date, the library by tag,
// the playlists -- answer the same way, in one place rather than three (#279).
//
// All three are views of the index served as collections, all three refuse
// every write, and since the browser half took the GETs none of them serves
// bytes either. The methods below are on x/net's interfaces and are reached by
// nothing: a write is refused before the library sees it, and a read is the
// other half's. They exist so the types satisfy the interface, and they are
// shared so that "no" is written once.

// errNoBytesHere is what a read of a generated resource is. Reaching it means
// the mount was wired without the browser half in front of it, which is a
// wiring mistake and says so.
var errNoBytesHere = errors.New("dav: these bytes are served by the browser half of this address")

// refusesWrites is the writing half of x/net's FileSystem.
type refusesWrites struct{}

func (refusesWrites) Mkdir(context.Context, string, os.FileMode) error { return errReadOnly }
func (refusesWrites) RemoveAll(context.Context, string) error          { return errReadOnly }
func (refusesWrites) Rename(context.Context, string, string) error     { return errReadOnly }

// noBytes is the reading half of x/net's File, for a resource whose bytes are
// served elsewhere.
type noBytes struct{}

func (noBytes) Read([]byte) (int, error)       { return 0, errNoBytesHere }
func (noBytes) Seek(int64, int) (int64, error) { return 0, errNoBytesHere }
func (noBytes) Write([]byte) (int, error)      { return 0, errReadOnly }
func (noBytes) Close() error                   { return nil }

// noSys is os.FileInfo's escape hatch, which nothing here has anything to put
// in.
type noSys struct{}

func (noSys) Sys() any { return nil }
