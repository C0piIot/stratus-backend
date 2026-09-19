//go:build unix

package disk

import (
	"context"
	"fmt"
	"syscall"

	"github.com/C0piIot/stratus-backend/internal/storage"
)

// FreeSpace implements storage.Storage by asking the filesystem the data
// directory is on.
//
// Bavail and not Bfree: the difference is the reserve the filesystem keeps for
// root, and this process is not root -- the container runs as an ordinary user
// on purpose. Answering with bytes we could not actually write would make the
// check worse than none.
func (s *Store) FreeSpace(_ context.Context) (int64, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(s.root.Name(), &fs); err != nil {
		return 0, fmt.Errorf("free space under %q: %w", s.root.Name(), err)
	}
	// Both are unsigned and the product is what is left; a filesystem large
	// enough to overflow this is not one this runs on.
	free := int64(fs.Bavail) * int64(fs.Bsize) //nolint:gosec // bounded by the device
	if free < 0 {
		return storage.Unlimited, nil
	}
	return free, nil
}
