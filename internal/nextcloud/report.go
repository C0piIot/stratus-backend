package nextcloud

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Report is what the survey found. Every count is exact; the lists beside them
// are capped at maxSamples, because the useful thing about ten thousand
// missing objects is the number and the first few names.
type Report struct {
	Database     string
	TablePrefix  string
	ObjectPrefix string
	Storage      Storage
	Encryption   Encryption
	// Buckets is empty on a single-bucket instance, which is the only shape
	// that can be adopted in place: this server is configured with one bucket.
	Buckets []UserBucket

	// Files and Folders are the user's own tree, under `files/`.
	Files   Tally
	Folders int64
	// Skipped is everything else in the home storage, by its top-level area:
	// files_versions, files_trashbin, uploads, cache.
	Skipped map[string]Tally
	// Unscanned counts rows Nextcloud never sized, which it writes as -1.
	Unscanned int64

	// Unclaimed counts the objects in the bucket that no row this import
	// would write points at: Nextcloud's versions and trash, another user's
	// files, the instance's own appdata. They matter because this server's
	// sweep lists the whole bucket and puts what no row claims into the
	// trash, so they are a thing to know before the import rather than a
	// month afterwards.
	UnclaimedCount int64
	UnclaimedBytes int64
	Unclaimed      []string
	// UnclaimedErr is why the bucket could not be listed, if it could not.
	// Reported rather than swallowed: a zero that means "not asked" and a
	// zero that means "nothing there" are different answers.
	UnclaimedErr string

	Present       Tally
	MissingCount  int64
	MissingBytes  int64
	Missing       []Entry
	MismatchCount int64
	Mismatch      []Mismatch
	FailedCount   int64
	Failed        []Failure
}

// Storage is the home storage the survey read.
type Storage struct {
	NumericID int64
	ID        string
	User      string
}

// UserBucket is one row of a multibucket instance's per-user mapping.
type UserBucket struct {
	User   string
	Bucket string
}

// Encryption is what the instance says about server-side encryption, from the
// three places that can say it.
type Encryption struct {
	// Enabled is core's `encryption_enabled`.
	Enabled bool
	// AppEnabled is whether the encryption app itself is on.
	AppEnabled bool
	// Rows is how many filecache rows are marked encrypted, which is the only
	// one of the three that is a fact about the bytes.
	Rows int64
}

// Blocks reports whether adopting this instance in place is impossible.
//
// Any of the three is enough: the objects would be ciphertext under keys this
// server does not have, and importing them would produce a library of noise
// that looks exactly like a successful migration until somebody opens a file.
func (e Encryption) Blocks() bool { return e.Enabled || e.AppEnabled || e.Rows > 0 }

// Blocked reports whether the survey stopped before it walked the library.
func (r *Report) Blocked() bool { return r.Encryption.Blocks() }

// Entry is one file in the user's tree, with the object name it already has.
type Entry struct {
	FileID int64
	// Path is relative to the user's `files/`, which is where this server
	// would hold it.
	Path string
	Size int64
	MIME string
	// Key is the object this file is already stored under, and the whole
	// reason the migration can move no bytes.
	Key string
}

// Mismatch is a file whose object is there and the wrong length.
type Mismatch struct {
	Entry
	Actual int64
}

// Failure is a file whose object could not be asked about at all, which is a
// different thing from one that is missing: a refused request says nothing
// about whether the object exists.
type Failure struct {
	Entry
	Err string
}

// Tally is a count of files, a count of folders and the bytes they cover.
type Tally struct {
	Files   int64
	Folders int64
	Bytes   int64
}

func (t *Tally) add(size int64) {
	t.Files++
	if size > 0 {
		t.Bytes += size
	}
}

// Adoptable reports whether every object the database claims was found, with
// the length the database says. It is the question the whole survey is for.
func (r *Report) Adoptable() bool {
	return !r.Blocked() && len(r.Buckets) == 0 &&
		r.MissingCount == 0 && r.MismatchCount == 0 && r.FailedCount == 0
}

// Render writes the report the way somebody about to migrate reads one.
func (r *Report) Render(w io.Writer) error {
	b := &strings.Builder{}

	fmt.Fprintf(b, "Nextcloud survey\n")
	fmt.Fprintf(b, "  database       %s (tables %s*)\n", r.Database, r.TablePrefix)
	fmt.Fprintf(b, "  user           %s (storage %s, id %d)\n", r.Storage.User, r.Storage.ID, r.Storage.NumericID)
	fmt.Fprintf(b, "  object prefix  %s\n", r.ObjectPrefix)

	b.WriteString("\nBefore anything can be adopted\n")
	fmt.Fprintf(b, "  encryption app         %s\n", onOff(r.Encryption.AppEnabled))
	fmt.Fprintf(b, "  encryption_enabled     %s\n", onOff(r.Encryption.Enabled))
	fmt.Fprintf(b, "  files marked encrypted %s\n", count(r.Encryption.Rows))
	if len(r.Buckets) == 0 {
		b.WriteString("  bucket per user        no, one bucket for the instance\n")
	} else {
		fmt.Fprintf(b, "  bucket per user        yes, %s mapped -- this instance is multibucket\n", count(int64(len(r.Buckets))))
		for _, u := range r.Buckets {
			fmt.Fprintf(b, "                           %s -> %s\n", u.User, u.Bucket)
		}
	}

	if r.Blocked() {
		b.WriteString("\nServer-side encryption is on, so the objects are ciphertext under keys\n")
		b.WriteString("this server does not have. The library was not walked. Nothing was written.\n")
		_, err := io.WriteString(w, b.String())
		return err
	}

	b.WriteString("\nThe library\n")
	fmt.Fprintf(b, "  files    %s  %s\n", count(r.Files.Files), humanBytes(r.Files.Bytes))
	fmt.Fprintf(b, "  folders  %s\n", count(r.Folders))
	if r.Unscanned > 0 {
		fmt.Fprintf(b, "  never scanned by Nextcloud (size -1)  %s\n", count(r.Unscanned))
	}
	for _, area := range sorted(r.Skipped) {
		t := r.Skipped[area]
		fmt.Fprintf(b, "  skipped: %-16s %s  %s\n", area, count(t.Files), humanBytes(t.Bytes))
	}

	b.WriteString("\nThe bucket, asked about every one of those files\n")
	fmt.Fprintf(b, "  present        %s  %s\n", count(r.Present.Files), humanBytes(r.Present.Bytes))
	fmt.Fprintf(b, "  missing        %s  %s\n", count(r.MissingCount), humanBytes(r.MissingBytes))
	fmt.Fprintf(b, "  wrong size     %s\n", count(r.MismatchCount))
	fmt.Fprintf(b, "  not answered   %s\n", count(r.FailedCount))

	samples(b, "missing", len(r.Missing), r.MissingCount, func(i int) string {
		return fmt.Sprintf("%s  %s", r.Missing[i].Key, r.Missing[i].Path)
	})
	samples(b, "wrong size", len(r.Mismatch), r.MismatchCount, func(i int) string {
		m := r.Mismatch[i]
		// Exact bytes rather than humanBytes: two sizes that differ by one
		// byte round to the same string, and a line whose whole job is to show
		// a disagreement would then show none.
		return fmt.Sprintf("%s  %s (database %s bytes, bucket %s bytes)", m.Key, m.Path, count(m.Size), count(m.Actual))
	})
	samples(b, "not answered", len(r.Failed), r.FailedCount, func(i int) string {
		return fmt.Sprintf("%s  %s: %s", r.Failed[i].Key, r.Failed[i].Path, r.Failed[i].Err)
	})

	b.WriteString("\nThe rest of the bucket\n")
	switch {
	case r.UnclaimedErr != "":
		fmt.Fprintf(b, "  could not be listed: %s\n", r.UnclaimedErr)
	case r.UnclaimedCount == 0:
		b.WriteString("  nothing else is in it\n")
	default:
		fmt.Fprintf(b, "  objects no imported row would claim  %s  %s\n",
			count(r.UnclaimedCount), humanBytes(r.UnclaimedBytes))
		samples(b, "unclaimed", len(r.Unclaimed), r.UnclaimedCount, func(i int) string {
			return r.Unclaimed[i]
		})
		b.WriteString("\n  Those are Nextcloud's versions, its trash, another user's files or the\n")
		b.WriteString("  instance's own appdata. This server's sweep lists the whole bucket and\n")
		b.WriteString("  puts what no row claims into the trash, where it waits thirty days.\n")
	}

	if r.Adoptable() {
		b.WriteString("\nEvery object the database claims is in the bucket at the length it claims.\n")
		b.WriteString("This library can be adopted without moving a byte.\n")
	} else {
		b.WriteString("\nThe database and the bucket do not agree. Importing now would write rows\n")
		b.WriteString("pointing at objects that are not there.\n")
	}
	b.WriteString("Nothing was written: this is a survey.\n")

	_, err := io.WriteString(w, b.String())
	return err
}

// samples prints the first few of a list and says how many were not printed.
func samples(b *strings.Builder, label string, shown int, total int64, line func(int) string) {
	if shown == 0 {
		return
	}
	fmt.Fprintf(b, "\n  %s:\n", label)
	for i := range shown {
		fmt.Fprintf(b, "    %s\n", line(i))
	}
	if rest := total - int64(shown); rest > 0 {
		fmt.Fprintf(b, "    ... and %s more\n", count(rest))
	}
}

func sorted(m map[string]Tally) []string {
	areas := make([]string, 0, len(m))
	for area := range m {
		areas = append(areas, area)
	}
	sort.Strings(areas)
	return areas
}

func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "off"
}

// count groups digits so that six figures can be read at a glance, which is
// the order of magnitude a camera roll arrives in.
func count(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// humanBytes renders a byte count the way somebody reads one, in binary units
// and their real names. A copy of internal/web's, which a feature package
// cannot import: ten lines duplicated rather than a package created to hold
// them.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for rest := n / unit; rest >= unit; rest /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
