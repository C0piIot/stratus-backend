package nextcloud

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/storage"
)

// batchSize is how many rows go in at a time. One transaction for a whole
// library would hold a write lock for as long as the import runs, and one per
// row would be a round trip per photograph.
const batchSize = 500

// Destination is the half of files.Service an import needs, declared here so
// that the dependency points inwards -- the same shape internal/incoming
// takes over the same service.
type Destination interface {
	Adopt(ctx context.Context, owner string, rows []db.File) (adopted, skipped int, err error)
}

// ImportOptions is Options plus the two things only writing has an opinion
// about.
type ImportOptions struct {
	Options

	// Into puts the adopted tree under this path instead of at the root of the
	// user's. Empty is the root, which is what a migration from an instance
	// somebody is replacing wants.
	Into string

	// ETag asks for this server's real validator, which means reading every
	// object to hash it.
	//
	// Off by default, and the reason is that Nextcloud has no content hash to
	// hand over: `oc_filecache.checksum` is empty unless a client volunteered
	// one, and the `etag` column beside it is a change token rather than a
	// digest of anything. An ETag here means SHA-256 of the bytes, so the
	// honest import writes none -- WebDAV then synthesises a validator from
	// the size and the modification time, and a client that cannot verify
	// knows it cannot. Turning this on buys the real thing for the price of
	// reading the whole library out of the bucket.
	ETag bool
}

// ImportReport is what the write half did.
type ImportReport struct {
	Owner string
	Into  string

	Files   int64
	Folders int64
	Bytes   int64

	// Adopted and Skipped come back from the destination: skipped is a path
	// that was already taken, which is what makes a second run of an
	// interrupted import finish rather than overwrite.
	Adopted int64
	Skipped int64

	// Refused are rows this server will not store under the name Nextcloud
	// gave them. Counted and named rather than fatal: one impossible path out
	// of a hundred thousand is not a reason to abandon a migration.
	RefusedCount int64
	Refused      []Refusal

	// Hashed is how many objects were read to compute an ETag, which is zero
	// unless it was asked for.
	Hashed int64
}

// Refusal is one row that could not be adopted, and why.
type Refusal struct {
	Path string
	Err  string
}

// Import writes the rows. The bytes stay exactly where Nextcloud put them.
//
// It is deliberately a second pass over the database rather than something the
// survey does on its way through: the survey is what decides whether this
// should happen at all, and a caller runs it first. What this adds is the
// tree, in path order so a parent is always written before what is under it --
// a parent is a prefix of its children, so that falls out of the ordering
// rather than needing a sort of our own.
// owner is this server's single user, which is a different thing from
// opts.User: that one says whose files to read out of the instance, and this
// one says who owns them here.
func Import(
	ctx context.Context,
	blobs storage.Storage,
	dest Destination,
	owner string,
	opts ImportOptions,
) (*ImportReport, error) {
	if owner == "" {
		return nil, errors.New("nextcloud: an import needs the owner to file things under")
	}
	into := strings.Trim(opts.Into, "/")
	if into != "" {
		if err := db.ValidatePath(into); err != nil {
			return nil, fmt.Errorf("nextcloud: --into: %w", err)
		}
	}
	if opts.Source == "" {
		return nil, errors.New("nextcloud: the database to read is required")
	}
	if err := validPrefix(opts.tablePrefix()); err != nil {
		return nil, err
	}

	conn, err := open(ctx, opts.Source)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.close() }()

	home, err := pickStorage(ctx, conn, opts.tablePrefix(), opts.User)
	if err != nil {
		return nil, err
	}

	rep := &ImportReport{Owner: owner, Into: into}
	if err := rep.adopt(ctx, conn, blobs, dest, home, opts, into); err != nil {
		return nil, err
	}
	return rep, nil
}

// adopt streams the filecache and hands it over a batch at a time.
func (rep *ImportReport) adopt(
	ctx context.Context,
	conn *source,
	blobs storage.Storage,
	dest Destination,
	home Storage,
	opts ImportOptions,
	into string,
) error {
	prefix := opts.tablePrefix()
	//nolint:gosec // G202: the prefix is checked by validPrefix
	query := "SELECT f.fileid, f.path, f.size, f.mtime, COALESCE(m.mimetype, '') FROM " + prefix + "filecache f" +
		" LEFT JOIN " + prefix + "mimetypes m ON m.id = f.mimetype" +
		" WHERE f.storage = " + conn.arg(1) + " ORDER BY f.path"
	rows, err := conn.q.QueryContext(ctx, query, home.NumericID)
	if err != nil {
		return fmt.Errorf("nextcloud: read filecache: %w", err)
	}
	defer func() { _ = rows.Close() }()

	batch := make([]db.File, 0, batchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if opts.ETag {
			if herr := rep.hash(ctx, blobs, batch, opts.workers()); herr != nil {
				return herr
			}
		}
		adopted, skipped, aerr := dest.Adopt(ctx, rep.Owner, batch)
		if aerr != nil {
			return fmt.Errorf("nextcloud: adopt %d rows: %w", len(batch), aerr)
		}
		rep.Adopted += int64(adopted)
		rep.Skipped += int64(skipped)
		batch = batch[:0]
		return nil
	}

	for rows.Next() {
		var (
			fileID     int64
			p          string
			size, when int64
			mime       string
		)
		if serr := rows.Scan(&fileID, &p, &size, &when, &mime); serr != nil {
			return fmt.Errorf("nextcloud: read filecache: %w", serr)
		}

		area, rest := split(p)
		// The storage's own root row has an empty path and is not a folder
		// anybody made; everything outside `files/` is Nextcloud's business
		// and the survey already said what is being left behind.
		if area != userFiles || rest == "" {
			continue
		}

		row := db.File{
			Path:  under(into, rest),
			MTime: time.Unix(when, 0).UTC(),
			IsDir: mime == directoryMIME,
		}
		if !row.IsDir {
			row.BlobKey = opts.objectPrefix() + strconv.FormatInt(fileID, 10)
			row.Size = max(size, 0)
			row.MIMEType = mime
		}

		if verr := db.ValidatePath(row.Path); verr != nil {
			rep.RefusedCount++
			if len(rep.Refused) < maxSamples {
				rep.Refused = append(rep.Refused, Refusal{Path: row.Path, Err: verr.Error()})
			}
			continue
		}

		if row.IsDir {
			rep.Folders++
		} else {
			rep.Files++
			rep.Bytes += row.Size
		}

		batch = append(batch, row)
		if len(batch) == batchSize {
			if ferr := flush(); ferr != nil {
				return ferr
			}
		}
	}
	if rerr := rows.Err(); rerr != nil {
		return fmt.Errorf("nextcloud: read filecache: %w", rerr)
	}
	return flush()
}

// hash fills in the ETag of every file in the batch by reading its object.
//
// Concurrent for the reason the survey's Stat calls are: each one is a round
// trip to a bucket, and a camera roll is a great many of them.
func (rep *ImportReport) hash(ctx context.Context, blobs storage.Storage, batch []db.File, workers int) error {
	indexes := make(chan int)
	errs := make(chan error, workers)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range indexes {
				tag, err := digest(ctx, blobs, batch[i].BlobKey)
				if err != nil {
					select {
					case errs <- err:
					default:
					}
					return
				}
				batch[i].ETag = tag
			}
		}()
	}

	for i := range batch {
		if batch[i].IsDir {
			continue
		}
		select {
		case indexes <- i:
			rep.Hashed++
		case <-ctx.Done():
			close(indexes)
			wg.Wait()
			return ctx.Err()
		}
	}
	close(indexes)
	wg.Wait()

	select {
	case err := <-errs:
		return fmt.Errorf("nextcloud: read an object to hash it: %w", err)
	default:
		return nil
	}
}

// digest is this server's ETag: the SHA-256 of the bytes, unquoted, which is
// what internal/files writes for a file that arrived through it.
func digest(ctx context.Context, blobs storage.Storage, key string) (string, error) {
	body, _, err := blobs.Get(ctx, key, storage.All())
	if err != nil {
		return "", err
	}
	defer func() { _ = body.Close() }()

	sum := sha256.New()
	if _, err := io.Copy(sum, body); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// under puts the tree below --into, if there is one.
func under(into, rest string) string {
	if into == "" {
		return rest
	}
	return into + "/" + rest
}

// Render writes what the import did.
func (rep *ImportReport) Render(w io.Writer) error {
	b := &strings.Builder{}

	fmt.Fprintf(b, "\nImported\n")
	fmt.Fprintf(b, "  owner          %s\n", rep.Owner)
	if rep.Into != "" {
		fmt.Fprintf(b, "  under          %s/\n", rep.Into)
	}
	fmt.Fprintf(b, "  files          %s  %s\n", count(rep.Files), humanBytes(rep.Bytes))
	fmt.Fprintf(b, "  folders        %s\n", count(rep.Folders))
	fmt.Fprintf(b, "  rows written   %s\n", count(rep.Adopted))
	if rep.Skipped > 0 {
		fmt.Fprintf(b, "  already there  %s\n", count(rep.Skipped))
	}
	if rep.Hashed > 0 {
		fmt.Fprintf(b, "  objects hashed %s\n", count(rep.Hashed))
	}
	if rep.RefusedCount > 0 {
		fmt.Fprintf(b, "  refused        %s\n", count(rep.RefusedCount))
		samples(b, "refused", len(rep.Refused), rep.RefusedCount, func(i int) string {
			return fmt.Sprintf("%s: %s", rep.Refused[i].Path, rep.Refused[i].Err)
		})
	}

	b.WriteString("\nNot a byte was moved: the objects are where Nextcloud left them, and the\n")
	b.WriteString("rows now point at them. Nextcloud's own database was not written to.\n")
	if rep.Hashed == 0 {
		b.WriteString("\nThe rows carry no ETag, because Nextcloud keeps no hash of the content to\n")
		b.WriteString("adopt. WebDAV answers a validator made from the size and the modification\n")
		b.WriteString("time instead; --etag reads the library and computes the real one.\n")
	}

	_, err := io.WriteString(w, b.String())
	return err
}
