package sqlutil_test

import (
	"database/sql"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/sqlutil"
)

// row is a row as a driver would hand it over: one value per column, in the
// column list's order. Scan assigns each to its destination the way
// database/sql would, with the Scanner destinations the codecs make handled
// through their own Scan -- which is what makes the three engines' conformance
// suites the test of the wire and this file the test of the mapping.
type row []any

func (r row) Scan(dest ...any) error {
	if len(dest) != len(r) {
		return errors.New("wrong number of destinations")
	}
	for i, d := range dest {
		if s, ok := d.(sql.Scanner); ok {
			if err := s.Scan(r[i]); err != nil {
				return err
			}
			continue
		}
		v := reflect.ValueOf(d).Elem()
		if r[i] == nil {
			v.Set(reflect.Zero(v.Type()))
			continue
		}
		v.Set(reflect.ValueOf(r[i]).Convert(v.Type()))
	}
	return nil
}

type refusing struct{}

func (refusing) Scan(...any) error { return errors.New("the backend is on fire") }

var (
	when  = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	later = when.Add(time.Hour)
)

// stamp is a time as each codec puts it on the wire, so one case runs over
// both.
func stamp(codec sqlutil.Times, t time.Time) any { return codec.Arg(t) }

var codecs = map[string]sqlutil.Times{"millis": sqlutil.Millis{}, "timestamps": sqlutil.Timestamps{}}

func TestTimesRoundTrip(t *testing.T) {
	t.Parallel()
	for name, codec := range codecs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var got time.Time
			if err := codec.Dest(&got).(sql.Scanner).Scan(codec.Arg(when)); err != nil || !got.Equal(when) || got.Location() != time.UTC {
				t.Errorf("Dest(Arg(when)) = %v, %v", got, err)
			}
			if codec.NullArg(time.Time{}) != nil {
				t.Error("NullArg of a zero time is not NULL")
			}
			if codec.NullArg(when) == nil {
				t.Error("NullArg of a time is NULL")
			}
			got = later
			if err := codec.NullDest(&got).(sql.Scanner).Scan(nil); err != nil || !got.IsZero() {
				t.Errorf("NullDest(NULL) = %v, %v; want zero", got, err)
			}
			if err := codec.Dest(&got).(sql.Scanner).Scan(nil); err == nil {
				t.Error("Dest took a NULL for a column that cannot hold one")
			}
			if err := codec.Dest(&got).(sql.Scanner).Scan("not a time"); err == nil {
				t.Error("Dest took a string")
			}
		})
	}
	// MySQL's text protocol hands an integer over as its digits.
	var got time.Time
	millis := sqlutil.Millis{}
	if err := millis.Dest(&got).(sql.Scanner).Scan([]byte(strconv.FormatInt(when.UnixMilli(), 10))); err != nil || !got.Equal(when) {
		t.Errorf("Millis.Dest of digits = %v, %v", got, err)
	}
	if err := millis.Dest(&got).(sql.Scanner).Scan([]byte("soon")); err == nil {
		t.Error("Millis.Dest took digits that are not")
	}
}

func TestJoined(t *testing.T) {
	t.Parallel()
	if got := sqlutil.Joined("f", "id, owner_id, path"); got != "f.id, f.owner_id, f.path" {
		t.Errorf("Joined = %q", got)
	}
}

func TestScanFile(t *testing.T) {
	t.Parallel()
	for name, codec := range codecs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, err := sqlutil.ScanFile(row{int64(7), "edu", "a/b.txt", "blob", int64(12), stamp(codec, when), `"e"`, "text/plain", true}, codec)
			if err != nil {
				t.Fatal(err)
			}
			want := db.File{ID: 7, OwnerID: "edu", Path: "a/b.txt", BlobKey: "blob", Size: 12, MTime: when, ETag: `"e"`, MIMEType: "text/plain", IsDir: true}
			if f != want {
				t.Errorf("ScanFile = %+v, want %+v", f, want)
			}
		})
	}
	if _, err := sqlutil.ScanFile(refusing{}, sqlutil.Millis{}); err == nil {
		t.Error("a scan that failed was not reported")
	}
}

// mediaRow is one row of MediaColumns, with a GPS fix and a retry time.
func mediaRow(codec sqlutil.Times, gps bool) row {
	var lat, lon any
	if gps {
		lat, lon = 41.3874, 2.1686
	}
	return row{int64(3), "video", stamp(codec, when), 10, `"e"`, "", codec.NullArg(later), nil,
		1920, 1080, 1, lat, lon, "Sony",
		int64(5000), "hevc", 320000, 48000, 6, 24, "Main 10", 41, 29970, "ac3",
		"bt2020", "arib-std-b67", "bt2020nc", 8, "Artist", "Album", "Title",
		1, 2, 1998, "Electronic", "Album Artist", "4cf8ba42-5d0b-3a4d-8e8f-4c8f2f3d1a1b", ""}
}

func TestScanMedia(t *testing.T) {
	t.Parallel()
	for name, codec := range codecs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m, err := sqlutil.ScanMedia(mediaRow(codec, true), codec)
			if err != nil {
				t.Fatal(err)
			}
			if m.FileID != 3 || m.Kind != db.KindVideo || !m.IndexedAt.Equal(when) || !m.RetryAt.Equal(later) || !m.TakenAt.IsZero() {
				t.Errorf("the row half is %+v", m)
			}
			if m.GPS == nil || m.GPS.Latitude != 41.3874 || m.Width != 1920 || m.DoViProfile != 8 || m.AlbumArtist != "Album Artist" || m.MBReleaseGroup == "" {
				t.Errorf("the columns are %+v", m)
			}
			bare, err := sqlutil.ScanMedia(mediaRow(codec, false), codec)
			if err != nil || bare.GPS != nil {
				t.Errorf("a row with no fix has one: %+v, %v", bare.GPS, err)
			}
		})
	}
	// A REAL may arrive as digits too.
	r := mediaRow(sqlutil.Millis{}, false)
	r[11], r[12] = []byte("41.5"), int64(2)
	m, err := sqlutil.ScanMedia(r, sqlutil.Millis{})
	if err != nil || m.GPS == nil || m.GPS.Latitude != 41.5 || m.GPS.Longitude != 2 {
		t.Errorf("a fix sent as digits and an integer = %+v, %v", m.GPS, err)
	}
	r[11] = []byte("north")
	if _, err := sqlutil.ScanMedia(r, sqlutil.Millis{}); err == nil {
		t.Error("a latitude that is not a number was taken")
	}
	r[11] = true
	if _, err := sqlutil.ScanMedia(r, sqlutil.Millis{}); err == nil {
		t.Error("a latitude that is a boolean was taken")
	}
}

func TestScanTrackAndCapture(t *testing.T) {
	t.Parallel()
	codec := sqlutil.Millis{}
	file := row{int64(7), "edu", "music/a.flac", "blob", int64(12), stamp(codec, when), `"e"`, "audio/flac", false}
	full := append(append(row{}, file...), mediaRow(codec, false)...)

	tr, err := sqlutil.ScanTrack(full, codec)
	if err != nil || tr.File.ID != 7 || tr.Media.FileID != 3 || !tr.File.MTime.Equal(when) {
		t.Errorf("ScanTrack = %+v, %v", tr, err)
	}
	var extra int64
	if _, eerr := sqlutil.ScanTrack(append(append(row{}, full...), int64(9)), codec, &extra); eerr != nil || extra != 9 {
		t.Errorf("the extra destination was not scanned: %d, %v", extra, eerr)
	}
	c, err := sqlutil.ScanCapture(append(append(row{}, full...), stamp(codec, later)), codec)
	if err != nil || !c.SortAt.Equal(later) || c.File.ID != 7 {
		t.Errorf("ScanCapture = %+v, %v", c, err)
	}
	if _, err := sqlutil.ScanCapture(refusing{}, codec); err == nil {
		t.Error("a scan that failed was not reported")
	}
}

func TestTheSmallerScanners(t *testing.T) {
	t.Parallel()
	codec := sqlutil.Timestamps{}

	id, state, err := sqlutil.ScanMediaState(row{int64(4), 10, `"e"`, false, int64(1), when}, codec)
	if err != nil || id != 4 || state.Thumb != db.ThumbMade || !state.ThumbAt.Equal(when) {
		t.Errorf("ScanMediaState = %d %+v, %v", id, state, err)
	}
	a, err := sqlutil.ScanAlbum(row{"Artist", "Album", 12, int64(3000), 1998, "Electronic", when}, codec)
	if err != nil || a.SongCount != 12 || !a.Created.Equal(when) {
		t.Errorf("ScanAlbum = %+v, %v", a, err)
	}
	if ar, aerr := sqlutil.ScanArtist(row{"Artist", 2}); aerr != nil || ar.AlbumCount != 2 {
		t.Errorf("ScanArtist = %+v, %v", ar, aerr)
	}
	if g, gerr := sqlutil.ScanGenre(row{"Rock", 3, 1}); gerr != nil || g.SongCount != 3 || g.AlbumCount != 1 {
		t.Errorf("ScanGenre = %+v, %v", g, gerr)
	}
	u, err := sqlutil.ScanUpload(row{"id", "edu", "a.mp4", int64(10), int64(4), "blob", "store", []byte{1}, "video/mp4", when}, codec)
	if err != nil || u.Received != 4 || !u.ExpiresAt.Equal(when) {
		t.Errorf("ScanUpload = %+v, %v", u, err)
	}
	tr, err := sqlutil.ScanTrashed(row{"batch", "edu", "a.txt", "blob", int64(1), when, `"e"`, "text/plain", false, later}, codec)
	if err != nil || tr.Batch != "batch" || !tr.MTime.Equal(when) || !tr.DeletedAt.Equal(later) {
		t.Errorf("ScanTrashed = %+v, %v", tr, err)
	}
	b, err := sqlutil.ScanBatch(row{"batch", "holiday", 3, int64(300), when}, codec)
	if err != nil || b.Files != 3 || !b.DeletedAt.Equal(when) {
		t.Errorf("ScanBatch = %+v, %v", b, err)
	}
	p, err := sqlutil.ScanPlaylist(row{int64(1), "edu", "Mix", "", false, when, later, 2, int64(500)}, codec)
	if err != nil || p.Name != "Mix" || !p.Changed.Equal(later) {
		t.Errorf("ScanPlaylist = %+v, %v", p, err)
	}
	l, err := sqlutil.ScanAlbumLookup(row{"edu", "Artist", "Album", when, nil, "rg", "blob", "", 1}, codec)
	if err != nil || l.ReleaseGroup != "rg" || !l.RetryAt.IsZero() || l.SearchVersion != 1 {
		t.Errorf("ScanAlbumLookup = %+v, %v", l, err)
	}
	ly, err := sqlutil.ScanLyrics(row{int64(7), `"e"`, when, later, "words", "", false, ""}, codec)
	if err != nil || ly.Plain != "words" || !ly.RetryAt.Equal(later) {
		t.Errorf("ScanLyrics = %+v, %v", ly, err)
	}
	cal, err := sqlutil.ScanCalendar(row{int64(3), "edu", "work", "Work", "", "#ff0000", int64(9), int64(2), when}, codec)
	if err != nil || cal.Name != "work" || cal.Color != "#ff0000" || cal.SyncToken != 9 || cal.PrunedToken != 2 || !cal.Created.Equal(when) {
		t.Errorf("ScanCalendar = %+v, %v", cal, err)
	}
	co, err := sqlutil.ScanCalendarObject(row{int64(3), "a.ics", "uid", "VEVENT", "BEGIN:VCALENDAR", "e", int64(9), when}, codec)
	if err != nil || co.UID != "uid" || co.SyncToken != 9 || !co.Changed.Equal(when) {
		t.Errorf("ScanCalendarObject = %+v, %v", co, err)
	}
	ch, err := sqlutil.ScanCalendarChange(row{"a.ics", int64(9), true})
	if err != nil || ch != (db.CalendarChange{Name: "a.ics", SyncToken: 9, Deleted: true}) {
		t.Errorf("ScanCalendarChange = %+v, %v", ch, err)
	}
	c, err := sqlutil.ScanAlbumCandidate(row{"edu", "Artist", "Album", "rg", "r"})
	if err != nil || c.Release != "r" {
		t.Errorf("ScanAlbumCandidate = %+v, %v", c, err)
	}
	s, an, err := sqlutil.ScanTrackAnnotation(row{int64(7), when, 4, int64(2), nil}, codec)
	if err != nil || s != db.TrackSubject(7) || an.Rating != 4 || !an.Starred.Equal(when) || !an.Played.IsZero() {
		t.Errorf("ScanTrackAnnotation = %+v %+v, %v", s, an, err)
	}
	s, an, err = sqlutil.ScanTagAnnotation(row{"album", "Artist", "Album", nil, 3}, codec)
	if err != nil || s != db.AlbumSubject("Artist", "Album") || an.Rating != 3 || !an.Starred.IsZero() {
		t.Errorf("ScanTagAnnotation = %+v %+v, %v", s, an, err)
	}
	s, an, err = sqlutil.ScanAlbumPlays(row{"Artist", "Album", int64(5), when}, codec)
	if err != nil || s != db.AlbumSubject("Artist", "Album") || an.PlayCount != 5 || !an.Played.Equal(when) {
		t.Errorf("ScanAlbumPlays = %+v %+v, %v", s, an, err)
	}

	// And every one of them reports a scan that failed.
	for name, scan := range map[string]func() error{
		"state":     func() error { _, _, err := sqlutil.ScanMediaState(refusing{}, codec); return err },
		"album":     func() error { _, err := sqlutil.ScanAlbum(refusing{}, codec); return err },
		"upload":    func() error { _, err := sqlutil.ScanUpload(refusing{}, codec); return err },
		"trashed":   func() error { _, err := sqlutil.ScanTrashed(refusing{}, codec); return err },
		"batch":     func() error { _, err := sqlutil.ScanBatch(refusing{}, codec); return err },
		"playlist":  func() error { _, err := sqlutil.ScanPlaylist(refusing{}, codec); return err },
		"lookup":    func() error { _, err := sqlutil.ScanAlbumLookup(refusing{}, codec); return err },
		"lyrics":    func() error { _, err := sqlutil.ScanLyrics(refusing{}, codec); return err },
		"candidate": func() error { _, err := sqlutil.ScanAlbumCandidate(refusing{}); return err },
		"track":     func() error { _, _, err := sqlutil.ScanTrackAnnotation(refusing{}, codec); return err },
		"tag":       func() error { _, _, err := sqlutil.ScanTagAnnotation(refusing{}, codec); return err },
		"plays":     func() error { _, _, err := sqlutil.ScanAlbumPlays(refusing{}, codec); return err },
		"media":     func() error { _, err := sqlutil.ScanMedia(refusing{}, codec); return err },
		"calendar":  func() error { _, err := sqlutil.ScanCalendar(refusing{}, codec); return err },
		"object":    func() error { _, err := sqlutil.ScanCalendarObject(refusing{}, codec); return err },
		"change":    func() error { _, err := sqlutil.ScanCalendarChange(refusing{}); return err },
	} {
		if err := scan(); err == nil || !strings.Contains(err.Error(), "on fire") {
			t.Errorf("%s: a scan that failed was not reported: %v", name, err)
		}
	}
}
