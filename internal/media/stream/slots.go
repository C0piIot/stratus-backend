package stream

import (
	"os"
	"runtime"

	"github.com/stratus-private-cloud/stratus-backend/internal/media"
)

// How many transcodes may run at once, worked out from the machine the way
// the thumbnails' decoders are: see media.MemoryLimit.

// transcodeBudget is what one audio transcode is allowed: measured at seven
// megabytes for MP3, Opus and AAC out of a 96 kHz FLAC and fourteen for FLAC,
// so a comfortable multiple of the worst. perCPU is how many share a core,
// which is several: a transcode encodes faster than anybody listens and spends
// the rest of the track waiting for the client to read.
const (
	transcodeBudget  = 32 << 20
	transcodesPerCPU = 4
)

// transcodes is how many audio transcodes may run at once (transcoder.go).
func transcodes() int {
	return transcodeSlots(runtime.GOMAXPROCS(0), media.MemoryLimit(os.DirFS("/")))
}

// transcodeSlots is decodeSlots for a lighter and longer job.
func transcodeSlots(cpus int, limit int64) int {
	slots := max(1, cpus) * transcodesPerCPU
	if limit > 0 {
		slots = min(slots, int((limit-media.ReservedMemory)/transcodeBudget))
	}
	return max(1, slots)
}
