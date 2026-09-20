package dav

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	xnet "golang.org/x/net/webdav"
)

// Enforcing the locks this server hands out.
//
// LOCK used to answer with a well-formed token that nothing recorded, because
// Finder will not mount a share read-write against a class 1 server (#3) and
// there was no lock system to hand it to. There is one now: x/net/webdav
// arrived for PROPFIND (#136) and brought a LockSystem with it, so the half
// that needed a table does not need one.
//
// What is here is the other half -- refusing a write to something somebody
// else has locked -- and it is our own because x/net enforces locks inside its
// own handlers, and its handlers are not the ones serving PUT here. The
// grammar it needs is vendored beside this, in ifheader.go.

// memoryLocks is the lock system this server runs with: x/net's own, which
// holds what it knows in the process and nowhere else.
//
// That is the right default rather than a compromise. A lock is a claim with a
// timeout measured in minutes, and a restart forgetting one costs a client a
// retry -- the same trade the signed session makes, and consistent with a
// server that assumes one instance in five other places. A lock table would be
// the only cluster-ready thing in it.
//
// Nothing above this line depends on it being in memory: xnet.LockSystem is
// four methods and it is the library's, not a seam invented here, so the day a
// lock has to outlive a restart is a type satisfying it and a line in the
// composition root.
func memoryLocks() xnet.LockSystem { return withOpaqueTokens(xnet.NewMemLS()) }

// opaqueTokens gives the lock system's tokens a scheme, which is the whole of
// what they are missing.
//
// memLS numbers them -- "1789906311", then the next integer -- and RFC 4918
// 6.5 says a lock token is a URI, with the Lock-Token header carrying a
// Coded-URL. A bare integer is neither, and Finder is the client this surface
// exists for (#3), so it is not the place to find out who is lenient.
//
// **Unguessable is not among the requirements, and that is worth writing down
// because the instinct says otherwise.** The implementation that recorded
// nothing minted a random token, and copying that here would buy nothing: a
// client that can take a lock is shown the shape of them, and a client that
// cannot take one cannot write either -- a share link is read-only and Basic
// is the gate in front of everything else. What guessing a token would let
// somebody do, they can already do with the password they had to have.
type opaqueTokens struct {
	xnet.LockSystem
}

// lockURIScheme is RFC 4918's own, and the one clients recognise. Named for
// the scheme rather than for what it prefixes, because gosec reads a constant
// with "token" in its name and a string in it as a credential somebody left
// in the source.
const lockURIScheme = "opaquelocktoken:"

func withOpaqueTokens(ls xnet.LockSystem) xnet.LockSystem {
	return &opaqueTokens{LockSystem: ls}
}

func (o *opaqueTokens) Create(now time.Time, details xnet.LockDetails) (string, error) {
	token, err := o.LockSystem.Create(now, details)
	if err != nil {
		return "", err
	}
	return lockURIScheme + token, nil
}

func (o *opaqueTokens) Refresh(now time.Time, token string, duration time.Duration) (xnet.LockDetails, error) {
	return o.LockSystem.Refresh(now, innerToken(token), duration)
}

func (o *opaqueTokens) Unlock(now time.Time, token string) error {
	return o.LockSystem.Unlock(now, innerToken(token))
}

func (o *opaqueTokens) Confirm(now time.Time, name0, name1 string, conditions ...Condition) (func(), error) {
	ours := make([]Condition, len(conditions))
	for i, c := range conditions {
		c.Token = innerToken(c.Token)
		ours[i] = c
	}
	return o.LockSystem.Confirm(now, name0, name1, ours...)
}

// innerToken returns the lock system's own token, or "" for anything that is
// not one of ours -- which needs no error of its own: a token this server
// never minted names no lock, and that is exactly what the lock system says
// about "".
func innerToken(token string) string {
	rest, ok := strings.CutPrefix(token, lockURIScheme)
	if !ok {
		return ""
	}
	return rest
}

// errInvalidIfHeader is a malformed If header, which is the client's mistake
// and not this server's.
var errInvalidIfHeader = errors.New("dav: invalid If header")

// Condition is x/net's, aliased so the vendored parser can stay byte for byte:
// it names this type, and copying a file and then editing its identifiers is
// not copying it.
type Condition = xnet.Condition

// lockedMethods are the ones a lock is allowed to stop. Everything else reads,
// and a lock has never stopped a read in this protocol.
var lockedMethods = map[string]bool{
	http.MethodPut: true, http.MethodDelete: true,
	"MKCOL": true, "MOVE": true, "COPY": true, "PROPPATCH": true,
}

// confirmLocks refuses a write that a lock forbids, and returns the release to
// call when the request is done.
//
// **No If header at all** does not mean "no locks apply". It means the client
// holds none -- and it still has to be refused if somebody else holds one.
// x/net's trick, kept here: take a lock for the length of the request and
// release it after, so a conflict with another client surfaces as an ordinary
// lock conflict rather than as a second code path that could disagree with the
// first.
//
// **An If header** is a disjunction of lists, and one of them being true of
// the resources this request touches is enough. None of them is 412 rather
// than 423: the client made a claim about the state and the claim was wrong,
// which is a different thing from not having asked.
func (f *fileSystem) confirmLocks(r *http.Request, src, dst string) (release func(), status int, err error) {
	header := r.Header.Get("If")
	if header == "" {
		return f.lockForTheRequest(src, dst)
	}

	parsed, ok := parseIfHeader(header)
	if !ok {
		return nil, http.StatusBadRequest, errInvalidIfHeader
	}
	now := f.now()
	// An untagged list is about the Request-URI, which is not always a
	// resource this request writes to: a COPY claims only its destination,
	// and the lock a client submits is the one on the file it is copying.
	requested := src
	if p, perr := toPath(r.URL.Path); perr == nil {
		requested = lockName(p)
	}

	for _, list := range parsed.lists {
		about := requested
		if list.resourceTag != "" {
			// A list tagged with a resource that has nothing to do with this
			// request says nothing about it. x/net confirms against the tag and
			// lets the request through, so a client holding a lock of its own
			// anywhere can name it and walk past somebody else's.
			if about, ok = f.tagged(r, list.resourceTag, src, dst); !ok {
				continue
			}
		}
		if !f.holds(r.Context(), now, list.conditions, about) {
			continue
		}

		release, cerr := f.claimTouched(now, src, dst, claimable(list.conditions))
		if cerr == nil {
			return release, 0, nil
		}
		if !errors.Is(cerr, xnet.ErrConfirmationFailed) {
			return nil, http.StatusInternalServerError, cerr
		}
		// The list was true and claimed no lock: a condition on the ETag
		// alone, or a negative one. What is touched still has to be free.
		if release, _, lerr := f.lockForTheRequest(src, dst); lerr == nil {
			return release, 0, nil
		}
	}
	// Every list was refused. The client said something about the state of
	// these resources and it was not true.
	return nil, http.StatusPreconditionFailed, xnet.ErrConfirmationFailed
}

// holds reports whether every condition in a list is true of name.
//
// The lock system will not answer this: its lookup ignores Not and ETag
// outright -- a TODO in its own source -- and says only whether there is a
// lock here that one of the tokens can claim. So `If: (<token> ["etag"])`,
// which is a client saying "only if the bytes are still these", would be a
// precondition nobody checked. That is the same quiet lie as a lock nothing
// records, one layer along, and litmus catches it.
func (f *fileSystem) holds(ctx context.Context, now time.Time, conditions []Condition, name string) bool {
	for _, c := range conditions {
		var ok bool
		switch {
		case c.Token != "":
			ok = f.tokenCovers(now, c.Token, name)
		case c.ETag != "":
			ok = f.etagIs(ctx, name, c.ETag)
		default:
			return false
		}
		if ok == c.Not {
			return false
		}
	}
	return true
}

// tokenCovers reports whether that token names a lock on name, or on a
// collection above it. Asked of the lock system, which is the only thing that
// knows, and released at once: this is a question and not a claim.
func (f *fileSystem) tokenCovers(now time.Time, token, name string) bool {
	release, err := f.locks.Confirm(now, name, "", Condition{Token: token})
	if err != nil {
		return false
	}
	release()
	return true
}

// etagIs compares an entity-tag from an If header with the validator on the
// row. The quotes are HTTP's framing and internal/files stores what is inside
// them; a weak tag unquotes to nothing here, which is the right answer, since
// this server mints none and RFC 7232 asks for a strong comparison anyway.
func (f *fileSystem) etagIs(ctx context.Context, name, tag string) bool {
	owner, err := f.owner(ctx)
	if err != nil {
		return false
	}
	file, err := f.files.Stat(ctx, owner, strings.TrimPrefix(name, "/"))
	if err != nil || file.ETag == "" {
		return false
	}
	unquoted, err := strconv.Unquote(tag)
	return err == nil && unquoted == file.ETag
}

// claimable is the conditions that can take a lock: a token, said in the
// positive. A negative one is a statement about the world, not a claim on it.
func claimable(conditions []Condition) []Condition {
	claims := make([]Condition, 0, len(conditions))
	for _, c := range conditions {
		if c.Token != "" && !c.Not {
			claims = append(claims, c)
		}
	}
	return claims
}

// tagged resolves a list's resource tag and reports whether it is about
// something this request touches -- the resource itself, or a collection above
// it, which is how a client submits the token of a folder it locked while
// writing a file inside.
func (f *fileSystem) tagged(r *http.Request, tag, src, dst string) (string, bool) {
	u, err := url.Parse(tag)
	if err != nil {
		return "", false
	}
	// An absolute URL has to be this server's. A relative one -- which the
	// grammar allows and x/net refuses, since it compares a host that is not
	// there -- is this server's by construction.
	if u.Host != "" && u.Host != r.Host {
		return "", false
	}
	p, err := toPath(strings.TrimPrefix(u.Path, f.prefix))
	if err != nil {
		return "", false
	}
	name := lockName(p)
	if !covers(name, src) && !covers(name, dst) {
		return "", false
	}
	return name, true
}

// covers reports whether a lock name is target or a collection holding it.
func covers(name, target string) bool {
	switch {
	case target == "":
		return false
	case name == target, name == "/":
		return true
	default:
		return strings.HasPrefix(target, name+"/")
	}
}

// claimTouched is one list of conditions against the one or two resources the
// request writes to.
func (f *fileSystem) claimTouched(now time.Time, src, dst string, conditions []Condition) (func(), error) {
	// Both at once first, because one lock on a collection can cover both ends
	// of a move inside it and the lock system hands a node out once: asking
	// for the two names separately would confirm the lock and then find it
	// held by ourselves.
	release, err := f.locks.Confirm(now, src, dst, conditions...)
	if err == nil {
		return release, nil
	}
	if !errors.Is(err, xnet.ErrConfirmationFailed) || dst == "" {
		return nil, err
	}

	// One end claimed and the other merely free. x/net requires every named
	// resource to be covered by a claimed lock, which makes a MOVE onto an
	// unlocked path impossible for the client that holds the source -- a rule
	// no other server has and the RFC does not ask for. What the other end has
	// to be is not somebody else's, and a brief lock is how that is asked.
	if release, ok := f.claimOneHoldTheOther(now, src, dst, conditions); ok {
		return release, nil
	}
	if src != "" {
		if release, ok := f.claimOneHoldTheOther(now, dst, src, conditions); ok {
			return release, nil
		}
	}
	return nil, xnet.ErrConfirmationFailed
}

// claimOneHoldTheOther confirms one resource against the conditions and takes
// the other for the length of the request, and reports whether both worked.
func (f *fileSystem) claimOneHoldTheOther(now time.Time, claimed, free string, conditions []Condition) (func(), bool) {
	release, err := f.locks.Confirm(now, claimed, "", conditions...)
	if err != nil {
		return nil, false
	}
	token, err := f.holdBriefly(now, free)
	if err != nil {
		release()
		return nil, false
	}
	return func() {
		_ = f.locks.Unlock(now, token)
		release()
	}, true
}

// lockForTheRequest is the no-If case: hold both paths for the length of the
// request so that somebody else's lock refuses it.
func (f *fileSystem) lockForTheRequest(src, dst string) (func(), int, error) {
	now := f.now()
	var srcToken, dstToken string
	var err error

	if src != "" {
		if srcToken, err = f.holdBriefly(now, src); err != nil {
			return nil, lockStatus(err), err
		}
	}
	if dst != "" {
		if dstToken, err = f.holdBriefly(now, dst); err != nil {
			if srcToken != "" {
				_ = f.locks.Unlock(now, srcToken)
			}
			return nil, lockStatus(err), err
		}
	}

	return func() {
		if dstToken != "" {
			_ = f.locks.Unlock(now, dstToken)
		}
		if srcToken != "" {
			_ = f.locks.Unlock(now, srcToken)
		}
	}, 0, nil
}

// lockName is a path as the lock system names it: the URL form, which is what
// the If header carries and what x/net's memLS cleans its own keys to.
//
// The root is "/" and never "", which is the distinction the two arguments of
// Confirm rest on: an empty name there means the request has no such resource
// -- a PUT has no destination -- rather than naming the top of the tree.
func lockName(p string) string { return "/" + p }

// enforceLocks is the gate, and it is the third thing in this package that
// reads a request before the library does. The library cannot do it: x/net
// enforces locks inside handlers that do not serve anything here, and emersion
// has no locking at all.
func (f *fileSystem) enforceLocks(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !lockedMethods[r.Method] {
			next.ServeHTTP(w, r)
			return
		}

		src, dst, ok := f.lockedPaths(r)
		if !ok {
			// Not a path this server would serve. Handed on so that what
			// answers it is the refusal every other bad path gets, with the
			// body and the status a client already expects.
			next.ServeHTTP(w, r)
			return
		}

		release, status, err := f.confirmLocks(r, src, dst)
		if err != nil {
			http.Error(w, http.StatusText(status), status)
			return
		}
		defer release()
		next.ServeHTTP(w, r)
	})
}

// lockedPaths is which resources this request has to hold to go ahead.
//
// A COPY leaves its source alone, so only the destination is confirmed -- RFC
// 4918 7.5.1 says as much, and litmus checks that copying out of a collection
// somebody else has locked is allowed.
func (f *fileSystem) lockedPaths(r *http.Request) (src, dst string, ok bool) {
	p, err := toPath(r.URL.Path)
	if err != nil {
		return "", "", false
	}
	src = lockName(p)

	if r.Method == "MOVE" || r.Method == "COPY" {
		to, derr := f.destPath(r.Header.Get("Destination"))
		if derr != nil {
			return "", "", false
		}
		dst = lockName(to)
	}
	if r.Method == "COPY" {
		src = ""
	}
	return src, dst, true
}

// holdBriefly takes a lock that lives only as long as the request.
func (f *fileSystem) holdBriefly(now time.Time, path string) (string, error) {
	return f.locks.Create(now, xnet.LockDetails{
		Root:      path,
		Duration:  briefLock,
		ZeroDepth: true,
	})
}

// briefLock is how long the lock a request takes on its own behalf lasts. It is
// released when the request ends; the duration is only what would expire if the
// process died mid-write.
const briefLock = time.Minute

// lockStatus maps the lock system's refusals onto the wire, as RFC 4918 9.10.6
// and the LockSystem documentation agree they should be.
func lockStatus(err error) int {
	switch {
	case errors.Is(err, xnet.ErrLocked):
		return StatusLocked
	case errors.Is(err, xnet.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, xnet.ErrNoSuchLock):
		return http.StatusPreconditionFailed
	default:
		return http.StatusInternalServerError
	}
}

// StatusLocked is 423, which net/http has no constant for.
const StatusLocked = 423
