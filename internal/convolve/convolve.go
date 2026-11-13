// Package convolve implements streaming linear convolution for spatial
// (room impulse response) processing.
//
// The engine uses uniform-partition overlap-save block convolution with a
// radix-2 FFT. Impulse responses are prepared once into shared, read-only
// frequency-domain partitions; every timeline span then gets its own fresh
// Convolver holding only a fixed-size input history (one FFT window per
// channel) plus per-partition delay lines. A Convolver never stores the
// span's full input or the full convolution output: blocks stream through it
// directly into the shared render mix, so memory stays proportional to the
// response length rather than the rendered project length, and only one
// span convolver is live at any moment.
package convolve

import (
	"context"
	"errors"
	"math"
)

// BlockFrames is the streaming block size for every convolver. The FFT
// window is twice the block size, as required by overlap-save.
const BlockFrames = 1024

const fftSize = BlockFrames * 2

// Prepared holds the frequency-domain partitions of one impulse response.
// A Prepared value is immutable and safe to share across spans; sharing the
// response working set keeps long responses from being duplicated per span.
type Prepared struct {
	// ChannelPartitions[channel][partition][bin] is the FFT of one
	// zero-padded response partition for one response channel. A mono
	// response has one set that both output channels reuse verbatim.
	ChannelPartitions [][][]complex128
	Channels          int
	Frames            int64
}

// Prepare converts an interleaved float32 impulse response into shared
// frequency-domain partitions. channels must be one or two. The response
// bytes are only read; the input slice is never modified.
func Prepare(samples []float32, channels int, frames int64) (*Prepared, error) {
	if channels != 1 && channels != 2 {
		return nil, errors.New("impulse response channel count must be one or two")
	}
	if frames < 1 || int64(len(samples)) != frames*int64(channels) {
		return nil, errors.New("impulse response frame data is not frame-aligned")
	}
	partitionCount := int((frames + BlockFrames - 1) / BlockFrames)
	if partitionCount < 1 {
		partitionCount = 1
	}
	channelPartitions := make([][][]complex128, channels)
	for channel := 0; channel < channels; channel++ {
		partitions := make([][]complex128, partitionCount)
		for partition := 0; partition < partitionCount; partition++ {
			window := make([]complex128, fftSize)
			start := int64(partition) * BlockFrames
			for frame := 0; frame < BlockFrames; frame++ {
				index := start + int64(frame)
				if index >= frames {
					break
				}
				// Overlap-save places each response partition in the first
				// half of its zero-padded FFT window.
				window[frame] = complex(float64(samples[index*int64(channels)+int64(channel)]), 0)
			}
			fft(window)
			partitions[partition] = window
		}
		channelPartitions[channel] = partitions
	}
	return &Prepared{ChannelPartitions: channelPartitions, Channels: channels, Frames: frames}, nil
}

// channelState holds the per-channel streaming working set of one span.
type channelState struct {
	// history is the most recent fftSize input samples; the newest block is
	// placed in the second half each iteration.
	history []float64
	// fdl[partition] stores the FFT of the input block scheduled to multiply
	// the response partition at that delay position.
	fdl [][]complex128
}

// Convolver is a fresh, span-local streaming convolver. It is not safe for
// concurrent use and is meant to be discarded after Drain: spans that reuse
// the same recording never inherit one another's processing history.
type Convolver struct {
	prepared     *Prepared
	channels     int
	states       []*channelState
	queuedFrames int64
	emitted      int64
	totalOutput  int64
	drained      bool
}

// New builds a span-local convolver. inputChannels is the channel count of
// the signal entering the convolver (one or two); the prepared response may
// be mono or stereo and channel pairing is resolved here, never by treating
// a stereo pair as two unrelated mono signals.
func New(prepared *Prepared, inputChannels int, inputFrames int64) (*Convolver, error) {
	if prepared == nil {
		return nil, errors.New("convolution requires a prepared impulse response")
	}
	if inputChannels != 1 && inputChannels != 2 {
		return nil, errors.New("convolution input channel count must be one or two")
	}
	if inputFrames < 1 {
		return nil, errors.New("convolution input must contain at least one frame")
	}
	channels := inputChannels
	if prepared.Channels == 2 && inputChannels == 1 {
		// A mono source through a stereo response yields a stereo wet image.
		channels = 2
	}
	states := make([]*channelState, channels)
	for channel := range states {
		fdl := make([][]complex128, len(prepared.ChannelPartitions[0]))
		for partition := range fdl {
			fdl[partition] = make([]complex128, fftSize)
		}
		states[channel] = &channelState{
			history: make([]float64, fftSize),
			fdl:     fdl,
		}
	}
	// Linear convolution of N input frames with an M-frame response yields
	// N+M-1 output frames.
	totalOutput := inputFrames + prepared.Frames - 1
	return &Convolver{
		prepared:    prepared,
		channels:    channels,
		states:      states,
		totalOutput: totalOutput,
	}, nil
}

// Channels reports the interleaved channel count of the wet output.
func (convolver *Convolver) Channels() int {
	return convolver.channels
}

// TotalOutputFrames is the exact number of wet frames the convolver will
// emit across ProcessBlock and Drain, i.e. N+M-1.
func (convolver *Convolver) TotalOutputFrames() int64 {
	return convolver.totalOutput
}

// QueuedInputFrames reports how many input frames have been consumed so far.
func (convolver *Convolver) QueuedInputFrames() int64 {
	return convolver.queuedFrames
}

// EmittedFrames reports how many wet frames have been produced so far,
// including frames still pending inside Drain chunks.
func (convolver *Convolver) EmittedFrames() int64 {
	return convolver.emitted
}

// ProcessBlock consumes one interleaved input block of one to BlockFrames
// frames and writes exactly BlockFrames wet frames into out, which must hold
// BlockFrames*outputChannels values. A block smaller than BlockFrames is the
// final input block and is zero-padded inside the sliding window.
func (convolver *Convolver) ProcessBlock(ctx context.Context, input []float32, inputChannels int, out []float32) error {
	if convolver.drained {
		return errors.New("convolver has already been drained")
	}
	frames := len(input) / inputChannels
	if frames < 1 || frames > BlockFrames {
		return errors.New("convolution block must contain one to BlockFrames frames")
	}
	if len(out) < BlockFrames*convolver.channels {
		return errors.New("convolution output buffer is too small")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	for channel := 0; channel < convolver.channels; channel++ {
		// Resolve the paired input channel. A stereo signal rendered through
		// a mono response keeps both L and R paths on their own output
		// channels; a mono input feeds every output channel.
		inputChannel := channel
		if inputChannels == 1 {
			inputChannel = 0
		}
		state := convolver.states[channel]
		// Standard overlap-save window [new block B | previous block B]:
		// shift the prior new block into the second half and load the new
		// input block into the first half. The circular convolution's first
		// half is then the valid linear result; its second half is discarded.
		copy(state.history[BlockFrames:], state.history[:BlockFrames])
		for frame := 0; frame < BlockFrames; frame++ {
			state.history[frame] = 0
		}
		for frame := 0; frame < frames; frame++ {
			state.history[frame] = float64(input[frame*inputChannels+inputChannel])
		}
		blockFFT := make([]complex128, fftSize)
		for bin := range blockFFT {
			blockFFT[bin] = complex(state.history[bin], 0)
		}
		fft(blockFFT)

		// Rotate the partition delay line: the newest input FFT enters at
		// position zero and older entries move toward the longest delay.
		for partition := len(state.fdl) - 1; partition > 0; partition-- {
			state.fdl[partition] = state.fdl[partition-1]
		}
		state.fdl[0] = blockFFT

		responsePartitions := convolver.responsePartitions(channel)
		spectrum := make([]complex128, fftSize)
		for partition, responseFFT := range responsePartitions {
			inputFFT := state.fdl[partition]
			for bin := 0; bin < fftSize; bin++ {
				spectrum[bin] += inputFFT[bin] * responseFFT[bin]
			}
		}
		ifft(spectrum)
		for frame := 0; frame < BlockFrames; frame++ {
			out[frame*convolver.channels+channel] = float32(real(spectrum[frame]))
		}
	}
	convolver.queuedFrames += int64(frames)
	convolver.emitted += BlockFrames
	return nil
}

// flushWindow advances one overlap-save window using an all-zero input block.
// It does not change the consumed input count: Drain flushes the tail only.
func (convolver *Convolver) flushWindow(ctx context.Context, out []float32) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	for channel := 0; channel < convolver.channels; channel++ {
		state := convolver.states[channel]
		copy(state.history[BlockFrames:], state.history[:BlockFrames])
		for frame := 0; frame < BlockFrames; frame++ {
			state.history[frame] = 0
		}
		blockFFT := make([]complex128, fftSize)
		for bin := range blockFFT {
			blockFFT[bin] = complex(state.history[bin], 0)
		}
		fft(blockFFT)
		for partition := len(state.fdl) - 1; partition > 0; partition-- {
			state.fdl[partition] = state.fdl[partition-1]
		}
		state.fdl[0] = blockFFT

		responsePartitions := convolver.responsePartitions(channel)
		spectrum := make([]complex128, fftSize)
		for partition, responseFFT := range responsePartitions {
			inputFFT := state.fdl[partition]
			for bin := 0; bin < fftSize; bin++ {
				spectrum[bin] += inputFFT[bin] * responseFFT[bin]
			}
		}
		ifft(spectrum)
		for frame := 0; frame < BlockFrames; frame++ {
			out[frame*convolver.channels+channel] = float32(real(spectrum[frame]))
		}
	}
	convolver.emitted += BlockFrames
	return nil
}

// Drain emits the convolution tail after the final input block. Each call
// returns up to BlockFrames interleaved frames; the final chunk may be
// shorter, after which (nil, nil) is returned and the convolver is
// exhausted. A response split into P partitions needs P flush windows after
// the last input block, so Drain keeps producing until the exact N+M-1
// output length has been emitted. Every chunk honors context cancellation.
func (convolver *Convolver) Drain(ctx context.Context) ([]float32, error) {
	if convolver.drained {
		return nil, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	if convolver.emitted >= convolver.totalOutput {
		convolver.drained = true
		return nil, nil
	}
	chunk := make([]float32, BlockFrames*convolver.channels)
	if err := convolver.flushWindow(ctx, chunk); err != nil {
		return nil, err
	}
	if overshoot := convolver.emitted - convolver.totalOutput; overshoot > 0 {
		valid := BlockFrames - int(overshoot)
		chunk = chunk[:int64(valid)*int64(convolver.channels)]
		convolver.emitted = convolver.totalOutput
	}
	if convolver.emitted >= convolver.totalOutput {
		convolver.drained = true
	}
	return chunk, nil
}

// responsePartitions returns the response FFT partition set for one output
// channel. A mono response is shared verbatim by both output channels; a
// stereo response pairs its left and right partitions with the matching
// output channel.
func (convolver *Convolver) responsePartitions(channel int) [][]complex128 {
	if convolver.prepared.Channels == 1 {
		return convolver.prepared.ChannelPartitions[0]
	}
	return convolver.prepared.ChannelPartitions[channel]
}

// fft performs an iterative in-place radix-2 discrete Fourier transform.
func fft(values []complex128) {
	size := len(values)
	if size&(size-1) != 0 || size < 2 {
		return
	}
	// Bit-reversal permutation.
	width := bitWidth(size)
	for index := 1; index < size; index++ {
		reversed := 0
		for bit := 0; bit < width; bit++ {
			if index&(1<<bit) != 0 {
				reversed |= 1 << (width - 1 - bit)
			}
		}
		if index < reversed {
			values[index], values[reversed] = values[reversed], values[index]
		}
	}
	for length := 2; length <= size; length <<= 1 {
		half := length >> 1
		angle := 2 * math.Pi / float64(length)
		root := complex(math.Cos(angle), math.Sin(angle))
		for start := 0; start < size; start += length {
			twiddle := complex(1, 0)
			for pair := 0; pair < half; pair++ {
				even := values[start+pair]
				odd := values[start+pair+half] * twiddle
				values[start+pair] = even + odd
				values[start+pair+half] = even - odd
				twiddle *= root
			}
		}
	}
}

// bitWidth returns log2(size) for a power-of-two size.
func bitWidth(size int) int {
	width := 0
	for size > 1 {
		size >>= 1
		width++
	}
	return width
}

// ifft computes the inverse transform in place with 1/N normalization. It
// uses the identity IFFT(X) = conj(FFT(conj(X)))/N.
func ifft(values []complex128) {
	for index := range values {
		values[index] = complex(real(values[index]), -imag(values[index]))
	}
	fft(values)
	scale := complex(1/float64(len(values)), 0)
	for index := range values {
		values[index] = scale * complex(real(values[index]), -imag(values[index]))
	}
}
