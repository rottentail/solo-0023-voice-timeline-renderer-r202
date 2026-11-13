package convolve

import (
	"math"
	"testing"
)

func TestFFTRoundTrip(t *testing.T) {
	for _, size := range []int{4, 8, 2048} {
		original := make([]complex128, size)
		for i := range original {
			original[i] = complex(math.Sin(float64(i)*0.31)+2.0, math.Cos(float64(i)*0.17))
		}
		work := make([]complex128, size)
		copy(work, original)
		fft(work)
		ifft(work)
		for i := range work {
			if math.Abs(real(work[i])-real(original[i])) > 1e-9 || math.Abs(imag(work[i])-imag(original[i])) > 1e-9 {
				t.Fatalf("size %d index %d round trip %v != %v", size, i, work[i], original[i])
			}
		}
	}
}

func TestFFTImpulseSpectrum(t *testing.T) {
	// FFT of a unit impulse must be all ones.
	size := 8
	work := make([]complex128, size)
	work[0] = 1
	fft(work)
	for i, value := range work {
		if math.Abs(real(value)-1) > 1e-9 || math.Abs(imag(value)) > 1e-9 {
			t.Fatalf("unit impulse bin %d = %v, want 1", i, value)
		}
	}
}
