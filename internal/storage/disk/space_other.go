//go:build !(linux || darwin || freebsd)

package disk

import (
	"context"

	"github.com/C0piIot/stratus-backend/internal/storage"
)

// FreeSpace implements storage.Storage where there is no Statfs to ask.
//
// OpenBSD and NetBSD name the fields differently or have no Statfs at all.
// This project ships a Linux container and its tests run on one, so the honest
// answer here is that nothing was measured rather than a number invented. It
// exists so the package compiles everywhere, not because anybody runs it.
func (s *Store) FreeSpace(_ context.Context) (int64, error) {
	return storage.Unlimited, nil
}
