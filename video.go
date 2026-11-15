package media

import (
	"errors"
	"fmt"
	"image"

	"github.com/go-avkit/avkit/container"
	"github.com/go-avkit/decode"
)

// ErrNoFrames means the video track decoded to nothing. Like ErrNoSamples it
// is a skip rather than a failure.
var ErrNoFrames = errors.New("media: the video track yielded no frames")

// Frames decodes a file's first video track, resampled to fps, and hands each
// frame to fn as an image.
//
// maxSeconds bounds how much is read (0 reads all of it). If fn returns an
// error, decoding stops and that error is returned.
//
// ⛔ Frames are resampled BY TIME, not by taking every nth frame. A video
// hash compares phases at integer frame indices, so a 30 fps and a 25 fps
// copy of one film must land on the SAME indices — which happens only if each
// index is filled by the frame showing at that instant. Taking every other
// frame of the 30 fps file and every frame of a 15 fps one produces two
// valid-looking hashes that cannot be compared, and nothing says so.
//
// ⛔ A callback rather than a returned slice, because a decoded video does not
// fit in memory and no caller wants it there: they hash frame by frame.
func Frames(path string, fps float64, maxSeconds int, fn func(index int, img image.Image) error) error {
	if fps <= 0 {
		return fmt.Errorf("media: a frame rate of %v", fps)
	}
	r, closeFile, err := container.OpenFile(path)
	if err != nil {
		return err
	}
	defer closeFile()

	id, err := decode.FirstVideoTrack(r)
	if err != nil {
		return err
	}
	frames, err := decode.DecodeTrack(r, id, decode.Config{})
	if err != nil {
		return err
	}
	if len(frames) == 0 {
		return ErrNoFrames
	}

	var timescale uint32 = 1
	for _, tr := range r.File().Tracks {
		if tr.ID == id {
			timescale = tr.Timescale
		}
	}
	if timescale == 0 {
		return fmt.Errorf("media: %s: the track declares a timescale of zero", path)
	}
	seconds := func(i int) float64 { return float64(frames[i].PTS) / float64(timescale) }

	duration := seconds(len(frames)-1) - seconds(0)
	if maxSeconds > 0 && duration > float64(maxSeconds) {
		duration = float64(maxSeconds)
	}
	steps := int(duration*fps) + 1

	cursor := 0
	for step := 0; step < steps; step++ {
		want := seconds(0) + float64(step)/fps
		// The frame showing at that instant: the last one presented at or
		// before it.
		for cursor+1 < len(frames) && seconds(cursor+1) <= want {
			cursor++
		}
		img, err := FrameImage(frames[cursor])
		if err != nil {
			return err
		}
		if err := fn(step, img); err != nil {
			return err
		}
	}
	return nil
}

// FrameImage wraps a decoded frame's planes as an image WITHOUT copying them.
//
// ⛔ It refuses what it cannot represent honestly rather than approximating.
// A 10-bit frame shifted down to 8 bits would hash perfectly well — and would
// hash DIFFERENTLY from the same film delivered as 8-bit, so one film would
// carry two fingerprints. For a tool whose whole purpose is deciding whether
// two files are the same thing, that is the one failure that must never be
// silent.
func FrameImage(f decode.Frame) (image.Image, error) {
	if f.BitDepth != 8 {
		return nil, fmt.Errorf("media: %d-bit video is not handled yet (8-bit only)", f.BitDepth)
	}
	var ratio image.YCbCrSubsampleRatio
	switch f.Chroma {
	case decode.Chroma420:
		ratio = image.YCbCrSubsampleRatio420
	case decode.Chroma422:
		ratio = image.YCbCrSubsampleRatio422
	case decode.Chroma444:
		ratio = image.YCbCrSubsampleRatio444
	default:
		return nil, fmt.Errorf("media: chroma sampling %v is not handled", f.Chroma)
	}
	return &image.YCbCr{
		Y: f.Y, Cb: f.Cb, Cr: f.Cr,
		YStride: f.YStride, CStride: f.CStride,
		SubsampleRatio: ratio,
		Rect:           image.Rect(0, 0, f.Width, f.Height),
	}, nil
}
