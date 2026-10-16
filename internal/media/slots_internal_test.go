package media

import (
	"testing"
	"testing/fstest"
)

// TestMemoryLimit reads the two places a limit is kept, in the order the
// kernel would enforce them.
func TestMemoryLimit(t *testing.T) {
	t.Parallel()
	meminfo := &fstest.MapFile{Data: []byte("MemTotal:         250880 kB\nMemFree:          100000 kB\n")}
	for name, c := range map[string]struct {
		files fstest.MapFS
		want  int64
	}{
		"a container's cgroup limit": {
			fstest.MapFS{"sys/fs/cgroup/memory.max": {Data: []byte("536870912\n")}, "proc/meminfo": meminfo},
			536_870_912,
		},
		"a cgroup with no limit falls back to the machine": {
			fstest.MapFS{"sys/fs/cgroup/memory.max": {Data: []byte("max\n")}, "proc/meminfo": meminfo},
			250_880 << 10,
		},
		"a microVM with no cgroup, which is Fly": {
			fstest.MapFS{"proc/meminfo": meminfo},
			250_880 << 10,
		},
		"neither file": {fstest.MapFS{}, 0},
		"a meminfo with no total": {
			fstest.MapFS{"proc/meminfo": {Data: []byte("MemFree: 1 kB\n")}},
			0,
		},
	} {
		if got := MemoryLimit(c.files); got != c.want {
			t.Errorf("%s: %d, want %d", name, got, c.want)
		}
	}
}
