package thumb

import (
	"os"
	"runtime"

	"github.com/C0piIot/stratus-backend/internal/media"
)

// How many decodes may run at once, worked out from the machine: see
// media.MemoryLimit for the reasoning that was measured.

// decodeBudget is what one decode is allowed. A decode was measured at seventy
// megabytes at worst; the rest of the budget is for the 4K recordings that
// were not.
const decodeBudget = 128 << 20

// generating is the number of decodes the semaphore allows.
func generating() int {
	return decodeSlots(runtime.GOMAXPROCS(0), media.MemoryLimit(os.DirFS("/")))
}

// decodeSlots is at most one decode per CPU -- decoding is CPU-bound and ffmpeg
// runs on one thread, so a second decode on a core buys nothing but its memory
// -- and at most as many as the memory past the reserve pays for. Never fewer
// than one: a machine too small for the budget still makes thumbnails, slowly,
// rather than none. A limit of zero means it could not be read, and the CPUs
// decide alone.
func decodeSlots(cpus int, limit int64) int {
	slots := max(1, cpus)
	if limit > 0 {
		slots = min(slots, int((limit-media.ReservedMemory)/decodeBudget))
	}
	return max(1, slots)
}

// waitingPerSlot is how many requests may stand in front of each decoder
// before the rest are refused rather than queued.
//
// The queue used to have no bottom and no deadline, which on a one-CPU machine
// is what made a photo grid look hung: forty tiles are asked for at once, one
// is decoded at a time, and the thirty-ninth waits a minute while the only CPU
// is pegged -- so the listing behind it is slow as well, and the stall is the
// whole UI rather than one picture. Four deep is enough that a grid fills in
// steadily and short enough that nobody waits behind a minute of work.
const waitingPerSlot = 4

// decodeQueue is the most requests that may be between the door and the end of
// a decode: the ones waiting and the ones decoding, counted together because
// what is being capped is how much of this machine thumbnails may hold, not
// where a request is standing.
func decodeQueue(slots int) int {
	return slots + slots*waitingPerSlot
}
