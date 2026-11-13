package checkflow

import (
	"math"

	"voice-timeline-renderer/internal/audiocodec"
)

func sineWAV(sampleRate, channels, durationMS int, frequency, amplitude float64) ([]byte, error) {
	frames := sampleRate * durationMS / 1000
	samples := make([]float32, frames*channels)
	for frame := 0; frame < frames; frame++ {
		seconds := float64(frame) / float64(sampleRate)
		first := float32(math.Sin(2*math.Pi*frequency*seconds) * amplitude)
		if channels == 1 {
			samples[frame] = first
			continue
		}
		second := float32(math.Sin(2*math.Pi*frequency*seconds+0.35) * amplitude * 0.8)
		samples[frame*2] = first
		samples[frame*2+1] = second
	}
	return audiocodec.EncodePCM16(sampleRate, channels, samples)
}

// impulseWAV builds a deterministic decaying-noise room impulse response
// with a distinct direct-sound spike and a long, exponentially decaying
// tail, optionally with different left/right energy so stereo pairing is
// observable in rendered output.
func impulseWAV(sampleRate, channels, durationMS int) ([]byte, error) {
	frames := sampleRate * durationMS / 1000
	samples := make([]float32, frames*channels)
	// Fixed-seed pseudo noise keeps the check deterministic.
	seed := uint32(0x1234abcd)
	nextNoise := func() float64 {
		seed = seed*1103515245 + 12345
		return float64(int32(seed)%1000) / 1000.0
	}
	for frame := 0; frame < frames; frame++ {
		envelope := math.Exp(-float64(frame) / (float64(sampleRate) * 0.18))
		if channels == 1 {
			samples[frame] = float32(envelope * nextNoise())
			continue
		}
		samples[frame*2] = float32(envelope * nextNoise())
		samples[frame*2+1] = float32(envelope * nextNoise() * 0.6)
	}
	// Direct-sound spike so the wet path has an unmistakable onset.
	samples[0] = 0.9
	if channels == 2 {
		samples[1] = 0.55
	}
	return audiocodec.EncodePCM16(sampleRate, channels, samples)
}
