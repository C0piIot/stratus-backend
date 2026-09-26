package media

import (
	"testing"
	"testing/fstest"
)

// TestDecodeSlots is the machines this runs on, and which of the two limits
// each one hits.
func TestDecodeSlots(t *testing.T) {
	t.Parallel()
	const mib = 1 << 20
	for name, c := range map[string]struct {
		cpus  int
		limit int64
		want  int
	}{
		"the demo instance: one CPU, 256 MB":         {1, 256 * mib, 1},
		"a Raspberry Pi 4: four CPUs, a gigabyte":    {4, 1024 * mib, 4},
		"a small VPS: four CPUs, 512 MB":             {4, 512 * mib, 3},
		"a container capped at 384 MB on a big host": {16, 384 * mib, 2},
		"smaller than the budget still gets one":     {8, 100 * mib, 1},
		"a limit that could not be read":             {6, 0, 6},
		"no CPUs reported":                           {0, 0, 1},
	} {
		if got := decodeSlots(c.cpus, c.limit); got != c.want {
			t.Errorf("%s: %d slots, want %d", name, got, c.want)
		}
	}
}

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
		if got := memoryLimit(c.files); got != c.want {
			t.Errorf("%s: %d, want %d", name, got, c.want)
		}
	}
}
