package web

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"time"
)

// The phone's share sheet, which is the one door into this server that does
// not start by opening it (#312): share a photograph from the gallery and it
// lands here.
//
// **It invents no API.** `share_target` is a member of the manifest and what
// it describes is an ordinary multipart form POST -- the same one this UI's
// own upload form sends -- so this is `upload`'s twin and reads the parts the
// same way, straight from the socket into internal/files rather than through
// ParseMultipartForm, because this is where somebody shares a long video.
//
// Three things make it a route of its own rather than a flag on that one, and
// all three are answers to the same fact: **a share carries no destination.**
//
//   - **Where it lands is the server's**: `shared/<year>/<month>/`, made on the
//     way. A folder picker in the middle of the one gesture this feature exists
//     to be is friction exactly where somebody wanted none.
//   - **It never replaces.** Two photographs shared a minute apart are both
//     called `image.jpg`, and a `PUT`'s replace would destroy the first.
//     `files.FreeName` is the rule the import folder already reads.
//   - **A share with no name of its own gets one**, from the moment it arrived
//     and the type the browser declared, which is everything anybody knows
//     about it.
//
// **What it does not invent is a page.** It redirects to the folder with
// `?added=`, exactly as an upload does, so the listing says what arrived and
// the page somebody lands on *is* the answer to where it went -- with the
// file's own row on it, and the rename and the delete that row has always had.
//
// **Whether a browser sends the session with this POST is not known here, and
// cannot be.** The cookie is `SameSite=Lax`, so a POST the browser judges
// cross-site carries no credential at all, and only a phone can say which this
// one is. The two outcomes are two different redirects in the request log, and
// the checklist for reading them is on #312.
const (
	shareTargetPath = "/share-target"
	// shareField is the name the manifest gives the file part, and it is the
	// one the upload form already uses: both doors speak the same multipart.
	shareField = "file"
	// shareDir is where a share lands, dated so that a year of them is not one
	// folder with ten thousand things in it.
	shareDir = "shared"
)

// shareExt is the extension a share gets when it brought no filename.
//
// Short on purpose: `mime.ExtensionsByType` answers an alphabetical list whose
// first entry for a JPEG is `.jfif`, and a table of everything is the standard
// library's job rather than this file's. A type that is not in here lands
// without an extension, which costs it the thumbnail a listing offers by name
// and nothing else -- what the row says the file is comes from the browser's
// own header, or from the bytes.
var shareExt = map[string]string{
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
	"image/gif":       ".gif",
	"image/webp":      ".webp",
	"image/heic":      ".heic",
	"video/mp4":       ".mp4",
	"video/quicktime": ".mov",
	"audio/mpeg":      ".mp3",
	"application/pdf": ".pdf",
	"text/plain":      ".txt",
}

func (h *handler) shareTarget(w http.ResponseWriter, r *http.Request, user string) {
	parts, err := r.MultipartReader()
	if err != nil {
		h.badRequest(w, user, "That was not something to share.")
		return
	}

	arrived := time.Now().UTC()
	dir := fmt.Sprintf("%s/%04d/%02d", shareDir, arrived.Year(), int(arrived.Month()))
	if err := h.files.MkdirAll(r.Context(), user, dir); err != nil {
		h.fail(w, r, user, err)
		return
	}

	var added int
	for {
		part, err := parts.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			h.badRequest(w, user, "What was shared did not arrive whole.")
			return
		}
		if part.FormName() != shareField {
			// A share sheet can send a title and a URL beside the file, and
			// this door is for files: the rest is dropped rather than written
			// into a note nobody asked for.
			_ = part.Close()
			continue
		}

		declared := part.Header.Get("Content-Type")
		// A share sheet can hand over a stream with no filename at all, and
		// what baseName makes of nothing is ".", which would join back to the
		// folder itself and land the file quietly beside it instead of in it.
		name := sharedName(arrived, declared)
		switch sent := baseName(part.FileName()); sent {
		case ".", "..", "/":
		default:
			name = sent
		}
		target, err := h.files.FreeName(r.Context(), user, path.Join(dir, name))
		if err != nil {
			_ = part.Close()
			h.fail(w, r, user, err)
			return
		}
		// Size -1 for the reason upload passes it: a part does not say how long
		// it is, and the ETag is a digest of what was stored rather than a
		// guess from a length.
		_, err = h.files.Write(r.Context(), user, target, part, -1, uploadType(declared, target))
		_ = part.Close()
		if err != nil {
			h.fail(w, r, user, err)
			return
		}
		added++
	}

	// See other, the same as an upload: the folder it landed in, and the count
	// it renders as a notice.
	redirectLocal(w, r, href(dir)+"?added="+strconv.Itoa(added))
}

// sharedName is what something shared without a filename is called: the moment
// it arrived, which is the one thing that distinguishes it from the next one.
func sharedName(at time.Time, declared string) string {
	name := at.Format("2006-01-02 15-04-05")
	if media, _, err := mime.ParseMediaType(declared); err == nil {
		name += shareExt[media]
	}
	return name
}
