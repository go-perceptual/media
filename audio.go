package media

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/go-avkit/avkit/container"
	"github.com/go-avkit/decode"
	"github.com/go-dsp/signal"
)

// ErrNoSamples means the track decoded to nothing, or to too little to
// fingerprint. It is a skip, not a failure: a one-frame file is not broken.
var ErrNoSamples = errors.New("media: the audio track yielded no samples")

// Mono decodes a file's first audio track to mono samples at rate, reading at
// most maxSeconds of it (0 reads all of it).
//
// The chain is demux, decode to PCM, downmix to mono, resample. Each step is
// somebody else's job done properly — the container and decoder are
// go-avkit's, the resampler is go-dsp's — and this is the plumbing between
// them.
//
// ⛔ The RATE is part of a fingerprint, not a preference. A feature's frame
// geometry and frequency bounds are expressed in samples at a particular
// rate, so audio fed at another rate produces a perfectly plausible
// fingerprint that matches nothing. Callers pass the rate their feature is
// defined at and do not treat it as a tuning knob.
func Mono(path string, rate, maxSeconds int) ([]int16, error) {
	if rate < 1 {
		return nil, fmt.Errorf("media: a target rate of %d Hz", rate)
	}
	samples, sourceRate, err := monoFloat(path, maxSeconds)
	if err != nil {
		return nil, err
	}
	return Resample(samples, sourceRate, rate)
}

// MonoFloat is Mono without the resampling or the conversion to int16: the
// track's own samples, downmixed, in [-1, 1], with the rate they are at.
//
// It exists for a caller whose feature wants float input or another rate.
func MonoFloat(path string, maxSeconds int) (samples []float64, rate int, err error) {
	return monoFloat(path, maxSeconds)
}

func monoFloat(path string, maxSeconds int) ([]float64, int, error) {
	r, closeFile, err := container.OpenFile(path)
	if err != nil {
		return nil, 0, err
	}
	defer closeFile()

	id, err := decode.FirstAudioTrack(r)
	if err != nil {
		return nil, 0, err
	}
	cfg, err := r.TrackConfig(id)
	if err != nil {
		return nil, 0, err
	}

	if IsPCM(cfg.Codec) {
		var tr container.Track
		for _, t := range r.File().Tracks {
			if t.ID == id {
				tr = t
			}
		}
		mono, err := pcmMono(r, tr, cfg, maxSeconds)
		if err != nil {
			return nil, 0, err
		}
		if len(mono) == 0 {
			return nil, 0, ErrNoSamples
		}
		return mono, cfg.SampleRate, nil
	}

	buffers, err := decode.DecodeAudioTrack(r, id, decode.AudioConfig{})
	if err != nil {
		return nil, 0, err
	}
	if len(buffers) == 0 {
		return nil, 0, ErrNoSamples
	}
	rate := buffers[0].SampleRate
	if rate < 1 {
		return nil, 0, fmt.Errorf("media: the track reports %d Hz", rate)
	}
	limit := maxSeconds * rate

	var mono []float64
	for _, b := range buffers {
		if b.SampleRate != rate {
			// ⛔ Refused rather than resampled piecewise. A track whose rate
			// changes mid-stream would need every buffer resampled on its own
			// and the results spliced, and splicing separately resampled
			// blocks introduces exactly the discontinuities a feature would
			// then report as content.
			return nil, 0, fmt.Errorf("media: sample rate changes mid-track (%d then %d)", rate, b.SampleRate)
		}
		m, err := Downmix(b)
		if err != nil {
			return nil, 0, err
		}
		mono = append(mono, m...)
		if maxSeconds > 0 && len(mono) >= limit {
			mono = mono[:limit]
			break
		}
	}
	if len(mono) == 0 {
		return nil, 0, ErrNoSamples
	}
	return mono, rate, nil
}

// Downmix averages one buffer's channels into samples in [-1, 1].
//
// ⛔ AVERAGING, not taking the first channel. A track whose content sits in
// one channel — an interview, a badly mixed rip, a karaoke version — would
// otherwise be compared against silence, and silence matches other silence,
// so such files would all group together as "the same recording".
func Downmix(a decode.Audio) ([]float64, error) {
	n := a.Samples()
	ch := a.Channels
	if ch < 1 {
		return nil, fmt.Errorf("media: a buffer with %d channels", ch)
	}
	out := make([]float64, n)

	switch a.Format {
	case decode.SampleS16:
		for i := 0; i < n; i++ {
			var sum float64
			for c := 0; c < ch; c++ {
				off := (i*ch + c) * 2
				sum += float64(int16(binary.LittleEndian.Uint16(a.Data[off:]))) / 32768
			}
			out[i] = sum / float64(ch)
		}
	case decode.SampleS32:
		for i := 0; i < n; i++ {
			var sum float64
			for c := 0; c < ch; c++ {
				off := (i*ch + c) * 4
				sum += float64(int32(binary.LittleEndian.Uint32(a.Data[off:]))) / 2147483648
			}
			out[i] = sum / float64(ch)
		}
	case decode.SampleF32:
		for i := 0; i < n; i++ {
			var sum float64
			for c := 0; c < ch; c++ {
				off := (i*ch + c) * 4
				sum += float64(math.Float32frombits(binary.LittleEndian.Uint32(a.Data[off:])))
			}
			out[i] = sum / float64(ch)
		}
	default:
		return nil, fmt.Errorf("media: sample format %v is not handled", a.Format)
	}
	return out, nil
}

// Resample brings mono samples from one rate to another and converts them to
// int16.
//
// ⛔ It CLAMPS. A polyphase resampler rings, and its ringing overshoots a
// full-scale signal; converting an overshoot to int16 without clamping wraps
// it to the opposite sign, which puts a click into the feature — content that
// is not in the recording.
func Resample(mono []float64, from, to int) ([]int16, error) {
	if from < 1 || to < 1 {
		return nil, fmt.Errorf("media: resampling %d Hz to %d Hz", from, to)
	}
	if from != to {
		g := gcd(to, from)
		out, err := signal.ResamplePoly(mono, to/g, from/g, nil)
		if err != nil {
			return nil, fmt.Errorf("media: resampling %d Hz to %d Hz: %w", from, to, err)
		}
		mono = out
	}
	pcm := make([]int16, len(mono))
	for i, v := range mono {
		switch {
		case v > 1:
			v = 1
		case v < -1:
			v = -1
		}
		pcm[i] = int16(v * 32767)
	}
	return pcm, nil
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
