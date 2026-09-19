package dav_test

import (
	"context"
	"encoding/xml"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/dav"
	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/storage/disk"
)

// PROPFIND is answered by a second library now, which is a thing you do not do
// quietly: these hold it to what the first one said, and then to the two
// properties it was swapped for.

// multistatus is enough of RFC 4918's document to compare two of them.
type multistatus struct {
	Responses []struct {
		Href     string `xml:"href"`
		Propstat []struct {
			Status string `xml:"status"`
			Prop   struct {
				Inner []byte `xml:",innerxml"`
			} `xml:"prop"`
		} `xml:"propstat"`
	} `xml:"response"`
}

func parseMultistatus(t *testing.T, body string) multistatus {
	t.Helper()
	var ms multistatus
	if err := xml.Unmarshal([]byte(body), &ms); err != nil {
		t.Fatalf("the answer is not a multistatus: %v\n%s", err, body)
	}
	return ms
}

// propsOf flattens a document into "href -> property -> value", which is what
// two libraries can be compared on: the ordering, the namespace prefixes and
// the whitespace are theirs, and the facts are ours.
func propsOf(t *testing.T, body string) map[string]map[string]string {
	t.Helper()
	out := map[string]map[string]string{}
	for _, resp := range parseMultistatus(t, body).Responses {
		href := strings.TrimSuffix(resp.Href, "/")
		out[href] = map[string]string{}
		for _, ps := range resp.Propstat {
			if !strings.Contains(ps.Status, "200") {
				continue
			}
			for name, value := range elements(string(ps.Prop.Inner)) {
				out[href][name] = value
			}
		}
	}
	return out
}

// elements pulls "local name -> text" out of a prop body, ignoring whichever
// namespace prefix the library felt like using.
func elements(inner string) map[string]string {
	out := map[string]string{}
	dec := xml.NewDecoder(strings.NewReader("<r>" + inner + "</r>"))
	var depth int
	var name string
	var value strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth == 2 {
				name, value = t.Name.Local, strings.Builder{}
			}
		case xml.CharData:
			if depth == 2 {
				value.Write(t)
			}
		case xml.EndElement:
			if depth == 2 {
				out[name] = strings.TrimSpace(value.String())
			}
			depth--
		}
	}
	return out
}

// TestPropfindStillSaysWhatItSaid is the test a library swap deserves: the
// fixtures in testdata were captured from emersion/go-webdav before the change,
// and everything they asserted has to still be true.
//
// Not byte equality -- the new one says more, and orders and prefixes
// differently. What is compared is every property the old one answered, for
// every href it answered about.
func TestPropfindStillSaysWhatItSaid(t *testing.T) {
	t.Parallel()

	for name, req := range map[string]struct{ target, depth, body string }{
		"depth1-allprop": {"/dav/album", "1", ""},
		"depth0-allprop": {"/dav/album", "0", ""},
		"file-allprop":   {"/dav/album/one.txt", "0", ""},
		"named-props": {"/dav/album/one.txt", "0", `<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:"><D:prop>
<D:getcontentlength/><D:getetag/><D:resourcetype/><D:displayname/>
</D:prop></D:propfind>`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := baselineTree(t)

			was, err := os.ReadFile(filepath.Join("testdata", name+".xml"))
			if err != nil {
				t.Fatal(err)
			}
			before := propsOf(t, string(was))
			after := propsOf(t, do(t, h, "PROPFIND", req.target, req.body,
				"Depth", req.depth, "Content-Type", "application/xml").Body.String())

			for href, props := range before {
				got, ok := after[href]
				if !ok {
					t.Errorf("%s is no longer in the answer", href)
					continue
				}
				for prop, want := range props {
					// The fixture's own times and validators move with the
					// tree, so what is checked is that they are still there.
					// Whether the validator is the *right* one is the test
					// below, and skipping that here is how a synthesised ETag
					// got past this once already.
					if prop == "getlastmodified" || prop == "getetag" {
						if _, there := got[prop]; !there {
							t.Errorf("%s lost %s", href, prop)
						}
						continue
					}
					if got[prop] != want {
						t.Errorf("%s %s = %q, was %q", href, prop, got[prop], want)
					}
				}
			}
		})
	}
}

// baselineTree is the tree the fixtures were captured over, and the one every
// comparison above is made against: a folder, a file, a subfolder and a name
// that needs escaping.
//
// The fixtures in testdata are emersion/go-webdav's answers, taken before the
// swap. They are a historical record and are not regenerated -- doing that from
// the library that replaced it would make the comparison compare nothing.
func baselineTree(t *testing.T) http.Handler {
	t.Helper()
	h := server(t)
	do(t, h, "MKCOL", "/dav/album", "")
	do(t, h, "MKCOL", "/dav/album/raw", "")
	do(t, h, http.MethodPut, "/dav/album/one.txt", "one")
	do(t, h, http.MethodPut, "/dav/album/a%20b%23c%20caf%C3%A9.txt", "escaped")
	return h
}

// TestPropfindDoesNotAskOncePerChild is the regression this design exists to
// avoid, and the reason readOnlyFS is built per request.
//
// x/net walks a directory, then opens every resource again to read its
// properties -- throwing away the os.FileInfo the walk already had. Without
// somewhere to keep what the listing returned, a folder of fifty children would
// be fifty-one lookups where the old library made two: the N+1 that #160 took
// out of this very surface.
func TestPropfindDoesNotAskOncePerChild(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	blobs, err := disk.New(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blobs.Close() })
	meta, err := sqlite.New(t.Context(), filepath.Join(dir, "stratus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	if merr := meta.Migrate(t.Context()); merr != nil {
		t.Fatal(merr)
	}

	counted := &countingStore{Store: meta}
	h := withUser(dav.Handler(prefix, files.New(blobs, counted)), "edu")

	do(t, h, "MKCOL", "/dav/album", "")
	const children = 50
	for i := range children {
		do(t, h, http.MethodPut, "/dav/album/file-"+strconv.Itoa(i)+".txt", "x")
	}

	counted.lookups = 0
	if rec := do(t, h, "PROPFIND", "/dav/album", "", "Depth", "1",
		"Content-Type", "application/xml"); rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND = %d", rec.Code)
	}

	// A handful for the collection itself, and none per child. The exact number
	// is the library's business; that it does not grow with the folder is ours.
	if counted.lookups > 4 {
		t.Errorf("a listing of %d children cost %d lookups: the cache is not being used",
			children, counted.lookups)
	}
}

// countingStore counts the one call that would grow with the folder.
type countingStore struct {
	db.Store
	lookups int
}

func (c *countingStore) FileByPath(ctx context.Context, owner, path string) (db.File, error) {
	c.lookups++
	return c.Store.FileByPath(ctx, owner, path)
}

// TestPropfindAgreesWithGetAboutTheETag is the one the fixture comparison
// cannot make, because a fixture's validators are its own.
//
// x/net computes an ETag from the modification time and the size unless the
// os.FileInfo says otherwise, so without rowInfo.ETag the same file has two
// validators -- one in a listing and another on a GET -- and a client that
// compared them would decide it had changed underneath. It shipped that way for
// a day and the app's conformance suite is what found it.
func TestPropfindAgreesWithGetAboutTheETag(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, http.MethodPut, "/dav/notes.txt", "notes")

	listed := propsOf(t, do(t, h, "PROPFIND", "/dav/notes.txt", "", "Depth", "0",
		"Content-Type", "application/xml").Body.String())["/dav/notes.txt"]["getetag"]
	served := do(t, h, http.MethodGet, "/dav/notes.txt", "").Header().Get("ETag")

	if listed == "" || served == "" {
		t.Fatalf("a validator went missing: listed %q, served %q", listed, served)
	}
	if listed != served {
		t.Errorf("PROPFIND says %s and GET says %s", listed, served)
	}
	// And it is the digest internal/files computed, not a time and a size.
	if len(listed) != 66 {
		t.Errorf("the validator is %s, which is not a SHA-256 in quotes", listed)
	}
}

// TestPropfindAnswersHasPreview is #136: a client drawing a grid asks once, in
// the listing it was making anyway, instead of asking for every picture and
// counting the ones that are not there.
func TestPropfindAnswersHasPreview(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, "MKCOL", "/dav/album", "")
	do(t, h, http.MethodPut, "/dav/album/photo.jpg", "not really a photograph")
	do(t, h, http.MethodPut, "/dav/album/notes.txt", "nothing to draw")

	props := propsOf(t, do(t, h, "PROPFIND", "/dav/album", "", "Depth", "1",
		"Content-Type", "application/xml").Body.String())

	if got := props["/dav/album/photo.jpg"]["has-preview"]; got != "true" {
		t.Errorf("a photograph = %q, want true", got)
	}
	if got := props["/dav/album/notes.txt"]["has-preview"]; got != "false" {
		t.Errorf("a text file = %q, want false", got)
	}
	// Not on a collection: there is no picture of a folder, and a property that
	// answered anyway would be a third state a client has to interpret.
	if _, there := props["/dav/album"]["has-preview"]; there {
		t.Error("a collection claims to have a preview")
	}
}

// TestPropfindAnswersQuota is RFC 4331, and the reason it is worth having is
// that it is not ours: Finder draws its bar from these and rclone about reads
// them.
func TestPropfindAnswersQuota(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, "MKCOL", "/dav/album", "")
	do(t, h, http.MethodPut, "/dav/album/one.txt", "twelve bytes")
	do(t, h, http.MethodPut, "/dav/outside.txt", "not under the album")

	props := propsOf(t, do(t, h, "PROPFIND", "/dav/album", "", "Depth", "0",
		"Content-Type", "application/xml").Body.String())["/dav/album"]

	// Used is what is under this collection and nothing else, which is the half
	// of RFC 4331 that a prefix match gets wrong.
	if got := props["quota-used-bytes"]; got != "12" {
		t.Errorf("quota-used-bytes = %q, want the twelve bytes under it", got)
	}
	free, err := strconv.ParseInt(props["quota-available-bytes"], 10, 64)
	if err != nil || free <= 0 {
		t.Errorf("quota-available-bytes = %q", props["quota-available-bytes"])
	}
	// And they are collection properties: a file has neither.
	file := propsOf(t, do(t, h, "PROPFIND", "/dav/album/one.txt", "", "Depth", "0",
		"Content-Type", "application/xml").Body.String())["/dav/album/one.txt"]
	if _, there := file["quota-used-bytes"]; there {
		t.Error("a file answers a collection's quota")
	}
}

// TestPropfindPropname lists the names, ours included, which is how a client
// discovers there is something extra to ask for.
func TestPropfindPropname(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, http.MethodPut, "/dav/photo.jpg", "not really a photograph")

	body := do(t, h, "PROPFIND", "/dav/photo.jpg", `<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:"><D:propname/></D:propfind>`,
		"Depth", "0", "Content-Type", "application/xml").Body.String()

	for _, want := range []string{"getcontentlength", "getetag", "resourcetype", "has-preview"} {
		if !strings.Contains(body, want) {
			t.Errorf("propname does not mention %s: %s", want, body)
		}
	}
}

// TestPropfindOfSomethingMissing: a property nobody has comes back in a 404
// propstat rather than being left out, which is what tells a client the
// difference between "no" and "I did not understand the question".
func TestPropfindOfSomethingMissing(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, http.MethodPut, "/dav/notes.txt", "notes")

	body := do(t, h, "PROPFIND", "/dav/notes.txt", `<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:"><D:prop><D:getetag/><D:nonesuch/></D:prop></D:propfind>`,
		"Depth", "0", "Content-Type", "application/xml").Body.String()

	if !strings.Contains(body, "nonesuch") || !strings.Contains(body, "404") {
		t.Errorf("a property nobody has was left out instead of refused: %s", body)
	}
}
