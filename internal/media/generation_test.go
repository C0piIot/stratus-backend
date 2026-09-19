package media

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// TestTheGeneratorOutputHasNotMoved is the mirror that makes
// files.DerivedGeneration work.
//
// A derived key carries the generation, so raising it is what replaces every
// picture an older generator made -- and nothing can notice on its own that the
// output moved, because it is pixels and a change there is a better picture
// rather than a failure. So this holds the two together the way byExtension is
// held to the demuxers in the recipe: by failing on the day the thing changes,
// rather than a release later when somebody notices their photographs are
// sideways.
//
// When it fails, one of two things happened.
//
//   - We changed the generator. Raise files.DerivedGeneration and put the new
//     digest here: every thumbnail already made will be regenerated as it is
//     asked for, and the old ones swept.
//   - A dependency changed it under us -- the JPEG encoder in the standard
//     library, the scaler in x/image. Then it is a decision: the pictures did
//     move, and whether that is worth regenerating a library for is a judgement
//     nobody but a person can make. Update only the digest to say it is not.
//
// A digest and not a golden file, because what matters is that it moved and not
// what it moved to.
func TestTheGeneratorOutputHasNotMoved(t *testing.T) {
	t.Parallel()

	// A photograph stored on its side with the tag that says so, so the digest
	// covers the rotation as well as the scaling and the encoding -- which is
	// most of what #148 moved.
	photo := exifJPEGWithPixels(t, 6, 40, 20)

	made, err := reduceTo(bytes.NewReader(photo), "IMG_0001.jpg", thumbSmall)
	if err != nil {
		t.Fatalf("reduceTo: %v", err)
	}

	const want = "bf8adeaf71b7a1e55d1b4d3ab9714e820e8bf5b0680541215f0b74739b63207c"
	sum := sha256.Sum256(made)
	if got := hex.EncodeToString(sum[:]); got != want {
		t.Errorf("the generator now produces %s, and this test expects %s.\n"+
			"If we changed it, raise files.DerivedGeneration and put the new digest here.\n"+
			"If a dependency changed it, decide whether every existing thumbnail should be\n"+
			"remade -- and if not, update only the digest.", got, want)
	}
}
