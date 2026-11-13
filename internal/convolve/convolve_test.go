package convolve

import (
	"context"
	"errors"
	"math"
	"testing"
)

func directConvolve(input, impulse []float64) []float64 {
	out := make([]float64, len(input)+len(impulse)-1)
	for i, x := range input {
		for j, h := range impulse {
			out[i+j] += x * h
		}
	}
	return out
}

func TestConvolutionMatchesDirect(t *testing.T) {
	// Deliberately non-block-aligned sizes spanning several partitions.
	inputFrames := 2500
	impulseFrames := 2503
	input := make([]float32, inputFrames)
	impulse := make([]float32, impulseFrames)
	for i := range input {
		input[i] = float32(math.Sin(float64(i) * 0.07))
	}
	impulse[0] = 0.9
	impulse[7] = -0.4
	impulse[1000] = 0.25
	impulse[2500] = 0.11

	prepared, err := Prepare(impulse, 1, int64(impulseFrames))
	if err != nil {
		t.Fatal(err)
	}
	conv, err := New(prepared, 1, int64(inputFrames))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := conv.TotalOutputFrames(), int64(inputFrames+impulseFrames-1); got != want {
		t.Fatalf("total output frames = %d, want %d", got, want)
	}
	var wet []float32
	ctx := context.Background()
	for offset := 0; offset < inputFrames; offset += BlockFrames {
		count := BlockFrames
		if offset+count > inputFrames {
			count = inputFrames - offset
		}
		out := make([]float32, BlockFrames)
		if err := conv.ProcessBlock(ctx, input[offset:offset+count], 1, out); err != nil {
			t.Fatal(err)
		}
		wet = append(wet, out...)
	}
	for {
		chunk, err := conv.Drain(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if chunk == nil {
			break
		}
		wet = append(wet, chunk...)
	}
	if len(wet) != inputFrames+impulseFrames-1 {
		t.Fatalf("emitted %d frames, want %d", len(wet), inputFrames+impulseFrames-1)
	}
	reference := directConvolve(toFloat64(input), toFloat64(impulse))
	maxErr := 0.0
	for i, want := range reference {
		got := float64(wet[i])
		if err := math.Abs(got - want); err > maxErr {
			maxErr = err
		}
	}
	if maxErr > 1e-4 {
		t.Fatalf("max convolution error %g exceeds tolerance", maxErr)
	}
}

func TestImpulseDelayAndStereoPairing(t *testing.T) {
	inputFrames := 64
	impulseFrames := 32
	// Unit impulse delayed by 5 frames must reproduce the input shifted by 5.
	input := make([]float32, inputFrames*2)
	for i := 0; i < inputFrames; i++ {
		input[i*2] = float32(i + 1)
		input[i*2+1] = -float32(i + 1)
	}
	impulse := make([]float32, impulseFrames*2)
	impulse[5*2] = 1
	impulse[5*2+1] = 1
	prepared, err := Prepare(impulse, 2, int64(impulseFrames))
	if err != nil {
		t.Fatal(err)
	}
	conv, err := New(prepared, 2, int64(inputFrames))
	if err != nil {
		t.Fatal(err)
	}
	if conv.Channels() != 2 {
		t.Fatalf("stereo convolver channels = %d", conv.Channels())
	}
	out := make([]float32, BlockFrames*2)
	if err := conv.ProcessBlock(context.Background(), input, 2, out); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < inputFrames; i++ {
		frame := i + 5
		if got := out[frame*2]; math.Abs(float64(got)-float64(i+1)) > 1e-5 {
			t.Fatalf("left channel at frame %d = %v, want %d", frame, got, i+1)
		}
		if got := out[frame*2+1]; math.Abs(float64(got)+float64(i+1)) > 1e-5 {
			t.Fatalf("right channel at frame %d = %v, want %d", frame, got, -(i + 1))
		}
	}
}

func TestMonoImpulsePreservesStereoImage(t *testing.T) {
	inputFrames := 32
	impulseFrames := 8
	input := make([]float32, inputFrames*2)
	for i := 0; i < inputFrames; i++ {
		input[i*2] = 0.5
		input[i*2+1] = -0.25
	}
	impulse := make([]float32, impulseFrames)
	impulse[0] = 1
	prepared, err := Prepare(impulse, 1, int64(impulseFrames))
	if err != nil {
		t.Fatal(err)
	}
	conv, err := New(prepared, 2, int64(inputFrames))
	if err != nil {
		t.Fatal(err)
	}
	if conv.Channels() != 2 {
		t.Fatalf("mono-impulse stereo convolver channels = %d", conv.Channels())
	}
	out := make([]float32, BlockFrames*2)
	if err := conv.ProcessBlock(context.Background(), input, 2, out); err != nil {
		t.Fatal(err)
	}
	if out[0] != 0.5 || out[1] != -0.25 {
		t.Fatalf("mono impulse must preserve independent L/R values, got %v/%v", out[0], out[1])
	}
}

func toFloat64(values []float32) []float64 {
	out := make([]float64, len(values))
	for i, value := range values {
		out[i] = float64(value)
	}
	return out
}

// TestConvolverWorkingSetIsBounded proves the live convolver retains only
// fixed-size windows and delay lines: multiplying the input length by a
// large factor must not multiply the reachable working-set size. The
// full-length input and full convolution output are never stored.
func TestConvolverWorkingSetIsBounded(t *testing.T) {
	impulseFrames := BlockFrames*3 + 7
	impulse := make([]float32, impulseFrames)
	impulse[0] = 1
	prepared, err := Prepare(impulse, 1, int64(impulseFrames))
	if err != nil {
		t.Fatal(err)
	}
	measure := func(inputFrames int) int {
		input := make([]float32, inputFrames)
		for i := range input {
			input[i] = 0.1
		}
		conv, err := New(prepared, 1, int64(inputFrames))
		if err != nil {
			t.Fatal(err)
		}
		reachable := 0
		for _, state := range conv.states {
			reachable += len(state.history)
			for _, fftBlock := range state.fdl {
				reachable += len(fftBlock)
			}
		}
		return reachable
	}
	small := measure(BlockFrames * 2)
	large := measure(BlockFrames * 200)
	if large != small {
		t.Fatalf("convolver working set grew with input length: small=%d large=%d", small, large)
	}
	if large > impulseFrames*fftSize/BlockFrames+fftSize*8 {
		t.Fatalf("working set %d is not bounded relative to response length", large)
	}
}

// TestConvolversDoNotShareHistory proves two spans reusing the same
// recording and response start from independent, zeroed processing states.
func TestConvolversDoNotShareHistory(t *testing.T) {
	impulseFrames := 16
	impulse := make([]float32, impulseFrames)
	impulse[0] = 1
	prepared, err := Prepare(impulse, 1, int64(impulseFrames))
	if err != nil {
		t.Fatal(err)
	}
	first, err := New(prepared, 1, BlockFrames)
	if err != nil {
		t.Fatal(err)
	}
	firstInput := make([]float32, BlockFrames)
	for i := range firstInput {
		firstInput[i] = 0.9
	}
	firstOut := make([]float32, BlockFrames)
	if err := first.ProcessBlock(context.Background(), firstInput, 1, firstOut); err != nil {
		t.Fatal(err)
	}
	// A second, brand-new convolver must reproduce the same first-block
	// output instead of inheriting the first convolver's delay-line state.
	second, err := New(prepared, 1, BlockFrames)
	if err != nil {
		t.Fatal(err)
	}
	secondOut := make([]float32, BlockFrames)
	if err := second.ProcessBlock(context.Background(), firstInput, 1, secondOut); err != nil {
		t.Fatal(err)
	}
	for i := range firstOut {
		if math.Abs(float64(firstOut[i]-secondOut[i])) > 1e-6 {
			t.Fatalf("span convolvers share processing history at sample %d: %v vs %v", i, firstOut[i], secondOut[i])
		}
	}
}

// TestDrainHonorsCancellation verifies the tail loop stops promptly when the
// context is canceled, so a canceled render cannot keep producing output.
func TestDrainHonorsCancellation(t *testing.T) {
	impulseFrames := BlockFrames * 4
	impulse := make([]float32, impulseFrames)
	impulse[0] = 1
	prepared, err := Prepare(impulse, 1, int64(impulseFrames))
	if err != nil {
		t.Fatal(err)
	}
	conv, err := New(prepared, 1, BlockFrames)
	if err != nil {
		t.Fatal(err)
	}
	input := make([]float32, BlockFrames)
	out := make([]float32, BlockFrames)
	if err := conv.ProcessBlock(context.Background(), input, 1, out); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := conv.Drain(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Drain with canceled context = %v, want context.Canceled", err)
	}
}
