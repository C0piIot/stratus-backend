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
// there was no lock system to hand it to. There was one for a while --
// x/net/webdav arrived for PROPFIND (#136) and brought a LockSystem with it,
// which held what it knew in the process -- and there is a table now (#243),
// because a promise that depends on which instance answered, and that a
// restart forgets, is not one. What is under this file is dblocks.go.
//
// What is in it is the other half -- refusing a write to something somebody
// else has locked -- and it is our own because x/net enforces locks inside its
// own handlers, and its handlers are not the ones serving PUT here. The
// grammar it needs is vendored beside this, in ifheader.go.

// lockSystem is the locking this server enforces: x/net's four methods, with a
// context and one extra fact about a create.
//
// **A context**, because a lock is a row now (#243). x/net's own interface takes
// none, and an implementation over a database that honoured it literally would
// send every statement out on a background context with a timeout of its own,
// untied from the client waiting for the answer. The four methods are otherwise
// x/net's, so the grammar vendored beside this and the handlers above it did
// not have to change shape.
//
// **ForTheRequest on a create**, because a lock system that outlives the
// process has to tell the two kinds of lock apart. See LockDetails.
//
// There is one implementation of this interface and there is deliberately no
// second one: an in-memory twin kept for the tests would be a second code path
// that could disagree with the first about what a lock means, which is the
// mistake confirmLocks warns about one layer down.
type lockSystem interface {
	Create(ctx context.Context, now time.Time, details LockDetails) (string, error)
	Refresh(ctx context.Context, now time.Time, token string, duration time.Duration) (xnet.LockDetails, error)
	Unlock(ctx context.Context, now time.Time, token string) error
	Confirm(ctx context.Context, now time.Time, name0, name1 string, conditions ...Condition) (func(), error)

	// Covers reports whether that token names a live lock on name, or on a
	// collection above it.
	//
	// The fifth method, and it is a question rather than one of the four. It
	// used to be asked as a Confirm followed immediately by its release, which
	// was the only way to ask x/net's interface -- and against a table that is
	// two statements and a heartbeat started and stopped, to answer something
	// one SELECT knows. An If header is a list of these, so it is the
	// difference between a handful of round trips and one.
	Covers(ctx context.Context, now time.Time, token, name string) bool
}

// LockDetails is x/net's, plus which of the two kinds of lock is being asked
// for.
//
// A client's LOCK is a claim meant to survive this request, this process and
// the next deploy, and it is the client that gives it back. The lock a PUT
// takes so that somebody else's write is refused belongs to the request: it is
// held while the request runs, renewed as it goes, and let go when it ends.
//
// In memory the distinction did not exist and did not need to, because a held
// node cannot expire and a process that dies takes every lock it knew with it.
// In a table both facts stop being true at once.
type LockDetails struct {
	xnet.LockDetails
	// ForTheRequest marks the second kind.
	ForTheRequest bool
}

// opaqueTokens gives the lock system's tokens a scheme, which is the whole of
// what they are missing.
//
// RFC 4918 6.5 says a lock token is a URI, with the Lock-Token header carrying
// a Coded-URL, and Finder is the client this surface exists for (#3), so it is
// not the place to find out who is lenient. What is under here mints an opaque
// string; the spelling a client sees is put on and taken off in one place.
//
// **Unguessable is not among the requirements, and that is worth writing down
// because the instinct says otherwise.** A client that can take a lock is shown
// the shape of them, and a client that cannot take one cannot write either -- a
// share link is read-only and Basic is the gate in front of everything else.
// What guessing a token would let somebody do, they can already do with the
// password they had to have.
type opaqueTokens struct {
	lockSystem
}

// lockURIScheme is RFC 4918's own, and the one clients recognise. Named for
// the scheme rather than for what it prefixes, because gosec reads a constant
// with "token" in its name and a string in it as a credential somebody left
// in the source.
const lockURIScheme = "opaquelocktoken:"

func withOpaqueTokens(ls lockSystem) lockSystem {
	return &opaqueTokens{lockSystem: ls}
}

func (o *opaqueTokens) Create(ctx context.Context, now time.Time, details LockDetails) (string, error) {
	token, err := o.lockSystem.Create(ctx, now, details)
	if err != nil {
		return "", err
	}
	return lockURIScheme + token, nil
}

func (o *opaqueTokens) Refresh(ctx context.Context, now time.Time, token string, duration time.Duration) (xnet.LockDetails, error) {
	return o.lockSystem.Refresh(ctx, now, innerToken(token), duration)
}

func (o *opaqueTokens) Unlock(ctx context.Context, now time.Time, token string) error {
	return o.lockSystem.Unlock(ctx, now, innerToken(token))
}

func (o *opaqueTokens) Covers(ctx context.Context, now time.Time, token, name string) bool {
	return o.lockSystem.Covers(ctx, now, innerToken(token), name)
}

func (o *opaqueTokens) Confirm(ctx context.Context, now time.Time, name0, name1 string, conditions ...Condition) (func(), error) {
	ours := make([]Condition, len(conditions))
	for i, c := range conditions {
		c.Token = innerToken(c.Token)
		ours[i] = c
	}
	return o.lockSystem.Confirm(ctx, now, name0, name1, ours...)
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

// boundLocks is the lock system as x/net wants it, tied to one request.
//
// x/net's Handler takes a LockSystem whose methods carry no context, and
// PROPFIND is served by it (see propfind.go). Binding the request's context
// here is what lets the interface above keep one and this one keep its shape.
type boundLocks struct {
	locks lockSystem
	ctx   context.Context
}

func (b boundLocks) Create(now time.Time, details xnet.LockDetails) (string, error) {
	return b.locks.Create(b.ctx, now, LockDetails{LockDetails: details})
}

func (b boundLocks) Refresh(now time.Time, token string, duration time.Duration) (xnet.LockDetails, error) {
	return b.locks.Refresh(b.ctx, now, token, duration)
}

func (b boundLocks) Unlock(now time.Time, token string) error {
	return b.locks.Unlock(b.ctx, now, token)
}

func (b boundLocks) Confirm(now time.Time, name0, name1 string, conditions ...Condition) (func(), error) {
	return b.locks.Confirm(b.ctx, now, name0, name1, conditions...)
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
	ctx := r.Context()
	header := r.Header.Get("If")
	if header == "" {
		return f.lockForTheRequest(ctx, src, dst)
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
		if !f.holds(ctx, now, list.conditions, about) {
			continue
		}

		release, cerr := f.claimTouched(ctx, now, src, dst, claimable(list.conditions))
		if cerr == nil {
			return release, 0, nil
		}
		if !errors.Is(cerr, xnet.ErrConfirmationFailed) {
			return nil, http.StatusInternalServerError, cerr
		}
		// The list was true and claimed no lock: a condition on the ETag
		// alone, or a negative one. What is touched still has to be free.
		if release, _, lerr := f.lockForTheRequest(ctx, src, dst); lerr == nil {
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
			ok = f.tokenCovers(ctx, now, c.Token, name)
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
// knows, and asked as a question: nothing is claimed and nothing is held.
func (f *fileSystem) tokenCovers(ctx context.Context, now time.Time, token, name string) bool {
	return f.locks.Covers(ctx, now, token, name)
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
func (f *fileSystem) claimTouched(ctx context.Context, now time.Time, src, dst string, conditions []Condition) (func(), error) {
	// Both at once first, because one lock on a collection can cover both ends
	// of a move inside it and the lock system hands a node out once: asking
	// for the two names separately would confirm the lock and then find it
	// held by ourselves.
	release, err := f.locks.Confirm(ctx, now, src, dst, conditions...)
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
	if release, ok := f.claimOneHoldTheOther(ctx, now, src, dst, conditions); ok {
		return release, nil
	}
	if src != "" {
		if release, ok := f.claimOneHoldTheOther(ctx, now, dst, src, conditions); ok {
			return release, nil
		}
	}
	return nil, xnet.ErrConfirmationFailed
}

// claimOneHoldTheOther confirms one resource against the conditions and takes
// the other for the length of the request, and reports whether both worked.
func (f *fileSystem) claimOneHoldTheOther(ctx context.Context, now time.Time, claimed, free string, conditions []Condition) (func(), bool) {
	release, err := f.locks.Confirm(ctx, now, claimed, "", conditions...)
	if err != nil {
		return nil, false
	}
	token, err := f.holdBriefly(ctx, now, free)
	if err != nil {
		release()
		return nil, false
	}
	return func() {
		_ = f.locks.Unlock(ctx, now, token)
		release()
	}, true
}

// lockForTheRequest is the no-If case: hold both paths for the length of the
// request so that somebody else's lock refuses it.
func (f *fileSystem) lockForTheRequest(ctx context.Context, src, dst string) (func(), int, error) {
	now := f.now()
	var srcToken, dstToken string
	var err error

	if src != "" {
		if srcToken, err = f.holdBriefly(ctx, now, src); err != nil {
			return nil, lockStatus(err), err
		}
	}
	if dst != "" {
		if dstToken, err = f.holdBriefly(ctx, now, dst); err != nil {
			if srcToken != "" {
				_ = f.locks.Unlock(ctx, now, srcToken)
			}
			return nil, lockStatus(err), err
		}
	}

	return func() {
		if dstToken != "" {
			_ = f.locks.Unlock(ctx, now, dstToken)
		}
		if srcToken != "" {
			_ = f.locks.Unlock(ctx, now, srcToken)
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

		if _, err := f.owner(r.Context()); err != nil {
			// No authenticated user, which is a routing mistake and not a lock
			// question. Handed on so the answer is the 401 the backend gives
			// every other method, rather than a 403 out of a lock system that
			// has no owner to look one up for.
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
func (f *fileSystem) holdBriefly(ctx context.Context, now time.Time, path string) (string, error) {
	return f.locks.Create(ctx, now, LockDetails{
		LockDetails: xnet.LockDetails{
			Root:      path,
			Duration:  briefLock,
			ZeroDepth: true,
		},
		ForTheRequest: true,
	})
}

// briefLock is how long the lock a request takes on its own behalf lasts
// without being renewed.
//
// It is released when the request ends, and renewed while the request runs, so
// this is only what an instance that died mid-write leaves behind: a minute of
// 423 on that one path. Long enough that the renewal has several chances to
// land, short enough that nobody waits on a process that is not coming back.
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
