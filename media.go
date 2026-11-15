// Package media decodes a media file into what a perceptual hash needs: mono
// PCM at a chosen sample rate, or video frames resampled to a chosen rate by
// time.
//
// It is the plumbing, not the policy. Thresholds, verdicts and what counts as
// a duplicate belong to the caller — this package only turns a path into
// samples or pictures, and refuses loudly when it cannot.
//
// # Why it is a package
//
// Every function here encodes a decision that goes wrong SILENTLY if two
// copies of it drift apart. A downmix that takes the first channel instead of
// averaging compares a one-channel recording against silence; PCM read at the
// wrong sample width is noise that still hashes; a 10-bit frame truncated to 8
// gives one film two different fingerprints; frames taken every nth instead of
// by time put a 25 fps and a 30 fps copy on different indices. None of those
// crash, and none fail a test. They produce two tools that disagree about
// whether two files are the same, confidently.
package media

import (
	"bytes"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"

	"github.com/go-avkit/avkit/container"
)

// Kind is what a file turned out to hold.
type Kind string

const (
	KindImage Kind = "image"
	KindAudio Kind = "audio"
	KindVideo Kind = "video"
	// KindOther is a file this package cannot hand to a perceptual hash. It
	// is not an error: most of a directory tree is not media.
	KindOther Kind = ""
)

// HeaderBytes is how much of a file Classify reads before deciding.
//
// Enough for every signature container.Sniff looks at, and for the
// configuration header of a JPEG, PNG or GIF. MPEG-TS is the greediest: its
// sync byte recurs every 188 bytes and a sniffer wants several packets before
// believing it, so this is generous rather than minimal.
const HeaderBytes = 64 << 10

// Classify says what a file holds, by READING it.
//
// ⛔ Not by its extension. An extension is a claim by whoever named the file,
// and a download folder is full of wrong ones: a .jpg holding a PNG, an .mp4
// holding only audio, a film saved with no extension at all. Code that
// believes the name returns a confident wrong answer — and for a tool that
// looks for copies, a file it cannot see reads as "nothing found", which is
// indistinguishable from a clean tree.
//
// It works in two stages, because a tree scan cannot afford to read every
// file whole. The header alone settles images and the audio-only containers.
// Only a container that could hold either gets opened properly, and that open
// STREAMS rather than slurping the file, which matters when the candidate is
// a 4 GB film.
//
// A file holding BOTH audio and video is KindVideo. Its pictures are the
// stronger evidence: two different films can share a soundtrack, while one
// film in two languages shares its pictures.
func Classify(path string) (Kind, error) {
	f, err := os.Open(path)
	if err != nil {
		return KindOther, err
	}
	head := make([]byte, HeaderBytes)
	n, readErr := f.Read(head)
	closeErr := f.Close()
	if n == 0 {
		if readErr != nil {
			return KindOther, readErr
		}
		return KindOther, closeErr
	}
	head = head[:n]

	// Images first: cheap, definitive, decided entirely by the header.
	if _, _, err := image.DecodeConfig(bytes.NewReader(head)); err == nil {
		return KindImage, nil
	}

	switch container.Sniff(head) {
	case container.FormatUnknown:
		return KindOther, nil
	case container.FormatADTS, container.FormatMPEGAudio, container.FormatFLAC, container.FormatWAV:
		// These carry audio and nothing else, so the header has answered.
		return KindAudio, nil
	}

	// MP4, Matroska, MPEG-TS, AVI and Ogg can hold either, so the track list
	// has to be read.
	r, closeFile, err := container.OpenFile(path)
	if err != nil {
		return KindOther, err
	}
	defer closeFile()

	hasVideo, hasAudio := false, false
	for _, tr := range r.File().Tracks {
		switch tr.Kind {
		case container.Video:
			hasVideo = true
		case container.Audio:
			hasAudio = true
		}
	}
	switch {
	case hasVideo:
		return KindVideo, nil
	case hasAudio:
		return KindAudio, nil
	}
	return KindOther, nil
}
