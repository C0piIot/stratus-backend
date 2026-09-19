//go:build !unix

package disk

import (
	"context"

	"github.com/C0piIot/stratus-backend/internal/storage"
)

// FreeSpace implements storage.Storage where there is no Statfs to ask.
//
// This project ships a Linux container and its tests run on one, so the honest
// answer here is that nothing was measured rather than a number invented. It
// exists so the package compiles everywhere, not because anybody runs it.
func (s *Store) FreeSpace(_ context.Context) (int64, error) {
	return storage.Unlimited, nil
}
