package render

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
)

func sineClip(sampleRate, channels, frames int, frequency float64) *audiocodec.Clip {
	samples := make([]float32, frames*channels)
	for frame := 0; frame < frames; frame++ {
		seconds := float64(frame) / float64(sampleRate)
		value := float32(math.Sin(2*math.Pi*frequency*seconds) * 0.4)
		if channels == 1 {
			samples[frame] = value
			continue
		}
		samples[frame*2] = value
		samples[frame*2+1] = value * 0.7
	}
	return &audiocodec.Clip{SampleRate: sampleRate, Channels: channels, Samples: samples}
}

func encode(t *testing.T, clip *audiocodec.Clip) []byte {
	t.Helper()
	payload, err := audiocodec.EncodePCM16(clip.SampleRate, clip.Channels, clip.Samples)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func basePlan() domain.Timeline {
	return domain.Timeline{
		ID:             "plan_test",
		Name:           "test plan",
		SampleRate:     48000,
		OutputChannels: 2,
		TargetPeakDBFS: -1,
	}
}

// TestLegacyRenderByteIdentity proves a timeline without spatial processing
// renders the exact same bytes whether or not spatial support exists.
func TestLegacyRenderByteIdentity(t *testing.T) {
	clip := sineClip(48000, 1, 4800, 220)
	wav := encode(t, clip)
	plan := basePlan()
	plan.Spans = []domain.TimelineSpan{
		{SourceID: "src", SourceStartFrame: 0, SourceEndFrame: 4800, GainDB: -2, CrossfadeFrames: 0},
	}
	first, err := Render(context.Background(), "r1", plan, map[string]SourceInput{
		"src": {Asset: domain.SourceAsset{ID: "src", Digest: "d", FrameCount: 4800, SampleRate: 48000, Channels: 1}, WAV: wav},
	}, nil, Limits{}, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Render(context.Background(), "r2", plan, map[string]SourceInput{
		"src": {Asset: domain.SourceAsset{ID: "src", Digest: "d", FrameCount: 4800, SampleRate: 48000, Channels: 1}, WAV: wav},
	}, map[string]ImpulseInput{}, Limits{}, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(first.WAV) != len(second.WAV) {
		t.Fatalf("legacy WAV length changed: %d vs %d", len(first.WAV), len(second.WAV))
	}
	for index := range first.WAV {
		if first.WAV[index] != second.WAV[index] {
			t.Fatalf("legacy WAV byte %d differs: %d vs %d", index, first.WAV[index], second.WAV[index])
		}
	}
	if first.Receipt.SchemaVersion != 1 || first.Receipt.Duration != nil {
		t.Fatalf("legacy receipt must stay schema v1 without duration block")
	}
}

// TestSpatialRenderTailAndReceipt checks the auditable duration math and the
// per-span independent processing for spans reusing one recording.
func TestSpatialRenderTailAndReceipt(t *testing.T) {
	sourceClip := sineClip(48000, 1, 4800, 220)
	sourceWAV := encode(t, sourceClip)
	// Response at a different sample rate: 24 kHz mono, 4800 frames.
	impulseClip := &audiocodec.Clip{
		SampleRate: 24000,
		Channels:   1,
		Samples:    make([]float32, 4800),
	}
	impulseClip.Samples[0] = 1
	for i := 1; i < 4800; i++ {
		impulseClip.Samples[i] = float32(math.Exp(-float64(i)/300) * 0.3)
	}
	impulseWAV := encode(t, impulseClip)
	plan := basePlan()
	plan.Spans = []domain.TimelineSpan{
		{SourceID: "src", SourceStartFrame: 0, SourceEndFrame: 4800, GainDB: 0, Spatial: &domain.SpatialProcess{ImpulseID: "ir", WetGainDB: 0, DryGainDB: 0}},
		{SourceID: "src", SourceStartFrame: 0, SourceEndFrame: 4800, GainDB: 0, Spatial: &domain.SpatialProcess{ImpulseID: "ir", WetGainDB: 0, DryGainDB: 0}},
	}
	result, err := Render(context.Background(), "r-spatial", plan,
		map[string]SourceInput{
			"src": {Asset: domain.SourceAsset{ID: "src", Digest: "src-digest", FrameCount: 4800, SampleRate: 48000, Channels: 1}, WAV: sourceWAV},
		},
		map[string]ImpulseInput{
			"ir": {Asset: domain.ImpulseAsset{ID: "ir", Digest: "ir-digest", FrameCount: 4800, SampleRate: 24000, Channels: 1}, WAV: impulseWAV},
		},
		Limits{}, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if result.Receipt.SchemaVersion != 2 || result.Receipt.Duration == nil {
		t.Fatalf("spatial receipt must be v2 with duration semantics")
	}
	// 4800 frames at 24 kHz -> 9600 frames at 48 kHz. Two 4800-frame spans,
	// final tail extends by 9600-1 = 9599.
	wantTotal := int64(9600 + 9599)
	if result.Receipt.Duration.DryTimelineFrames != 9600 ||
		result.Receipt.Duration.TailExtensionFrames != 9599 ||
		result.Receipt.Duration.TotalOutputFrames != wantTotal ||
		result.Output.FrameCount != wantTotal {
		t.Fatalf("duration math mismatch: %+v output=%d", result.Receipt.Duration, result.Output.FrameCount)
	}
	for index, span := range result.Receipt.Spans {
		if span.Spatial == nil || !span.Spatial.Resampled || span.Spatial.ImpulseDigest != "ir-digest" {
			t.Fatalf("span %d missing auditable spatial facts: %+v", index, span.Spatial)
		}
	}
}

// TestSpatialRenderCancellation confirms a canceled context aborts the
// render with an error and no result, so no finished artifact can be
// published afterward.
func TestSpatialRenderCancellation(t *testing.T) {
	sourceClip := sineClip(48000, 1, 9600, 220)
	sourceWAV := encode(t, sourceClip)
	impulseClip := &audiocodec.Clip{SampleRate: 48000, Channels: 1, Samples: make([]float32, 4800)}
	impulseClip.Samples[0] = 1
	impulseWAV := encode(t, impulseClip)
	plan := basePlan()
	plan.Spans = []domain.TimelineSpan{
		{SourceID: "src", SourceStartFrame: 0, SourceEndFrame: 9600, GainDB: 0, Spatial: &domain.SpatialProcess{ImpulseID: "ir", WetGainDB: 0, DryGainDB: 0}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Render(ctx, "r-cancel", plan,
		map[string]SourceInput{"src": {Asset: domain.SourceAsset{ID: "src", Digest: "d", FrameCount: 9600, SampleRate: 48000, Channels: 1}, WAV: sourceWAV}},
		map[string]ImpulseInput{"ir": {Asset: domain.ImpulseAsset{ID: "ir", Digest: "i", FrameCount: 4800, SampleRate: 48000, Channels: 1}, WAV: impulseWAV}},
		Limits{}, time.Unix(100, 0))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// TestNoSpatialConfigMeansNoConvolution ensures an omitted spatial block
// leaves a span's dry signal unmodified apart from its existing gain.
func TestNoSpatialConfigMeansNoConvolution(t *testing.T) {
	clip := sineClip(48000, 1, 2048, 330)
	wav := encode(t, clip)
	plan := basePlan()
	plan.OutputChannels = 1
	plan.Spans = []domain.TimelineSpan{
		{SourceID: "src", SourceStartFrame: 0, SourceEndFrame: 2048, GainDB: 0, CrossfadeFrames: 0},
	}
	result, err := Render(context.Background(), "r-dry", plan,
		map[string]SourceInput{"src": {Asset: domain.SourceAsset{ID: "src", Digest: "d", FrameCount: 2048, SampleRate: 48000, Channels: 1}, WAV: wav}},
		nil, Limits{}, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	decoded, _, err := audiocodec.DecodeWAV(result.WAV)
	if err != nil {
		t.Fatal(err)
	}
	// Peak normalization targets -1 dBFS, so a pure dry render should reach
	// approximately that peak with no reverb smearing.
	metrics := audiocodec.Measure(decoded.Samples)
	if math.Abs(metrics.PeakDBFS-(-1)) > 0.25 {
		t.Fatalf("dry render peak %.3f not near -1 dBFS target", metrics.PeakDBFS)
	}
	if result.Output.FrameCount != 2048 {
		t.Fatalf("dry render must not grow beyond span frames, got %d", result.Output.FrameCount)
	}
}
