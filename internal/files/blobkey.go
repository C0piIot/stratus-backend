package files

import (
	"crypto/rand"
	"fmt"
	"path"
	"strings"
	"time"
)

// The kinds a blob key can start with. They deliberately repeat three of
// db.Kind's names without depending on them: that vocabulary is what an
// extractor decided a file *is*, decided by reading it, and this one is a guess
// made from a filename before the bytes have been seen. Tying them together
// would make a change to either a change to both, for two things that are only
// sometimes the same answer.
const (
	kindImage    = "image"
	kindVideo    = "video"
	kindAudio    = "audio"
	kindDocument = "document"
	kindOther    = "other"
)

// maxExtLen is generous for a real extension and short enough that a filename
// ending in something that merely looks like one -- a version, a timestamp --
// does not become a directory of its own.
const maxExtLen = 8

// byExtension is short on purpose. It is the list somebody recovering a library
// would thank us for, not a MIME registry: everything it does not know is
// kindOther, which is a place to be rather than a failure.
var byExtension = map[string]string{
	"jpg": kindImage, "jpeg": kindImage, "png": kindImage, "gif": kindImage,
	"webp": kindImage, "heic": kindImage, "heif": kindImage, "avif": kindImage,
	"tif": kindImage, "tiff": kindImage, "bmp": kindImage, "svg": kindImage,
	"dng": kindImage, "cr2": kindImage, "nef": kindImage, "arw": kindImage,
	"raf": kindImage, "orf": kindImage, "rw2": kindImage,

	"mp4": kindVideo, "mov": kindVideo, "m4v": kindVideo, "mkv": kindVideo,
	"webm": kindVideo, "avi": kindVideo, "mpg": kindVideo, "mpeg": kindVideo,
	"3gp": kindVideo, "wmv": kindVideo, "flv": kindVideo, "ts": kindVideo,

	"mp3": kindAudio, "flac": kindAudio, "m4a": kindAudio, "aac": kindAudio,
	"ogg": kindAudio, "oga": kindAudio, "opus": kindAudio, "wav": kindAudio,
	"wma": kindAudio, "aiff": kindAudio, "aif": kindAudio, "ape": kindAudio,

	"pdf": kindDocument, "epub": kindDocument, "txt": kindDocument,
	"md": kindDocument, "rtf": kindDocument, "csv": kindDocument,
	"doc": kindDocument, "docx": kindDocument, "xls": kindDocument,
	"xlsx": kindDocument, "ppt": kindDocument, "pptx": kindDocument,
	"odt": kindDocument, "ods": kindDocument, "odp": kindDocument,
}

// newBlobKey names the object a write is about to store:
//
//	<kind>/<year>/<month>/<day>/<id>[.<ext>]
//
// It exists for one reader, and it is not this program: somebody looking at a
// data directory with no database left. The database holds the naming, so
// nothing here is ever parsed back -- which is what lets an import adopt
// somebody else's bucket unchanged (#24) and lets three generations of key
// shape sit in one store. The day something reads a kind out of a key, this
// layout stops being a convenience and becomes a schema.
//
// Best effort is the whole specification (#123). The kind is inherited from the
// name the file arrived with and stays wrong when that name lied: a video
// uploaded without an extension is filed under other/ and is still there after
// the indexer has decided otherwise. Correcting it later would mean moving
// objects, which is a copy of every byte.
//
// The date is the upload date because it is the only one known here, and it is
// also what fans the tree out now that the key carries no random prefix. The
// worst case it accepts is a bulk import: everything that arrives on one day
// lands in one directory.
//
// The id stays random rather than a digest of the content. A digest is not
// known until the body has been read, the storage port has no rename, and
// nobody recovering files can tell the two apart.
func newBlobKey(name string) string {
	now := time.Now().UTC()
	ext := extensionOf(name)

	key := fmt.Sprintf("%s/%04d/%02d/%02d/%s",
		kindOf(ext), now.Year(), int(now.Month()), now.Day(), rand.Text())
	if ext != "" {
		key += "." + ext
	}
	return key
}

func kindOf(ext string) string {
	if kind, ok := byExtension[ext]; ok {
		return kind
	}
	return kindOther
}

// extensionOf takes the extension off the name the file arrived with, and
// deliberately does not round-trip it through a MIME type: ext to MIME and back
// hands over a different extension than the one that arrived -- ".jpeg" becomes
// ".jpe" -- which is the opposite of inheriting it.
//
// Lowercased ASCII alphanumerics only. Lowercasing is what keeps two uploads of
// Photo.JPG and photo.jpg from producing keys that differ only in case, which a
// case-insensitive filesystem cannot hold apart (#16); the rest is because an
// extension that is not one is better dropped than made into a directory name.
func extensionOf(name string) string {
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))
	if ext == "" || len(ext) > maxExtLen {
		return ""
	}
	for i := range len(ext) {
		if c := ext[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return ""
		}
	}
	return ext
}
