package media

import (
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// TestNoticeNeverBlocksAndNamesTheFile pins both halves of what a notice is.
//
// It cannot block, because the caller is internal/files with a client
// connection still open. And it carries the file, because the whole point is
// that the ordinary path does not have to ask the database which file (#158).
func TestNoticeNeverBlocksAndNamesTheFile(t *testing.T) {
	t.Parallel()
	i := NewIndexer(nil, nil, "", "")

	i.Notice(db.File{ID: 7, Path: "photos/IMG_0001.jpg"})

	select {
	case f := <-i.Noticed():
		if f.ID != 7 || f.Path != "photos/IMG_0001.jpg" {
			t.Errorf("noticed %+v, want the file that was written", f)
		}
	default:
		t.Fatal("a write named nobody")
	}
	select {
	case <-i.LostTrack():
		t.Error("a notice that fitted reported itself as lost")
	default:
	}
}

// TestNoticeSaysWhenItLostOne is what makes a small buffer safe: more writes at
// once than can be held is not silence, it is the query being brought forward.
func TestNoticeSaysWhenItLostOne(t *testing.T) {
	t.Parallel()
	i := NewIndexer(nil, nil, "", "")

	for id := range noticeBuffer + 500 {
		i.Notice(db.File{ID: int64(id)})
	}

	select {
	case <-i.LostTrack():
	default:
		t.Fatal("five hundred writes were dropped in silence")
	}
	// And it holds one, however many were lost: what is on the other side is a
	// query over everything, so a second signal would only run it twice.
	select {
	case <-i.LostTrack():
		t.Error("a second signal was queued: one query covers every file that was dropped")
	default:
	}

	// What did fit is still there to be worked through.
	if got := len(i.Noticed()); got != noticeBuffer {
		t.Errorf("%d notices held, want the buffer full at %d", got, noticeBuffer)
	}
}
