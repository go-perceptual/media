package media

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/go-avkit/avkit/container"
)

// IsPCM says whether a track's samples are already PCM and need no decoder.
//
// ⛔ These are the codecs a container reports for a WAV: `ipcm` signed
// integer, `fpcm` float, `raw ` unsigned 8-bit. go-avkit's decoder REFUSES
// all three — correctly, since the samples already ARE PCM and there is
// nothing to decode — so a consumer that only knows how to call a decoder
// cannot read a WAV at all, which is the simplest format in the list.
//
// Reported upstream (go-avkit/decode, 2026-11-15, with a working
// implementation offered). This package carries the passthrough meanwhile, in
// one place, so the workaround does not get copied into every consumer.
func IsPCM(codec string) bool {
	switch codec {
	case "ipcm", "fpcm", "raw ":
		return true
	}
	return false
}

// pcmMono reads a PCM track's samples, averages the channels, and stops after
// maxSeconds (0 reads all of it).
func pcmMono(r *container.Reader, tr container.Track, cfg container.TrackConfig, maxSeconds int) ([]float64, error) {
	samples, err := r.Samples(tr.ID)
	if err != nil {
		return nil, err
	}
	ch := cfg.Channels
	if ch < 1 {
		return nil, fmt.Errorf("media: a PCM track with %d channels", ch)
	}
	if cfg.SampleRate < 1 {
		return nil, fmt.Errorf("media: a PCM track at %d Hz", cfg.SampleRate)
	}

	// ⛔ The sample width is DERIVED, because TrackConfig does not carry one,
	// and the derivation is valid only while a sample's Duration counts
	// FRAMES — that is, while the track's timescale IS its sample rate.
	// Measured on go-avkit's WAV reader at 8000, 22050, 44100 and 48000 Hz
	// (2026-11-15): timescale equals the rate, and 4096 frames arrive in
	// 16384 bytes of two-channel audio, so 2 bytes a sample.
	//
	// The equality is ASSERTED rather than assumed. A container that chose
	// another timescale would yield a plausible wrong width, and PCM read at
	// the wrong width is noise that still hashes — a confident wrong answer
	// where an error belongs.
	if cfg.Timescale != uint32(cfg.SampleRate) {
		return nil, fmt.Errorf("media: cannot derive the PCM width: timescale %d is not the sample rate %d",
			cfg.Timescale, cfg.SampleRate)
	}

	var width int
	for _, s := range samples {
		if s.Duration == 0 {
			continue
		}
		frames := int(s.Duration)
		if len(s.Data)%(frames*ch) != 0 {
			return nil, fmt.Errorf("media: a PCM chunk of %d bytes is not a whole number of %d-channel frames over %d frames",
				len(s.Data), ch, frames)
		}
		width = len(s.Data) / (frames * ch)
		break
	}
	switch width {
	case 1, 2, 3, 4, 8:
	default:
		return nil, fmt.Errorf("media: cannot tell the PCM sample width (derived %d bytes)", width)
	}

	limit := maxSeconds * cfg.SampleRate
	var out []float64
	for _, s := range samples {
		frames := len(s.Data) / (width * ch)
		for i := 0; i < frames; i++ {
			var sum float64
			for c := 0; c < ch; c++ {
				off := (i*ch + c) * width
				v, err := PCMValue(cfg.Codec, s.Data[off:off+width], width)
				if err != nil {
					return nil, err
				}
				sum += v
			}
			out = append(out, sum/float64(ch))
		}
		if maxSeconds > 0 && len(out) >= limit {
			return out[:limit], nil
		}
	}
	return out, nil
}

// PCMValue turns one PCM sample's bytes into a value in [-1, 1].
//
// It is exported because the sample width cannot be read from the track
// configuration, so a caller doing its own PCM reading needs the same
// conversion rather than a second guess at it.
func PCMValue(codec string, b []byte, width int) (float64, error) {
	switch codec {
	case "raw ": // unsigned 8-bit, silence at 128
		if width != 1 {
			return 0, fmt.Errorf("media: codec %q at %d bytes per sample", codec, width)
		}
		return (float64(b[0]) - 128) / 128, nil
	case "fpcm":
		switch width {
		case 4:
			return float64(math.Float32frombits(binary.LittleEndian.Uint32(b))), nil
		case 8:
			return math.Float64frombits(binary.LittleEndian.Uint64(b)), nil
		}
	case "ipcm":
		switch width {
		case 2:
			return float64(int16(binary.LittleEndian.Uint16(b))) / 32768, nil
		case 3:
			// 24-bit little-endian, sign-extended. pcm_s24le is what ffmpeg
			// writes by default for 24-bit WAV, so this is not an exotic case.
			v := int32(b[0]) | int32(b[1])<<8 | int32(b[2])<<16
			if v&0x800000 != 0 {
				v |= ^0xffffff
			}
			return float64(v) / 8388608, nil
		case 4:
			return float64(int32(binary.LittleEndian.Uint32(b))) / 2147483648, nil
		}
	}
	return 0, fmt.Errorf("media: PCM codec %q at %d bytes per sample is not handled", codec, width)
}
