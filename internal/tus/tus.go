// Package tus implements the resumable upload protocol at tus.io, version
// 1.0.0, over internal/files.
//
// It exists because a PUT is all or nothing: a four-gigabyte video uploaded
// from a phone on mobile data restarts from zero on every drop, forever, and no
// amount of retry logic in the client changes that (#122). HTTP has no standard
// answer -- RFC 9110 says a server should *reject* Content-Range on PUT -- and
// tus is the smallest honest one, with clients that already exist for every
// platform this project cares about.
//
// Written by hand rather than taken from tusd, which arrives with a storage
// abstraction of its own that would sit absurdly beside the one this project
// already has. What is left after that decision is a few hundred lines of
// header handling over the upload half of internal/files.
//
// The core is implemented, with the creation, expiration and termination
// extensions. Deferred length is deliberately not: every client this is for
// knows how big the file is, and a server that accepts an upload of unknown
// length has to decide when it ended.
package tus

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/C0piIot/stratus-backend/internal/auth"
	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/storage"
)

// Version is the only one of the protocol this server speaks.
const Version = "1.0.0"

// extensions is what OPTIONS advertises. Order is not significant and the list
// is exactly what is implemented: advertising one that is not would send a
// client down a path that ends in a 400.
const extensions = "creation,expiration,termination"

// offsetContentType is the media type every PATCH has to carry. The protocol
// insists on it, and it is what makes an accidental form post impossible to
// mistake for a chunk.
const offsetContentType = "application/offset+octet-stream"

type handler struct {
	prefix string
	files  *files.Service
}

// Handler serves the tus surface under prefix, which is where a created
// upload's URL is rooted.
func Handler(prefix string, service *files.Service) http.Handler {
	h := &handler{prefix: strings.TrimSuffix(prefix, "/"), files: service}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Announced on every response, including the errors: a client that
		// cannot find it has to assume it is talking to something else.
		w.Header().Set("Tus-Resumable", Version)

		// OPTIONS is the one request that may arrive without the version
		// header, because it is how a client discovers what to send.
		if r.Method == http.MethodOptions {
			h.options(w)
			return
		}
		if v := r.Header.Get("Tus-Resumable"); v != Version {
			w.Header().Set("Tus-Version", Version)
			http.Error(w, "this server speaks tus "+Version, http.StatusPreconditionFailed)
			return
		}

		id := strings.Trim(strings.TrimPrefix(r.URL.Path, h.prefix), "/")
		switch {
		case r.Method == http.MethodPost && id == "":
			h.create(w, r)
		case id == "":
			w.Header().Set("Allow", "OPTIONS, POST")
			http.Error(w, "method not allowed on the creation endpoint", http.StatusMethodNotAllowed)
		case r.Method == http.MethodHead:
			h.status(w, r, id)
		case r.Method == http.MethodPatch:
			h.patch(w, r, id)
		case r.Method == http.MethodDelete:
			h.terminate(w, r, id)
		default:
			w.Header().Set("Allow", "HEAD, PATCH, DELETE")
			http.Error(w, "method not allowed on an upload", http.StatusMethodNotAllowed)
		}
	})
}

// options answers the discovery request.
func (h *handler) options(w http.ResponseWriter) {
	w.Header().Set("Tus-Version", Version)
	w.Header().Set("Tus-Extension", extensions)
	w.WriteHeader(http.StatusNoContent)
}

// create starts an upload and answers with its URL.
func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	owner, ok := auth.User(r.Context())
	if !ok {
		http.Error(w, "no authenticated user", http.StatusUnauthorized)
		return
	}

	length, err := strconv.ParseInt(r.Header.Get("Upload-Length"), 10, 64)
	if err != nil || length < 0 {
		// Deferred length is not advertised, so its absence is a malformed
		// request rather than a client using an extension we lack.
		http.Error(w, "Upload-Length is required and must be a byte count", http.StatusBadRequest)
		return
	}

	meta := parseMetadata(r.Header.Get("Upload-Metadata"))
	path := strings.Trim(meta["filename"], "/")
	if path == "" {
		http.Error(w, "Upload-Metadata must carry a filename", http.StatusBadRequest)
		return
	}

	u, err := h.files.BeginUpload(r.Context(), owner, path, length, meta["filetype"])
	if err != nil {
		writeErr(w, err)
		return
	}

	w.Header().Set("Location", h.prefix+"/"+u.ID)
	w.Header().Set("Upload-Expires", u.ExpiresAt.Format(http.TimeFormat))
	w.WriteHeader(http.StatusCreated)
}

// status is HEAD: where the client should carry on from.
func (h *handler) status(w http.ResponseWriter, r *http.Request, id string) {
	owner, ok := auth.User(r.Context())
	if !ok {
		http.Error(w, "no authenticated user", http.StatusUnauthorized)
		return
	}

	u, err := h.files.Upload(r.Context(), owner, id)
	if err != nil {
		writeErr(w, err)
		return
	}

	// An offset a proxy cached would be a client writing over itself.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Upload-Offset", strconv.FormatInt(u.Received, 10))
	w.Header().Set("Upload-Length", strconv.FormatInt(u.Size, 10))
	w.Header().Set("Upload-Expires", u.ExpiresAt.Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
}

// patch appends a chunk, and completes the upload when the last one lands.
func (h *handler) patch(w http.ResponseWriter, r *http.Request, id string) {
	owner, ok := auth.User(r.Context())
	if !ok {
		http.Error(w, "no authenticated user", http.StatusUnauthorized)
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != offsetContentType {
		http.Error(w, "a chunk is "+offsetContentType, http.StatusUnsupportedMediaType)
		return
	}
	offset, err := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
	if err != nil || offset < 0 {
		http.Error(w, "Upload-Offset is required and must be a byte count", http.StatusBadRequest)
		return
	}

	u, err := h.files.AppendUpload(r.Context(), owner, id, offset, r.Body)
	if err != nil {
		writeErr(w, err)
		return
	}

	// The protocol has no "finish": an upload is over when it is as long as it
	// said it would be, and the file appears then.
	if u.Received == u.Size {
		if _, cerr := h.files.CompleteUpload(r.Context(), owner, id); cerr != nil {
			writeErr(w, cerr)
			return
		}
	}

	w.Header().Set("Upload-Offset", strconv.FormatInt(u.Received, 10))
	w.Header().Set("Upload-Expires", u.ExpiresAt.Format(http.TimeFormat))
	w.WriteHeader(http.StatusNoContent)
}

// terminate is a client saying it is not coming back.
func (h *handler) terminate(w http.ResponseWriter, r *http.Request, id string) {
	owner, ok := auth.User(r.Context())
	if !ok {
		http.Error(w, "no authenticated user", http.StatusUnauthorized)
		return
	}

	if _, err := h.files.Upload(r.Context(), owner, id); err != nil {
		writeErr(w, err)
		return
	}
	if err := h.files.AbortUpload(r.Context(), owner, id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseMetadata reads Upload-Metadata: comma-separated pairs of a key and a
// base64 value, and keys with no value at all.
//
// A pair that does not decode is dropped rather than refused: the header is a
// bag a client may put anything in, and the only key this server needs is
// checked by its caller.
func parseMetadata(header string) map[string]string {
	out := map[string]string{}
	for pair := range strings.SplitSeq(header, ",") {
		key, value, _ := strings.Cut(strings.TrimSpace(pair), " ")
		if key == "" {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
		if err != nil {
			continue
		}
		out[key] = string(decoded)
	}
	return out
}

// writeErr turns this project's sentinels into the status codes the protocol
// is specific about. It is the one piece of translation the layer below cannot
// do for us.
func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrNotFound):
		// Covers both an upload id that is not there and a parent directory
		// that is not: from the client's side the request named something that
		// does not exist either way.
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, storage.ErrUploadOffset), errors.Is(err, db.ErrConflict):
		http.Error(w, "the upload is not at that offset", http.StatusConflict)
	case errors.Is(err, db.ErrInvalidPath):
		http.Error(w, "that is not a path this server will store", http.StatusBadRequest)
	default:
		http.Error(w, fmt.Sprintf("the upload failed: %v", err), http.StatusInternalServerError)
	}
}
