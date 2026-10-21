//go:build !unix

package app

// freeBytes has no Statfs to ask here; see disk.FreeSpace for why that is
// an answer rather than a gap.
func freeBytes(string) int64 { return -1 }

func sameDevice(_, _ string) bool { return false }
