// Package trash is the trash seen as folders (#323): /trash/holiday/ is the
// tree one deletion took, exactly as it was when it was deleted.
//
// It exists for the reason internal/timeline does: two adapters answer for the
// same names. The collection a client walks and the bytes a browser reads are
// one set of addresses split by method, so the name a PROPFIND lists has to be
// the name the other half resolves -- and an adapter may not import another.
//
// **The page lists deletions and this lists them too**, which is the whole
// shape of it: deleting a folder of a thousand photographs is one accident,
// and a thousand collections at the top would be a worse way to find it again
// than the one it already has. So the top level is a deletion -- a collection
// when a folder was deleted, the file itself when a file was -- and everything
// below it is what was in it, under its own names.
//
// What is **not** here is the other half of the trash page: a blob the sweep
// could not account for (#276) has no path, so there is nothing to call it and
// nothing to put under it. It stays on the page, which can say in a sentence
// what a collection cannot. Nothing filters it out either -- those rows belong
// to no owner and this tree is always somebody's.
//
// Nothing here writes. The rows are a view of what was deleted, the bytes
// belong to the blob that was never moved, and putting something back is the
// page's business.
package trash

import (
	"cmp"
	"context"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/stratus-private-cloud/stratus-backend/internal/db"
)

// batchPage is how many deletions one query reads. The list is read whole, a
// page at a time: a name is unique among all of them or it is not unique.
const batchPage = 500

// Source is what the tree reads: the deletions, and what one holds.
type Source interface {
	Trash(ctx context.Context, owner string, after db.TrashCursor, limit int) ([]db.TrashBatch, error)
	Trashed(ctx context.Context, owner, batch string) ([]db.Trashed, error)
}

// Node is one place in the tree: the mount root, a deletion, or something that
// was inside one.
type Node struct {
	// Name is what this is called inside its parent -- the deletion's
	// generated name at the top, the file's own name below it -- and empty at
	// the mount root.
	Name string
	Dir  bool
	// Row is what was deleted, and the zero value at the mount root, which is
	// the one place here that nothing was deleted at.
	Row db.Trashed
}

// Tree is one owner's trash as folders, for the length of one request.
//
// It caches what it reads, like internal/timeline's, because one request asks
// the same questions several times -- resolving a path and then listing what
// is in it -- and each answer is a query. Listing the top costs one query per
// deletion, because what a deletion is called comes from the batch and whether
// it is a collection comes from the row underneath it; a deletion is an
// accident and there are few.
type Tree struct {
	source Source
	owner  string

	named map[string]db.TrashBatch
	order []string
	rows  map[string]map[string]db.Trashed
}

// New makes the tree over owner's trash.
func New(source Source, owner string) *Tree {
	return &Tree{source: source, owner: owner, rows: map[string]map[string]db.Trashed{}}
}

// Resolve turns a path under the mount into what is at it, or os.ErrNotExist.
func (t *Tree) Resolve(ctx context.Context, name string) (Node, error) {
	name = strings.Trim(path.Clean("/"+name), "/")
	if name == "" {
		return Node{Dir: true}, nil
	}
	first, rest, _ := strings.Cut(name, "/")

	if err := t.list(ctx); err != nil {
		return Node{}, err
	}
	batch, ok := t.named[first]
	if !ok {
		return Node{}, os.ErrNotExist
	}
	rows, err := t.read(ctx, batch.ID)
	if err != nil {
		return Node{}, err
	}

	// The deletion's root is the shortest path in it, which the port already
	// answers with; a row missing under it is one destroyed between the
	// listing and this read, by the button or by the pass that empties the
	// trash.
	want := batch.Root
	named := first
	if rest != "" {
		want += "/" + rest
		named = path.Base(rest)
	}
	row, ok := rows[want]
	if !ok {
		return Node{}, os.ErrNotExist
	}
	return Node{Name: named, Dir: row.IsDir, Row: row}, nil
}

// Children lists what is inside a node: the deletions at the top, and what was
// in one below that.
func (t *Tree) Children(ctx context.Context, n Node) ([]Node, error) {
	if !n.Dir {
		return nil, nil
	}
	if n.Row.Batch == "" {
		return t.deletions(ctx)
	}

	rows, err := t.read(ctx, n.Row.Batch)
	if err != nil {
		return nil, err
	}
	var out []Node
	for p, row := range rows {
		if db.ParentOf(p) != n.Row.Path {
			continue
		}
		out = append(out, Node{Name: path.Base(p), Dir: row.IsDir, Row: row})
	}
	slices.SortFunc(out, func(a, b Node) int { return cmp.Compare(a.Row.Path, b.Row.Path) })
	return out, nil
}

// deletions is the top of the tree, newest first, as the page shows them.
func (t *Tree) deletions(ctx context.Context) ([]Node, error) {
	if err := t.list(ctx); err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(t.order))
	for _, name := range t.order {
		batch := t.named[name]
		rows, err := t.read(ctx, batch.ID)
		if err != nil {
			return nil, err
		}
		root, ok := rows[batch.Root]
		if !ok {
			continue
		}
		out = append(out, Node{Name: name, Dir: root.IsDir, Row: root})
	}
	return out, nil
}

// list reads every deletion and names it, once per request.
func (t *Tree) list(ctx context.Context) error {
	if t.named != nil {
		return nil
	}
	var all []db.TrashBatch
	var after db.TrashCursor
	for {
		page, err := t.source.Trash(ctx, t.owner, after, batchPage)
		if err != nil {
			return err
		}
		all = append(all, page...)
		if len(page) < batchPage {
			break
		}
		last := page[len(page)-1]
		after = db.TrashCursor{DeletedAt: last.DeletedAt, Batch: last.ID}
	}
	t.named, t.order = Names(all)
	return nil
}

// read reads one deletion and maps it by the path each row had, once per
// request.
func (t *Tree) read(ctx context.Context, batch string) (map[string]db.Trashed, error) {
	if rows, ok := t.rows[batch]; ok {
		return rows, nil
	}
	rows, err := t.source.Trashed(ctx, t.owner, batch)
	if err != nil {
		return nil, err
	}
	byPath := make(map[string]db.Trashed, len(rows))
	for _, row := range rows {
		byPath[row.Path] = row
	}
	t.rows[batch] = byPath
	return byPath, nil
}

// Names gives every deletion a name under the mount, and this is the rule
// (#323): **what was deleted**, which is the same name the page shows -- the
// folder for a tree, the file for a file -- so that a deletion is as
// recognisable in Finder as it is on the page.
//
// Two deletions can share one, and often will: deleting "holiday" twice is
// what a trash is for. The older keeps the plain name and the newer is
// "holiday (2)", by db.CopyName, which is the rule the generated mounts and
// the import folder already follow. Oldest first because that is what makes a
// name stable: deleting something today cannot rename what you deleted last
// week under somebody who is looking at it.
//
// The order answered is the page's, newest first; the names are not. Nothing
// below the top is named here -- those are the files' own names, as they were
// deleted, which is what the tree underneath promised to show.
func Names(batches []db.TrashBatch) (map[string]db.TrashBatch, []string) {
	oldest := slices.Clone(batches)
	slices.SortFunc(oldest, func(a, b db.TrashBatch) int {
		return cmp.Or(a.DeletedAt.Compare(b.DeletedAt), cmp.Compare(a.ID, b.ID))
	})

	named := make(map[string]db.TrashBatch, len(oldest))
	taken := make(map[string]bool, len(oldest))
	order := make([]string, 0, len(oldest))
	for _, b := range oldest {
		base := path.Base(b.Root)
		name := base
		for n := 2; taken[strings.ToLower(name)]; n++ {
			name = db.CopyName(base, n)
		}
		taken[strings.ToLower(name)] = true
		named[name] = b
		order = append(order, name)
	}
	slices.Reverse(order)
	return named, order
}
