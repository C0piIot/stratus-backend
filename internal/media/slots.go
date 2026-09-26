package media

import (
	"bufio"
	"bytes"
	"io/fs"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// How many thumbnails may be decoded at once, from what the machine has.
//
// A photo grid asks for everything it can see the first time a folder is
// opened, and each decode holds its own working memory: about fifty megabytes
// for a twelve-megapixel JPEG, about seventy for a frame of 1080p HEVC Main 10
// through ffmpeg. It was a constant four, which on the 256 MB demo instance
// with its one CPU meant four ffmpegs sharing a core and nearly all of its
// memory. So it is now whichever runs out first, the CPUs or the memory.

// decodeBudget is what one decode is allowed, and reservedMemory what is left
// to the server itself before any. A decode was measured at seventy megabytes
// at worst; the rest of the budget is for the 4K recordings that were not.
const (
	decodeBudget   = 128 << 20
	reservedMemory = 128 << 20
)

// generating is the number of decodes the semaphore allows.
func generating() int {
	return decodeSlots(runtime.GOMAXPROCS(0), memoryLimit(os.DirFS("/")))
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
		slots = min(slots, int((limit-reservedMemory)/decodeBudget))
	}
	return max(1, slots)
}

// memoryLimit is the memory this process may use, in bytes, or zero when
// neither place says.
//
// The cgroup's memory.max first, because that is what a container runtime
// sets and what the kernel kills by. It says "max" when there is no limit,
// and then the machine's own memory is the limit: /proc/meminfo, which is
// also the only answer on Fly, where a machine is a microVM with no cgroup
// around it and all of the VM's memory is the server's.
//
// GOMAXPROCS has followed a cgroup's CPU limit since Go 1.25, though never
// below two -- `--cpus 1` still gives two, measured -- and nothing in the
// runtime does the same for memory, so this reads the two files itself. On
// that one-CPU container with 256 MB it is the memory that brings it to one.
func memoryLimit(root fs.FS) int64 {
	if b, err := fs.ReadFile(root, "sys/fs/cgroup/memory.max"); err == nil {
		if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil && n > 0 {
			return n
		}
	}
	b, err := fs.ReadFile(root, "proc/meminfo")
	if err != nil {
		return 0
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		// MemTotal:        1015764 kB
		fields := strings.Fields(sc.Text())
		if len(fields) == 3 && fields[0] == "MemTotal:" && fields[2] == "kB" {
			if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil && kb > 0 {
				return kb << 10
			}
		}
	}
	return 0
}
