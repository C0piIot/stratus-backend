//go:build unix

package app

import (
	"os"
	"syscall"
)

// freeBytes is what an ordinary user can still write on the filesystem under
// dir, or -1 when it would not say. Bavail, for the reason disk.FreeSpace
// gives.
func freeBytes(dir string) int64 {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		return -1
	}
	return int64(fs.Bavail) * int64(fs.Bsize) //nolint:gosec // bounded by the device
}

// sameDevice is whether two directories are on one filesystem, so the page
// says its free space once.
func sameDevice(a, b string) bool {
	ia, erra := os.Stat(a)
	ib, errb := os.Stat(b)
	if erra != nil || errb != nil {
		return false
	}
	sa, oka := ia.Sys().(*syscall.Stat_t)
	sb, okb := ib.Sys().(*syscall.Stat_t)
	return oka && okb && sa.Dev == sb.Dev
}
