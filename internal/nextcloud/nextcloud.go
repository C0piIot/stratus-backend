// Package nextcloud reads a Nextcloud instance's own database, so that a
// library already sitting in S3 can be adopted rather than copied.
//
// Nextcloud with S3 primary storage builds its bucket the way this server
// does: the object is named `urn:oid:<fileid>`, the bucket is flat, and every
// name, path and tree lives in the database (#24). That is what makes an
// import possible without moving a single byte -- and it holds only while
// nothing on either side parses a blob key, which is why internal/files says
// so where the key is made.
//
// Two halves, in the order they have to happen. The survey reads the instance
// and asks the bucket about every object it names, because Nextcloud's own
// well-known failure is a database and a bucket that have drifted apart and
// discovering that after the rows are written is a data loss event rather than
// a migration. The import then writes the rows, and nothing else: not one byte
// is put, copied or deleted, and the instance it came from is never written to
// at all -- it is opened read-only, since it is probably still running.
//
// Whichever of SQLite, PostgreSQL and MySQL that instance runs on, which are
// the three this server already speaks. See source.go.
package nextcloud

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/C0piIot/stratus-backend/internal/storage"
)

// Defaults for what Nextcloud does not keep in its database.
const (
	// DefaultTablePrefix is `dbtableprefix` from config.php.
	DefaultTablePrefix = "oc_"
	// DefaultObjectPrefix is `objectstore.arguments.objectPrefix`, and it is
	// the one piece of the object's name that is not the file id.
	DefaultObjectPrefix = "urn:oid:"
	// DefaultWorkers bounds the concurrent Stat calls. Each one is a round
	// trip and a camera roll is a hundred thousand of them, so serial is
	// hours; it is also somebody's live instance, so this stays modest.
	DefaultWorkers = 16
)

// maxSamples caps how many missing or mismatched objects are kept by name. The
// count is always exact -- it is the list that is bounded, because a bucket
// that answers for nothing would otherwise be reported one line at a time.
const maxSamples = 1000

// directoryMIME is how Nextcloud marks a folder in the filecache.
const directoryMIME = "httpd/unix-directory"

// userFiles is the only prefix of a home storage that holds the user's own
// tree. Everything beside it -- versions, trash, chunked uploads, caches -- is
// Nextcloud's business and not a file somebody put somewhere.
const userFiles = "files"

// Options says which instance to read and how it names its objects.
type Options struct {
	// Source is the Nextcloud database: a path to its SQLite file, or a
	// `postgres://` or `mysql://` DSN. Those three are what Nextcloud runs on
	// and what this server already speaks, so there is no fourth shape and no
	// instance this cannot be pointed at.
	Source string
	// TablePrefix is Nextcloud's, not ours. Empty means DefaultTablePrefix.
	TablePrefix string
	// User selects the home storage to read. Empty is allowed only when the
	// instance has exactly one, which is the single-user case this server is
	// for; more than one has to be named rather than guessed at.
	User string
	// ObjectPrefix is not in the database -- it is in config.php -- so it is
	// asked for here. Empty means DefaultObjectPrefix.
	ObjectPrefix string
	// Workers is how many objects are asked about at once. Zero means
	// DefaultWorkers.
	Workers int
}

func (o Options) tablePrefix() string {
	if o.TablePrefix == "" {
		return DefaultTablePrefix
	}
	return o.TablePrefix
}

func (o Options) objectPrefix() string {
	if o.ObjectPrefix == "" {
		return DefaultObjectPrefix
	}
	return o.ObjectPrefix
}

func (o Options) workers() int {
	if o.Workers <= 0 {
		return DefaultWorkers
	}
	return o.Workers
}

// Survey reads the instance and asks blobs about every object it claims.
//
// It returns a Report even when the news is bad, because "what is wrong with
// it" is the whole point; an error here means the survey could not be carried
// out at all. The one exception is encryption, which is reported and then
// refused: see Report.Blocked.
func Survey(ctx context.Context, blobs storage.Storage, opts Options) (*Report, error) {
	if opts.Source == "" {
		return nil, errors.New("nextcloud: the database to read is required")
	}
	// A table name cannot be a bind parameter, so the prefix is concatenated
	// into every query below and is checked here instead -- once, before any
	// of them is built. That is what the //nolint:gosec on each of them is
	// standing on.
	if err := validPrefix(opts.tablePrefix()); err != nil {
		return nil, err
	}

	conn, err := open(ctx, opts.Source)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.close() }()

	rep := &Report{
		Database:     conn.name,
		TablePrefix:  opts.tablePrefix(),
		ObjectPrefix: opts.objectPrefix(),
	}

	if rep.Encryption, err = readEncryption(ctx, conn, opts.tablePrefix()); err != nil {
		return nil, err
	}
	if rep.Buckets, err = readBuckets(ctx, conn, opts.tablePrefix()); err != nil {
		return nil, err
	}
	if rep.Storage, err = pickStorage(ctx, conn, opts.tablePrefix(), opts.User); err != nil {
		return nil, err
	}

	// An encrypted instance is not a migration anybody can finish, so the walk
	// is not started: every object in it would be ciphertext this server has
	// no key for, and reporting a hundred thousand files as "present" would be
	// true and completely misleading.
	if rep.Encryption.Blocks() {
		return rep, nil
	}

	if err := rep.walk(ctx, conn, blobs, opts); err != nil {
		return nil, err
	}
	return rep, nil
}

// readEncryption asks the instance three questions about encryption and keeps
// all three answers. The setting and the app can each be on without the other,
// and the filecache rows are the only one of the three that is a fact about
// the bytes rather than about the configuration.
func readEncryption(ctx context.Context, conn *source, prefix string) (Encryption, error) {
	var enc Encryption

	//nolint:gosec // G202: the prefix is checked by validPrefix
	query := "SELECT appid, configkey, configvalue FROM " + prefix + "appconfig" +
		" WHERE (appid = 'core' AND configkey = 'encryption_enabled')" +
		" OR (appid = 'encryption' AND configkey = 'enabled')"
	rows, err := conn.q.QueryContext(ctx, query)
	if err != nil {
		return enc, fmt.Errorf("nextcloud: read appconfig: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var appid, key, value string
		if err := rows.Scan(&appid, &key, &value); err != nil {
			return enc, fmt.Errorf("nextcloud: read appconfig: %w", err)
		}
		on := value == "yes" || value == "1" || value == "true"
		if appid == "core" {
			enc.Enabled = on
		} else {
			enc.AppEnabled = on
		}
	}
	if err := rows.Err(); err != nil {
		return enc, fmt.Errorf("nextcloud: read appconfig: %w", err)
	}

	if err := conn.q.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM "+prefix+"filecache WHERE encrypted <> 0").Scan(&enc.Rows); err != nil {
		return enc, fmt.Errorf("nextcloud: count encrypted rows: %w", err)
	}
	return enc, nil
}

// readBuckets reports the per-user buckets of a multibucket instance.
//
// Nextcloud assigns those at account creation and records them here, so an
// empty result is the single-bucket case -- which is what an adoption in place
// needs, since this server is configured with exactly one bucket.
func readBuckets(ctx context.Context, conn *source, prefix string) ([]UserBucket, error) {
	//nolint:gosec // G202: the prefix is checked by validPrefix
	query := "SELECT userid, configvalue FROM " + prefix + "preferences" +
		" WHERE appid = 'homeobjectstore' AND configkey = 'bucket' ORDER BY userid"
	rows, err := conn.q.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("nextcloud: read per-user buckets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var buckets []UserBucket
	for rows.Next() {
		var b UserBucket
		if err := rows.Scan(&b.User, &b.Bucket); err != nil {
			return nil, fmt.Errorf("nextcloud: read per-user buckets: %w", err)
		}
		buckets = append(buckets, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("nextcloud: read per-user buckets: %w", err)
	}
	return buckets, nil
}

// pickStorage finds the home storage to read.
//
// A home storage is `home::<user>` on a local instance and
// `object::user:<user>` on one with an object store, and those are the only
// two shapes that hold somebody's own files; everything else in the table is
// an external mount or a shared storage, which this does not import.
func pickStorage(ctx context.Context, conn *source, prefix, user string) (Storage, error) {
	//nolint:gosec // G202: the prefix is checked by validPrefix
	rows, err := conn.q.QueryContext(ctx, "SELECT numeric_id, id FROM "+prefix+"storages ORDER BY numeric_id")
	if err != nil {
		return Storage{}, fmt.Errorf("nextcloud: read storages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var homes []Storage
	for rows.Next() {
		var s Storage
		if err := rows.Scan(&s.NumericID, &s.ID); err != nil {
			return Storage{}, fmt.Errorf("nextcloud: read storages: %w", err)
		}
		if owner, ok := homeOwner(s.ID); ok {
			s.User = owner
			homes = append(homes, s)
		}
	}
	if err := rows.Err(); err != nil {
		return Storage{}, fmt.Errorf("nextcloud: read storages: %w", err)
	}

	switch {
	case len(homes) == 0:
		return Storage{}, errors.New("nextcloud: no home storage in this database")
	case user == "" && len(homes) == 1:
		return homes[0], nil
	case user == "":
		names := make([]string, 0, len(homes))
		for _, h := range homes {
			names = append(names, h.User)
		}
		return Storage{}, fmt.Errorf("nextcloud: %d users in this database (%s): name one",
			len(homes), strings.Join(names, ", "))
	}

	for _, h := range homes {
		if h.User == user {
			return h, nil
		}
	}
	return Storage{}, fmt.Errorf("nextcloud: no home storage for user %q", user)
}

// homeOwner reports the user a storage id belongs to, and whether it is a home
// storage at all.
func homeOwner(id string) (string, bool) {
	for _, prefix := range []string{"home::", "object::user:"} {
		if owner, ok := strings.CutPrefix(id, prefix); ok && owner != "" {
			return owner, true
		}
	}
	return "", false
}

// validPrefix refuses anything that is not an identifier.
//
// `dbtableprefix` is somebody's installation choice and arrives here from a
// flag, so it is the one piece of these queries that is not a constant. SQL
// has no placeholder for a table name, so the defence is to accept nothing a
// table name could not be.
func validPrefix(prefix string) error {
	for _, r := range prefix {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
			continue
		}
		return fmt.Errorf("nextcloud: table prefix %q is not an identifier", prefix)
	}
	return nil
}

// walk reads every filecache row of the chosen storage and asks the blob store
// about the ones that are the user's own files.
//
// The rows are streamed rather than collected: a filecache has a row per file
// and this has to work on a library too big to hold in memory, which is the
// same rule the storage port states for List.
func (r *Report) walk(ctx context.Context, conn *source, blobs storage.Storage, opts Options) error {
	prefix := opts.tablePrefix()
	//nolint:gosec // G202: the prefix is checked by validPrefix
	query := "SELECT f.fileid, f.path, f.size, COALESCE(m.mimetype, '') FROM " + prefix + "filecache f" +
		" LEFT JOIN " + prefix + "mimetypes m ON m.id = f.mimetype" +
		" WHERE f.storage = " + conn.arg(1) + " ORDER BY f.fileid"
	rows, err := conn.q.QueryContext(ctx, query, r.Storage.NumericID)
	if err != nil {
		return fmt.Errorf("nextcloud: read filecache: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// Every key the import would write, kept so the bucket can be asked what
	// else is in it. It is the one thing here held in memory rather than
	// streamed, and it is worth it: a million files is some tens of megabytes
	// in a command somebody runs once, and the alternative is not knowing what
	// the sweep is going to find.
	claimed := make(map[string]struct{})

	entries := make(chan Entry)
	results := make(chan result)

	var workers sync.WaitGroup
	for range opts.workers() {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for e := range entries {
				results <- check(ctx, blobs, e)
			}
		}()
	}
	go func() {
		workers.Wait()
		close(results)
	}()

	// The rows are read on this goroutine and the results collected on
	// another, so that a slow bucket does not stop the database being read and
	// a long read does not leave the workers idle.
	collected := make(chan struct{})
	go func() {
		defer close(collected)
		for res := range results {
			r.record(res)
		}
	}()

	scanErr := r.feed(ctx, rows, entries, opts.objectPrefix(), claimed)
	close(entries)
	<-collected
	if scanErr != nil {
		return scanErr
	}
	r.countUnclaimed(ctx, blobs, claimed)
	return nil
}

// countUnclaimed asks the bucket what is in it that this import would not
// adopt.
//
// The sweep in internal/files lists the whole store and puts every object no
// row points at into the trash, so after an import the versions, the trash and
// the other users Nextcloud keeps in the same bucket are on a thirty-day
// clock. That is very often what somebody migrating wants; it is never what
// they want to discover afterwards.
//
// A failure is recorded rather than returned: not being able to list is a
// worse report, not a failed survey.
func (r *Report) countUnclaimed(ctx context.Context, blobs storage.Storage, claimed map[string]struct{}) {
	for info, err := range blobs.List(ctx, "") {
		if err != nil {
			r.UnclaimedErr = err.Error()
			return
		}
		if _, ours := claimed[info.Key]; ours {
			continue
		}
		r.UnclaimedCount++
		r.UnclaimedBytes += max(info.Size, 0)
		if len(r.Unclaimed) < maxSamples {
			r.Unclaimed = append(r.Unclaimed, info.Key)
		}
	}
}

// feed classifies every row and sends the files on to be checked.
func (r *Report) feed(ctx context.Context, rows *sql.Rows, entries chan<- Entry, objectPrefix string, claimed map[string]struct{}) error {
	for rows.Next() {
		var (
			e    Entry
			p    string
			size int64
			mime string
		)
		if err := rows.Scan(&e.FileID, &p, &size, &mime); err != nil {
			return fmt.Errorf("nextcloud: read filecache: %w", err)
		}

		area, rest := split(p)
		if area != userFiles {
			// The root row of the storage has an empty path and is nobody's
			// file; everything else here is versions, trash or a chunked
			// upload, counted by the area it is in so the report can say what
			// it is leaving behind.
			if p != "" {
				r.skip(area, size, mime == directoryMIME)
			}
			continue
		}
		if mime == directoryMIME {
			r.Folders++
			continue
		}

		e.Path = rest
		e.Size = size
		e.MIME = mime
		e.Key = objectPrefix + strconv.FormatInt(e.FileID, 10)
		claimed[e.Key] = struct{}{}
		r.Files.add(size)

		// A negative size is Nextcloud saying it has never scanned the file.
		// Adopting one means adopting a row whose size is a lie, so it is
		// counted and still checked -- the object may well be fine.
		if size < 0 {
			r.Unscanned++
		}

		select {
		case entries <- e:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return rows.Err()
}

// result is one answer from the blob store.
type result struct {
	entry Entry
	size  int64
	err   error
}

// check asks the blob store about one object.
func check(ctx context.Context, blobs storage.Storage, e Entry) result {
	info, err := blobs.Stat(ctx, e.Key)
	if err != nil {
		return result{entry: e, err: err}
	}
	return result{entry: e, size: info.Size}
}

// record folds one answer into the report. Called from one goroutine only.
func (r *Report) record(res result) {
	switch {
	case res.err == nil && (res.entry.Size < 0 || res.size == res.entry.Size):
		r.Present.add(res.size)
	case res.err == nil:
		r.MismatchCount++
		if len(r.Mismatch) < maxSamples {
			r.Mismatch = append(r.Mismatch, Mismatch{Entry: res.entry, Actual: res.size})
		}
	case errors.Is(res.err, storage.ErrNotFound):
		r.MissingCount++
		r.MissingBytes += max(res.entry.Size, 0)
		if len(r.Missing) < maxSamples {
			r.Missing = append(r.Missing, res.entry)
		}
	default:
		r.FailedCount++
		if len(r.Failed) < maxSamples {
			r.Failed = append(r.Failed, Failure{Entry: res.entry, Err: res.err.Error()})
		}
	}
}

// skip counts a row outside the user's own tree, by the area it is in.
func (r *Report) skip(area string, size int64, dir bool) {
	if r.Skipped == nil {
		r.Skipped = map[string]Tally{}
	}
	t := r.Skipped[area]
	if !dir {
		t.add(size)
	} else {
		t.Folders++
	}
	r.Skipped[area] = t
}

// split takes the first segment of a filecache path off the rest. The paths
// are stored with forward slashes and no leading one, whatever the instance
// runs on.
func split(p string) (area, rest string) {
	p = path.Clean("/" + p)[1:]
	area, rest, _ = strings.Cut(p, "/")
	return area, rest
}
