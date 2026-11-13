package service

import (
	"math"
	"testing"

	"voice-timeline-renderer/internal/audiocodec"
)

// makeTestWAV encodes a deterministic sine WAV for service lifecycle tests.
func makeTestWAV(t *testing.T, sampleRate, channels, frames int) []byte {
	t.Helper()
	samples := make([]float32, frames*channels)
	for frame := 0; frame < frames; frame++ {
		seconds := float64(frame) / float64(sampleRate)
		value := float32(math.Sin(2*math.Pi*220*seconds) * 0.4)
		if channels == 1 {
			samples[frame] = value
			continue
		}
		samples[frame*2] = value
		samples[frame*2+1] = value * 0.7
	}
	payload, err := audiocodec.EncodePCM16(sampleRate, channels, samples)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
