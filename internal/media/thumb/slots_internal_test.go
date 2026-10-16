package thumb

import "testing"

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

// TestDecodeQueue is how many requests the machines above may have in hand at
// once, which is the decoders plus the queue standing in front of them.
func TestDecodeQueue(t *testing.T) {
	t.Parallel()
	for slots, want := range map[int]int{1: 5, 2: 10, 4: 20} {
		if got := decodeQueue(slots); got != want {
			t.Errorf("%d slots: a queue of %d, want %d", slots, got, want)
		}
	}
}
