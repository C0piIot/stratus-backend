package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Turning into pixels what Go cannot decode: HEIC, which needs libheif and
// therefore cgo, and a frame out of a video.
//
// The binary is ours and was configured for exactly this (build/ffmpeg/
// Dockerfile). **It decodes and scales and nothing else**: the frame comes back
// as raw pixels already reduced to the size asked for -- about 360 KB rather
// than the 48 MB a full 12 MP frame would push through a pipe -- and the JPEG
// is written in Go by the same encoder every other thumbnail goes through.

// thumbTimeout bounds one decode. Two minutes is what the indexer allows
// ffprobe, and this is not that: somebody is looking at a page while it runs,
// and a picture that takes half a minute to appear has already failed.
const thumbTimeout = 30 * time.Second

// maxFrameBytes bounds what will be read back. The largest size on the ladder
// is 1200 pixels, so a frame is at most 1200x1200x4 -- about 5.8 MB -- and this
// is the room around that. A scale filter that came back with something else is
// a bug, not a big picture.
const maxFrameBytes = 64 << 20

// videoFrameAt is how far into a video the frames are taken from. The opening
// of a recording is very often black -- a phone starts recording before the
// sensor has settled -- so the first second is skipped outright and the filter
// below chooses among what follows. A clip shorter than this is handled by
// asking again from the start.
const videoFrameAt = time.Second

// framesConsidered is how many frames the thumbnail filter weighs before it
// picks one, which at ordinary frame rates is about four seconds of film. More
// would survive a longer fade-in and cost more decoding for every video in the
// library.
const framesConsidered = 100

// decodeScaled runs ffmpeg over a local file and returns one frame, already
// reduced so that its longest side is at most px.
//
// A local path and not a pipe, for the reason the whole of #48 was about: the
// tools seek, and an MP4 whose moov atom sits at the end cannot be read from a
// stream at all.
func decodeScaled(ctx context.Context, ffmpeg, path string, px int, video bool) (image.Image, error) {
	ctx, cancel := context.WithTimeout(ctx, thumbTimeout)
	defer cancel()

	if video {
		frame, err := runFFmpeg(ctx, ffmpeg, path, px, videoFrameAt)
		if err == nil {
			return frame, nil
		}
		// A clip shorter than the offset has no frame there, and ffmpeg says so
		// by producing nothing. Asking again from the start costs a second run
		// of something that has already been spooled.
		if ctx.Err() != nil {
			return nil, err
		}
	}
	return runFFmpeg(ctx, ffmpeg, path, px, 0)
}

// runFFmpeg is one invocation, and the whole of what this project asks the
// binary to do.
func runFFmpeg(ctx context.Context, ffmpeg, path string, px int, at time.Duration) (image.Image, error) {
	args := make([]string, 0, 14)
	if at > 0 {
		// Before -i, which is the seek that jumps rather than the one that
		// decodes everything on the way.
		args = append(args, "-ss", strconv.FormatFloat(at.Seconds(), 'f', -1, 64))
	}
	args = append(args,
		"-hide_banner", "-v", "error",
		// One decoding thread. A frame-threaded decoder keeps a set of frames
		// per thread, and on a 1080p HEVC Main 10 that was most of the memory:
		// the demo instance, 256 MB in all, had ffmpeg killed by the kernel
		// making one thumbnail. Measured with the filters below: 123 MB with
		// the default threads, 66 MB with one, and no slower on the single
		// core it runs on -- thumbnails are parallel already, one per CPU.
		"-threads", "1",
		"-i", path,
		"-frames:v", "1",
		// The width is exactly what was asked for and the height follows the
		// aspect ratio, which is what makes the frame readable at the other end:
		// rawvideo has no header, so the only way to know how tall it is is to
		// know how wide it is. A portrait frame therefore comes back taller than
		// the box and reduce fits it afterwards -- one resize of something
		// already small, against a size nothing could have inferred.
		// thumbnail is what keeps a thumbnail from being a black rectangle: it
		// scores a batch of frames against their own average and hands back
		// the least ordinary one. Measured on a film that opens with two
		// seconds of black, the first frame and the frame a second in both
		// average zero brightness, and this picks one at 125 out of 255.
		//
		// **scale before thumbnail, never after**: the filter holds every frame
		// it weighs, so ahead of the scale it held a hundred of them at full
		// size -- 432 MB for fifteen seconds of 1080p HEVC Main 10, which is
		// what took the demo instance down, and a 4K phone recording would do
		// the same to a Raspberry Pi. Behind it, the hundred are thumbnails.
		// The choice barely moves, since the filter compares colour histograms
		// and a histogram survives being reduced.
		"-vf", "scale="+strconv.Itoa(px)+":-1,thumbnail="+strconv.Itoa(framesConsidered),
		// Rotation is deliberately left to ffmpeg, which reads the matrix in the
		// container and inserts the transpose itself -- that filter is in the
		// build for no other reason. A phone held upright is the ordinary case,
		// not the exception.
		"-f", "rawvideo", "-pix_fmt", "rgba", "-",
	)

	// The binary is resolved once at startup and never comes from a request.
	//nolint:gosec // the path is LookupFFmpeg's, and px is an int
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && stderr.Len() > 0 {
			return nil, fmt.Errorf("ffmpeg: %s", strings.TrimSpace(stderr.String()))
		}
		return nil, fmt.Errorf("ffmpeg: %w", err)
	}
	return frameFromRaw(out.Bytes(), px)
}

// frameFromRaw wraps what came back in an image.
//
// rawvideo carries no header at all, so the height is arithmetic: the width is
// the one the scale filter was given and four bytes a pixel is what rgba means.
// The division has to come out exact, and that is the whole check -- a frame
// that does not divide is not the frame that was asked for, and guessing at a
// width would be reading somebody else's bytes as a picture.
func frameFromRaw(raw []byte, px int) (image.Image, error) {
	if len(raw) == 0 {
		return nil, errors.New("ffmpeg produced no frame")
	}
	if len(raw) > maxFrameBytes {
		return nil, fmt.Errorf("ffmpeg produced %d bytes, more than a thumbnail can be", len(raw))
	}

	row := px * 4
	if len(raw)%row != 0 {
		return nil, fmt.Errorf("ffmpeg produced %d bytes, which is not whole rows of %d pixels", len(raw), px)
	}
	return &image.RGBA{
		Pix: raw, Stride: row,
		Rect: image.Rect(0, 0, px, len(raw)/row),
	}, nil
}
