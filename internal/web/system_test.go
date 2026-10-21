package web_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/auth"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/web"
)

// statusWith is the status page over a handler that reports the process, its
// last sweep and the machine as given.
func statusWith(t *testing.T, started time.Time, sweep web.Sweep, sys web.System) string {
	t.Helper()
	blobs, meta := backends(t)
	s := files.New(blobs, meta)
	creds := credentials()
	h := web.Handler(web.Deps{
		Version: version, BuildDate: buildDate, Verifier: creds,
		Sessions: auth.NewSessions(creds, auth.DefaultSessionTTL), Shares: auth.NewShares(creds),
		Files: s, Indexing: indexing(meta), TrashRetention: aMonth,
		Started: started, Database: "sqlite", Storage: "file",
		Sweep:  func() web.Sweep { return sweep },
		System: func() web.System { return sys },
	})
	return get(t, h, "/status", signIn(t, h)).Body.String()
}

func TestStatusSaysWhatItRunsOn(t *testing.T) {
	t.Parallel()
	body := statusWith(t, time.Now().Add(-26*time.Hour), web.Sweep{}, web.System{
		DiskFree: 3 << 30, MemFree: 200 << 20, MemTotal: 512 << 20, CPUs: 2,
		Load: "0.10 0.20 0.30", Platform: "linux/arm64", Kernel: "6.12.1", Go: "go1.27.3",
	})
	for _, want := range []string{
		`<label for="indexed">Indexed</label>`,
		"1 d 2 h", "sqlite · file",
		"200.0 MiB of 512.0 MiB", "load 0.10 0.20 0.30",
		"linux/arm64", "kernel 6.12.1", "go1.27.3",
		"Room for scratch", "3.0 GiB",
		"not yet since the start",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the status page does not say %q:\n%s", want, between(body, "System"))
		}
	}
}

// A blob store on the data directory's own disk is one number, said once.
func TestStatusSaysASharedDiskOnce(t *testing.T) {
	t.Parallel()
	body := statusWith(t, time.Now(), web.Sweep{}, web.System{DiskFree: 3 << 30, SharedDisk: true})
	if strings.Contains(body, "data-free-space") || strings.Contains(body, "Room for scratch") {
		t.Errorf("a shared disk is said twice:\n%s", between(body, "System"))
	}
	if !strings.Contains(body, "Room left") {
		t.Errorf("a shared disk is not said at all:\n%s", between(body, "System"))
	}
}

// What could not be read is left out rather than shown as a zero.
func TestStatusLeavesOutWhatItCouldNotRead(t *testing.T) {
	t.Parallel()
	body := statusWith(t, time.Now().Add(-90*time.Minute), web.Sweep{}, web.System{DiskFree: -1, CPUs: 1, Platform: "linux/amd64", Go: "go1.27.3"})
	for _, absent := range []string{"data-disk", "data-memory", "load ", "kernel "} {
		if strings.Contains(body, absent) {
			t.Errorf("the status page shows %q, which nothing could read", absent)
		}
	}
	if !strings.Contains(body, "1 h 30 min") {
		t.Errorf("the uptime is not in hours and minutes:\n%s", between(body, "data-running"))
	}
}

func TestStatusSaysWhatTheLastSweepDid(t *testing.T) {
	t.Parallel()
	body := statusWith(t, time.Now().Add(-5*time.Minute), web.Sweep{
		At: time.Now(), Emptied: 4, Deleted: 2, Unaccounted: 1, Uploads: 3, Freed: 5 << 20,
		Err: "emptying the trash: " + errors.New("the store went away").Error(),
	}, web.System{})
	for _, want := range []string{
		"5.0 MiB freed", "4 out of the trash", "2 orphans deleted", "1 unaccounted for",
		"3 abandoned uploads", "the store went away", "5 min",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the status page does not say %q:\n%s", want, between(body, "data-sweep"))
		}
	}
}
