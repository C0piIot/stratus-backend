package subsonic

import (
	"net/http"
	"strconv"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// Library is the music by tag and what its user has said about it. The two
// halves are separate ports because one only reads tags and the other is
// written by somebody; this adapter needs both.
type Library interface {
	db.Music
	db.Annotations
}

// annotated is the part of a song, an album and an artist that says what the
// user thinks of it and how much they have played it. Embedded rather than repeated, so the three cannot drift
// apart in how they spell it, and filled after the payload is built: see
// annotate.
type annotated struct {
	Starred    string `xml:"starred,attr,omitempty" json:"starred,omitempty"`
	UserRating int    `xml:"userRating,attr,omitempty" json:"userRating,omitempty"`
	PlayCount  int64  `xml:"playCount,attr,omitempty" json:"playCount,omitempty"`
	// Played is OpenSubsonic's, not Subsonic's: when it was last played.
	Played string `xml:"played,attr,omitempty" json:"played,omitempty"`
}

func (a *annotated) set(v db.Annotation) {
	if !v.Starred.IsZero() {
		a.Starred = stamp(v.Starred)
	}
	a.UserRating = v.Rating
	a.PlayCount = v.PlayCount
	if !v.Played.IsZero() {
		a.Played = stamp(v.Played)
	}
}

// annotatable is a payload that can carry an annotation. Its id is what says
// which subject it is, so a folder -- whose id names a path -- takes none.
type annotatable interface {
	subjectID() string
	annotation() *annotated
}

func (c *child) subjectID() string             { return c.ID }
func (c *child) annotation() *annotated        { return &c.annotated }
func (a *albumRef) subjectID() string          { return a.ID }
func (a *albumRef) annotation() *annotated     { return &a.annotated }
func (a *artistRef) subjectID() string         { return a.ID }
func (a *artistRef) annotation() *annotated    { return &a.annotated }
func (a *artistDetail) subjectID() string      { return a.ID }
func (a *artistDetail) annotation() *annotated { return &a.annotated }

// each takes the address of every element, which is what annotate writes
// through.
func each[T any, P interface {
	*T
	annotatable
}](items []T) []annotatable {
	out := make([]annotatable, len(items))
	for i := range items {
		out[i] = P(&items[i])
	}
	return out
}

// annotate fills in the stars and ratings of everything about to be sent, in
// one query however long the response is. It runs after the payload is built
// rather than inside the queries that built it, so that none of the library's
// queries had to learn about annotations and a listing costs one query more
// rather than a join in every one of them.
func (h *handler) annotate(r *http.Request, username string, groups ...[]annotatable) *apiError {
	var subjects []db.Subject
	var targets []*annotated
	for _, group := range groups {
		for _, item := range group {
			if s, ok := subjectOf(item.subjectID()); ok {
				subjects = append(subjects, s)
				targets = append(targets, item.annotation())
			}
		}
	}
	if len(subjects) == 0 {
		return nil
	}

	got, err := h.lib.AnnotationsOf(r.Context(), username, subjects)
	if err != nil {
		return ptr(h.internal(r, "read annotations", err))
	}
	for i, s := range subjects {
		if a, ok := got[s]; ok {
			targets[i].set(a)
		}
	}
	return nil
}

// subjectOf is the subject an id names, for the three kinds of id that name
// one. A folder does not: starring one would need a third kind of key, by path,
// and a rename would lose it (#194).
func subjectOf(id string) (db.Subject, bool) {
	if fileID, ok := parseSongID(id); ok {
		return db.TrackSubject(fileID), true
	}
	if artist, album, ok := parseAlbumID(id); ok {
		return db.AlbumSubject(artist, album), true
	}
	if name, ok := parseArtistID(id); ok {
		return db.ArtistSubject(name), true
	}
	return db.Subject{}, false
}

// star and unstar take any number of ids in three parameters. id is the one
// the folder-era clients send, and they send an album's id there as well as a
// song's -- getAlbumList hands out album ids as Child elements -- so it takes
// all three kinds.
func (h *handler) star(w http.ResponseWriter, r *http.Request, username string) {
	subjects, apiErr := h.subjectsFor(r, username, true)
	if apiErr != nil {
		h.fail(w, r, *apiErr)
		return
	}
	at := time.Now()
	for _, s := range subjects {
		if err := h.lib.Star(r.Context(), username, s, at); err != nil {
			h.fail(w, r, h.internal(r, "star", err))
			return
		}
	}
	h.write(w, r, h.ok())
}

func (h *handler) unstar(w http.ResponseWriter, r *http.Request, username string) {
	subjects, apiErr := h.subjectsFor(r, username, false)
	if apiErr != nil {
		h.fail(w, r, *apiErr)
		return
	}
	for _, s := range subjects {
		if err := h.lib.Unstar(r.Context(), username, s); err != nil {
			h.fail(w, r, h.internal(r, "unstar", err))
			return
		}
	}
	h.write(w, r, h.ok())
}

// setRating rates one thing, and 0 takes the rating away.
func (h *handler) setRating(w http.ResponseWriter, r *http.Request, username string) {
	id, apiErr := requiredID(r)
	if apiErr != nil {
		h.fail(w, r, *apiErr)
		return
	}
	rating, err := strconv.Atoi(r.URL.Query().Get("rating"))
	if err != nil || db.ValidateRating(rating) != nil {
		h.fail(w, r, apiError{errMissingParam, "rating must be a number from 0 to 5"})
		return
	}
	s, ok := subjectOf(id)
	if !ok {
		h.fail(w, r, notFound("song, album or artist"))
		return
	}
	if apiErr := h.exists(r, username, s); apiErr != nil {
		h.fail(w, r, *apiErr)
		return
	}
	if err := h.lib.SetRating(r.Context(), username, s, rating); err != nil {
		h.fail(w, r, h.internal(r, "rate", err))
		return
	}
	h.write(w, r, h.ok())
}

// subjectsFor reads the three id parameters, and refuses the whole request if
// any of them names nothing: a client told "ok" believes every one of them
// took.
//
// Whether the thing is still there is checked only for a star. Taking a star
// away from an album whose tracks have gone is how a client clears it, and
// refusing would leave a star nobody can see or remove.
func (h *handler) subjectsFor(r *http.Request, username string, mustExist bool) ([]db.Subject, *apiError) {
	q := r.URL.Query()

	var subjects []db.Subject
	for _, param := range []struct {
		name  string
		parse func(string) (db.Subject, bool)
	}{
		{"id", subjectOf},
		{"albumId", func(id string) (db.Subject, bool) {
			artist, album, ok := parseAlbumID(id)
			return db.AlbumSubject(artist, album), ok
		}},
		{"artistId", func(id string) (db.Subject, bool) {
			name, ok := parseArtistID(id)
			return db.ArtistSubject(name), ok
		}},
	} {
		for _, id := range q[param.name] {
			s, ok := param.parse(id)
			if !ok {
				return nil, ptr(notFound("song, album or artist"))
			}
			subjects = append(subjects, s)
		}
	}
	if len(subjects) == 0 {
		return nil, &apiError{errMissingParam, "one of id, albumId or artistId is required"}
	}

	if mustExist {
		for _, s := range subjects {
			if apiErr := h.exists(r, username, s); apiErr != nil {
				return nil, apiErr
			}
		}
	}
	return subjects, nil
}

// exists refuses a subject the library does not hold, so a star cannot be
// written against a tag a client invented.
func (h *handler) exists(r *http.Request, username string, s db.Subject) *apiError {
	switch s.Kind {
	case db.SubjectTrack:
		_, apiErr := h.audioTrack(r, username, songID(s.FileID))
		return apiErr
	case db.SubjectAlbum:
		tracks, err := h.lib.Tracks(r.Context(), username, s.Artist, s.Album)
		if err != nil {
			return ptr(h.internal(r, "find an album", err))
		}
		if len(tracks) == 0 {
			return ptr(notFound("album"))
		}
		return nil
	}

	// An artist, which is the only kind subjectOf has left.
	albums, err := h.lib.Albums(r.Context(), username, s.Artist)
	if err != nil {
		return ptr(h.internal(r, "find an artist", err))
	}
	if len(albums) == 0 {
		return ptr(notFound("artist"))
	}
	return nil
}

// starred2 is what a client reads on sync to find the user's favourites.
type starred2 struct {
	Artists []artistRef `xml:"artist" json:"artist"`
	Albums  []albumRef  `xml:"album" json:"album"`
	Songs   []child     `xml:"song" json:"song"`
}

// starred is the same for a client that predates the ID3 endpoints, and like
// search2 its albums are Child elements.
type starred struct {
	Artists []artistRef `xml:"artist" json:"artist"`
	Albums  []child     `xml:"album" json:"album"`
	Songs   []child     `xml:"song" json:"song"`
}

func (h *handler) starred2(w http.ResponseWriter, r *http.Request, username string) {
	items, err := h.lib.Starred(r.Context(), username)
	if err != nil {
		h.fail(w, r, h.internal(r, "list what is starred", err))
		return
	}

	list := starred2{
		Artists: make([]artistRef, 0, len(items.Artists)),
		Albums:  make([]albumRef, 0, len(items.Albums)),
		Songs:   make([]child, 0, len(items.Tracks)),
	}
	for _, a := range items.Artists {
		list.Artists = append(list.Artists, artistRefOf(a))
	}
	for _, a := range items.Albums {
		list.Albums = append(list.Albums, albumOf(a))
	}
	for _, t := range items.Tracks {
		list.Songs = append(list.Songs, songOf(t))
	}
	if apiErr := h.annotate(r, username, each(list.Artists), each(list.Albums), each(list.Songs)); apiErr != nil {
		h.fail(w, r, *apiErr)
		return
	}

	env := h.ok()
	env.Starred2 = &list
	h.write(w, r, env)
}

func (h *handler) starred(w http.ResponseWriter, r *http.Request, username string) {
	items, err := h.lib.Starred(r.Context(), username)
	if err != nil {
		h.fail(w, r, h.internal(r, "list what is starred", err))
		return
	}

	list := starred{
		Artists: make([]artistRef, 0, len(items.Artists)),
		Albums:  make([]child, 0, len(items.Albums)),
		Songs:   make([]child, 0, len(items.Tracks)),
	}
	for _, a := range items.Artists {
		list.Artists = append(list.Artists, artistRefOf(a))
	}
	for _, a := range items.Albums {
		list.Albums = append(list.Albums, albumChild(a))
	}
	for _, t := range items.Tracks {
		list.Songs = append(list.Songs, songOf(t))
	}
	if apiErr := h.annotate(r, username, each(list.Artists), each(list.Albums), each(list.Songs)); apiErr != nil {
		h.fail(w, r, *apiErr)
		return
	}

	env := h.ok()
	env.Starred = &list
	h.write(w, r, env)
}

// scrobble records plays, and it is the only thing that does: stream counts
// nothing, because the specification says plays come from here, and a client
// that seeks or buffers ahead would otherwise count one play many times.
//
// A client that played offline sends every id at once with a time for each, in
// milliseconds. One with no time was played now. submission=false is "now
// playing", which is accepted and not kept: there is no getNowPlaying for it to
// feed.
func (h *handler) scrobble(w http.ResponseWriter, r *http.Request, username string) {
	q := r.URL.Query()

	ids := q["id"]
	if len(ids) == 0 {
		h.fail(w, r, apiError{errMissingParam, "the id parameter is required"})
		return
	}
	times := q["time"]
	if len(times) > len(ids) {
		h.fail(w, r, apiError{errMissingParam, "more times than ids"})
		return
	}

	type play struct {
		fileID int64
		at     time.Time
	}
	now := time.Now()
	plays := make([]play, 0, len(ids))
	for i, id := range ids {
		t, apiErr := h.audioTrack(r, username, id)
		if apiErr != nil {
			h.fail(w, r, *apiErr)
			return
		}
		at := now
		if i < len(times) {
			ms, err := strconv.ParseInt(times[i], 10, 64)
			if err != nil || ms <= 0 {
				h.fail(w, r, apiError{errMissingParam, "a time is milliseconds since the epoch"})
				return
			}
			at = time.UnixMilli(ms)
		}
		plays = append(plays, play{t.File.ID, at})
	}

	if q.Get("submission") == "false" {
		h.write(w, r, h.ok())
		return
	}
	for _, p := range plays {
		if err := h.lib.RecordPlay(r.Context(), username, p.fileID, p.at); err != nil {
			h.fail(w, r, h.internal(r, "record a play", err))
			return
		}
	}
	h.write(w, r, h.ok())
}
