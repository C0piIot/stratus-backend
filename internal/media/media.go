// Package media extracts the metadata that turns a pile of files into a
// library: when a photo was taken, how long a track runs, who recorded it.
//
// It is a feature rather than an adapter because every protocol needs the same
// answers. A gallery sorted by capture date and an OpenSubsonic browse are the
// same rows read two ways.
package media

import (
	"path"
	"strings"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// Version is the extractor generation. Raising it puts every file back in the
// queue, which is how a better extractor reaches what it already looked at --
// no migration, no script.
//
// It went to 3 for the folded search columns (#85), which is the one bump so far
// that the extractor itself did not need: the columns derive from tags already
// in the row. The alternative was backfilling them in the migration with each
// engine's own lower(), which is exactly the divergence between the two drivers
// that folding in Go removes -- and it would have applied to the rows nothing
// would ever re-check.
//
// It went to 4 when the kind started coming from the file's first bytes (#146),
// which is the bump this mechanism is for: a camcorder's .mts and a recording
// with no extension at all were filed as "other" and never looked at again, and
// they are looked at again now without a migration or a script.
const Version = 4

// kindOf decides which extractor a file gets.
//
// The bytes have the first word and the name only answers for what they could
// not say. That order is the whole of #146: an extension is something somebody
// typed, and the two cases it gets wrong are the ones that matter -- a video
// from a camcorder, whose extension is not in any table, and a file that has no
// extension at all, which is what a camera roll copied off a phone looks like.
//
// The declared MIME type is last and is a hint at that: WebDAV clients send
// application/octet-stream for everything.
func kindOf(f db.File, sniffed db.Kind) db.Kind {
	if sniffed != db.KindOther {
		return sniffed
	}
	if kind := kindFromExtension(path.Ext(f.Path)); kind != "" {
		return kind
	}
	switch {
	case strings.HasPrefix(f.MIMEType, "image/"):
		return db.KindImage
	case strings.HasPrefix(f.MIMEType, "audio/"):
		return db.KindAudio
	case strings.HasPrefix(f.MIMEType, "video/"):
		return db.KindVideo
	default:
		return db.KindOther
	}
}

// byExtension is deliberately explicit. mime.TypeByExtension reads
// /etc/mime.types on Linux, which a distroless image does not have, so a table
// that travels with the binary is the only one that behaves the same in a test
// and in production.
//
// Every audio and video entry here needs a demuxer enabled in
// build/ffprobe/Dockerfile, which builds the trimmed ffprobe the image ships.
// Adding one without the other does not fail the build, it fails the probe for
// that format in production -- so TestFFprobeReadsEveryExtension holds the two
// lists together. Images never reach ffprobe: EXIF is read in pure Go.
var byExtension = map[string]db.Kind{
	".jpg": db.KindImage, ".jpeg": db.KindImage, ".png": db.KindImage,
	".gif": db.KindImage, ".webp": db.KindImage, ".tif": db.KindImage,
	".tiff": db.KindImage, ".heic": db.KindImage, ".heif": db.KindImage,
	".avif": db.KindImage, ".dng": db.KindImage, ".cr2": db.KindImage,
	".cr3": db.KindImage, ".nef": db.KindImage, ".arw": db.KindImage,

	".mp3": db.KindAudio, ".flac": db.KindAudio, ".m4a": db.KindAudio,
	".ogg": db.KindAudio, ".opus": db.KindAudio, ".wav": db.KindAudio,
	".aac": db.KindAudio, ".wma": db.KindAudio,

	".mp4": db.KindVideo, ".mov": db.KindVideo, ".mkv": db.KindVideo,
	".webm": db.KindVideo, ".avi": db.KindVideo, ".m4v": db.KindVideo,
	".mpg": db.KindVideo, ".mpeg": db.KindVideo, ".wmv": db.KindVideo,
	// AVCHD, which is what a camcorder records, and the transport stream a
	// set-top box writes. Not ".ts", which is a transport stream and a
	// TypeScript file in equal measure -- that one is answered by its first
	// bytes or not at all (#146).
	".mts": db.KindVideo, ".m2ts": db.KindVideo, ".m2t": db.KindVideo,
}

func kindFromExtension(ext string) db.Kind {
	return byExtension[strings.ToLower(ext)]
}
