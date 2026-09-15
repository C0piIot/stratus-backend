package files

import (
	"strings"
	"testing"
	"time"
)

// The layout is for a person with a lost database, so what these cases pin is
// what that person sees: a kind they can recognise, a date they can navigate,
// and a name their file manager will open.
func TestBlobKeyLayout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		wantKind string
		wantExt  string
	}{
		{name: "holiday/IMG_0001.jpg", wantKind: "image", wantExt: "jpg"},
		{name: "IMG_0002.HEIC", wantKind: "image", wantExt: "heic"},
		{name: "music/track.flac", wantKind: "audio", wantExt: "flac"},
		{name: "clip.MOV", wantKind: "video", wantExt: "mov"},
		{name: "taxes/2024.pdf", wantKind: "document", wantExt: "pdf"},
		// Nothing to inherit from, which is what other/ is for.
		{name: "backup", wantKind: "other"},
		{name: "archive.tar.zst", wantKind: "other", wantExt: "zst"},
		// A directory with a dot in it is not an extension: path.Ext looks at
		// the last element only.
		{name: "holiday.2024/notes", wantKind: "other"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			key := newBlobKey(tt.name)

			segs := strings.Split(key, "/")
			if len(segs) != 5 {
				t.Fatalf("newBlobKey(%q) = %q, want five segments", tt.name, key)
			}
			if segs[0] != tt.wantKind {
				t.Errorf("kind = %q, want %q", segs[0], tt.wantKind)
			}

			// The date is today's, read back rather than string-compared, so a
			// run that crosses midnight does not fail the build.
			day, err := time.Parse("2006/01/02", strings.Join(segs[1:4], "/"))
			if err != nil {
				t.Fatalf("the date segments of %q do not parse: %v", key, err)
			}
			if since := time.Since(day); since < 0 || since > 48*time.Hour {
				t.Errorf("date = %q, which is not today", day.Format("2006/01/02"))
			}

			id, ext, _ := strings.Cut(segs[4], ".")
			if ext != tt.wantExt {
				t.Errorf("extension = %q, want %q", ext, tt.wantExt)
			}
			if id == "" {
				t.Error("the leaf carries no id")
			}
		})
	}
}

// Two keys differing only in case are one file on APFS, exFAT and a Windows
// share -- all of them plausible homes for STRATUS_DATA_PATH -- so the disk
// backend would let the second Put overwrite the first while S3 held two
// objects. ValidateKey cannot reject the pair, since it sees one key at a time,
// so the property holds by construction: every part of a key a caller can
// influence is lowercased, and the part it cannot is RFC 4648 base32, which has
// no lowercase in it.
func TestBlobKeysCannotDifferOnlyInCase(t *testing.T) {
	t.Parallel()

	// The same file twice, named as two operating systems would name it.
	upper := newBlobKey("Photo.JPG")
	lower := newBlobKey("photo.jpg")
	for _, key := range []string{upper, lower} {
		if !strings.HasSuffix(key, ".jpg") {
			t.Errorf("newBlobKey = %q, want a lowercased extension", key)
		}
	}

	for range 100 {
		key := newBlobKey("photo.jpg")
		segs := strings.Split(key, "/")
		for _, seg := range segs[:4] {
			if strings.ToLower(seg) != seg {
				t.Fatalf("segment %q of %q is not lowercase", seg, key)
			}
		}
		id, _, _ := strings.Cut(segs[4], ".")
		if strings.ToUpper(id) != id {
			t.Fatalf("the id in %q is not single-case, so two keys can differ only in case", key)
		}
	}
}

func TestExtensionOf(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"photo.jpg":   "jpg",
		"photo.JPG":   "jpg",
		"photo.jpeg":  "jpeg",
		"noextension": "",
		"trailing.":   "",
		// Long enough to be a word rather than an extension.
		"backup.superlongext": "",
		// Not an extension, whatever it looks like: a version, a date, a name
		// with punctuation in it.
		"stratus.v1-2":     "",
		"dump.2026-09-15":  "",
		"photo.jpg ":       "",
		"presentación.año": "",
		// The last element is the only one that counts.
		"holiday.2024/notes": "",
		"a.b/c.png":          "png",
	}
	for name, want := range tests {
		if got := extensionOf(name); got != want {
			t.Errorf("extensionOf(%q) = %q, want %q", name, got, want)
		}
	}
}
