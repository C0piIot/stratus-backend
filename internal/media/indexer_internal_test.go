package media

import (
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// TestNoticeNeverBlocksAndCoalesces pins both halves of what a notice is.
//
// It cannot block, because the caller is internal/files with a client
// connection still open. And it holds one, because what runs afterwards is the
// batch over the whole queue: five hundred photographs arriving together are
// one wake-up and one pass, not five hundred of each.
func TestNoticeNeverBlocksAndCoalesces(t *testing.T) {
	t.Parallel()
	i := NewIndexer(nil, nil, "", "")

	for range 500 {
		i.Notice(db.File{ID: 1})
	}

	select {
	case <-i.Woken():
	default:
		t.Fatal("five hundred writes woke nobody")
	}
	select {
	case <-i.Woken():
		t.Error("a second wake-up was queued: the pass that is about to run covers them all")
	default:
	}
}
