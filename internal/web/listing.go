package web

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// listPrefs is where a reader's last choice of ordering and page size is kept.
//
// A cookie rather than a row: it is a preference and not an authority, so it
// needs no signature, no storage and no migration, and the worst a forged one
// can do is order somebody's own folder by size. It is read only when the URL
// says nothing, so a link somebody was sent always opens the way it was meant
// to and never the way the receiver last left a different folder.
const listPrefs = "stratus_list"

// listPrefsTTL is a year. There is nothing in here that goes stale, and a
// preference that forgot itself every week would be worse than none.
const listPrefsTTL = 365 * 24 * time.Hour

// The three orderings a listing can be asked for, by the name the URL uses.
// Three and not one per column of the table: a column with nothing to sort by
// -- the thumbnail, the actions -- is not an ordering.
var listKeys = map[string]db.FileSortKey{
	"name":     db.SortName,
	"size":     db.SortSize,
	"modified": db.SortMTime,
}

// listRows is what a reader may ask for, and a closed set on purpose. The
// reason listPageSize gives for its own value is still the reason there is a
// largest one: five hundred rows is five hundred thumbnails to offer and a
// document to lay out, and the row count is the one thing on this page that
// decides how much of both.
var listRows = []int{50, 100, 500}

// listing is what a browser asked of a folder. It is a value rather than three
// parameters because everything this page emits -- the column headers, the row
// counts, the link to the next page -- has to say it again.
type listing struct {
	// Key is the URL's name for the ordering, and Desc its direction.
	Key  string
	Desc bool
	Rows int
}

// defaultListing is a folder nobody has expressed an opinion about.
var defaultListing = listing{Key: "name", Rows: listPageSize}

// order is what the port is asked for.
func (l listing) order() db.FileOrder {
	return db.FileOrder{By: listKeys[l.Key], Desc: l.Desc}
}

// query renders the listing back into a URL, with pairs appended -- the cursor,
// in the one place that has one. Always all three, even when they are the
// defaults: a link that dropped them would silently hand the next page back to
// whatever the cookie said.
func (l listing) query(pairs ...string) string {
	q := url.Values{}
	q.Set("sort", l.Key)
	if l.Desc {
		q.Set("order", "desc")
	} else {
		q.Set("order", "asc")
	}
	q.Set("rows", strconv.Itoa(l.Rows))
	for i := 0; i+1 < len(pairs); i += 2 {
		q.Set(pairs[i], pairs[i+1])
	}
	return q.Encode()
}

// parseListing reads a listing out of query values, falling back to from for
// whatever they do not name.
//
// Every value is checked against the set that exists. A page size out of a
// closed set rather than a number is the point: "rows=100000" would otherwise
// be a way to ask this server to render a hundred thousand rows and offer a
// thumbnail for each.
func parseListing(q url.Values, from listing) (listing, error) {
	out := from
	if v := q.Get("sort"); v != "" {
		if _, ok := listKeys[v]; !ok {
			return listing{}, fmt.Errorf("no listing is ordered by %q", v)
		}
		out.Key = v
	}
	switch v := q.Get("order"); v {
	case "":
	case "asc":
		out.Desc = false
	case "desc":
		out.Desc = true
	default:
		return listing{}, fmt.Errorf("a listing runs asc or desc, not %q", v)
	}
	if v := q.Get("rows"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || !slices.Contains(listRows, n) {
			return listing{}, fmt.Errorf("a page is one of %v rows, not %q", listRows, v)
		}
		out.Rows = n
	}
	return out, nil
}

// listingOf is the listing this request is to be answered in: what the URL
// says, then what the reader last chose, then the default.
func listingOf(r *http.Request) (listing, error) {
	from := defaultListing
	if c, err := r.Cookie(listPrefs); err == nil {
		// A cookie that does not parse is one somebody wrote by hand or one
		// this build no longer understands. Neither is worth a 400 on a folder:
		// it is a preference, so a bad one is simply not one.
		if saved, err := url.ParseQuery(c.Value); err == nil {
			if kept, err := parseListing(saved, defaultListing); err == nil {
				from = kept
			}
		}
	}
	return parseListing(r.URL.Query(), from)
}

// rememberListing writes the choice back, and only when the URL made one: a
// folder opened with no opinion must not overwrite the opinion somebody
// expressed on the last one.
func rememberListing(w http.ResponseWriter, r *http.Request, l listing) {
	q := r.URL.Query()
	if q.Get("sort") == "" && q.Get("order") == "" && q.Get("rows") == "" {
		return
	}
	//nolint:gosec // G124: Secure follows the request, for the reason overTLS gives.
	http.SetCookie(w, &http.Cookie{
		Name:  listPrefs,
		Value: l.query(),
		Path:  "/",
		// No HttpOnly, and nothing to protect with it: there is no credential
		// in here. Lax for the reason the session cookie has it.
		MaxAge:   int(listPrefsTTL.Seconds()),
		SameSite: http.SameSiteLaxMode,
		Secure:   overTLS(r),
	})
}

// column is one sortable heading of the table, already carrying where it goes
// and how it sits -- no formatting logic in the markup, the same rule entry
// follows.
type column struct {
	Label string
	Href  string
	Class string
	// Arrow is the glyph beside the label, empty on the columns this listing is
	// not ordered by, and Sorted is what aria-sort is told.
	Arrow  string
	Sorted string
}

// rowChoice is one of the page sizes on offer.
type rowChoice struct {
	Label  string
	Href   string
	Active bool
}

// columns is the header: one link per ordering, each saying where clicking it
// goes.
//
// A column that is already the one in use flips its direction. A column that is
// not starts in the direction somebody means by it -- names from A, sizes from
// the largest, dates from the most recent -- because "sort by modified" is
// never a request to see 2019 first.
func columns(here string, l listing, token string) []column {
	labels := []struct{ key, label, class string }{
		{"name", "Name", ""},
		{"size", "Size", "text-end"},
		// The modified column is the one a narrow screen drops, so its heading
		// goes with it.
		{"modified", "Modified", "text-end d-none d-sm-table-cell"},
	}
	out := make([]column, 0, len(labels))
	for _, c := range labels {
		next := listing{Key: c.key, Desc: c.key != "name", Rows: l.Rows}
		col := column{Label: c.label, Class: c.class, Sorted: "none"}
		if c.key == l.Key {
			next.Desc = !l.Desc
			col.Arrow, col.Sorted = "↑", "ascending"
			if l.Desc {
				col.Arrow, col.Sorted = "↓", "descending"
			}
		}
		// No cursor: a listing reordered starts again at its first page, since
		// a position in one ordering is not a position in another.
		col.Href = shared(here+"?"+next.query(), token)
		out = append(out, col)
	}
	return out
}

// rowChoices is the page-size control: three links rather than a select that
// submits itself, which would need a script this page does not have.
func rowChoices(here string, l listing, token string) []rowChoice {
	out := make([]rowChoice, 0, len(listRows))
	for _, n := range listRows {
		out = append(out, rowChoice{
			Label:  strconv.Itoa(n),
			Href:   shared(here+"?"+listing{Key: l.Key, Desc: l.Desc, Rows: n}.query(), token),
			Active: n == l.Rows,
		})
	}
	return out
}
