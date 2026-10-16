package media

import (
	"bufio"
	"bytes"
	"io/fs"
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

// ReservedMemory is what is left to the server itself before any decode or
// transcode is allowed a share of the rest.
const ReservedMemory = 128 << 20

// MemoryLimit is the memory this process may use, in bytes, or zero when
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
func MemoryLimit(root fs.FS) int64 {
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
