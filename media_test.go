package media

import (
	"encoding/binary"
	"errors"
	"image"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-avkit/avkit/container"
	"github.com/go-avkit/decode"
)

// --- fixtures ------------------------------------------------------------

func copyFixture(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeWAV emits 16-bit stereo PCM as a RIFF/WAVE file.
//
// Generated rather than committed: a WAV is two orders of magnitude larger
// than its encoded equivalents, and the PCM path is the one place where the
// bytes on disk are the thing under test.
func writeWAV(t *testing.T, path string, mono []int16, rate int) {
	t.Helper()
	dataBytes := len(mono) * 2 * 2

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := func(v any) {
		if err := binary.Write(f, binary.LittleEndian, v); err != nil {
			t.Fatal(err)
		}
	}
	f.WriteString("RIFF")
	w(uint32(36 + dataBytes))
	f.WriteString("WAVEfmt ")
	w(uint32(16))
	w(uint16(1)) // linear PCM
	w(uint16(2)) // channels
	w(uint32(rate))
	w(uint32(rate * 2 * 2))
	w(uint16(4))  // block align
	w(uint16(16)) // bits per sample
	f.WriteString("data")
	w(uint32(dataBytes))
	for _, s := range mono {
		w(s)
		w(s)
	}
}

// tone renders one chord for a number of seconds.
//
// The frequencies sit away from the chroma band edges a caller is likely to
// use. Those bands are centred on the equal-tempered semitones above
// A0 = 27.5 Hz, so a tone on a boundary splits its energy between two bands
// and a probe ends up measuring the band geometry instead of the thing under
// test.
func tone(freqs []float64, seconds float64, rate int) []int16 {
	n := int(seconds * float64(rate))
	out := make([]int16, n)
	for i := range out {
		var v float64
		for _, f := range freqs {
			v += math.Sin(2 * math.Pi * f * float64(i) / float64(rate))
		}
		out[i] = int16(v / float64(len(freqs)) * 0.7 * 32767)
	}
	return out
}

func chord() []float64 { return []float64{261.10, 329.30, 391.60} }

// --- Classify ------------------------------------------------------------

// TestClassifyReadsBytesNotNames is the central claim of this package.
func TestClassifyReadsBytesNotNames(t *testing.T) {
	dir := t.TempDir()

	wav := filepath.Join(dir, "looks-like-a-picture.jpg")
	writeWAV(t, wav, tone(chord(), 1, 44100), 44100)

	for _, c := range []struct {
		fixture, as string
		want        Kind
	}{
		// Every one is given an extension that is wrong, belongs to another
		// medium, or is absent entirely.
		{"testdata/melody.flac", "a.mp4", KindAudio},
		{"testdata/melody.mp3", "b.png", KindAudio},
		{"testdata/melody.opus", "c", KindAudio},
		{"testdata/melody.m4a", "d.txt", KindAudio},
		{"testdata/clip-360p.mp4", "e.flac", KindVideo},
		{"testdata/clip-90p.mp4", "f", KindVideo},
		{"testdata/unsupported-vp9.webm", "g.mp3", KindVideo},
	} {
		p := filepath.Join(dir, c.as)
		copyFixture(t, c.fixture, p)
		got, err := Classify(p)
		if err != nil {
			t.Errorf("%s as %q: %v", c.fixture, c.as, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s named %q classified as %q, want %q", c.fixture, c.as, got, c.want)
		}
	}

	// And the generated WAV, which the fixture table cannot carry.
	if got, err := Classify(wav); err != nil || got != KindAudio {
		t.Errorf("a WAV named .jpg classified as %q (%v), want audio", got, err)
	}
}

// TestClassifyIsNotFooledByANameEither is the inverse: a media extension must
// not get a non-media file in.
func TestClassifyIsNotFooledByANameEither(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"notes.txt", "fake.mp4", "fake.jpg", "fake.flac", "archive.zip"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(strings.Repeat("this is not media at all. ", 300)), 0o644); err != nil {
			t.Fatal(err)
		}
		got, _ := Classify(p)
		if got != KindOther {
			t.Errorf("%s classified as %q, want not-media", name, got)
		}
	}
}

func TestClassifyRefusesWhatItCannotOpen(t *testing.T) {
	if _, err := Classify(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("a missing file was accepted")
	}
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if k, _ := Classify(empty); k != KindOther {
		t.Errorf("an empty file classified as %q", k)
	}
}

// TestClassifyReportsACorruptContainer: a truncated MP4 must be an ERROR, not
// quietly "not media". Those two print differently and only one warns.
func TestClassifyReportsACorruptContainer(t *testing.T) {
	data, err := os.ReadFile("testdata/clip-360p.mp4")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "half.mp4")
	if err := os.WriteFile(p, data[:len(data)*2/3], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Classify(p); err == nil {
		t.Error("a truncated MP4 classified cleanly, so nothing warns about it")
	}
}

// TestAFileWithBothIsVideo pins the choice, since it decides which pass a
// film ends up in.
func TestAFileWithBothIsVideo(t *testing.T) {
	// clip-360p.mp4 is video-only, so this checks the branch that matters by
	// confirming a video file never answers "audio" merely because it has a
	// decodable audio path available.
	if got, err := Classify("testdata/clip-360p.mp4"); err != nil || got != KindVideo {
		t.Fatalf("Classify = %q, %v; want video", got, err)
	}
	if _, _, err := MonoFloat("testdata/clip-360p.mp4", 0); err == nil {
		t.Error("a silent video yielded audio samples")
	}
}

// --- Mono ---------------------------------------------------------------

func TestMonoReadsEveryCodec(t *testing.T) {
	for _, f := range []string{
		"testdata/melody.flac",
		"testdata/melody.mp3",
		"testdata/melody.opus",
		"testdata/melody.m4a",
	} {
		pcm, err := Mono(f, 11025, 0)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		if len(pcm) == 0 {
			t.Errorf("%s: no samples", f)
		}
		// Four seconds at 11025 Hz, give or take an encoder's priming and
		// padding. A result an order of magnitude off means the resampling
		// ratio was wrong, which is the failure that still produces a
		// plausible fingerprint.
		if got := len(pcm); got < 11025*3 || got > 11025*6 {
			t.Errorf("%s: %d samples, want roughly %d", f, got, 11025*4)
		}
	}
}

// TestMonoReadsAWAVThroughThePassthrough is the regression test for the gap
// this package works around: go-avkit's decoder refuses PCM, rightly, so
// without a passthrough a WAV is unreadable.
func TestMonoReadsAWAVThroughThePassthrough(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.wav")
	writeWAV(t, p, tone(chord(), 2, 44100), 44100)

	pcm, err := Mono(p, 11025, 0)
	if err != nil {
		t.Fatalf("reading a WAV: %v", err)
	}
	// Two seconds, resampled 44100 -> 11025, is a clean 4:1 so the count is
	// exact rather than approximate.
	if want := 2 * 11025; len(pcm) != want {
		t.Errorf("%d samples, want %d", len(pcm), want)
	}
}

// TestWAVTimescaleIsItsSampleRate guards the assumption the PCM width
// derivation rests on, at four rates.
//
// ⛔ TrackConfig carries no width. pcmMono derives it from a chunk's byte
// length divided by its frame count, which is valid only while Duration
// counts FRAMES — only while the timescale IS the sample rate. It asserts
// that at run time; this says so out loud when it breaks, instead of leaving
// every WAV silently unreadable until someone files a bug.
func TestWAVTimescaleIsItsSampleRate(t *testing.T) {
	for _, rate := range []int{8000, 22050, 44100, 48000} {
		p := filepath.Join(t.TempDir(), "probe.wav")
		writeWAV(t, p, tone(chord(), 1, rate), rate)

		r, closeFile, err := container.OpenFile(p)
		if err != nil {
			t.Fatalf("%d Hz: %v", rate, err)
		}
		tracks := r.File().Tracks
		if len(tracks) != 1 {
			closeFile()
			t.Fatalf("%d Hz: %d tracks, want 1", rate, len(tracks))
		}
		cfg, err := r.TrackConfig(tracks[0].ID)
		if err != nil {
			closeFile()
			t.Fatal(err)
		}
		closeFile()

		if !IsPCM(cfg.Codec) {
			t.Errorf("%d Hz: codec %q is not one the passthrough handles", rate, cfg.Codec)
		}
		if cfg.SampleRate != rate {
			t.Errorf("%d Hz: the reader says %d", rate, cfg.SampleRate)
		}
		if cfg.Timescale != uint32(cfg.SampleRate) {
			t.Fatalf("%d Hz: timescale %d is no longer the sample rate %d — the PCM width can no longer be derived from a sample's duration",
				rate, cfg.Timescale, cfg.SampleRate)
		}

		// And end to end: one second in must be one second out.
		mono, rateOut, err := MonoFloat(p, 0)
		if err != nil {
			t.Fatalf("%d Hz: %v", rate, err)
		}
		if rateOut != rate {
			t.Errorf("%d Hz: MonoFloat reports %d", rate, rateOut)
		}
		if len(mono) != rate {
			t.Errorf("%d Hz: %d samples from one second, want %d", rate, len(mono), rate)
		}
	}
}

func TestMaxSecondsBoundsWhatIsRead(t *testing.T) {
	p := filepath.Join(t.TempDir(), "long.wav")
	writeWAV(t, p, tone(chord(), 8, 44100), 44100)

	for _, limit := range []int{1, 2, 4} {
		mono, _, err := MonoFloat(p, limit)
		if err != nil {
			t.Fatal(err)
		}
		if want := limit * 44100; len(mono) != want {
			t.Errorf("maxSeconds %d gave %d samples, want %d", limit, len(mono), want)
		}
	}
	all, _, err := MonoFloat(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := 8 * 44100; len(all) != want {
		t.Errorf("maxSeconds 0 gave %d samples, want all %d", len(all), want)
	}
}

// TestMaxSecondsBoundsAnEncodedTrackToo: the bound is applied on the decoded
// side, so it has to work for a codec as well as for PCM.
func TestMaxSecondsBoundsAnEncodedTrackToo(t *testing.T) {
	short, _, err := MonoFloat("testdata/melody.flac", 1)
	if err != nil {
		t.Fatal(err)
	}
	all, _, err := MonoFloat("testdata/melody.flac", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(short) >= len(all) {
		t.Errorf("maxSeconds 1 gave %d samples and no bound gave %d", len(short), len(all))
	}
	// A codec arrives in whole frames, so the cut lands at or just past the
	// boundary rather than exactly on it.
	if len(short) < 44100 || len(short) > 44100+8192 {
		t.Errorf("one second of FLAC gave %d samples, want about 44100", len(short))
	}
}

func TestMonoRefusesAnImpossibleRate(t *testing.T) {
	if _, err := Mono("testdata/melody.flac", 0, 0); err == nil {
		t.Error("a target rate of 0 Hz was accepted")
	}
	if _, err := Mono("testdata/melody.flac", -1, 0); err == nil {
		t.Error("a negative target rate was accepted")
	}
}

func TestMonoRefusesAFileWithNoAudio(t *testing.T) {
	if _, err := Mono("testdata/clip-360p.mp4", 11025, 0); err == nil {
		t.Error("a silent video yielded audio")
	}
	if _, err := Mono(filepath.Join(t.TempDir(), "absent.wav"), 11025, 0); err == nil {
		t.Error("a missing file yielded audio")
	}
}

// --- Downmix, Resample, PCMValue ----------------------------------------

// TestDownmixAveragesRatherThanTakingTheFirstChannel is the one that matters.
//
// Taking channel one would compare a recording whose content sits in one
// channel against silence — and silence matches other silence, so every such
// file would group together as "the same recording".
func TestDownmixAveragesRatherThanTakingTheFirstChannel(t *testing.T) {
	got, err := Downmix(decode.Audio{
		Channels: 2, Format: decode.SampleS16,
		Data: []byte{0xff, 0x7f, 0x00, 0x00}, // left full scale, right silent
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("%d samples, want 1", len(got))
	}
	if want := (32767.0 / 32768) / 2; got[0] != want {
		t.Errorf("got %v, want %v — a downmix that is not an average", got[0], want)
	}
}

func TestDownmixHandlesEachSampleFormat(t *testing.T) {
	for _, c := range []struct {
		name   string
		format decode.SampleFormat
		data   []byte
		want   float64
	}{
		{"S16", decode.SampleS16, []byte{0x00, 0x40}, 0.5},
		{"S32", decode.SampleS32, []byte{0, 0, 0, 0x40}, 0.5},
		{"F32", decode.SampleF32, []byte{0, 0, 0, 0x3f}, 0.5},
	} {
		got, err := Downmix(decode.Audio{Channels: 1, Format: c.format, Data: c.data})
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if len(got) != 1 || got[0] != c.want {
			t.Errorf("%s: got %v, want [%v]", c.name, got, c.want)
		}
	}
}

func TestDownmixRefusesWhatItCannotRead(t *testing.T) {
	if _, err := Downmix(decode.Audio{Channels: 2, Format: decode.SampleFormat(99), Data: make([]byte, 8)}); err == nil {
		t.Error("an unknown sample format was accepted")
	}
	if _, err := Downmix(decode.Audio{Channels: 0, Format: decode.SampleS16}); err == nil {
		t.Error("a buffer with no channels was accepted")
	}
}

// TestResampleClampsRatherThanWrapping: a resampler's ringing overshoots, and
// an overshoot converted to int16 without clamping wraps to the OPPOSITE
// sign, putting a click into the feature — content that is not in the
// recording.
func TestResampleClampsRatherThanWrapping(t *testing.T) {
	out, err := Resample([]float64{0, 1.5, -1.5, 1, -1, 0.5}, 11025, 11025)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 6 {
		t.Fatalf("%d samples out, 6 in", len(out))
	}
	if out[1] != 32767 {
		t.Errorf("1.5 became %d, want 32767", out[1])
	}
	if out[2] != -32767 {
		t.Errorf("-1.5 became %d, want -32767", out[2])
	}
}

func TestResampleChangesTheRate(t *testing.T) {
	// One second of silence at 44100, down to 11025: a clean 4:1.
	out, err := Resample(make([]float64, 44100), 44100, 11025)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 11025 {
		t.Errorf("%d samples, want 11025", len(out))
	}
	// And up, by a ratio that is not a whole number: 147/640.
	out, err = Resample(make([]float64, 48000), 48000, 11025)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) < 11000 || len(out) > 11050 {
		t.Errorf("%d samples from 48 kHz, want about 11025", len(out))
	}
}

func TestResampleRefusesAnImpossibleRate(t *testing.T) {
	for _, c := range [][2]int{{0, 11025}, {44100, 0}, {-1, 11025}} {
		if _, err := Resample([]float64{0, 1}, c[0], c[1]); err == nil {
			t.Errorf("Resample(%d, %d) was accepted", c[0], c[1])
		}
	}
}

func TestIsPCMNamesOnlyPCM(t *testing.T) {
	for _, c := range []string{"ipcm", "fpcm", "raw "} {
		if !IsPCM(c) {
			t.Errorf("IsPCM(%q) is false", c)
		}
	}
	for _, c := range []string{"mp4a", "Opus", "fLaC", "alac", "mp3", ""} {
		if IsPCM(c) {
			t.Errorf("IsPCM(%q) is true", c)
		}
	}
}

// TestPCMValueScalesToUnit checks each width against its full-scale value: a
// wrong divisor reads as a quieter recording that still hashes.
func TestPCMValueScalesToUnit(t *testing.T) {
	for _, c := range []struct {
		name, codec string
		b           []byte
		width       int
		want        float64
	}{
		{"16-bit max", "ipcm", []byte{0xff, 0x7f}, 2, 32767.0 / 32768},
		{"16-bit min", "ipcm", []byte{0x00, 0x80}, 2, -1},
		{"16-bit zero", "ipcm", []byte{0x00, 0x00}, 2, 0},
		{"24-bit max", "ipcm", []byte{0xff, 0xff, 0x7f}, 3, 8388607.0 / 8388608},
		{"24-bit min", "ipcm", []byte{0x00, 0x00, 0x80}, 3, -1},
		{"24-bit minus one", "ipcm", []byte{0xff, 0xff, 0xff}, 3, -1.0 / 8388608},
		{"32-bit min", "ipcm", []byte{0, 0, 0, 0x80}, 4, -1},
		{"32-bit zero", "ipcm", []byte{0, 0, 0, 0}, 4, 0},
		{"8-bit silence", "raw ", []byte{128}, 1, 0},
		{"8-bit min", "raw ", []byte{0}, 1, -1},
		{"8-bit max", "raw ", []byte{255}, 1, 127.0 / 128},
		{"float32 one", "fpcm", []byte{0, 0, 0x80, 0x3f}, 4, 1},
		{"float64 one", "fpcm", []byte{0, 0, 0, 0, 0, 0, 0xf0, 0x3f}, 8, 1},
	} {
		got, err := PCMValue(c.codec, c.b, c.width)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// TestPCMValueRefusesWhatItCannotRead is the negative witness.
//
// ⛔ Without it the refusal branches are code no test executes: every WAV in
// the corpus is 16-bit, so the derivation always succeeds and both the error
// message and its condition stay unverified.
func TestPCMValueRefusesWhatItCannotRead(t *testing.T) {
	for _, c := range []struct {
		codec string
		b     []byte
		width int
	}{
		{"ipcm", []byte{0, 0, 0, 0, 0, 0}, 6},
		{"ipcm", []byte{0}, 1},
		{"fpcm", []byte{0, 0}, 2},
		{"raw ", []byte{0, 0}, 2},
		{"mp4a", []byte{0, 0}, 2},
		{"", []byte{0, 0}, 2},
	} {
		if _, err := PCMValue(c.codec, c.b, c.width); err == nil {
			t.Errorf("PCMValue(%q, %d bytes) was accepted", c.codec, c.width)
		}
	}
}

// --- Frames -------------------------------------------------------------

func TestFramesResamplesByTime(t *testing.T) {
	var seen []int
	var sizes []string
	err := Frames("testdata/clip-360p.mp4", 15, 0, func(i int, img image.Image) error {
		seen = append(seen, i)
		if i == 0 {
			b := img.Bounds()
			sizes = append(sizes, b.String())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The clip is two seconds at 15 fps.
	if len(seen) < 25 || len(seen) > 35 {
		t.Errorf("%d frames from a two-second clip at 15 fps", len(seen))
	}
	// Indices must be consecutive from zero: a hash compares phases at
	// integer indices, so a gap silently shifts everything after it.
	for i, got := range seen {
		if got != i {
			t.Fatalf("frame %d reported index %d — the indices are not consecutive", i, got)
		}
	}
	if len(sizes) == 0 {
		t.Error("no frame was handed over")
	}
}

// TestFramesAtTwoRatesLandOnTheSameContent is the property the by-time
// resampling exists for.
//
// ⛔ At 15 fps and at 30 fps, frame index n must show the same instant
// doubled: index 2n at 30 fps is index n at 15 fps. Taking every nth decoded
// frame instead would make that false for any clip whose own rate is not a
// multiple, and nothing would say so — both hashes would look fine.
func TestFramesAtTwoRatesLandOnTheSameContent(t *testing.T) {
	at := func(fps float64) []string {
		var out []string
		err := Frames("testdata/clip-360p.mp4", fps, 0, func(i int, img image.Image) error {
			// A cheap content signature: the luma of a few fixed points.
			y, ok := img.(*image.YCbCr)
			if !ok {
				t.Fatalf("got %T, want *image.YCbCr", img)
			}
			b := y.Bounds()
			var sb strings.Builder
			for _, p := range [][2]int{{10, 10}, {b.Dx() / 2, b.Dy() / 2}, {b.Dx() - 10, b.Dy() - 10}} {
				sb.WriteByte(y.Y[p[1]*y.YStride+p[0]])
			}
			out = append(out, sb.String())
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	slow, fast := at(15), at(30)
	if len(slow) == 0 || len(fast) == 0 {
		t.Fatal("no frames")
	}
	matched := 0
	for i := range slow {
		if 2*i < len(fast) && slow[i] == fast[2*i] {
			matched++
		}
	}
	// Not all of them: the clip's own frames land where they land, and a
	// requested instant between two of them resolves to the earlier one at
	// both rates only when the arithmetic agrees. Most must match, though.
	if matched*4 < len(slow)*3 {
		t.Errorf("only %d of %d frames at 15 fps match index 2n at 30 fps", matched, len(slow))
	}
}

func TestFramesBoundsWhatItReads(t *testing.T) {
	count := func(maxSeconds int) int {
		n := 0
		if err := Frames("testdata/clip-360p.mp4", 15, maxSeconds, func(int, image.Image) error {
			n++
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	one, all := count(1), count(0)
	if one >= all {
		t.Errorf("maxSeconds 1 gave %d frames and no bound gave %d", one, all)
	}
	if one < 14 || one > 17 {
		t.Errorf("one second at 15 fps gave %d frames", one)
	}
}

func TestFramesStopsWhenTheCallbackFails(t *testing.T) {
	wanted := errors.New("enough")
	n := 0
	err := Frames("testdata/clip-360p.mp4", 15, 0, func(int, image.Image) error {
		n++
		if n == 3 {
			return wanted
		}
		return nil
	})
	if !errors.Is(err, wanted) {
		t.Errorf("Frames returned %v, want the callback's own error", err)
	}
	if n != 3 {
		t.Errorf("the callback ran %d times after asking to stop at 3", n)
	}
}

func TestFramesRefusesWhatItCannotRead(t *testing.T) {
	nothing := func(int, image.Image) error { return nil }
	if err := Frames("testdata/melody.flac", 15, 0, nothing); err == nil {
		t.Error("an audio file yielded frames")
	}
	if err := Frames(filepath.Join(t.TempDir(), "absent.mp4"), 15, 0, nothing); err == nil {
		t.Error("a missing file yielded frames")
	}
	if err := Frames("testdata/clip-360p.mp4", 0, 0, nothing); err == nil {
		t.Error("a frame rate of 0 was accepted")
	}
	if err := Frames("testdata/clip-360p.mp4", -1, 0, nothing); err == nil {
		t.Error("a negative frame rate was accepted")
	}
}

// TestAnUnsupportedCodecIsAnError pins what happens for VP9.
//
// ⚠ go-avkit's message for it is WRONG and is reported upstream: it says
// "invalid sample: NAL unit of 2185855810 bytes with 2939 left", which is an
// H.264 NAL parser handed VP9 data — the decoder does not check the codec
// before parsing, so it calls the FILE corrupt instead of saying the codec is
// unsupported. This asserts that an error is RETURNED, not its wording, so it
// will not silently start passing for the wrong reason when that is fixed.
func TestAnUnsupportedCodecIsAnError(t *testing.T) {
	// The container reads it: this is a decode failure, not a classification
	// one.
	if k, err := Classify("testdata/unsupported-vp9.webm"); err != nil || k != KindVideo {
		t.Fatalf("Classify = %q, %v; want video", k, err)
	}
	err := Frames("testdata/unsupported-vp9.webm", 15, 0, func(int, image.Image) error {
		t.Error("a VP9 frame was decoded, which this build cannot do")
		return nil
	})
	if err == nil {
		t.Fatal("decoding VP9 succeeded, or failed silently")
	}
}

// --- FrameImage ---------------------------------------------------------

// TestFrameImageRefusesWhatItCannotHashHonestly covers the two refusals.
//
// ⛔ A 10-bit frame shifted down to 8 bits WOULD hash — and would hash
// differently from the same film delivered as 8-bit, so one film would carry
// two fingerprints. For code whose purpose is deciding whether two files are
// the same thing, that is the one failure that must never be silent.
func TestFrameImageRefusesWhatItCannotHashHonestly(t *testing.T) {
	_, err := FrameImage(decode.Frame{
		Y: make([]byte, 64*64*2), Cb: make([]byte, 32*32*2), Cr: make([]byte, 32*32*2),
		YStride: 128, CStride: 64, Width: 64, Height: 64,
		Chroma: decode.Chroma420, BitDepth: 10,
	})
	if err == nil {
		t.Error("a 10-bit frame was accepted")
	} else if !strings.Contains(err.Error(), "10-bit") {
		t.Errorf("the error does not name the bit depth: %v", err)
	}

	if _, err := FrameImage(decode.Frame{
		Y: make([]byte, 64*64), Cb: make([]byte, 64*64), Cr: make([]byte, 64*64),
		YStride: 64, CStride: 64, Width: 64, Height: 64,
		Chroma: decode.Chroma(99), BitDepth: 8,
	}); err == nil {
		t.Error("an unknown chroma layout was accepted")
	}
}

// TestFrameImageWrapsEachChromaLayout is the positive half: a frame wrapped
// at the wrong ratio reads its chroma planes at the wrong stride and hashes a
// picture nobody filmed.
func TestFrameImageWrapsEachChromaLayout(t *testing.T) {
	for _, c := range []struct {
		chroma decode.Chroma
		cw, ch int
		want   image.YCbCrSubsampleRatio
	}{
		{decode.Chroma420, 32, 32, image.YCbCrSubsampleRatio420},
		{decode.Chroma422, 32, 64, image.YCbCrSubsampleRatio422},
		{decode.Chroma444, 64, 64, image.YCbCrSubsampleRatio444},
	} {
		img, err := FrameImage(decode.Frame{
			Y: make([]byte, 64*64), Cb: make([]byte, c.cw*c.ch), Cr: make([]byte, c.cw*c.ch),
			YStride: 64, CStride: c.cw, Width: 64, Height: 64,
			Chroma: c.chroma, BitDepth: 8,
		})
		if err != nil {
			t.Errorf("%v: %v", c.chroma, err)
			continue
		}
		ycbcr, ok := img.(*image.YCbCr)
		if !ok {
			t.Errorf("%v: got %T", c.chroma, img)
			continue
		}
		if ycbcr.SubsampleRatio != c.want {
			t.Errorf("%v: ratio %v, want %v", c.chroma, ycbcr.SubsampleRatio, c.want)
		}
		if b := ycbcr.Bounds(); b.Dx() != 64 || b.Dy() != 64 {
			t.Errorf("%v: bounds %v", c.chroma, b)
		}
	}
}

// TestFrameImageDoesNotCopy: the planes are shared with the decoder's frame,
// which is what keeps a video pass from allocating per frame.
func TestFrameImageDoesNotCopy(t *testing.T) {
	f := decode.Frame{
		Y: make([]byte, 64*64), Cb: make([]byte, 32*32), Cr: make([]byte, 32*32),
		YStride: 64, CStride: 32, Width: 64, Height: 64,
		Chroma: decode.Chroma420, BitDepth: 8,
	}
	img, err := FrameImage(f)
	if err != nil {
		t.Fatal(err)
	}
	y := img.(*image.YCbCr)
	f.Y[0] = 42
	if y.Y[0] != 42 {
		t.Error("the wrapper copied the luma plane instead of sharing it")
	}
}
