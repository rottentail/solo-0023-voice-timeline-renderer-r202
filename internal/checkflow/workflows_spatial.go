package checkflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
)

// runSpatial exercises convolution reverb end to end: a response registered
// at a different sample rate, per-span spatial config persisted on the
// timeline, reverb tails that continue into following audio and extend the
// project, explicit auditable duration semantics, and a failed configuration
// path that publishes nothing.
func runSpatial(ctx context.Context, harness *Harness) error {
	// 44.1 kHz stereo response: deliberately different rate from the 48 kHz
	// timeline so resampling is exercised through the real HTTP boundary.
	impulseWAVBytes, err := impulseWAV(44100, 2, 300)
	if err != nil {
		return err
	}
	impulse, status, err := harness.createImpulse(ctx, "impulse_room_01", "studio booth", impulseWAVBytes)
	if err != nil {
		return err
	}
	if status != 201 || impulse.SampleRate != 44100 || impulse.Channels != 2 {
		return fmt.Errorf("impulse registration returned status=%d meta=%+v", status, impulse)
	}
	readImpulse, status, err := harness.getImpulse(ctx, impulse.ID)
	if err != nil || status != 200 || readImpulse.Digest != impulse.Digest {
		return fmt.Errorf("impulse read-back mismatch: status=%d err=%v", status, err)
	}

	// One recording reused by two spans. Each span must convolve
	// independently with a fresh processing history.
	sourceWAVBytes, err := sineWAV(48000, 1, 400, 220, 0.25)
	if err != nil {
		return err
	}
	source, status, err := harness.createSource(ctx, "source_spatial_01", "reused take", sourceWAVBytes)
	if err != nil || status != 201 {
		return fmt.Errorf("source setup failed: status=%d err=%v", status, err)
	}
	spatial := func() *domain.SpatialProcess {
		return &domain.SpatialProcess{ImpulseID: impulse.ID, WetGainDB: -3, DryGainDB: -6}
	}
	spans := []domain.TimelineSpan{
		{SourceID: source.ID, SourceStartFrame: 0, SourceEndFrame: 19200, GainDB: 0, CrossfadeFrames: 0, Spatial: spatial()},
		{SourceID: source.ID, SourceStartFrame: 0, SourceEndFrame: 19200, GainDB: 0, CrossfadeFrames: 0, Spatial: spatial()},
	}
	plan, status, err := harness.createTimeline(ctx, "timeline_spatial_01", "spatial assembly", 48000, 2, -1, spans)
	if err != nil {
		return err
	}
	if status != 201 || plan.State != "ready" {
		return fmt.Errorf("spatial timeline returned status=%d state=%s", status, plan.State)
	}
	if plan.OutputFrames != 38400 {
		return fmt.Errorf("dry output frames = %d, want 38400", plan.OutputFrames)
	}
	// Response is 13230 frames at 44.1 kHz -> 14400 frames at 48 kHz; the
	// final span's tail extends the project by M-1 = 14399 frames.
	if plan.ProjectedFrames != 52799 {
		return fmt.Errorf("projected frames = %d, want 52799", plan.ProjectedFrames)
	}

	job, status, err := harness.createRender(ctx, "render_spatial_01", plan.ID)
	if err != nil {
		return err
	}
	if status != 201 || job.State != domain.RenderCompleted || job.Output == nil {
		return fmt.Errorf("spatial render returned status=%d state=%s", status, job.State)
	}
	receipt, status, err := harness.getReceipt(ctx, job.ID)
	if err != nil || status != 200 {
		return fmt.Errorf("receipt read failed: status=%d err=%v", status, err)
	}
	if receipt.SchemaVersion != 2 || receipt.Duration == nil || receipt.Convolution == nil {
		return fmt.Errorf("spatial receipt missing v2 duration/convolution blocks: %+v", receipt)
	}
	if receipt.Duration.DryTimelineFrames != 38400 ||
		receipt.Duration.TailExtensionFrames != 14399 ||
		receipt.Duration.TotalOutputFrames != 52799 ||
		receipt.Output.FrameCount != 52799 {
		return fmt.Errorf("duration semantics mismatch: %+v output=%d", receipt.Duration, receipt.Output.FrameCount)
	}
	if receipt.Duration.Meaning == "" {
		return fmt.Errorf("duration meaning must be human-readable and re-checkable")
	}
	if len(receipt.Spans) != 2 {
		return fmt.Errorf("receipt span count = %d", len(receipt.Spans))
	}
	for index, span := range receipt.Spans {
		if span.Spatial == nil {
			return fmt.Errorf("span %d missing spatial receipt", index)
		}
		if span.Spatial.ImpulseDigest != impulse.Digest || !span.Spatial.Resampled ||
			span.Spatial.ImpulseSourceRate != 44100 || span.Spatial.ImpulseTimelineFrames != 14400 {
			return fmt.Errorf("span %d spatial receipt mismatch: %+v", index, span.Spatial)
		}
		if span.Spatial.TailFrames != 14399 {
			return fmt.Errorf("span %d tail frames = %d, want 14399", index, span.Spatial.TailFrames)
		}
	}
	if receipt.Convolution.ConvolverLimit != 1 || receipt.Convolution.BlockFrames <= 0 {
		return fmt.Errorf("convolution receipt must document the bounded streaming engine: %+v", receipt.Convolution)
	}

	output, status, err := harness.getOutput(ctx, job.ID)
	if err != nil || status != 200 {
		return fmt.Errorf("output read failed: status=%d err=%v", status, err)
	}
	clip, info, err := audiocodec.DecodeWAV(output)
	if err != nil {
		return fmt.Errorf("decode spatial output: %w", err)
	}
	if info.FrameCount != 52799 || info.Channels != 2 {
		return fmt.Errorf("decoded output frames=%d channels=%d", info.FrameCount, info.Channels)
	}
	digest := sha256.Sum256(output)
	if hex.EncodeToString(digest[:]) != receipt.Output.Digest {
		return fmt.Errorf("spatial output digest does not match receipt")
	}
	// The tail beyond the dry timeline must contain audible reverb energy,
	// and the stereo pair must not collapse to duplicated mono.
	tailEnergy, tailSamples := 0.0, 0
	leftRightDifference := 0.0
	for frame := int64(38400); frame < info.FrameCount; frame++ {
		left := float64(clip.Samples[frame*2])
		right := float64(clip.Samples[frame*2+1])
		tailEnergy += left*left + right*right
		tailSamples += 2
		leftRightDifference += math.Abs(left - right)
	}
	if tailSamples == 0 || tailEnergy/float64(tailSamples) < 1e-9 {
		return fmt.Errorf("reverb tail past the dry timeline is silent")
	}
	if leftRightDifference/float64(tailSamples) < 1e-6 {
		return fmt.Errorf("stereo response collapsed into duplicated mono channels")
	}

	// Spatial processing must be genuine convolution, not response playback:
	// identical config over different source content must change the output.
	otherWAV, err := sineWAV(48000, 1, 400, 660, 0.25)
	if err != nil {
		return err
	}
	if _, status, err := harness.createSource(ctx, "source_spatial_02", "different take", otherWAV); err != nil || status != 201 {
		return fmt.Errorf("second source setup failed: status=%d err=%v", status, err)
	}
	otherSpans := []domain.TimelineSpan{
		{SourceID: "source_spatial_02", SourceStartFrame: 0, SourceEndFrame: 19200, GainDB: 0, Spatial: spatial()},
		{SourceID: "source_spatial_02", SourceStartFrame: 0, SourceEndFrame: 19200, GainDB: 0, Spatial: spatial()},
	}
	otherPlan, status, err := harness.createTimeline(ctx, "timeline_spatial_02", "other spatial assembly", 48000, 2, -1, otherSpans)
	if err != nil || status != 201 {
		return fmt.Errorf("second timeline failed: status=%d err=%v", status, err)
	}
	otherJob, status, err := harness.createRender(ctx, "render_spatial_02", otherPlan.ID)
	if err != nil || status != 201 || otherJob.State != domain.RenderCompleted {
		return fmt.Errorf("second render failed: status=%d state=%s err=%v", status, otherJob.State, err)
	}
	otherOutput, status, err := harness.getOutput(ctx, otherJob.ID)
	if err != nil || status != 200 {
		return fmt.Errorf("second output failed: status=%d err=%v", status, err)
	}
	if sha256.Sum256(otherOutput) == digest {
		return fmt.Errorf("different source content produced identical spatial output")
	}

	// A timeline without spatial processing keeps the legacy v1 receipt and
	// no duration block, and its length stays at the dry span length.
	legacySpans := []domain.TimelineSpan{
		{SourceID: source.ID, SourceStartFrame: 0, SourceEndFrame: 19200, GainDB: -2, CrossfadeFrames: 0},
	}
	legacyPlan, status, err := harness.createTimeline(ctx, "timeline_spatial_legacy", "legacy assembly", 48000, 2, -1, legacySpans)
	if err != nil || status != 201 {
		return fmt.Errorf("legacy timeline failed: status=%d err=%v", status, err)
	}
	legacyJob, status, err := harness.createRender(ctx, "render_spatial_legacy", legacyPlan.ID)
	if err != nil || status != 201 {
		return fmt.Errorf("legacy render failed: status=%d err=%v", status, err)
	}
	legacyReceipt, status, err := harness.getReceipt(ctx, legacyJob.ID)
	if err != nil || status != 200 {
		return fmt.Errorf("legacy receipt failed: status=%d err=%v", status, err)
	}
	if legacyReceipt.SchemaVersion != 1 || legacyReceipt.Duration != nil || legacyReceipt.Convolution != nil {
		return fmt.Errorf("legacy project must keep a v1 receipt without spatial blocks: %+v", legacyReceipt)
	}
	if legacyReceipt.Output.FrameCount != 19200 {
		return fmt.Errorf("legacy render frames = %d, want 19200", legacyReceipt.Output.FrameCount)
	}

	// An unknown impulse response is rejected before persistence and never
	// starts a render.
	badSpans := []domain.TimelineSpan{
		{SourceID: source.ID, SourceStartFrame: 0, SourceEndFrame: 19200, GainDB: 0,
			Spatial: &domain.SpatialProcess{ImpulseID: "impulse_missing", WetGainDB: 0, DryGainDB: 0}},
	}
	_, status, err = harness.createTimeline(ctx, "timeline_spatial_bad", "bad spatial", 48000, 2, -1, badSpans)
	if err != nil {
		return err
	}
	if status != 422 {
		return fmt.Errorf("missing impulse returned status=%d, want 422", status)
	}

	// Source and response media remain untouched after all renders.
	sourceAfter, status, err := harness.getSource(ctx, source.ID)
	if err != nil || status != 200 || sourceAfter.Digest != source.Digest {
		return fmt.Errorf("source media changed after rendering")
	}
	impulseAfter, status, err := harness.getImpulse(ctx, impulse.ID)
	if err != nil || status != 200 || impulseAfter.Digest != impulse.Digest {
		return fmt.Errorf("impulse media changed after rendering")
	}
	return nil
}
