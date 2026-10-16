package lookup

import (
	"context"
	"errors"
)

// maxCover bounds a picture from the archive. The 500 pixel thumbnail is a
// few hundred kilobytes; eight megabytes is not a thumbnail.
const maxCover = 8 << 20

// coverSize is the archive's own ladder step that is fetched. 500 is what a
// grid cell and an album page are made from; 1200 is four times the bytes
// for a size the viewer asks for once.
const coverSize = "500"

// frontCover fetches the front cover of a release group, or of a release
// when the tags named only that. The archive answers a 307 to archive.org,
// which the client follows, and a 404 when the community has chosen no front
// image -- which is errNothing, and the normal answer for half a library.
func (s *Service) frontCover(ctx context.Context, group, release string) ([]byte, error) {
	var path string
	switch {
	case group != "":
		path = "/release-group/" + group
	case release != "":
		path = "/release/" + release
	default:
		return nil, errors.New("lookup: no id to fetch a cover by")
	}
	return s.get(ctx, "coverartarchive", s.coverArt+path+"/front-"+coverSize, maxCover, "")
}

// isNothing says a source answered that it has nothing.
func isNothing(err error) bool { return errors.Is(err, errNothing) }
