package app

import (
	"bufio"
	"bytes"
	"io/fs"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/C0piIot/stratus-backend/internal/config"
	"github.com/C0piIot/stratus-backend/internal/web"
)

// system is what the status page says about the machine, read when it is
// asked for: it is a page somebody opens, not a metric anything samples.
func (a *App) system() web.System {
	root := os.DirFS("/")
	s := web.System{
		CPUs:     runtime.GOMAXPROCS(0),
		Load:     loadAverage(root),
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
		Kernel:   kernel(root),
		Go:       runtime.Version(),
	}
	s.MemFree, s.MemTotal = memory(root)
	// The data directory and not the blob store, which may be a bucket: it is
	// where the spools and the windows a decoder reads are written, and where
	// the SQLite database lives.
	s.DiskFree = freeBytes(a.cfg.DataDir)
	s.SharedDisk = a.cfg.Storage.Scheme == config.SchemeFile && sameDevice(a.cfg.DataDir, a.cfg.Storage.Dir)
	return s
}

// memory is how much of the memory this process may use is not in use, and
// how much that is, or zeros when neither place says. The same two places as
// media.MemoryLimit and in the same order: the cgroup's, which is what the
// kernel kills by, and then the machine's, which is the whole answer on Fly.
func memory(root fs.FS) (free, total int64) {
	limit, lerr := readInt(root, "sys/fs/cgroup/memory.max")
	used, uerr := readInt(root, "sys/fs/cgroup/memory.current")
	if lerr == nil && uerr == nil && limit > 0 {
		return max(limit-used, 0), limit
	}
	b, err := fs.ReadFile(root, "proc/meminfo")
	if err != nil {
		return 0, 0
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 3 || fields[2] != "kB" {
			continue
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total = kb << 10
		case "MemAvailable:":
			free = kb << 10
		}
	}
	return free, total
}

// loadAverage is the one, five and fifteen minute load, as the kernel writes
// it, or empty where there is no /proc.
func loadAverage(root fs.FS) string {
	b, err := fs.ReadFile(root, "proc/loadavg")
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(b))
	if len(fields) < 3 {
		return ""
	}
	return strings.Join(fields[:3], " ")
}

// kernel is the running kernel's release, which is the host's even inside a
// container, or empty where there is no /proc.
func kernel(root fs.FS) string {
	b, err := fs.ReadFile(root, "proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// readInt reads a file holding one number; "max" is not one.
func readInt(root fs.FS, name string) (int64, error) {
	b, err := fs.ReadFile(root, name)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}
