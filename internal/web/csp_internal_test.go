package web

import (
	"io/fs"
	"strings"
	"testing"
)

// TestNoTemplateWritesAnInlineStyle is the guard this file exists for.
//
// The policy is style-src 'self' with no 'unsafe-inline', so a browser drops
// every style attribute the templates send -- and says nothing the server can
// see. That is the same failure as the connect-src one recorded in CLAUDE.md,
// and it shipped: the status page set its progress bar's width that way, so a
// fully indexed library rendered as "100%" painted in a sliver, and the box
// holding a thumbnail's place was not held open at all.
//
// A grep in a test rather than a rule in a linter, because what makes it true
// is a directive fifty lines away in this same package.
func TestNoTemplateWritesAnInlineStyle(t *testing.T) {
	t.Parallel()

	if strings.Contains(contentSecurityPolicy, "'unsafe-inline'") {
		t.Skip("the policy allows inline styles now, so this no longer holds")
	}

	err := fs.WalkDir(templateFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, rerr := fs.ReadFile(templateFS, path)
		if rerr != nil {
			return rerr
		}
		for i, line := range strings.Split(string(body), "\n") {
			// Not "style=" alone: the comments in these files talk about the
			// attribute, and a rule that could not be explained in place would
			// be worse than the bug.
			if strings.Contains(line, `style="`) {
				t.Errorf("%s:%d writes an inline style, which the policy drops: %s",
					path, i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
