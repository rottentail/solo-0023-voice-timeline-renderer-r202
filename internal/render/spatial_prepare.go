package render

import (
	"context"
	"errors"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/convolve"
	"voice-timeline-renderer/internal/domain"
)

// ImpulseInput is one registered room impulse response loaded for a render.
// The stored WAV bytes are read-only inputs; preparation only copies samples.
type ImpulseInput struct {
	Asset domain.ImpulseAsset
	WAV   []byte
}

// preparedImpulse is the render-local, shared working set for one response.
// The convolve.Prepared partitions are read-only and reused by every span
// that references the response, while each span still owns a fresh convolver
// so processing histories never leak between spans.
type preparedImpulse struct {
	Asset          domain.ImpulseAsset
	Prepared       *convolve.Prepared
	TimelineFrames int64
	Resampled      bool
	ChannelAction  string
}

// prepareImpulses decodes, resamples to the timeline rate, and adapts every
// referenced response to the timeline output channel layout. Source media is
// never modified; all work happens on copied float32 frames.
func prepareImpulses(
	ctx context.Context,
	plan domain.Timeline,
	impulses map[string]ImpulseInput,
) (map[string]*preparedImpulse, error) {
	prepared := make(map[string]*preparedImpulse, len(impulses))
	seen := make(map[string]struct{})
	for _, span := range plan.Spans {
		if span.Spatial == nil {
			continue
		}
		if _, ok := seen[span.Spatial.ImpulseID]; ok {
			continue
		}
		seen[span.Spatial.ImpulseID] = struct{}{}
		input, ok := impulses[span.Spatial.ImpulseID]
		if !ok {
			return nil, errors.New("timeline references a missing impulse response")
		}
		clip, _, err := audiocodec.DecodeWAV(input.WAV)
		if err != nil {
			return nil, err
		}
		originalRate := clip.SampleRate
		resampled, err := audiocodec.Resample(ctx, clip, plan.SampleRate)
		if err != nil {
			return nil, err
		}
		// Adapt the response to the output layout once, so channel pairing is
		// explicit and a stereo left/right relationship is never split into
		// two independent mono convolutions.
		adapted, action, err := audiocodec.MapChannels(ctx, resampled, plan.OutputChannels)
		if err != nil {
			return nil, err
		}
		shared, err := convolve.Prepare(adapted.Samples, adapted.Channels, adapted.FrameCount())
		if err != nil {
			return nil, err
		}
		prepared[span.Spatial.ImpulseID] = &preparedImpulse{
			Asset:          input.Asset,
			Prepared:       shared,
			TimelineFrames: adapted.FrameCount(),
			Resampled:      originalRate != plan.SampleRate,
			ChannelAction:  action,
		}
	}
	return prepared, nil
}
