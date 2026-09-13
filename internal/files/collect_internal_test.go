package files

import "testing"

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
