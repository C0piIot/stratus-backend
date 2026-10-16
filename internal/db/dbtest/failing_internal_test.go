package dbtest

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// repoType is the port every case is written against.
var repoType = reflect.TypeOf((*db.Repo)(nil)).Elem()

// TestFailableMethodsAreRepoMethods is the guard the list did not have (#364):
// FailOn refuses a name that is not in repoMethods, and nothing refused a name
// in repoMethods that the port no longer has -- a renamed method would leave a
// string that FailOn accepts and that never fires.
func TestFailableMethodsAreRepoMethods(t *testing.T) {
	t.Parallel()
	for _, name := range repoMethods {
		if _, ok := repoType.MethodByName(name); !ok {
			t.Errorf("repoMethods names %q, which db.Repo does not have", name)
		}
	}
}

// TestEveryRepoMethodHasACase is what makes "adding a port method forces a
// case" a failing test rather than a habit: every method of db.Repo is called
// by name somewhere in this package's cases. A text check, since a method a
// case calls is a method the suite exercises on every engine.
func TestEveryRepoMethodHasACase(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var cases strings.Builder
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "failing.go" {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		cases.Write(body)
	}
	text := cases.String()
	for i := range repoType.NumMethod() {
		name := repoType.Method(i).Name
		if !strings.Contains(text, "."+name+"(") {
			t.Errorf("no case in dbtest calls %s", name)
		}
	}
}
