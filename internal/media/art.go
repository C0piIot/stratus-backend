package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"log/slog"
	"strconv"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/storage"
)

// A front cover fetched from outside (#217) is the third place an album's
// picture can come from, after a file beside the tracks and a picture inside
// one of them -- and it is tried third, because the other two are the user's
// own and this one is somebody else's idea of the record.
//
// It is kept as a derived object under one of the album's tracks, which is
// what makes it the sweep's to collect: delete the track and the picture goes
// with it, raise the generation and it is made again. The row in album_lookups
// says which track, so a request for any track of the album finds it.

// ErrNotAPicture is what StoreArt says about bytes that do not decode, which
// the pass records against the album rather than retrying in an hour.
var ErrNotAPicture = errors.New("media: not a picture")

// artKey is where the fetched cover lives, and artSizeKey where each size
// made from it does. Under the track's blob, like coverKey, and with a name
// of their own because the track may also carry a picture in its tag whose
// sizes must not be confused with these.
func artKey(blobKey string) string { return files.DerivedKey(blobKey, "art.jpg") }

func artSizeKey(blobKey string, size thumbSize) string {
	return files.DerivedKey(blobKey, "art-"+strconv.Itoa(int(size))+".jpg")
}

// artVerdictKey is where a fetched picture that would not decode is written
// down, so it is not decoded again on every request for a week.
func artVerdictKey(blobKey string) string {
	return files.DerivedKey(blobKey, files.CachePrefix+"art-none.txt")
}

// WithLookups gives the generator the rows that say which albums have a
// fetched cover and where it is. Without them Cover answers from the folder
// and the tag alone, which is what it did before #217.
func (t *Thumbs) WithLookups(l db.Lookups) *Thumbs {
	t.lookups = l
	return t
}

// StoreArt keeps picture as the fetched front cover of the album f belongs to.
//
// Decoded far enough to know it is a picture and how big one it would be,
// because what arrives is somebody else's bytes: the archive's answer, or a
// redirect that landed somewhere unexpected. Not re-encoded, since the sizes
// are made from it on request the way they are from a tag's picture.
func (t *Thumbs) StoreArt(ctx context.Context, f db.File, picture []byte) error {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(picture))
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotAPicture, err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height*4 > maxThumbSource {
		return fmt.Errorf("%w: %dx%d", ErrNotAPicture, cfg.Width, cfg.Height)
	}
	if _, err := t.blobs.Put(ctx, artKey(f.BlobKey), bytes.NewReader(picture), int64(len(picture))); err != nil {
		return fmt.Errorf("store the fetched cover of %q: %w", f.Path, err)
	}
	return nil
}

// errNoArt says nothing was fetched for this album, which is the answer for
// an album the pass has not reached, one the archive had nothing for, and one
// whose picture went with the track it was filed under.
var errNoArt = errors.New("media: no fetched cover")

// fetchedArt is the third source: the picture the pass filed for the album
// the track belongs to, at the size asked for.
func (t *Thumbs) fetchedArt(ctx context.Context, track db.File, px int) (io.ReadCloser, int64, error) {
	if t.lookups == nil {
		return nil, 0, errNoArt
	}
	m, err := t.meta.MediaByFile(ctx, track.ID)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return nil, 0, errNoArt
	case err != nil:
		return nil, 0, fmt.Errorf("read the tags of %q: %w", track.Path, err)
	case m.AlbumArtist == "" || m.Album == "":
		return nil, 0, errNoArt
	}
	row, err := t.lookups.AlbumLookup(ctx, track.OwnerID, m.AlbumArtist, m.Album)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return nil, 0, errNoArt
	case err != nil:
		return nil, 0, fmt.Errorf("read the lookup of %q / %q: %w", m.AlbumArtist, m.Album, err)
	case row.CoverBlob == "":
		return nil, 0, errNoArt
	}

	size := snapSize(px)
	refused := verdict{note: artVerdictKey(row.CoverBlob), says: errNoArt}
	return t.cached(ctx, artSizeKey(row.CoverBlob, size), refused, func() ([]byte, error) {
		body, _, err := t.blobs.Get(ctx, artKey(row.CoverBlob), storage.All())
		if errors.Is(err, storage.ErrNotFound) {
			// The track it was filed under was replaced or deleted and the
			// sweep took the picture with it. The row is told, so the next
			// pass fetches it again under whichever track is first now --
			// and the error stays the store's, which is what keeps this
			// from being remembered as a verdict.
			if ferr := t.lookups.ForgetCover(ctx, row.Owner, row.Artist, row.Album, time.Now()); ferr != nil {
				slog.WarnContext(ctx, "forgetting a lost cover", "artist", row.Artist, "album", row.Album, "err", ferr)
			}
			return nil, fmt.Errorf("%w: the fetched cover of %q / %q is gone", err, row.Artist, row.Album)
		}
		if err != nil {
			return nil, fmt.Errorf("%w: read the fetched cover: %w", errUnreachable, err)
		}
		defer func() { _ = body.Close() }()
		data, err := io.ReadAll(io.LimitReader(body, maxThumbSource))
		if err != nil {
			return nil, fmt.Errorf("%w: read the fetched cover: %w", errUnreachable, err)
		}
		return reduceTo(bytes.NewReader(data), "art.jpg", size)
	})
}
