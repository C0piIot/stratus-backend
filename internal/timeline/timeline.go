// Package timeline is the library seen as folders by date: /2024/06/ holds
// everything of one kind the camera says was made in June 2024, wherever it
// was filed.
//
// **One kind at a time, and the kind is the caller's.** The photographs are a
// tree and the videos are another (#215), identical in everything but what
// they hold: the same ordering column, the same month seek, the same generated
// names. One package rather than two, for the reason the port underneath it is
// one.
//
// It exists because two adapters generate the same names. The WebDAV mount and
// the browser pages are the same tree at the same addresses (#279), so a link
// on a page and a PROPFIND of the collection behind it have to agree about
// what a file is called -- and an adapter may not import another, so the
// agreement cannot live in either. This is the case CLAUDE.md's own restraint
// note asks for before a package is made: two handlers that would otherwise
// duplicate something load-bearing.
//
// Nothing here writes. The tree is a view of the index, the names are
// generated, and the bytes belong to the file already.
package timeline

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// monthBatch is how many photos one query reads while listing a month. A month
// is listed whole, so this only bounds a single round trip.
const monthBatch = 1000

// Source is what the tree reads: the index by date, and nothing else.
type Source interface {
	PhotoMonths(ctx context.Context, owner string, kind db.Kind) ([]db.PhotoMonth, error)
	PhotoTimeline(ctx context.Context, owner string, f db.PhotoFilter) ([]db.Photo, error)
}

// Node is one place in the tree: the root, a year, a month or a photograph.
type Node struct {
	Year  int
	Month time.Month
	Name  string
	Dir   bool
	Photo db.Photo
}

// Base is what this node is called inside its parent.
func (n Node) Base() string {
	switch {
	case n.Name != "":
		return n.Name
	case n.Month != 0:
		return fmt.Sprintf("%02d", n.Month)
	case n.Year != 0:
		return strconv.Itoa(n.Year)
	}
	return ""
}

// Path is the node's address under the mount, with no leading slash.
func (n Node) Path() string {
	switch {
	case n.Year == 0:
		return ""
	case n.Month == 0:
		return strconv.Itoa(n.Year)
	case n.Name == "":
		return fmt.Sprintf("%04d/%02d", n.Year, n.Month)
	}
	return fmt.Sprintf("%04d/%02d/%s", n.Year, n.Month, n.Name)
}

// Tree is one owner's files of one kind by date, for the length of one
// request.
//
// It caches what it reads because a single request asks the same questions
// several times -- resolving a path and then listing what is in it, or naming
// a photograph and then the two either side of it -- and each answer is a
// query. A value per request rather than a cache with a lifetime: the index
// changes under it, and a listing that is consistent with itself is worth more
// than one that is fresh halfway down.
type Tree struct {
	source Source
	owner  string
	kind   db.Kind

	months []db.PhotoMonth
	named  map[db.PhotoMonth]map[string]db.Photo
}

// New makes the tree over one kind for owner.
func New(source Source, owner string, kind db.Kind) *Tree {
	return &Tree{source: source, owner: owner, kind: kind, named: map[db.PhotoMonth]map[string]db.Photo{}}
}

// Resolve turns a path under the mount into what is at it, or os.ErrNotExist.
func (t *Tree) Resolve(ctx context.Context, name string) (Node, error) {
	parts := strings.Split(strings.Trim(name, "/"), "/")
	if parts[0] == "" {
		return Node{Dir: true}, nil
	}
	if len(parts) > 3 {
		return Node{}, os.ErrNotExist
	}

	year, err := strconv.Atoi(parts[0])
	if err != nil || parts[0] != strconv.Itoa(year) {
		return Node{}, os.ErrNotExist
	}
	months, err := t.Months(ctx)
	if err != nil {
		return Node{}, err
	}
	if !slices.ContainsFunc(months, func(m db.PhotoMonth) bool { return m.Year == year }) {
		return Node{}, os.ErrNotExist
	}
	if len(parts) == 1 {
		return Node{Year: year, Dir: true}, nil
	}

	mo, err := strconv.Atoi(parts[1])
	if err != nil || parts[1] != fmt.Sprintf("%02d", mo) {
		return Node{}, os.ErrNotExist
	}
	month := db.PhotoMonth{Year: year, Month: time.Month(mo)}
	if !slices.Contains(months, month) {
		return Node{}, os.ErrNotExist
	}
	if len(parts) == 2 {
		return Node{Year: year, Month: month.Month, Dir: true}, nil
	}

	named, err := t.Month(ctx, month)
	if err != nil {
		return Node{}, err
	}
	p, ok := named[parts[2]]
	if !ok {
		return Node{}, os.ErrNotExist
	}
	return Node{Year: year, Month: month.Month, Name: parts[2], Photo: p}, nil
}

// Children lists what is inside a node: years, months, or a month's photographs.
func (t *Tree) Children(ctx context.Context, n Node) ([]Node, error) {
	months, err := t.Months(ctx)
	if err != nil {
		return nil, err
	}
	var out []Node
	switch {
	case n.Year == 0:
		seen := map[int]bool{}
		for _, m := range months {
			if !seen[m.Year] {
				seen[m.Year] = true
				out = append(out, Node{Year: m.Year, Dir: true})
			}
		}
	case n.Month == 0:
		for _, m := range months {
			if m.Year == n.Year {
				out = append(out, Node{Year: m.Year, Month: m.Month, Dir: true})
			}
		}
	default:
		named, err := t.Month(ctx, db.PhotoMonth{Year: n.Year, Month: n.Month})
		if err != nil {
			return nil, err
		}
		for name, p := range named {
			out = append(out, Node{Year: n.Year, Month: n.Month, Name: name, Photo: p})
		}
	}
	return out, nil
}

// PathOf is where a photograph lives in this tree.
//
// It is the function that keeps a link and a mount in step: a page that knows
// a photograph and wants its address asks here rather than building one, and
// what it gets back is the name the collection will answer to -- which means
// reading the month, because two photographs in one can share a filename.
func (t *Tree) PathOf(ctx context.Context, p db.Photo) (string, error) {
	month := db.MonthOf(p.SortAt)
	named, err := t.Month(ctx, month)
	if err != nil {
		return "", err
	}
	for name, candidate := range named {
		if candidate.File.ID == p.File.ID {
			return Node{Year: month.Year, Month: month.Month, Name: name}.Path(), nil
		}
	}
	return "", os.ErrNotExist
}

// Months is every month with a photograph in it, newest first.
func (t *Tree) Months(ctx context.Context) ([]db.PhotoMonth, error) {
	if t.months != nil {
		return t.months, nil
	}
	months, err := t.source.PhotoMonths(ctx, t.owner, t.kind)
	if err != nil {
		return nil, err
	}
	t.months = append([]db.PhotoMonth{}, months...)
	return t.months, nil
}

// Month reads one month whole and names its photographs.
func (t *Tree) Month(ctx context.Context, m db.PhotoMonth) (map[string]db.Photo, error) {
	if named, ok := t.named[m]; ok {
		return named, nil
	}
	var all []db.Photo
	f := db.PhotoFilter{Kind: t.kind, From: m.Start(), To: m.End(), Limit: monthBatch}
	for {
		page, err := t.source.PhotoTimeline(ctx, t.owner, f)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < monthBatch {
			break
		}
		f.After = page[len(page)-1].Cursor()
	}
	named := Names(all)
	t.named[m] = named
	return named, nil
}

// Names names a month's photographs after their files, in id order so that a
// name is stable, and without two that a case-folding client would take for
// one.
func Names(photos []db.Photo) map[string]db.Photo {
	byID := slices.Clone(photos)
	slices.SortFunc(byID, func(a, b db.Photo) int { return cmp.Compare(a.File.ID, b.File.ID) })

	named := make(map[string]db.Photo, len(byID))
	taken := make(map[string]bool, len(byID))
	for _, p := range byID {
		base := path.Base(p.File.Path)
		name := base
		for n := 2; taken[strings.ToLower(name)]; n++ {
			name = db.CopyName(base, n)
		}
		taken[strings.ToLower(name)] = true
		named[name] = p
	}
	return named
}
