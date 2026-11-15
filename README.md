# media

Decode any media file into what a perceptual hash needs: **mono PCM at a
chosen rate**, or **video frames resampled by time**.

```go
import "github.com/go-perceptual/media"

kind, err := media.Classify(path)        // image, audio, video — by reading, not by name

pcm, err := media.Mono(path, 11025, 180) // mono int16 at 11025 Hz, first 180 s

err = media.Frames(path, 15, 180, func(i int, img image.Image) error {
    return hasher.Add(i, feature(img))   // one frame per index, resampled to 15 fps
})
```

It is the **plumbing, not the policy**. Thresholds, verdicts and what counts
as a duplicate belong to the caller. This package turns a path into samples or
pictures, and refuses loudly when it cannot.

Used by [go-perceptual/perceptual](https://github.com/go-perceptual/perceptual)
and [go-perceptual/dedupe](https://github.com/go-perceptual/dedupe), where it
was copied code before.

## Why this is a package and not a snippet

Every function here encodes a decision that goes wrong **silently** if two
copies of it drift apart. None of these crash. None fail a test. Each one
produces two tools that disagree about whether two files are the same thing —
confidently.

| the decision | what the other way does |
| --- | --- |
| `Downmix` **averages** the channels | taking the first channel compares a one-channel recording against silence, and silence matches other silence |
| `Resample` **clamps** | an overshoot converted to int16 wraps to the opposite sign, putting a click into the feature — content that is not in the recording |
| the PCM width is **asserted, then derived** | PCM read at the wrong width is noise that still hashes |
| `FrameImage` **refuses** 10-bit | truncating to 8 gives one film two different fingerprints |
| `Frames` resamples **by time** | taking every nth frame puts a 25 fps and a 30 fps copy of one film on different indices |
| `Classify` reads **bytes** | believing the extension makes a renamed copy invisible, and an invisible file reads as "nothing found" |

That last row is the whole reason the rest matters. For a tool that looks for
copies, a file it cannot see is indistinguishable from a clean result.

## Classify reads the file

An extension is a claim by whoever named it. A download folder is full of
wrong ones, and the copy a deduplicator exists to find is often *precisely the
one that was renamed*.

Two stages, because a tree scan cannot read every file whole:

1. the first 64 KiB — `image.DecodeConfig` settles images, `container.Sniff`
   settles WAV, FLAC, ADTS, MPEG audio and anything unrecognisable;
2. only a container that could hold **either** gets opened properly, and that
   open **streams** rather than slurping, which matters when the candidate is
   a 4 GB film.

A file holding both audio and video is `KindVideo`: its pictures are the
stronger evidence, since two different films can share a soundtrack while one
film in two languages shares its pictures.

`KindOther` is not an error — most of a directory tree is not media. A file
that *is* media but cannot be read **is** an error, and the two are kept
distinct because only one of them should make a caller warn.

## Linear PCM

A WAV holds samples that already **are** PCM, so `go-avkit/decode` refuses
them — rightly, since there is nothing to decode. The consequence is that WAV,
the simplest format in its list, is unreachable through the decoder API. This
package carries the passthrough, in one place, so the workaround is not copied
into every consumer. Reported upstream with an implementation offered.

The sample width is **not carried by `TrackConfig`**, so it is derived from a
chunk's byte length over its frame count. That is valid only while a sample's
`Duration` counts frames — only while the track's timescale **is** its sample
rate. Measured on go-avkit's WAV reader at 8000, 22050, 44100 and 48000 Hz:
the equality holds. It is **asserted at run time** and refused otherwise.

Every width is tested through a real file rather than only through
`PCMValue`, because the arithmetic and the *derivation* are different claims:

| width | worst error on a full-scale ramp |
| --- | --- |
| 8-bit unsigned | 1.5e-2 |
| 16-bit signed | 6.0e-5 |
| 24-bit signed | 2.4e-7 |
| 32-bit signed | 9.2e-10 |
| 32-bit float | 3.0e-8 |

The sample **count** is asserted too, and that is the assertion that catches a
bad derivation: a width inferred as 2 instead of 3 returns half again as many
samples, all garbage, with no error anywhere.

24-bit gets its own test. Three bytes have no sign bit where Go expects one,
and without explicit sign extension every negative sample reads as a large
positive one — not noise, but a recording with its bottom half folded over,
which hashes consistently and so matches other copies of itself while matching
nothing else.

## Frames

`Frames` takes a callback rather than returning a slice, because a decoded
video does not fit in memory and no caller wants it there — they hash frame by
frame.

Indices are consecutive from zero and each one holds the frame **showing at
that instant**. A test checks that index *n* at 15 fps and index *2n* at 30 fps
land on the same picture, which is the property the by-time resampling exists
for and which taking every nth frame silently breaks.

## Unsupported codecs

VP9, AV1, Vorbis and MP2 are missing from go-avkit's decoders. `Frames`
returns an error for them rather than skipping.

⚠ The message is misleading, and is reported upstream. A VP9 file produces
`decode: invalid sample: NAL unit of 2185855810 bytes with 2939 left`, which
is an H.264 NAL-unit parser handed VP9 data: the decoder does not check the
codec before parsing, so it calls the file **corrupt** instead of saying the
codec is unsupported. The length is bounds-checked, so this is a wrong
diagnosis rather than an unbounded allocation. A fixture is committed and the
test asserts an error is **returned**, not its wording, so it will not
silently start passing for the wrong reason when that is fixed.

## Formats

Containers and codecs come from [go-avkit](https://github.com/go-avkit): MP4,
Matroska/WebM, MPEG-TS, Ogg, WAV, AVI, native FLAC, ADTS and MPEG audio;
H.264 and H.265 video; FLAC, MP3, Opus, AAC-LC and HE-AAC audio. Images go
through Go's own decoders: JPEG, PNG and GIF.

Resampling is [go-dsp/signal](https://github.com/go-dsp)'s polyphase
resampler, with SciPy's `resample_poly` semantics.

## Test fixtures

Committed so CI runs the real decoders on a machine with no ffmpeg, and their
provenance is recorded rather than remembered. The audio fixtures are four
encodings of one four-second melody; the video fixtures are a clip and
**scaled copies of that clip**. WAVs are generated inside the tests, since the
PCM path is the one place where the bytes on disk are the thing under test —
and `writeWAVWidth` **refuses** a sample outside [-1, 1] rather than clamping
it, because a fixture that overflows its own width produces garbage that still
decodes and reads exactly like a decoder defect.

See `go-perceptual/dedupe`'s README for the ffmpeg commands that produced the
committed files, including why the 180p clip is *derived from* the 360p one
rather than rendered at that size.

## Licence

BSD-3-Clause.
