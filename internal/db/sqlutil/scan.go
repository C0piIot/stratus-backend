package sqlutil

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// The port's entities read off a row, once (#360). Each scanner reads the
// columns its list names, in that order, which is why the lists live beside
// them: a scanner over a fixed column order is only safe when the order is
// in the same file. A driver selects these lists and hands the row and its
// Times here; what stays with the driver is every query string.

// Scanner is what *sql.Row and *sql.Rows have in common, so one scanner
// serves a lookup and a listing.
type Scanner interface {
	Scan(dest ...any) error
}

// Joined is a column list qualified for a join, where both tables carry
// columns the other also has.
func Joined(prefix, list string) string {
	return prefix + "." + strings.ReplaceAll(list, ", ", ", "+prefix+".")
}

// FileColumns is a file row.
const FileColumns = `id, owner_id, path, blob_key, size, mtime, etag, mime_type, is_dir`

// ScanFile reads one row of FileColumns.
func ScanFile(s Scanner, t Times) (db.File, error) {
	var f db.File
	if err := s.Scan(&f.ID, &f.OwnerID, &f.Path, &f.BlobKey, &f.Size, t.Dest(&f.MTime), &f.ETag, &f.MIMEType, &f.IsDir); err != nil {
		return db.File{}, err
	}
	return f, nil
}

// MediaColumns is a media row as a db.Media reads it, and MediaWriteColumns
// the same plus the folded columns a search matches on, which are written
// and filtered but never read back: they are how the row is stored, not part
// of what a db.Media is.
const (
	MediaColumns      = `file_id, kind, indexed_at, version, etag, error, retry_at, taken_at, width, height, orientation, latitude, longitude, camera, duration_ms, codec, bitrate, sample_rate, channels, bit_depth, codec_profile, level, frame_rate, audio_codec, color_primaries, color_transfer, color_space, dovi_profile, artist, album, title, track_no, disc_no, year, genre, album_artist, mb_release_group, mb_release`
	MediaWriteColumns = MediaColumns + `, search_title, search_artist, search_album, search_album_artist, search_photo`
)

// mediaDests is the destinations for one row of MediaColumns, shared by the
// media and the track scanners so the forty columns are listed once.
func mediaDests(m *db.Media, kind *string, lat, lon *nullFloat, t Times) []any {
	return []any{
		&m.FileID, kind, t.Dest(&m.IndexedAt), &m.Version, &m.ETag, &m.Error, t.NullDest(&m.RetryAt), t.NullDest(&m.TakenAt),
		&m.Width, &m.Height, &m.Orientation, lat, lon, &m.Camera,
		&m.DurationMS, &m.Codec, &m.Bitrate, &m.SampleRate, &m.Channels, &m.BitDepth, &m.CodecProfile, &m.Level, &m.FrameRate, &m.AudioCodec,
		&m.ColorPrimaries, &m.ColorTransfer, &m.ColorSpace, &m.DoViProfile, &m.Artist, &m.Album, &m.Title,
		&m.TrackNo, &m.DiscNo, &m.Year, &m.Genre, &m.AlbumArtist, &m.MBReleaseGroup, &m.MBRelease,
	}
}

// nullFloat is a REAL that may be NULL, for the two halves of a GPS fix.
type nullFloat struct {
	v     float64
	valid bool
}

func (n *nullFloat) Scan(v any) error {
	switch x := v.(type) {
	case nil:
		n.valid = false
		return nil
	case float64:
		n.v, n.valid = x, true
		return nil
	case int64:
		n.v, n.valid = float64(x), true
		return nil
	case []byte:
		var f float64
		if _, err := fmt.Sscan(string(x), &f); err != nil {
			return err
		}
		n.v, n.valid = f, true
		return nil
	}
	return errNotAFloat
}

var errNotAFloat = errors.New("sqlutil: not a REAL")

func finishMedia(m *db.Media, kind string, lat, lon nullFloat) {
	m.Kind = db.Kind(kind)
	if lat.valid && lon.valid {
		m.GPS = &db.GPS{Latitude: lat.v, Longitude: lon.v}
	}
}

// ScanMedia reads one row of MediaColumns.
func ScanMedia(s Scanner, t Times) (db.Media, error) {
	var m db.Media
	var kind string
	var lat, lon nullFloat
	if err := s.Scan(mediaDests(&m, &kind, &lat, &lon, t)...); err != nil {
		return db.Media{}, err
	}
	finishMedia(&m, kind, lat, lon)
	return m, nil
}

// ScanTrack reads a file row and a media row from one joined result --
// FileColumns then MediaColumns -- and whatever the query selected after
// them into extra.
func ScanTrack(s Scanner, t Times, extra ...any) (db.Track, error) {
	var tr db.Track
	var kind string
	var lat, lon nullFloat
	dests := make([]any, 0, 9+38+len(extra))
	dests = append(dests,
		&tr.File.ID, &tr.File.OwnerID, &tr.File.Path, &tr.File.BlobKey, &tr.File.Size,
		t.Dest(&tr.File.MTime), &tr.File.ETag, &tr.File.MIMEType, &tr.File.IsDir)
	dests = append(dests, mediaDests(&tr.Media, &kind, &lat, &lon, t)...)
	dests = append(dests, extra...)
	if err := s.Scan(dests...); err != nil {
		return db.Track{}, err
	}
	finishMedia(&tr.Media, kind, lat, lon)
	return tr, nil
}

// ScanCapture is ScanTrack with the sort column after the two halves, which
// is what the gallery pages on.
func ScanCapture(s Scanner, t Times) (db.Capture, error) {
	var at time.Time
	tr, err := ScanTrack(s, t, t.Dest(&at))
	if err != nil {
		return db.Capture{}, err
	}
	return db.Capture{Track: tr, SortAt: at}, nil
}

// ScanMediaState reads what a listing asks about one file: the row's id and
// what is known about its metadata and its pictures.
func ScanMediaState(s Scanner, t Times) (int64, db.MediaState, error) {
	var id int64
	var state db.MediaState
	err := s.Scan(&id, &state.Version, &state.ETag, &state.Failed, &state.Thumb, t.NullDest(&state.ThumbAt))
	return id, state, err
}

// ScanAlbum reads an album aggregate: the two tags, the counts and the
// earliest arrival.
func ScanAlbum(s Scanner, t Times) (db.Album, error) {
	var a db.Album
	if err := s.Scan(&a.Artist, &a.Name, &a.SongCount, &a.DurationMS, &a.Year, &a.Genre, t.Dest(&a.Created)); err != nil {
		return db.Album{}, err
	}
	return a, nil
}

// ScanArtist reads an artist aggregate.
func ScanArtist(s Scanner) (db.Artist, error) {
	var a db.Artist
	err := s.Scan(&a.Name, &a.AlbumCount)
	return a, err
}

// ScanGenre reads a genre aggregate.
func ScanGenre(s Scanner) (db.Genre, error) {
	var g db.Genre
	err := s.Scan(&g.Name, &g.SongCount, &g.AlbumCount)
	return g, err
}

// UploadColumns is an upload row.
const UploadColumns = `id, owner_id, path, size, received, blob_key, store_id, digest, mime_type, expires_at`

// ScanUpload reads one row of UploadColumns.
func ScanUpload(s Scanner, t Times) (db.Upload, error) {
	var u db.Upload
	if err := s.Scan(&u.ID, &u.OwnerID, &u.Path, &u.Size, &u.Received,
		&u.BlobKey, &u.StoreID, &u.Digest, &u.MIMEType, t.Dest(&u.ExpiresAt)); err != nil {
		return db.Upload{}, err
	}
	return u, nil
}

// TrashColumns is a trash row: the file row as it was, with the deletion it
// arrived in and when.
const TrashColumns = `batch, owner_id, path, blob_key, size, mtime, etag, mime_type, is_dir, deleted_at`

// ScanTrashed reads one row of TrashColumns.
func ScanTrashed(s Scanner, t Times) (db.Trashed, error) {
	var tr db.Trashed
	if err := s.Scan(&tr.Batch, &tr.OwnerID, &tr.Path, &tr.BlobKey, &tr.Size,
		t.Dest(&tr.MTime), &tr.ETag, &tr.MIMEType, &tr.IsDir, t.Dest(&tr.DeletedAt)); err != nil {
		return db.Trashed{}, err
	}
	return tr, nil
}

// ScanBatch reads one deletion: its id, its root, what it holds and when.
func ScanBatch(s Scanner, t Times) (db.TrashBatch, error) {
	var b db.TrashBatch
	if err := s.Scan(&b.ID, &b.Root, &b.Files, &b.Bytes, t.Dest(&b.DeletedAt)); err != nil {
		return db.TrashBatch{}, err
	}
	return b, nil
}

// ScanPlaylist reads a playlist row with its two counts behind it.
func ScanPlaylist(s Scanner, t Times) (db.Playlist, error) {
	var p db.Playlist
	if err := s.Scan(&p.ID, &p.OwnerID, &p.Name, &p.Comment, &p.Public, t.Dest(&p.Created), t.Dest(&p.Changed),
		&p.SongCount, &p.DurationMS); err != nil {
		return db.Playlist{}, err
	}
	return p, nil
}

// AlbumLookupColumns is an album's lookup row.
const AlbumLookupColumns = `owner_id, artist, album, looked_up_at, retry_at, release_group, cover_blob, error, search_version`

// ScanAlbumLookup reads one row of AlbumLookupColumns.
func ScanAlbumLookup(s Scanner, t Times) (db.AlbumLookup, error) {
	var l db.AlbumLookup
	if err := s.Scan(&l.Owner, &l.Artist, &l.Album, t.Dest(&l.LookedUpAt), t.NullDest(&l.RetryAt),
		&l.ReleaseGroup, &l.CoverBlob, &l.Error, &l.SearchVersion); err != nil {
		return db.AlbumLookup{}, err
	}
	return l, nil
}

// LyricsColumns is a track's lyrics row.
const LyricsColumns = `file_id, etag, fetched_at, retry_at, plain, synced, instrumental, error`

// ScanLyrics reads one row of LyricsColumns.
func ScanLyrics(s Scanner, t Times) (db.Lyrics, error) {
	var l db.Lyrics
	if err := s.Scan(&l.FileID, &l.ETag, t.Dest(&l.FetchedAt), t.NullDest(&l.RetryAt),
		&l.Plain, &l.Synced, &l.Instrumental, &l.Error); err != nil {
		return db.Lyrics{}, err
	}
	return l, nil
}

// ScanAlbumCandidate reads an album the lookup pass should ask about.
func ScanAlbumCandidate(s Scanner) (db.AlbumCandidate, error) {
	var c db.AlbumCandidate
	err := s.Scan(&c.Owner, &c.Artist, &c.Album, &c.ReleaseGroup, &c.Release)
	return c, err
}

// ScanTrackAnnotation reads a track's annotation row, keyed by its file.
func ScanTrackAnnotation(s Scanner, t Times) (db.Subject, db.Annotation, error) {
	var id int64
	var a db.Annotation
	if err := s.Scan(&id, t.NullDest(&a.Starred), &a.Rating, &a.PlayCount, t.NullDest(&a.Played)); err != nil {
		return db.Subject{}, db.Annotation{}, err
	}
	return db.TrackSubject(id), a, nil
}

// ScanTagAnnotation reads an album's or an artist's annotation row, keyed by
// its tags.
func ScanTagAnnotation(s Scanner, t Times) (db.Subject, db.Annotation, error) {
	var kind, artist, album string
	var a db.Annotation
	if err := s.Scan(&kind, &artist, &album, t.NullDest(&a.Starred), &a.Rating); err != nil {
		return db.Subject{}, db.Annotation{}, err
	}
	return db.Subject{Kind: db.SubjectKind(kind), Artist: artist, Album: album}, a, nil
}

// ScanAlbumPlays reads an album's plays, summed from its tracks'.
func ScanAlbumPlays(s Scanner, t Times) (db.Subject, db.Annotation, error) {
	var artist, album string
	var a db.Annotation
	if err := s.Scan(&artist, &album, &a.PlayCount, t.Dest(&a.Played)); err != nil {
		return db.Subject{}, db.Annotation{}, err
	}
	return db.AlbumSubject(artist, album), a, nil
}

// CalendarColumns is a calendar's row.
const CalendarColumns = `id, owner_id, name, display_name, description, sync_token, pruned_token, created_at`

// ScanCalendar reads one row of CalendarColumns.
func ScanCalendar(s Scanner, t Times) (db.Calendar, error) {
	var c db.Calendar
	if err := s.Scan(&c.ID, &c.OwnerID, &c.Name, &c.DisplayName, &c.Description,
		&c.SyncToken, &c.PrunedToken, t.Dest(&c.Created)); err != nil {
		return db.Calendar{}, err
	}
	return c, nil
}

// CalendarObjectColumns is a calendar object's row.
const CalendarObjectColumns = `calendar_id, name, uid, component, data, etag, sync_token, changed_at`

// ScanCalendarObject reads one row of CalendarObjectColumns.
func ScanCalendarObject(s Scanner, t Times) (db.CalendarObject, error) {
	var o db.CalendarObject
	if err := s.Scan(&o.CalendarID, &o.Name, &o.UID, &o.Component, &o.Data, &o.ETag,
		&o.SyncToken, t.Dest(&o.Changed)); err != nil {
		return db.CalendarObject{}, err
	}
	return o, nil
}

// ScanCalendarChange reads a name, its token and whether it was deleted.
func ScanCalendarChange(s Scanner) (db.CalendarChange, error) {
	var c db.CalendarChange
	if err := s.Scan(&c.Name, &c.SyncToken, &c.Deleted); err != nil {
		return db.CalendarChange{}, err
	}
	return c, nil
}
