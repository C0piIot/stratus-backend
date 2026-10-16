package stream

import "testing"

// TestTranscodeSlots: several to a core, and the memory still has the last
// word.
func TestTranscodeSlots(t *testing.T) {
	t.Parallel()
	const mib = 1 << 20
	for name, c := range map[string]struct {
		cpus  int
		limit int64
		want  int
	}{
		"the demo instance: one CPU, 256 MB":      {1, 256 * mib, 4},
		"a Raspberry Pi 4: four CPUs, a gigabyte": {4, 1024 * mib, 16},
		"a container capped at 192 MB":            {8, 192 * mib, 2},
		"smaller than the reserve still gets one": {2, 64 * mib, 1},
		"a limit that could not be read":          {2, 0, 8},
	} {
		if got := transcodeSlots(c.cpus, c.limit); got != c.want {
			t.Errorf("%s: %d slots, want %d", name, got, c.want)
		}
	}
}
