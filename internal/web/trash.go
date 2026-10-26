package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/stratus-private-cloud/stratus-backend/internal/db"
	"github.com/stratus-private-cloud/stratus-backend/internal/trash"
)

// The trash (#274): what has been deleted and not yet destroyed.
//
// It is a page of **deletions** and not of files, which is the whole shape of
// it: deleting a folder of a thousand photographs is one accident, and a list
// of a thousand rows is not a way to find it again. What a row offers is the
// two things that can be done with one: put it back (#275), or destroy it now
// instead of waiting out the month.
const (
	// trashPrefix is one address and two protocols, like /photos/ and the rest
	// (#323): this page to a browser, the collection of deletions to a WebDAV
	// client, split by method in the composition root.
	trashPrefix = "/trash/"
	// destroyPrefix and restorePrefix are where the two things a deletion can
	// have done to it are asked for, beside /delete/ and /rename/ rather than
	// under /trash/ as they were. What moved them is that collection:
	// everything under it is now a name somebody deleted, and a form at an
	// address a deleted file could also have is one of the two answering
	// wrongly.
	destroyPrefix = "/destroy/"
	restorePrefix = "/restore/"
	// trashPageSize is a page of deletions, not of files. Fifty accidents is
	// already more than anybody has.
	trashPageSize = 50
)

// trashFragment is what htmx asks for when it extends the list, as the
// listing's rows are.
const trashFragment = "deletions"

func (h *handler) trash(w http.ResponseWriter, r *http.Request, user string) {
	after, err := parseTrashCursor(r.URL.Query().Get("after"))
	if err != nil {
		h.badRequest(w, user, "That is not a place in the trash to carry on from.")
		return
	}

	// One more than a page, so the page knows whether to offer more without a
	// second query.
	batches, err := h.files.Trash(r.Context(), user, after, trashPageSize+1)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	totals, err := h.files.TrashTotals(r.Context(), user)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}

	page, more := batches, false
	if len(page) > trashPageSize {
		page, more = page[:trashPageSize], true
	}

	v := view{
		Title: "Trash", User: user, Here: trashPrefix,
		Deletions: deletions(page),
		TrashSize: humanBytes(totals.Bytes),
		Kept:      h.keptDays(),
	}
	if more {
		last := page[len(page)-1]
		v.NextPage = trashLink(db.TrashCursor{DeletedAt: last.DeletedAt, Batch: last.ID})
	}

	// The second kind of row, and the reason the page has two sections rather
	// than one list with a disabled button on half of it: a blob nobody can
	// account for has no path to be put back at (#276).
	//
	// Not paged. A healthy server has none of these at all, and a server with
	// thousands has one problem rather than thousands -- what the page is for
	// there is to say the number, which the first fifty say as well as all of
	// them.
	unaccounted, err := h.files.Trash(r.Context(), "", db.TrashCursor{}, trashPageSize)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	v.Unaccounted = deletions(unaccounted)

	if r.Header.Get("HX-Request") == "true" {
		h.renderTemplate(w, http.StatusOK, pageTrash, trashFragment, v)
		return
	}
	h.render(w, http.StatusOK, pageTrash, v)
}

// destroyForm asks before throwing a deletion away for good, which is the one
// page in this project where that question is final: there is nothing behind
// the trash.
func (h *handler) destroyForm(w http.ResponseWriter, r *http.Request, user string) {
	batch := r.PathValue("batch")
	v := view{
		Title: "Delete for good", User: user,
		Name: batch, Action: destroyPrefix + url.PathEscape(batch), Back: trashPrefix,
	}
	h.render(w, http.StatusOK, pageDestroy, v)
}

// destroy throws one deletion away for good: this reader's own, or one of the
// sweep's, which belong to nobody.
//
// Two calls and not a lookup, because that is the policy written out: a
// signed-in person may destroy what they deleted and what the server cannot
// account for, and nothing else. The owner is the scope that will keep one
// person out of another's trash when there is more than one of them (#276),
// and "" is the scope that is nobody's.
func (h *handler) destroy(w http.ResponseWriter, r *http.Request, user string) {
	batch := r.PathValue("batch")
	destroyed, _, err := h.files.DestroyTrashed(r.Context(), user, batch)
	if err == nil && destroyed == 0 {
		_, _, err = h.files.DestroyTrashed(r.Context(), "", batch)
	}
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	redirectLocal(w, r, trashPrefix)
}

// restore puts a deletion back and sends the browser to where it landed --
// which is not always where it left from, so the folder is the answer rather
// than a message about it.
func (h *handler) restore(w http.ResponseWriter, r *http.Request, user string) {
	back, err := h.files.Restore(r.Context(), user, r.PathValue("batch"))
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	redirectLocal(w, r, href(db.ParentOf(back.Path)))
}

// trashed serves what is at a path under the collection (#323): the bytes of a
// deleted file, which are the blob the deletion never moved.
//
// It is the browser half of the mount, like the photographs' and the music's,
// and it is what makes the collection worth mounting: one photograph can be
// pulled out of an accident by copying it, instead of putting a thousand back
// to get at one. Read-only like everything else here -- a copy out is not a
// restore, and what was deleted stays deleted until the page says otherwise.
func (h *handler) trashed(w http.ResponseWriter, r *http.Request, user string) {
	n, err := trash.New(h.files, user).Resolve(r.Context(), r.PathValue("path"))
	switch {
	case errors.Is(err, os.ErrNotExist):
		h.fail(w, r, user, db.ErrNotFound)
		return
	case err != nil:
		h.fail(w, r, user, err)
		return
	}
	// A deleted folder has no page of its own, and the trash is the page for
	// all of them: a deletion is one line there, which is the whole shape of
	// that page. So the collection is a client's and the list is a person's.
	if n.Dir {
		redirectLocal(w, r, trashPrefix)
		return
	}
	h.download(w, r, user, n.Row.File)
}

// deletion is one accident, as the page shows it.
type deletion struct {
	ID string
	// Name is what was deleted: the folder, or the file when it was one.
	Name string
	// In is the folder it was in, empty at the root, so that two deletions of
	// the same name are told apart.
	In      string
	Files   int64
	Size    string
	When    string
	Destroy string
	Restore string
}

func deletions(batches []db.TrashBatch) []deletion {
	out := make([]deletion, 0, len(batches))
	for _, b := range batches {
		row := deletion{
			ID:      b.ID,
			Name:    path.Base(b.Root),
			In:      db.ParentOf(b.Root),
			Files:   b.Files,
			Size:    humanBytes(b.Bytes),
			When:    b.DeletedAt.Format("2006-01-02 15:04"),
			Destroy: destroyPrefix + url.PathEscape(b.ID),
			Restore: restorePrefix + url.PathEscape(b.ID),
		}
		// A sweep's batch has no root, because nothing in it has a path. It
		// is named by what it is and offers no way back: the name was in the
		// index that went wrong.
		if b.Root == "" {
			row.Name = "found by the sweep"
			row.Restore = ""
		}
		out = append(out, row)
	}
	return out
}

// trashLink is the rest of the list, resumed.
func trashLink(after db.TrashCursor) string {
	q := url.Values{}
	q.Set("after", encodeTrashCursor(after))
	return trashPrefix + "?" + q.Encode()
}

// encodeTrashCursor writes a moment and a deletion into one parameter. The
// time is milliseconds since the epoch, which is what every driver stores it
// to and therefore the precision a cursor can resume at.
func encodeTrashCursor(c db.TrashCursor) string {
	return strconv.FormatInt(c.DeletedAt.UnixMilli(), 10) + "/" + c.Batch
}

func parseTrashCursor(v string) (db.TrashCursor, error) {
	if v == "" {
		return db.TrashCursor{}, nil
	}
	when, batch, ok := strings.Cut(v, "/")
	if !ok || batch == "" {
		return db.TrashCursor{}, fmt.Errorf("cursor %q: not a moment and a deletion", v)
	}
	ms, err := strconv.ParseInt(when, 10, 64)
	if err != nil {
		return db.TrashCursor{}, fmt.Errorf("cursor %q: %w", v, err)
	}
	return db.TrashCursor{DeletedAt: time.UnixMilli(ms).UTC(), Batch: batch}, nil
}
