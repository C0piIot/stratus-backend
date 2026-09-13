package files

import (
	"strings"
	"testing"
)

// DerivedKey and parentOf have to stay inverses of each other, and nothing
// catches it when they stop: a key the sweep reads back wrong is not a failed
// request, it is an object attributed to a parent no row holds and deleted an
// hour later. Asserted here because parentOf is unexported.
func TestDerivedKeyRoundTrip(t *testing.T) {
	t.Parallel()

	for _, parent := range []string{
		"blobs/ab/cd/efgh",
		// A key this project did not generate. Adopting a Nextcloud bucket in
		// place means the parent comes from somewhere else entirely, so the
		// pair may not assume our own shape.
		"urn:oid:123",
	} {
		key := DerivedKey(parent, "300.jpg")
		got, derived := parentOf(key)
		if !derived || got != parent {
			t.Errorf("parentOf(%q) = %q, %v, want %q, true", key, got, derived, parent)
		}
	}
}

// The name that would have broken it is a transcoded segment, which is the next
// thing to want a derived key.
func TestDerivedKeyRefusesANestedName(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("DerivedKey accepted a name with a slash in it")
		}
	}()
	DerivedKey("blobs/ab/cd/efgh", "hls/seg001.ts")
}

// Two keys differing only in case are one file on APFS, exFAT and a Windows
// share -- all of them plausible homes for STRATUS_DATA_PATH -- so the disk
// backend would let the second Put overwrite the first while S3 held two
// objects. ValidateKey cannot reject the pair, since it sees one key at a time,
// so the property holds by construction instead: rand.Text is RFC 4648 base32,
// which has no lowercase in it. A generator returning hex or base64 would keep
// passing every other test in this package and quietly reintroduce it.
func TestBlobKeysCannotDifferOnlyInCase(t *testing.T) {
	t.Parallel()

	for range 100 {
		key := newBlobKey()
		rest, ok := strings.CutPrefix(key, "blobs/")
		if !ok {
			t.Fatalf("newBlobKey = %q, want the blobs/ prefix", key)
		}
		if strings.ToUpper(rest) != rest {
			t.Fatalf("newBlobKey = %q: the generated part is not single-case, so two keys can differ only in case", key)
		}
	}
}
