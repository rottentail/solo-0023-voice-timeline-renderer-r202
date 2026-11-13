package render

import (
	"context"
	"errors"
	"time"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
)

type Limits struct {
	MaxOutputFrames int64
}

type Result struct {
	WAV     []byte
	Receipt domain.RenderReceipt
	Output  domain.OutputAsset
}

// Render executes one timeline. Timelines without any spatial process use
// the legacy gain/fade-only pipeline unchanged, so old projects produce the
// exact same bytes and a schema v1 receipt. Timelines with at least one
// spatially processed span run the streaming convolution pipeline, extend
// the project by the final reverb tail, and emit a schema v2 receipt whose
// duration semantics make the resulting project length explicit.
func Render(
	ctx context.Context,
	renderID string,
	plan domain.Timeline,
	sources map[string]SourceInput,
	impulses map[string]ImpulseInput,
	limits Limits,
	now time.Time,
) (Result, error) {
	spans, dryFrames, err := prepareSpans(ctx, plan, sources)
	if err != nil {
		return Result{}, err
	}
	if dryFrames < 1 {
		return Result{}, errors.New("timeline produced no output frames")
	}

	if !hasSpatialSpan(plan) {
		if limits.MaxOutputFrames > 0 && dryFrames > limits.MaxOutputFrames {
			return Result{}, errors.New("rendered timeline exceeds the configured frame limit")
		}
		mix, _, err := mixPrepared(ctx, spans, dryFrames, plan.OutputChannels)
		if err != nil {
			return Result{}, err
		}
		normalized := normalizePeak(mix, plan.TargetPeakDBFS)
		encoded, err := audiocodec.EncodePCM16(plan.SampleRate, plan.OutputChannels, normalized.Samples)
		if err != nil {
			return Result{}, err
		}
		receipt, output, err := buildReceipt(renderID, plan, encoded, normalized, spans, nil, now)
		if err != nil {
			return Result{}, err
		}
		return Result{WAV: encoded, Receipt: receipt, Output: output}, nil
	}

	preparedImpulses, err := prepareImpulses(ctx, plan, impulses)
	if err != nil {
		return Result{}, err
	}
	mix, totalFrames, facts, err := mixSpatial(ctx, spans, preparedImpulses, plan.OutputChannels)
	if err != nil {
		return Result{}, err
	}
	if totalFrames < 1 {
		return Result{}, errors.New("spatial timeline produced no output frames")
	}
	if limits.MaxOutputFrames > 0 && totalFrames > limits.MaxOutputFrames {
		return Result{}, errors.New("rendered timeline including convolution tails exceeds the configured frame limit")
	}
	normalized := normalizePeak(mix, plan.TargetPeakDBFS)
	encoded, err := audiocodec.EncodePCM16(plan.SampleRate, plan.OutputChannels, normalized.Samples)
	if err != nil {
		return Result{}, err
	}
	receipt, output, err := buildReceipt(renderID, plan, encoded, normalized, spans, &spatialReceiptData{
		Facts:       facts,
		DryFrames:   dryFrames,
		TotalFrames: totalFrames,
		Impulses:    preparedImpulses,
	}, now)
	if err != nil {
		return Result{}, err
	}
	return Result{WAV: encoded, Receipt: receipt, Output: output}, nil
}

func hasSpatialSpan(plan domain.Timeline) bool {
	for _, span := range plan.Spans {
		if span.Spatial != nil {
			return true
		}
	}
	return false
}
