package web

import (
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/files"
)

// The trash (#274): what has been deleted and not yet destroyed.
//
// It is a page of **deletions** and not of files, which is the whole shape of
// it: deleting a folder of a thousand photographs is one accident, and a list
// of a thousand rows is not a way to find it again. What a row offers is the
// one thing this release can do with it -- destroy it now, instead of waiting
// out the month.
const (
	trashPrefix = "/trash"
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
		Kept:      int(files.DefaultTrashRetention / (24 * time.Hour)),
	}
	if more {
		last := page[len(page)-1]
		v.NextPage = trashLink(db.TrashCursor{DeletedAt: last.DeletedAt, Batch: last.ID})
	}

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
		Name: batch, Action: trashPrefix + "/" + url.PathEscape(batch), Back: trashPrefix,
	}
	h.render(w, http.StatusOK, pageDestroy, v)
}

// destroy throws one deletion away for good.
func (h *handler) destroy(w http.ResponseWriter, r *http.Request, user string) {
	if _, _, err := h.files.DestroyTrashed(r.Context(), user, r.PathValue("batch")); err != nil {
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
		out = append(out, deletion{
			ID:      b.ID,
			Name:    path.Base(b.Root),
			In:      db.ParentOf(b.Root),
			Files:   b.Files,
			Size:    humanBytes(b.Bytes),
			When:    b.DeletedAt.Format("2006-01-02 15:04"),
			Destroy: trashPrefix + "/" + url.PathEscape(b.ID),
			Restore: trashPrefix + "/" + url.PathEscape(b.ID) + "/restore",
		})
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
