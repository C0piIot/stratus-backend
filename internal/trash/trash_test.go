package trash_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/stratus-private-cloud/stratus-backend/internal/db"
	"github.com/stratus-private-cloud/stratus-backend/internal/trash"
)

// deletions is the source as a map, for the cases that are about the rule and
// not about the database: the mount's own tests run over a real one.
type deletions struct {
	batches []db.TrashBatch
	rows    map[string][]db.Trashed
	reads   int
}

func (d *deletions) Trash(_ context.Context, _ string, after db.TrashCursor, limit int) ([]db.TrashBatch, error) {
	out := slices.Clone(d.batches)
	if !after.AtStart() {
		i := slices.IndexFunc(out, func(b db.TrashBatch) bool { return b.ID == after.Batch })
		out = out[i+1:]
	}
	return out[:min(len(out), limit)], nil
}

func (d *deletions) Trashed(_ context.Context, _, batch string) ([]db.Trashed, error) {
	d.reads++
	return d.rows[batch], nil
}

func deleted(batch, path string, dir bool) db.Trashed {
	return db.Trashed{Batch: batch, File: db.File{OwnerID: "edu", Path: path, IsDir: dir}}
}

// TestTheOlderDeletionKeepsTheName is the naming rule (#323): what was
// deleted, with db.CopyName behind it and the oldest first, so that today's
// accident cannot rename last week's under somebody reading it.
func TestTheOlderDeletionKeepsTheName(t *testing.T) {
	t.Parallel()
	day := func(n int) time.Time { return time.Date(2026, 10, n, 12, 0, 0, 0, time.UTC) }
	// Newest first, which is the order the port answers in.
	named, order := trash.Names([]db.TrashBatch{
		{ID: "d", Root: "holiday", DeletedAt: day(4)},
		{ID: "c", Root: "Holiday", DeletedAt: day(3)},
		{ID: "b", Root: "sunset.jpg", DeletedAt: day(2)},
		{ID: "a", Root: "album/holiday", DeletedAt: day(1)},
	})

	// The name is the thing's own and not its path: "album/holiday" is
	// "holiday" here, which is what the page calls it too.
	for name, want := range map[string]string{
		"holiday":     "a",
		"Holiday (2)": "c",
		"holiday (3)": "d",
		"sunset.jpg":  "b",
	} {
		if got := named[name]; got.ID != want {
			t.Errorf("%q is deletion %q, want %q", name, got.ID, want)
		}
	}
	// A case-folding client would take "Holiday" for "holiday", and there the
	// second would hide the first -- so it is disambiguated too, in the case
	// it was deleted under.
	if _, ok := named["Holiday"]; ok {
		t.Error("two names that fold to one")
	}
	// Listed newest first, as the page lists them.
	if want := []string{"holiday (3)", "Holiday (2)", "sunset.jpg", "holiday"}; !slices.Equal(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
}

// TestTheTreeIsReadOncePerRequest: resolving a path and then listing it are
// two questions about one deletion, and a value that lives for one request is
// what keeps them one query.
func TestTheTreeIsReadOncePerRequest(t *testing.T) {
	t.Parallel()
	source := &deletions{
		batches: []db.TrashBatch{{ID: "one", Root: "album", DeletedAt: time.Now()}},
		rows: map[string][]db.Trashed{"one": {
			deleted("one", "album", true),
			deleted("one", "album/one.txt", false),
			deleted("one", "album/sub", true),
			deleted("one", "album/sub/two.txt", false),
		}},
	}
	tree := trash.New(source, "edu")

	n, err := tree.Resolve(t.Context(), "/album")
	if err != nil {
		t.Fatal(err)
	}
	children, err := tree.Children(t.Context(), n)
	if err != nil {
		t.Fatal(err)
	}
	// What was in it, under its own names, and not what was under that.
	if got := []string{children[0].Name, children[1].Name}; !slices.Equal(got, []string{"one.txt", "sub"}) {
		t.Errorf("children = %v", got)
	}
	if len(children) != 2 {
		t.Errorf("children = %d, want 2", len(children))
	}
	if source.reads != 1 {
		t.Errorf("the deletion was read %d times in one request", source.reads)
	}
}

// TestADeletionThatIsNotThere: a name nobody deleted, and one destroyed
// between the listing and the read.
func TestADeletionThatIsNotThere(t *testing.T) {
	t.Parallel()
	source := &deletions{
		batches: []db.TrashBatch{{ID: "gone", Root: "album", DeletedAt: time.Now()}},
		rows:    map[string][]db.Trashed{},
	}
	tree := trash.New(source, "edu")

	for _, name := range []string{"album", "album/one.txt", "invented"} {
		if _, err := tree.Resolve(t.Context(), name); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("resolving %q = %v, want not-exist", name, err)
		}
	}
	// And the top of the tree is empty rather than holding a deletion with
	// nothing in it.
	children, err := tree.Children(t.Context(), trash.Node{Dir: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 0 {
		t.Errorf("children = %v, want none", children)
	}
}
