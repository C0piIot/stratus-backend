package web

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// upload writes the files a form sent into the directory it was shown in, and
// sends the browser back to the listing to see them.
//
// The parts are streamed straight into internal/files rather than parsed with
// ParseMultipartForm, which would spool every one of them to a temporary file
// first. A personal cloud is where somebody puts a 4 GB video, so the bytes go
// from the socket to the blob store and are never on this machine twice.
func (h *handler) upload(w http.ResponseWriter, r *http.Request, user string) {
	dir, err := toPath(r.PathValue("path"))
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	// A file is not somewhere to put a file. The root has no row and is always
	// a directory, so it needs no asking.
	if dir != "" {
		f, statErr := h.files.Stat(r.Context(), user, dir)
		if statErr != nil {
			h.fail(w, r, user, statErr)
			return
		}
		if !f.IsDir {
			h.fail(w, r, user, fmt.Errorf("%w: %q is not a directory", db.ErrConflict, dir))
			return
		}
	}

	parts, err := r.MultipartReader()
	if err != nil {
		// Not the tree's fault and not a path: whatever was posted, it was not
		// a form with files in it.
		h.badRequest(w, user, "That was not a file upload.")
		return
	}

	var added int
	for {
		part, err := parts.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// A body this server cannot read to the end is the sender's to fix,
			// whether it was truncated on the way or malformed to begin with --
			// a header with a control character in it lands here too.
			h.badRequest(w, user, "The upload did not arrive whole.")
			return
		}
		// A part with no filename is an ordinary form field, not a file.
		if part.FormName() != "file" || part.FileName() == "" {
			_ = part.Close()
			continue
		}

		// Not validated here: Write refuses the same paths, with the same error
		// and therefore the same page, and a check that can never answer
		// differently from the one below it is a check nothing can test.
		target := path.Join(dir, uploadName(part.FileName()))
		// Size -1: a multipart part does not say how long it is, and Write is
		// built for exactly that -- the ETag is a digest of what was actually
		// stored rather than a guess from a length.
		//
		// Whatever was at that path is replaced, which is what a PUT over
		// WebDAV does to the same tree. One door behaving differently from the
		// other is worse than the surprise.
		_, err = h.files.Write(r.Context(), user, target, part,
			-1, uploadType(part.Header.Get("Content-Type"), target))
		_ = part.Close()
		if err != nil {
			h.fail(w, r, user, err)
			return
		}
		added++
	}

	// See other, so a reload does not offer to send the files again.
	//nolint:gosec // G710: href builds a path under /files/ out of what toPath already tidied.
	http.Redirect(w, r, href(dir)+"?added="+strconv.Itoa(added), http.StatusSeeOther)
}

// badRequest is the answer to a form that was never going to work. It is the
// client's mistake, so it says what it can and no more.
func (h *handler) badRequest(w http.ResponseWriter, user, message string) {
	h.render(w, http.StatusBadRequest, pageError, view{
		Title: "Bad request", User: user, Message: message,
	})
}

// uploadName is the filename a browser sent, reduced to a name.
//
// Only the last element survives: a directory upload sends a relative path, an
// old Windows browser sends a whole one with backslashes, and a hostile client
// sends whatever it likes. What is left still goes through db.ValidatePath,
// which is where "." and ".." are refused.
func uploadName(sent string) string {
	return path.Base(strings.ReplaceAll(sent, `\`, "/"))
}

// uploadType is what the file will be served back as. The browser's word for
// it first, since it is the only one that saw the file, then the extension, and
// then the honest shrug.
func uploadType(declared, target string) string {
	if declared != "" {
		return declared
	}
	if byExt := mime.TypeByExtension(path.Ext(target)); byExt != "" {
		return byExt
	}
	return "application/octet-stream"
}

// uploaded reads the count the redirect above carries. It is a number or it is
// nothing: the query string belongs to whoever typed the URL.
func uploaded(raw string) string {
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return ""
	}
	if n == 1 {
		return "1 file uploaded."
	}
	return strconv.Itoa(n) + " files uploaded."
}
