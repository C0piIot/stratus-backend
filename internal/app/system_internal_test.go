package app

import (
	"errors"
	"runtime"
	"testing"
	"testing/fstest"

	"github.com/C0piIot/stratus-backend/internal/config"
	"github.com/C0piIot/stratus-backend/internal/web"
)

func TestMemoryReadsTheCgroupFirst(t *testing.T) {
	t.Parallel()
	root := fstest.MapFS{
		"sys/fs/cgroup/memory.max":     {Data: []byte("536870912\n")},
		"sys/fs/cgroup/memory.current": {Data: []byte("134217728\n")},
		"proc/meminfo":                 {Data: []byte("MemTotal: 8000000 kB\nMemAvailable: 4000000 kB\n")},
	}
	if free, total := memory(root); free != 384<<20 || total != 512<<20 {
		t.Errorf("memory = %d of %d, want the cgroup's 384 MiB of 512", free, total)
	}
}

// "max" is a cgroup with no limit, and then the machine's is the answer.
func TestMemoryFallsBackToTheMachine(t *testing.T) {
	t.Parallel()
	root := fstest.MapFS{
		"sys/fs/cgroup/memory.max":     {Data: []byte("max\n")},
		"sys/fs/cgroup/memory.current": {Data: []byte("1\n")},
		"proc/meminfo": {Data: []byte("MemTotal:        1015764 kB\nMemFree: 1 kB\n" +
			"MemAvailable:     500000 kB\nHugePages_Total: 0\n")},
	}
	if free, total := memory(root); free != 500000<<10 || total != 1015764<<10 {
		t.Errorf("memory = %d of %d, want MemAvailable of MemTotal", free, total)
	}
	if free, total := memory(fstest.MapFS{}); free != 0 || total != 0 {
		t.Errorf("memory with nothing to read = %d of %d, want zeros", free, total)
	}
}

func TestLoadAndKernel(t *testing.T) {
	t.Parallel()
	root := fstest.MapFS{
		"proc/loadavg":              {Data: []byte("0.52 0.31 0.12 1/123 4567\n")},
		"proc/sys/kernel/osrelease": {Data: []byte("6.12.1-fly\n")},
	}
	if got := loadAverage(root); got != "0.52 0.31 0.12" {
		t.Errorf("loadAverage = %q", got)
	}
	if got := kernel(root); got != "6.12.1-fly" {
		t.Errorf("kernel = %q", got)
	}
	empty := fstest.MapFS{"proc/loadavg": {Data: []byte("0.5\n")}}
	if loadAverage(empty) != "" || loadAverage(fstest.MapFS{}) != "" || kernel(fstest.MapFS{}) != "" {
		t.Error("what could not be read was not left empty")
	}
}

func TestSystemReadsThisMachine(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := &App{cfg: config.Config{DataDir: dir, Storage: config.StorageDSN{Scheme: config.SchemeFile, Dir: dir}}}
	s := a.system()
	if s.CPUs < 1 || s.Platform != runtime.GOOS+"/"+runtime.GOARCH || s.Go != runtime.Version() {
		t.Errorf("system = %+v", s)
	}
	if s.DiskFree <= 0 || !s.SharedDisk {
		t.Errorf("a blob store in the data directory: free %d, shared %v", s.DiskFree, s.SharedDisk)
	}
	if freeBytes(dir+"/not-there") != -1 || sameDevice(dir, dir+"/not-there") {
		t.Error("a directory that is not there was measured")
	}
}

func TestTheFirstFailureOfASweepIsKept(t *testing.T) {
	t.Parallel()
	var s web.Sweep
	failed(&s, "emptying the trash", errors.New("first"))
	failed(&s, "collecting orphan blobs", errors.New("second"))
	if s.Err != "emptying the trash: first" {
		t.Errorf("Err = %q", s.Err)
	}

	a := &App{}
	if !a.lastSweep().At.IsZero() {
		t.Error("a sweep before the first")
	}
	a.swept.Store(&s)
	if a.lastSweep().Err != s.Err {
		t.Error("the last sweep is not the one stored")
	}
}
