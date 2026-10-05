package timeline_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/timeline"
)

// index is the date index, with a count of what was asked of it: the tree
// answers the same question several times per request and must not ask twice.
type index struct {
	photos  []db.Photo
	months  int
	queries int
	fail    string
}

func (i *index) PhotoMonths(context.Context, string, db.Kind) ([]db.PhotoMonth, error) {
	i.months++
	if i.fail == "PhotoMonths" {
		return nil, errors.New("the index is on fire")
	}
	var out []db.PhotoMonth
	for _, p := range i.photos {
		if m := db.MonthOf(p.SortAt); !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	slices.SortFunc(out, func(a, b db.PhotoMonth) int {
		if a.Year != b.Year {
			return b.Year - a.Year
		}
		return int(b.Month) - int(a.Month)
	})
	return out, nil
}

func (i *index) PhotoTimeline(_ context.Context, _ string, f db.PhotoFilter) ([]db.Photo, error) {
	i.queries++
	if i.fail == "PhotoTimeline" {
		return nil, errors.New("the index is on fire")
	}
	var out []db.Photo
	for _, p := range i.photos {
		if !f.From.IsZero() && p.SortAt.Before(f.From) {
			continue
		}
		if !f.To.IsZero() && !p.SortAt.Before(f.To) {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

var june = time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)

// shot is one indexed photograph: an id, a path in the tree and when the
// camera says it was taken.
func shot(id int64, path string, at time.Time) db.Photo {
	p := db.Photo{SortAt: at}
	p.File = db.File{ID: id, Path: path, Size: 10}
	return p
}

func tree(t *testing.T, shots ...db.Photo) (*timeline.Tree, *index) {
	t.Helper()
	src := &index{photos: shots}
	return timeline.New(src, "edu", db.KindImage), src
}

func TestTheTreeIsYearsThenMonthsThenPhotographs(t *testing.T) {
	t.Parallel()
	tr, _ := tree(t,
		shot(1, "Camera/IMG_0001.JPG", june),
		shot(2, "Holiday/beach.heic", june.AddDate(0, 0, 3)),
		shot(3, "old.jpg", time.Date(2019, 1, 2, 0, 0, 0, 0, time.UTC)))

	years, err := tr.Children(t.Context(), timeline.Node{Dir: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(years); fmt.Sprint(got) != "[2024 2019]" {
		t.Errorf("years = %v", got)
	}

	months, err := tr.Children(t.Context(), timeline.Node{Year: 2024, Dir: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(months); fmt.Sprint(got) != "[06]" {
		t.Errorf("months = %v", got)
	}

	shots, err := tr.Children(t.Context(), timeline.Node{Year: 2024, Month: time.June, Dir: true})
	if err != nil {
		t.Fatal(err)
	}
	got := names(shots)
	slices.Sort(got)
	if fmt.Sprint(got) != "[IMG_0001.JPG beach.heic]" {
		t.Errorf("a month holds %v", got)
	}
}

func TestResolveAnswersWhatIsThereAndNothingElse(t *testing.T) {
	t.Parallel()
	tr, _ := tree(t, shot(1, "Camera/a.jpg", june))

	for _, at := range []string{"", "2024", "2024/06", "2024/06/a.jpg"} {
		if _, err := tr.Resolve(t.Context(), at); err != nil {
			t.Errorf("resolve %q: %v", at, err)
		}
	}
	for _, at := range []string{
		"1999", "2024/05", "2024/6", "2024/13", "abc", "02024",
		"2024/06/b.jpg", "2024/06/a.jpg/x",
	} {
		if _, err := tr.Resolve(t.Context(), at); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("resolve %q = %v, want not-exist", at, err)
		}
	}
}

// TestNamesInAMonthCannotCollide: two cameras both count from IMG_0001, and a
// case-folding client would take these for one file.
func TestNamesInAMonthCannotCollide(t *testing.T) {
	t.Parallel()
	named := timeline.Names([]db.Photo{
		shot(3, "C/img_0001.jpg", june),
		shot(1, "A/IMG_0001.JPG", june),
		shot(2, "B/IMG_0001.JPG", june),
	})

	for name, want := range map[string]int64{
		"IMG_0001.JPG":     1,
		"IMG_0001 (2).JPG": 2,
		"img_0001 (3).jpg": 3,
	} {
		if got, ok := named[name]; !ok || got.File.ID != want {
			t.Errorf("%q = %+v, want the photo with id %d", name, got.File.ID, want)
		}
	}
	if len(named) != 3 {
		t.Errorf("three photographs got %d names", len(named))
	}
}

// TestPathOfIsTheNameTheCollectionAnswersTo, which is what keeps a link on a
// page and the mount behind it in step.
func TestPathOfIsTheNameTheCollectionAnswersTo(t *testing.T) {
	t.Parallel()
	second := shot(2, "B/IMG_0001.JPG", june)
	tr, _ := tree(t, shot(1, "A/IMG_0001.JPG", june), second)

	at, err := tr.PathOf(t.Context(), second)
	if err != nil {
		t.Fatal(err)
	}
	if at != "2024/06/IMG_0001 (2).JPG" {
		t.Errorf("PathOf = %q", at)
	}
	n, err := tr.Resolve(t.Context(), at)
	if err != nil || n.Photo.File.ID != 2 {
		t.Errorf("resolving what PathOf gave back = %+v, %v", n.Photo.File.ID, err)
	}

	// One that is not in the index has no address, and says so rather than
	// making one up.
	if _, err := tr.PathOf(t.Context(), shot(99, "ghost.jpg", june)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("PathOf for a photograph that is not there = %v", err)
	}
}

// TestAMonthIsReadOnce: a page of a hundred tiles asks for each one's address,
// and that has to cost one query for the month rather than a hundred.
func TestAMonthIsReadOnce(t *testing.T) {
	t.Parallel()
	var shots []db.Photo
	for i := range 50 {
		shots = append(shots, shot(int64(i+1), fmt.Sprintf("p%02d.jpg", i), june))
	}
	tr, src := tree(t, shots...)

	for _, p := range shots {
		if _, err := tr.PathOf(t.Context(), p); err != nil {
			t.Fatal(err)
		}
	}
	if src.queries != 1 {
		t.Errorf("naming fifty photographs took %d queries, want 1", src.queries)
	}
	if _, err := tr.Children(t.Context(), timeline.Node{Dir: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Months(t.Context()); err != nil {
		t.Fatal(err)
	}
	if src.months != 1 {
		t.Errorf("the months were read %d times", src.months)
	}
}

func TestABrokenIndexIsAnError(t *testing.T) {
	t.Parallel()
	for _, call := range []string{"PhotoMonths", "PhotoTimeline"} {
		src := &index{photos: []db.Photo{shot(1, "a.jpg", june)}, fail: call}
		tr := timeline.New(src, "edu", db.KindImage)
		if _, err := tr.Resolve(t.Context(), "2024/06/a.jpg"); err == nil || errors.Is(err, os.ErrNotExist) {
			t.Errorf("resolve with %s broken = %v, want a real error", call, err)
		}
	}
}

func names(nodes []timeline.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Base())
	}
	return out
}
