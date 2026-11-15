package media

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeWAVWidth emits mono PCM at a chosen sample width, so the width
// derivation is exercised through a REAL FILE rather than through PCMValue
// alone.
//
// ⛔ This is the difference that matters. PCMValue can be unit-tested with a
// byte slice, and that proves the arithmetic. It does not prove that pcmMono
// DERIVES the width correctly, and the derivation is the part with no
// authority behind it: TrackConfig carries no width, so it is inferred from a
// chunk's byte length over its frame count. 16-bit is the only width in the
// rest of the corpus, so without these files the inference is only ever
// tested at the one value where an off-by-a-factor would not show.
//
// bits is 8, 16, 24 or 32; float32 is written when asFloat is set.
func writeWAVWidth(t *testing.T, path string, samples []float64, rate, bits int, asFloat bool) {
	t.Helper()
	width := bits / 8
	dataBytes := len(samples) * width

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
	format := uint16(1) // WAVE_FORMAT_PCM
	if asFloat {
		format = 3 // WAVE_FORMAT_IEEE_FLOAT
	}
	f.WriteString("RIFF")
	w(uint32(36 + dataBytes))
	f.WriteString("WAVEfmt ")
	w(uint32(16))
	w(format)
	w(uint16(1)) // one channel, so a frame IS a sample
	w(uint32(rate))
	w(uint32(rate * width))
	w(uint16(width))
	w(uint16(bits))
	f.WriteString("data")
	w(uint32(dataBytes))

	for i, v := range samples {
		// ⛔ REFUSED, not clamped. A sample outside [-1, 1] overflows the
		// integer width it is written at and lands as a value of the opposite
		// sign — garbage that still decodes, still hashes, and looks exactly
		// like a decoder defect. It cost a debugging detour here: a probe
		// signal written as -0.9 + 0.2*sin spans -1.1, and the resulting file
		// read back with a third of its samples positive, which reads as
		// "24-bit sign extension is broken". The code was right; the fixture
		// was out of range. A helper that clamped silently would have hidden
		// it just as well.
		if v < -1 || v > 1 {
			t.Fatalf("writeWAVWidth: sample %d is %g, outside [-1, 1]: it would overflow %d-bit and the file would be garbage that still decodes", i, v, bits)
		}
		switch {
		case asFloat && bits == 32:
			w(math.Float32bits(float32(v)))
		case bits == 8:
			// Unsigned, silence at 128 -- this is what `raw ` means.
			w(uint8(v*127 + 128))
		case bits == 16:
			w(int16(v * 32767))
		case bits == 24:
			n := int32(v * 8388607)
			w([3]byte{byte(n), byte(n >> 8), byte(n >> 16)})
		case bits == 32:
			w(int32(v * 2147483647))
		default:
			t.Fatalf("writeWAVWidth cannot write %d bits (float=%v)", bits, asFloat)
		}
	}
}

// ramp is a signal whose value at each sample is known exactly, so a wrong
// width shows up as a wrong SHAPE rather than as plausible noise.
func ramp(n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = -1 + 2*float64(i)/float64(n-1)
	}
	return out
}

// TestEveryPCMWidthIsReadCorrectly drives each width through the container
// and the passthrough, and checks the SHAPE of what comes back.
//
// A derivation that picked the wrong width does not fail: it returns the
// right number of bytes read as the wrong number of samples, which is a
// perfectly plausible recording. Checking the sample COUNT catches that --
// one second in must be one second out at every width.
func TestEveryPCMWidthIsReadCorrectly(t *testing.T) {
	const rate = 8000
	want := ramp(rate)

	for _, c := range []struct {
		name    string
		bits    int
		asFloat bool
		// tolerance on each sample, which is the quantisation of that width.
		tol float64
	}{
		{"8-bit unsigned", 8, false, 1.0 / 127},
		{"16-bit signed", 16, false, 1.0 / 32767},
		{"24-bit signed", 24, false, 1.0 / 8388607},
		{"32-bit signed", 32, false, 1.0 / 2147483647},
		{"32-bit float", 32, true, 1e-6},
	} {
		p := filepath.Join(t.TempDir(), "w.wav")
		writeWAVWidth(t, p, want, rate, c.bits, c.asFloat)

		kind, err := Classify(p)
		if err != nil || kind != KindAudio {
			t.Errorf("%s: Classify = %q, %v", c.name, kind, err)
			continue
		}

		got, gotRate, err := MonoFloat(p, 0)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if gotRate != rate {
			t.Errorf("%s: rate %d, want %d", c.name, gotRate, rate)
		}
		// ⛔ The count is the real assertion: a width derived as 2 instead of
		// 3 gives half again as many samples, all of them garbage, and no
		// error anywhere.
		if len(got) != len(want) {
			t.Errorf("%s: %d samples, want %d — the width was derived wrong", c.name, len(got), len(want))
			continue
		}
		worst := 0.0
		at := -1
		for i := range want {
			if d := math.Abs(got[i] - want[i]); d > worst {
				worst, at = d, i
			}
		}
		// A generous multiple of the quantisation step: the point is to
		// separate "read correctly" from "read at the wrong width", not to
		// pin the rounding.
		if worst > 4*c.tol+1e-9 {
			t.Errorf("%s: worst error %g at sample %d (got %g, want %g), beyond %g",
				c.name, worst, at, got[at], want[at], 4*c.tol)
		} else {
			t.Logf("%s: %d samples, worst error %g", c.name, len(got), worst)
		}
	}
}

// TestTwentyFourBitSignExtensionIsCorrect is the one width where getting it
// wrong is subtle rather than obvious.
//
// ⛔ A 24-bit sample is three bytes with no sign bit in the place Go expects
// one. Without explicit sign extension, every negative sample reads as a
// large POSITIVE one -- which is not noise, it is a recording with its
// bottom half folded over. It would still hash, and it would hash
// consistently, so two copies of the same file would match each other and
// neither would match the same audio stored at any other width.
func TestTwentyFourBitSignExtensionIsCorrect(t *testing.T) {
	const rate = 8000
	// Deliberately all negative, and well inside [-1, 1]: the range is what
	// the test is about, not the amplitude. See writeWAVWidth's refusal for
	// why the first version of this signal was wrong.
	want := make([]float64, rate)
	for i := range want {
		want[i] = -0.8 + 0.15*math.Sin(2*math.Pi*float64(i)/100)
	}
	p := filepath.Join(t.TempDir(), "neg24.wav")
	writeWAVWidth(t, p, want, rate, 24, false)

	got, _, err := MonoFloat(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("%d samples, want %d", len(got), len(want))
	}
	negatives := 0
	for _, v := range got {
		if v < 0 {
			negatives++
		}
	}
	// Essentially all of them should be negative. If sign extension were
	// missing they would all be positive.
	if negatives < len(got)*9/10 {
		t.Fatalf("only %d of %d samples are negative — 24-bit sign extension is wrong", negatives, len(got))
	}
	for i := range want {
		if math.Abs(got[i]-want[i]) > 1e-5 {
			t.Fatalf("sample %d: got %g, want %g", i, got[i], want[i])
		}
	}
}

// TestAWidthThatCannotBeDerivedIsRefused is the negative witness for the
// derivation's guard.
//
// It builds a WAV whose declared bits-per-sample is one the switch does not
// handle, so the derived width falls outside {1,2,3,4,8}.
func TestAWidthThatCannotBeDerivedIsRefused(t *testing.T) {
	// 48 bits = 6 bytes a sample: a legal-looking header, an unhandled width.
	const rate = 8000
	n := 100
	dataBytes := n * 6

	p := filepath.Join(t.TempDir(), "w48.wav")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	w := func(v any) {
		if err := binary.Write(f, binary.LittleEndian, v); err != nil {
			t.Fatal(err)
		}
	}
	f.WriteString("RIFF")
	w(uint32(36 + dataBytes))
	f.WriteString("WAVEfmt ")
	w(uint32(16))
	w(uint16(1))
	w(uint16(1))
	w(uint32(rate))
	w(uint32(rate * 6))
	w(uint16(6))
	w(uint16(48))
	f.WriteString("data")
	w(uint32(dataBytes))
	w(make([]byte, dataBytes))
	f.Close()

	// Whether the container accepts such a file at all is its business; what
	// must NOT happen is samples coming back as if the width were understood.
	got, _, err := MonoFloat(p, 0)
	if err == nil {
		t.Fatalf("a 48-bit WAV yielded %d samples instead of an error", len(got))
	}
	t.Logf("refused, as it must be: %v", err)
	if !strings.Contains(err.Error(), "media:") && !strings.Contains(err.Error(), "container") {
		t.Errorf("the error says nothing about where it came from: %v", err)
	}
}
