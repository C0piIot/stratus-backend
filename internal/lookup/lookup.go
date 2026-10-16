// Package lookup asks outside sources about the music library (#217): the
// identity of an album on MusicBrainz, the front cover the Cover Art Archive
// holds for it, and the words of a track from LRCLIB.
//
// Three things shape it. **It is off unless asked for**, because a self-hosted
// server must keep working with no route out, and what it sends -- artist,
// album and title tags, a duration -- leaves the machine. **It never rewrites a
// file**: what comes back is a row beside the library, or a derived object the
// sweep already knows how to collect, and a tag stays what the user wrote.
// And **it is polite by construction**: one request a second to each service,
// which is what MusicBrainz asks for and what the other two deserve, and a
// User-Agent that says who is asking.
//
// The pass is the indexer's shape a third time. What is pending is a query --
// an album with no row, a track with no row, a row whose time to ask again has
// come -- so nothing is enqueued, a restart loses nothing, and a library
// adopted or imported turns up on its own. A source that has nothing today is
// asked again in a month; a source that did not answer says nothing about the
// album, and is asked again in an hour.
package lookup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// The batches one pass looks at. Small, because every item is a request a
// second apart: a pass is a minute of work and not an hour holding a goroutine.
const (
	albumBatch  = 8
	lyricsBatch = 16
)

// SearchVersion is which search this build makes, written on every album's
// row. Raising it asks again, once, about every album an older search found
// nothing for -- media.Version's mechanism for the extractor, applied to the
// question rather than the file. It went to 1 when the search learned to ask
// by the first credited name after the whole credit found nothing.
const SearchVersion = 1

// How long a row waits before it is asked about again.
//
// A month for a source that answered "nothing": covers are uploaded and lyrics
// contributed, and an album missing today may be there next month, while
// asking every day would be the same question to the same service for ever.
// An hour for a source that did not answer at all, which is the indexer's
// retryAfter and for the same reason (#157).
const (
	retryNothing     = 30 * 24 * time.Hour
	retryUnreachable = time.Hour
)

// Index is what the pass reads and writes: the rows, and the tracks of an
// album, which is where the cover is filed.
type Index interface {
	db.Lookups
	Tracks(ctx context.Context, owner, artist, album string) ([]db.Track, error)
}

// ArtStore keeps a fetched front cover beside the pictures a track's own tag
// would have made. *thumb.Service is the one implementation: it owns the names
// derived objects are kept under, and the sweep reads them back.
type ArtStore interface {
	StoreArt(ctx context.Context, f db.File, picture []byte) error
}

// Service runs the pass.
type Service struct {
	index  Index
	art    ArtStore
	client *http.Client
	// agent is what every request says it is. MusicBrainz refuses anonymous
	// clients and asks for a version and a way to reach the maintainer.
	agent string
	pace  *pacer
	// The three services, as base URLs, so a test can stand in for them.
	musicBrainz, coverArt, lrclib string
	now                           func() time.Time
}

// Option configures a Service.
type Option func(*Service)

// WithEndpoints points the three clients somewhere other than the real
// services, which is what a test does.
func WithEndpoints(musicBrainz, coverArt, lrclib string) Option {
	return func(s *Service) { s.musicBrainz, s.coverArt, s.lrclib = musicBrainz, coverArt, lrclib }
}

// WithPace sets how long two requests to one service are kept apart. Zero is
// no pacing at all, for a test against a server of its own.
func WithPace(d time.Duration) Option {
	return func(s *Service) { s.pace = newPacer(d) }
}

// WithClock replaces the clock the rows are stamped by.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// New wires a Service. version goes into the User-Agent, which is the one
// place this server says who it is to anybody else.
func New(index Index, art ArtStore, version string, opts ...Option) *Service {
	s := &Service{
		index: index, art: art,
		client:      &http.Client{Timeout: requestTimeout},
		agent:       userAgent(version),
		pace:        newPacer(defaultPace),
		musicBrainz: musicBrainzURL, coverArt: coverArtURL, lrclib: lrclibURL,
		now: time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Done is what one pass did.
type Done struct {
	// Albums is how many were asked about and answered, one way or the
	// other; Covers how many of those ended with a picture; Lyrics how many
	// tracks were answered.
	Albums, Covers, Lyrics int
	// Deferred is how many could not be asked because a source did not
	// answer, and will be asked again in an hour.
	Deferred int
	// More says a batch was full, so there is probably more to do.
	More bool
	// Refused says a source turned the client away and a half of the pass
	// stopped there. The rows it reached wait an hour; the rest of that
	// batch is still pending, and the caller decides how soon to come back
	// for it -- sooner than the hour, since the refusal was about a moment
	// and not about those albums.
	Refused bool
}

// Pass asks about one batch of albums and one of tracks, and reports.
//
// A source that is refusing requests stops its half of the pass where it is:
// the rest of that batch is still pending and will be found by the next one,
// and asking seven more times after a 503 is how a server gets its agent
// blocked. Its half and not the whole, because the two halves ask different
// services -- MusicBrainz turning the client away says nothing about LRCLIB.
func (s *Service) Pass(ctx context.Context) (Done, error) {
	var done Done
	now := s.now()

	// Albums that were retagged out of existence would otherwise keep their
	// rows for good: a retag is a different album as far as anything here
	// can tell.
	if pruned, err := s.index.PruneAlbumLookups(ctx); err != nil {
		return done, fmt.Errorf("prune album lookups: %w", err)
	} else if pruned > 0 {
		slog.Info("forgot albums the library no longer has", "rows", pruned)
	}

	albums, err := s.index.PendingAlbumLookups(ctx, SearchVersion, now, albumBatch)
	if err != nil {
		return done, fmt.Errorf("find albums to look up: %w", err)
	}
	albumsRefused := false
	for _, c := range albums {
		if cerr := ctx.Err(); cerr != nil {
			return done, cerr
		}
		outcome, aerr := s.lookupAlbum(ctx, c, now)
		if aerr != nil {
			return done, aerr
		}
		done.add(outcome)
		if outcome.refused {
			albumsRefused = true
			break
		}
	}

	tracks, err := s.index.PendingLyrics(ctx, now, lyricsBatch)
	if err != nil {
		return done, fmt.Errorf("find tracks to look up: %w", err)
	}
	lyricsRefused := false
	for _, t := range tracks {
		if cerr := ctx.Err(); cerr != nil {
			return done, cerr
		}
		outcome, lerr := s.lookupLyrics(ctx, t, now)
		if lerr != nil {
			return done, lerr
		}
		done.add(outcome)
		if outcome.refused {
			lyricsRefused = true
			break
		}
	}

	// More is about the half that was not refused: a batch cut short by a
	// refusal is not a full one, and coming straight back for it would be
	// asking again what was just turned away.
	done.More = (!albumsRefused && len(albums) == albumBatch) || (!lyricsRefused && len(tracks) == lyricsBatch)
	done.Refused = albumsRefused || lyricsRefused
	return done, nil
}

// outcome is what one album's or one track's turn came to.
type outcome struct {
	answered, cover, lyrics, deferred bool
	// refused says a source is turning requests away, which ends the pass.
	refused bool
}

func (d *Done) add(o outcome) {
	if o.answered && !o.lyrics {
		d.Albums++
	}
	if o.cover {
		d.Covers++
	}
	if o.lyrics {
		d.Lyrics++
	}
	if o.deferred {
		d.Deferred++
	}
}

// lookupAlbum settles one album: its release group, from the tags or from a
// search, and then the front cover filed under that.
func (s *Service) lookupAlbum(ctx context.Context, c db.AlbumCandidate, now time.Time) (outcome, error) {
	row := db.AlbumLookup{Owner: c.Owner, Artist: c.Artist, Album: c.Album, LookedUpAt: now, SearchVersion: SearchVersion}

	group, release := c.ReleaseGroup, c.Release
	if group == "" && release == "" {
		found, err := s.releaseGroup(ctx, c.Artist, c.Album)
		if err != nil {
			return s.deferAlbum(ctx, row, err)
		}
		group = found
	}
	row.ReleaseGroup = group
	if group == "" && release == "" {
		row.RetryAt = now.Add(retryNothing)
		row.Error = "not found on MusicBrainz"
		return s.putAlbum(ctx, row, outcome{answered: true})
	}

	picture, err := s.frontCover(ctx, group, release)
	switch {
	case errors.Is(err, errNothing):
		row.RetryAt = now.Add(retryNothing)
		row.Error = "no front cover in the Cover Art Archive"
		return s.putAlbum(ctx, row, outcome{answered: true})
	case err != nil:
		return s.deferAlbum(ctx, row, err)
	}

	// Filed under the album's first track, which is a stable choice for as
	// long as that track is there, and what the sweep collects it with.
	tracks, err := s.index.Tracks(ctx, c.Owner, c.Artist, c.Album)
	if err != nil {
		return outcome{}, fmt.Errorf("list the tracks of %q / %q: %w", c.Artist, c.Album, err)
	}
	if len(tracks) == 0 {
		// Deleted between the query and now. Nothing to file it under, and
		// nothing to write about.
		return outcome{}, nil
	}
	if err := s.art.StoreArt(ctx, tracks[0].File, picture); err != nil {
		// The archive answered with something that is not a picture, or the
		// store would not take it. Either way the identity is kept and the
		// picture is asked for again in a month.
		row.RetryAt = now.Add(retryNothing)
		row.Error = err.Error()
		return s.putAlbum(ctx, row, outcome{answered: true})
	}
	row.CoverBlob = tracks[0].File.BlobKey
	return s.putAlbum(ctx, row, outcome{answered: true, cover: true})
}

// deferAlbum writes a row that says to come back in an hour, because the
// failure was the source's and not the album's.
func (s *Service) deferAlbum(ctx context.Context, row db.AlbumLookup, why error) (outcome, error) {
	row.RetryAt = row.LookedUpAt.Add(retryUnreachable)
	row.Error = why.Error()
	slog.Warn("deferred an album lookup", "artist", row.Artist, "album", row.Album,
		"retrying_in", retryUnreachable, "err", why)
	return s.putAlbum(ctx, row, outcome{deferred: true, refused: errors.Is(why, errRefused)})
}

func (s *Service) putAlbum(ctx context.Context, row db.AlbumLookup, o outcome) (outcome, error) {
	if err := s.index.PutAlbumLookup(ctx, row); err != nil {
		return outcome{}, fmt.Errorf("record the lookup of %q / %q: %w", row.Artist, row.Album, err)
	}
	return o, nil
}

// lookupLyrics settles one track.
func (s *Service) lookupLyrics(ctx context.Context, t db.Track, now time.Time) (outcome, error) {
	row := db.Lyrics{FileID: t.File.ID, ETag: t.File.ETag, FetchedAt: now}

	found, err := s.lyrics(ctx, t.Media)
	switch {
	case errors.Is(err, errNothing):
		row.RetryAt = now.Add(retryNothing)
		row.Error = err.Error()
	case err != nil:
		row.RetryAt = now.Add(retryUnreachable)
		row.Error = err.Error()
		slog.Warn("deferred a lyrics lookup", "path", t.File.Path, "retrying_in", retryUnreachable, "err", err)
	default:
		row.Plain, row.Synced, row.Instrumental = found.Plain, found.Synced, found.Instrumental
	}

	switch perr := s.index.PutLyrics(ctx, row); {
	case errors.Is(perr, db.ErrNotFound):
		// Deleted between the query and now: nothing to record it against.
		return outcome{}, nil
	case perr != nil:
		return outcome{}, fmt.Errorf("record the lyrics of %q: %w", t.File.Path, perr)
	}
	switch {
	case err == nil, errors.Is(err, errNothing):
		return outcome{answered: true, lyrics: true}, nil
	default:
		return outcome{deferred: true, refused: errors.Is(err, errRefused)}, nil
	}
}
