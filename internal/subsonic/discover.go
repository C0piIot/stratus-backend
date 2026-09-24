package subsonic

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// The listings a client's home screen and search box are made of.
//
// Every default and every maximum here is the specification's, not a choice:
// a client that omits size is asking for ten, and one that asks for a thousand
// is answered with five hundred. Getting those wrong is invisible until a
// library is big enough to page.
const (
	albumPageSize  = 10
	songPageSize   = 10
	searchPageSize = 20
	// maxPageSize is the specification's cap on every size and count.
	maxPageSize = 500
)

type albumList2 struct {
	Albums []albumRef `xml:"album" json:"album"`
}

// albumList is the same listing for a client that predates the ID3 endpoints.
// Its albums are Child elements, which is why they carry isDir.
type albumList struct {
	Albums []child `xml:"album" json:"album"`
}

type searchResult3 struct {
	Artists []artistRef `xml:"artist" json:"artist"`
	Albums  []albumRef  `xml:"album" json:"album"`
	Songs   []child     `xml:"song" json:"song"`
}

// searchResult2 is search3's predecessor, and the shape is genuinely different
// rather than renamed: its albums are Child elements too.
type searchResult2 struct {
	Artists []artistRef `xml:"artist" json:"artist"`
	Albums  []child     `xml:"album" json:"album"`
	Songs   []child     `xml:"song" json:"song"`
}

type genres struct {
	Genres []genre `xml:"genre" json:"genre"`
}

// genre is the one payload whose name is not an attribute. In XML the genre is
// the element's own text -- `<genre songCount="3" albumCount="1">Rock</genre>`
// -- while in JSON it is a field called "value". Two shapes for one field, and
// nothing complains if it is written as an attribute instead.
type genre struct {
	SongCount  int    `xml:"songCount,attr" json:"songCount"`
	AlbumCount int    `xml:"albumCount,attr" json:"albumCount"`
	Name       string `xml:",chardata" json:"value"`
}

type songList struct {
	Songs []child `xml:"song" json:"song"`
}

// albumOrders maps the protocol's ten types onto what the library can answer,
// which is all of them.
var albumOrders = map[string]db.AlbumOrder{
	"alphabeticalByName":   db.AlbumsByName,
	"alphabeticalByArtist": db.AlbumsByArtist,
	"newest":               db.AlbumsByAdded,
	"random":               db.AlbumsRandom,
	"starred":              db.AlbumsStarred,
	"highest":              db.AlbumsHighest,
	"frequent":             db.AlbumsFrequent,
	"recent":               db.AlbumsRecent,
	"byGenre":              db.AlbumsByName,
	"byYear":               db.AlbumsByYear,
}

func (h *handler) albumList2(w http.ResponseWriter, r *http.Request, username string) {
	albums, apiErr := h.albumsFor(r, username)
	if apiErr != nil {
		h.fail(w, r, *apiErr)
		return
	}

	list := albumList2{Albums: make([]albumRef, 0, len(albums))}
	for _, a := range albums {
		list.Albums = append(list.Albums, albumOf(a))
	}
	if apiErr := h.annotate(r, username, each(list.Albums)); apiErr != nil {
		h.fail(w, r, *apiErr)
		return
	}

	env := h.ok()
	env.AlbumList2 = &list
	h.write(w, r, env)
}

func (h *handler) albumList(w http.ResponseWriter, r *http.Request, username string) {
	albums, apiErr := h.albumsFor(r, username)
	if apiErr != nil {
		h.fail(w, r, *apiErr)
		return
	}

	list := albumList{Albums: make([]child, 0, len(albums))}
	for _, a := range albums {
		list.Albums = append(list.Albums, albumChild(a))
	}
	if apiErr := h.annotate(r, username, each(list.Albums)); apiErr != nil {
		h.fail(w, r, *apiErr)
		return
	}

	env := h.ok()
	env.AlbumList = &list
	h.write(w, r, env)
}

// albumsFor is the query both listings share, including the parameter checking:
// the two endpoints differ in how they render an album and in nothing else.
func (h *handler) albumsFor(r *http.Request, username string) ([]db.Album, *apiError) {
	q := r.URL.Query()

	kind := q.Get("type")
	if kind == "" {
		return nil, &apiError{errMissingParam, "the type parameter is required"}
	}
	order, ok := albumOrders[kind]
	if !ok {
		return nil, &apiError{errMissingParam, "unknown type " + kind}
	}

	f := db.AlbumFilter{
		Order: order,
		Page:  db.Page{Limit: sizeParam(q, "size", albumPageSize), Offset: intParam(q, "offset", 0)},
	}

	switch kind {
	case "byGenre":
		if f.Genre = q.Get("genre"); f.Genre == "" {
			return nil, &apiError{errMissingParam, "type=byGenre needs a genre"}
		}
	case "byYear":
		from, to := q.Get("fromYear"), q.Get("toYear")
		if from == "" || to == "" {
			return nil, &apiError{errMissingParam, "type=byYear needs fromYear and toYear"}
		}
		f.FromYear, f.ToYear = intParam(q, "fromYear", 0), intParam(q, "toYear", 0)
		// The protocol asks for a descending list by giving the range
		// backwards. It is a quirk and it belongs here rather than in the
		// port: the database is asked for a range and an order, not for what a
		// client meant by inverting one.
		if f.FromYear > f.ToYear {
			f.FromYear, f.ToYear = f.ToYear, f.FromYear
			f.Order = db.AlbumsByYearDesc
		}
	}

	albums, err := h.lib.AlbumList(r.Context(), username, f)
	if err != nil {
		return nil, ptr(h.internal(r, "list albums", err))
	}
	return albums, nil
}

func (h *handler) search3(w http.ResponseWriter, r *http.Request, username string) {
	found, apiErr := h.searchFor(r, username)
	if apiErr != nil {
		h.fail(w, r, *apiErr)
		return
	}

	result := searchResult3{
		Artists: make([]artistRef, 0, len(found.Artists)),
		Albums:  make([]albumRef, 0, len(found.Albums)),
		Songs:   make([]child, 0, len(found.Tracks)),
	}
	for _, a := range found.Artists {
		result.Artists = append(result.Artists, artistRefOf(a))
	}
	for _, a := range found.Albums {
		result.Albums = append(result.Albums, albumOf(a))
	}
	for _, t := range found.Tracks {
		result.Songs = append(result.Songs, songOf(t))
	}
	if apiErr := h.annotate(r, username, each(result.Artists), each(result.Albums), each(result.Songs)); apiErr != nil {
		h.fail(w, r, *apiErr)
		return
	}

	env := h.ok()
	env.SearchResult3 = &result
	h.write(w, r, env)
}

func (h *handler) search2(w http.ResponseWriter, r *http.Request, username string) {
	found, apiErr := h.searchFor(r, username)
	if apiErr != nil {
		h.fail(w, r, *apiErr)
		return
	}

	result := searchResult2{
		Artists: make([]artistRef, 0, len(found.Artists)),
		Albums:  make([]child, 0, len(found.Albums)),
		Songs:   make([]child, 0, len(found.Tracks)),
	}
	for _, a := range found.Artists {
		result.Artists = append(result.Artists, artistRefOf(a))
	}
	for _, a := range found.Albums {
		result.Albums = append(result.Albums, albumChild(a))
	}
	for _, t := range found.Tracks {
		result.Songs = append(result.Songs, songOf(t))
	}
	if apiErr := h.annotate(r, username, each(result.Artists), each(result.Albums), each(result.Songs)); apiErr != nil {
		h.fail(w, r, *apiErr)
		return
	}

	env := h.ok()
	env.SearchResult2 = &result
	h.write(w, r, env)
}

// searchFor reads the six counts and offsets and runs the search.
//
// An absent query is not an error and not a mistake: the specification requires
// a server to answer an empty one with everything, because that is how a client
// downloads a library it wants to browse with no network.
func (h *handler) searchFor(r *http.Request, username string) (db.SearchResult, *apiError) {
	q := r.URL.Query()

	f := db.SearchFilter{
		Text: q.Get("query"),
		Artists: db.Page{
			Limit:  sizeParam(q, "artistCount", searchPageSize),
			Offset: intParam(q, "artistOffset", 0),
		},
		Albums: db.Page{
			Limit:  sizeParam(q, "albumCount", searchPageSize),
			Offset: intParam(q, "albumOffset", 0),
		},
		Tracks: db.Page{
			Limit:  sizeParam(q, "songCount", searchPageSize),
			Offset: intParam(q, "songOffset", 0),
		},
	}

	found, err := h.lib.Search(r.Context(), username, f)
	if err != nil {
		return db.SearchResult{}, ptr(h.internal(r, "search", err))
	}
	return found, nil
}

func (h *handler) genres(w http.ResponseWriter, r *http.Request, username string) {
	list, err := h.lib.Genres(r.Context(), username)
	if err != nil {
		h.fail(w, r, h.internal(r, "list genres", err))
		return
	}

	answer := genres{Genres: make([]genre, 0, len(list))}
	for _, g := range list {
		answer.Genres = append(answer.Genres, genre{
			Name:       g.Name,
			SongCount:  g.SongCount,
			AlbumCount: g.AlbumCount,
		})
	}

	env := h.ok()
	env.Genres = &answer
	h.write(w, r, env)
}

// songsByGenre is what makes getGenres worth answering: a list of genres
// nothing can open is decoration.
func (h *handler) songsByGenre(w http.ResponseWriter, r *http.Request, username string) {
	q := r.URL.Query()
	name := q.Get("genre")
	if name == "" {
		h.fail(w, r, apiError{errMissingParam, "the genre parameter is required"})
		return
	}

	f := db.TrackFilter{
		Order: db.TracksByPath,
		Genre: name,
		Page:  db.Page{Limit: sizeParam(q, "count", songPageSize), Offset: intParam(q, "offset", 0)},
	}
	tracks, err := h.lib.TrackList(r.Context(), username, f)
	if err != nil {
		h.fail(w, r, h.internal(r, "list a genre", err))
		return
	}

	list := songs(tracks)
	if apiErr := h.annotate(r, username, each(list.Songs)); apiErr != nil {
		h.fail(w, r, *apiErr)
		return
	}

	env := h.ok()
	env.SongsByGenre = list
	h.write(w, r, env)
}

// randomSongs is the shuffle button, and the same query with another order.
func (h *handler) randomSongs(w http.ResponseWriter, r *http.Request, username string) {
	q := r.URL.Query()

	f := db.TrackFilter{
		Order:    db.TracksRandom,
		Genre:    q.Get("genre"),
		FromYear: intParam(q, "fromYear", 0),
		ToYear:   intParam(q, "toYear", 0),
		Page:     db.Page{Limit: sizeParam(q, "size", songPageSize)},
	}
	tracks, err := h.lib.TrackList(r.Context(), username, f)
	if err != nil {
		h.fail(w, r, h.internal(r, "list random tracks", err))
		return
	}

	list := songs(tracks)
	if apiErr := h.annotate(r, username, each(list.Songs)); apiErr != nil {
		h.fail(w, r, *apiErr)
		return
	}

	env := h.ok()
	env.RandomSongs = list
	h.write(w, r, env)
}

func songs(tracks []db.Track) *songList {
	list := &songList{Songs: make([]child, 0, len(tracks))}
	for _, t := range tracks {
		list.Songs = append(list.Songs, songOf(t))
	}
	return list
}

// intParam reads a number a client sent, falling back to the default when it is
// absent or nonsense.
//
// Nonsense is not an error on purpose. These are sizes, offsets and years:
// refusing a whole screen because one of them was misspelled helps nobody, and
// the specification gives every one of them a default precisely so that a
// client may leave it out.
func intParam(q url.Values, name string, def int) int {
	n, err := strconv.Atoi(q.Get(name))
	if err != nil || n < 0 {
		return def
	}
	return n
}

// sizeParam is intParam for a count, where the specification also sets a
// ceiling. It is the only thing between one request and a whole library in
// memory, so it caps rather than refuses.
func sizeParam(q url.Values, name string, def int) int {
	n := intParam(q, name, def)
	if n > maxPageSize {
		return maxPageSize
	}
	return n
}
