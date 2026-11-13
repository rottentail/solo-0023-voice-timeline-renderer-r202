package render

import (
	"context"
	"math"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/convolve"
	"voice-timeline-renderer/internal/domain"
)

// spanSpatialFacts captures the auditable per-span convolution details that
// feed the render receipt.
type spanSpatialFacts struct {
	Receipt       *domain.ReceiptSpatial
	Resampled     bool
	ChannelAction string
}

// mixSpatial renders a timeline that contains at least one spatially
// processed span. Spans are handled sequentially. Each span gets a brand new
// convolver whose working set is released before the next span starts, so
// spans reusing the same recording never inherit one another's processing
// history and at most one span convolver is live. The wet reverb tail is
// streamed straight into the shared mix, including frames beyond the span's
// dry content, so reverb can continue into following audio.
func mixSpatial(
	ctx context.Context,
	spans []preparedSpan,
	impulses map[string]*preparedImpulse,
	channels int,
) ([]float32, int64, []spanSpatialFacts, error) {
	dryFrames := int64(0)
	for _, span := range spans {
		if end := span.OutputEndFrame; end > dryFrames {
			dryFrames = end
		}
	}
	// Pre-compute total frames from the longest full convolution output.
	totalFrames := dryFrames
	for _, span := range spans {
		if span.Spatial == nil {
			continue
		}
		impulse := impulses[span.Spatial.ImpulseID]
		// The span's wet signal occupies outputFrames+responseFrames-1
		// frames starting at OutputStartFrame, which is also the frame count
		// of the output region it reaches.
		spanWetFrames := span.OutputStartFrame + span.OutputFrames + impulse.TimelineFrames - 1
		if spanWetFrames > totalFrames {
			totalFrames = spanWetFrames
		}
	}
	mix := make([]float32, int(totalFrames)*channels)
	facts := make([]spanSpatialFacts, len(spans))
	for _, span := range spans {
		select {
		case <-ctx.Done():
			return nil, 0, nil, ctx.Err()
		default:
		}
		gain := float32(audiocodec.DBToLinear(span.GainDB))
		if span.Spatial == nil {
			if err := writeDrySpan(ctx, mix, span, gain, channels); err != nil {
				return nil, 0, nil, err
			}
			continue
		}
		impulse := impulses[span.Spatial.ImpulseID]
		convolver, err := convolve.New(impulse.Prepared, channels, span.OutputFrames)
		if err != nil {
			return nil, 0, nil, err
		}
		wetGain := float32(audiocodec.DBToLinear(span.Spatial.WetGainDB))
		drySpatialGain := float32(audiocodec.DBToLinear(span.Spatial.DryGainDB))
		spanFrames := int(span.OutputFrames)
		blockFrames := convolve.BlockFrames
		block := make([]float32, blockFrames*channels)
		for offset := 0; offset < spanFrames; offset += blockFrames {
			select {
			case <-ctx.Done():
				return nil, 0, nil, ctx.Err()
			default:
			}
			count := blockFrames
			if offset+count > spanFrames {
				count = spanFrames - offset
			}
			// Build the faded, gain-scaled input block. The same crossfade
			// weights feed the dry path and the convolver, so the spatial
			// processing is a true convolution of the placed signal rather
			// than a repeated response.
			input := make([]float32, count*channels)
			for frame := 0; frame < count; frame++ {
				absolute := offset + frame
				incoming, outgoing := crossfadeWeights(span, int64(absolute))
				inStart := absolute * channels
				for channel := 0; channel < channels; channel++ {
					value := span.Clip.Samples[inStart+channel] * gain * incoming
					input[frame*channels+channel] = value
					// Dry contribution of a spatial span uses its explicit
					// dry gain instead of the legacy unit gain.
					dryValue := value * drySpatialGain
					outIndex := (int(span.OutputStartFrame) + absolute) * channels
					if span.CrossfadeFrames > 0 && int64(absolute) < span.CrossfadeFrames {
						mix[outIndex+channel] *= outgoing
					}
					mix[outIndex+channel] += dryValue
				}
			}
			if err := convolver.ProcessBlock(ctx, input, channels, block); err != nil {
				return nil, 0, nil, err
			}
			// Each processed block emits BlockFrames wet frames starting at
			// the span's output position.
			base := int(span.OutputStartFrame) + offset
			for index := 0; index < blockFrames*channels; index++ {
				mix[(base*channels)+index] += block[index] * wetGain
			}
		}
		// Stream the convolution tail one chunk at a time. Only the block
		// chunk is retained; the full tail working set is never held.
		for {
			chunk, drainErr := convolver.Drain(ctx)
			if drainErr != nil {
				return nil, 0, nil, drainErr
			}
			if len(chunk) == 0 {
				break
			}
			chunkFrames := len(chunk) / channels
			base := int(span.OutputStartFrame) + int(convolver.EmittedFrames()) - chunkFrames
			for index, value := range chunk {
				mix[base*channels+index] += value * wetGain
			}
		}
		wetEndFrame := span.OutputStartFrame + span.OutputFrames + impulse.TimelineFrames - 1
		tailStart := span.OutputEndFrame
		fact := spanSpatialFacts{
			Resampled:     impulse.Resampled,
			ChannelAction: impulse.ChannelAction,
			Receipt: &domain.ReceiptSpatial{
				ImpulseID:             span.Spatial.ImpulseID,
				ImpulseDigest:         impulse.Asset.Digest,
				ImpulseSourceRate:     impulse.Asset.SampleRate,
				ImpulseChannels:       impulse.Asset.Channels,
				ImpulseSourceFrames:   impulse.Asset.FrameCount,
				ImpulseTimelineFrames: impulse.TimelineFrames,
				Resampled:             impulse.Resampled,
				ChannelAction:         impulse.ChannelAction,
				WetGainDB:             span.Spatial.WetGainDB,
				DryGainDB:             span.Spatial.DryGainDB,
				TailStartFrame:        tailStart,
				TailEndFrame:          wetEndFrame,
				TailFrames:            wetEndFrame - tailStart,
			},
		}
		facts[span.Index] = fact
	}
	return mix, totalFrames, facts, nil
}

// writeDrySpan renders one non-spatial span with the exact legacy gain and
// equal-power crossfade semantics, keeping old-project output byte-identical.
func writeDrySpan(ctx context.Context, mix []float32, span preparedSpan, gain float32, channels int) error {
	frames := int(span.OutputFrames)
	for frame := 0; frame < frames; frame++ {
		if frame%4096 == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
		incoming, outgoing := crossfadeWeights(span, int64(frame))
		inStart := frame * channels
		outStart := (int(span.OutputStartFrame) + frame) * channels
		for channel := 0; channel < channels; channel++ {
			value := span.Clip.Samples[inStart+channel] * gain * incoming
			if span.CrossfadeFrames > 0 && int64(frame) < span.CrossfadeFrames {
				mix[outStart+channel] *= outgoing
			}
			mix[outStart+channel] += value
		}
	}
	return nil
}

// crossfadeWeights returns the equal-power incoming/outgoing weights for a
// frame inside a span's leading crossfade region.
func crossfadeWeights(span preparedSpan, frame int64) (incoming, outgoing float32) {
	incoming = 1
	outgoing = 1
	if span.CrossfadeFrames > 0 && frame < span.CrossfadeFrames {
		progress := (float64(frame) + 0.5) / float64(span.CrossfadeFrames)
		angle := progress * math.Pi / 2
		incoming = float32(math.Sin(angle))
		outgoing = float32(math.Cos(angle))
	}
	return incoming, outgoing
}
