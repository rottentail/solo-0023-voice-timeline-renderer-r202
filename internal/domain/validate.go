package domain

import (
	"math"
	"regexp"
	"strings"
)

var resourceIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{2,63}$`)

type SourceFacts struct {
	SampleRate    int
	Channels      int
	BitsPerSample int
	FrameCount    int64
}

func ValidateResourceID(field, value string) error {
	if !resourceIDPattern.MatchString(value) {
		return E(
			KindInvalid,
			"invalid_id",
			field,
			"identifier must be 3-64 characters using letters, digits, underscore, or hyphen",
		)
	}
	return nil
}

func ValidateName(value string) error {
	value = strings.TrimSpace(value)
	if len(value) < 1 || len(value) > 120 {
		return E(KindInvalid, "invalid_name", "name", "name must contain 1-120 characters")
	}
	return nil
}

func ValidateSourceFacts(field string, facts SourceFacts) error {
	if facts.SampleRate < 8000 || facts.SampleRate > 192000 {
		return E(KindInvalid, "invalid_sample_rate", field+".sample_rate", "sample rate must be between 8000 and 192000")
	}
	if facts.Channels != 1 && facts.Channels != 2 {
		return E(KindInvalid, "invalid_channels", field+".channels", "source channels must be mono or stereo")
	}
	if facts.BitsPerSample != 16 {
		return E(KindInvalid, "invalid_sample_format", field+".bits_per_sample", "source samples must use 16-bit integer PCM")
	}
	if facts.FrameCount < 1 {
		return E(KindInvalid, "empty_source", field+".frame_count", "source must contain at least one frame")
	}
	return nil
}

func ValidateTimeline(
	plan Timeline,
	sources map[string]SourceAsset,
	impulses map[string]ImpulseAsset,
	maxSpans int,
	maxOutputFrames int64,
) error {
	if err := ValidateResourceID("timeline_id", plan.ID); err != nil {
		return err
	}
	if err := ValidateName(plan.Name); err != nil {
		return err
	}
	if plan.SampleRate < 8000 || plan.SampleRate > 192000 {
		return E(KindInvalid, "invalid_sample_rate", "sample_rate", "timeline sample rate must be between 8000 and 192000")
	}
	if plan.OutputChannels != 1 && plan.OutputChannels != 2 {
		return E(KindInvalid, "invalid_channels", "output_channels", "output channels must be one or two")
	}
	if plan.TargetPeakDBFS < -24 || plan.TargetPeakDBFS > -0.1 {
		return E(KindInvalid, "invalid_target_peak", "target_peak_dbfs", "target peak must be between -24 and -0.1 dBFS")
	}
	if len(plan.Spans) < 1 || len(plan.Spans) > maxSpans {
		return E(KindInvalid, "invalid_span_count", "spans", "timeline span count is outside the allowed range")
	}

	cursor := int64(0)
	previousFrames := int64(0)
	for index, span := range plan.Spans {
		field := "spans[" + intString(index) + "]"
		source, ok := sources[span.SourceID]
		if !ok {
			return E(KindInvalid, "source_not_found", field+".source_id", "referenced source does not exist")
		}
		if span.SourceStartFrame < 0 || span.SourceEndFrame <= span.SourceStartFrame || span.SourceEndFrame > source.FrameCount {
			return E(
				KindInvalid,
				"invalid_source_interval",
				field+".source_end_frame",
				"source interval must be non-empty and remain inside the source frame count",
			)
		}
		if math.IsNaN(span.GainDB) || math.IsInf(span.GainDB, 0) || span.GainDB < -36 || span.GainDB > 12 {
			return E(KindInvalid, "invalid_gain", field+".gain_db", "gain must be finite and between -36 and 12 dB")
		}
		if span.CrossfadeFrames < 0 {
			return E(KindInvalid, "invalid_crossfade", field+".crossfade_frames", "crossfade cannot be negative")
		}
		if index == 0 && span.CrossfadeFrames != 0 {
			return E(KindInvalid, "invalid_crossfade", field+".crossfade_frames", "the first span cannot crossfade")
		}
		if err := ValidateSpatialProcess(field+".spatial", span.Spatial, impulses); err != nil {
			return err
		}

		outputFrames := ScaledFrames(span.SourceEndFrame-span.SourceStartFrame, source.SampleRate, plan.SampleRate)
		if outputFrames < 1 {
			return E(KindInvalid, "empty_span_output", field+".source_end_frame", "source interval becomes empty at the timeline sample rate")
		}
		if index > 0 {
			if span.CrossfadeFrames > previousFrames || span.CrossfadeFrames > outputFrames {
				return E(
					KindInvalid,
					"invalid_crossfade",
					field+".crossfade_frames",
					"crossfade cannot exceed either neighboring span duration",
				)
			}
			cursor -= span.CrossfadeFrames
		}
		cursor += outputFrames
		previousFrames = outputFrames
		if cursor > maxOutputFrames {
			return E(KindTooLarge, "timeline_too_long", "spans", "timeline output exceeds the configured frame limit")
		}
	}
	projected := TimelineProjectedFrames(plan, sources, impulses)
	if projected > maxOutputFrames {
		return E(KindTooLarge, "timeline_too_long", "spans", "timeline output including convolution tails exceeds the configured frame limit")
	}
	return nil
}

// ValidateSpatialProcess checks a per-span convolution configuration. A nil
// process means the span keeps the legacy gain/fade-only behavior.
func ValidateSpatialProcess(field string, process *SpatialProcess, impulses map[string]ImpulseAsset) error {
	if process == nil {
		return nil
	}
	if err := ValidateResourceID(field+".impulse_id", process.ImpulseID); err != nil {
		return err
	}
	if _, ok := impulses[process.ImpulseID]; !ok {
		return E(KindInvalid, "impulse_not_found", field+".impulse_id", "referenced impulse response does not exist")
	}
	for gainField, value := range map[string]float64{
		".wet_gain_db": process.WetGainDB,
		".dry_gain_db": process.DryGainDB,
	} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < -36 || value > 12 {
			return E(KindInvalid, "invalid_gain", field+gainField, "spatial gain must be finite and between -36 and 12 dB")
		}
	}
	return nil
}

func ScaledFrames(frameCount int64, sourceRate, targetRate int) int64 {
	if frameCount <= 0 || sourceRate <= 0 || targetRate <= 0 {
		return 0
	}
	return int64(math.Round(float64(frameCount) * float64(targetRate) / float64(sourceRate)))
}

func TimelineOutputFrames(plan Timeline, sources map[string]SourceAsset) int64 {
	total := int64(0)
	for _, span := range plan.Spans {
		source, ok := sources[span.SourceID]
		if !ok {
			return 0
		}
		total += ScaledFrames(span.SourceEndFrame-span.SourceStartFrame, source.SampleRate, plan.SampleRate)
		total -= span.CrossfadeFrames
	}
	return total
}

// TimelineProjectedFrames reports the frame count the render will produce,
// including the full convolution tail of the final spatially processed span.
// Linear convolution of N input frames with an M-frame response produces
// N+M-1 frames, so a span contributes (responseFrames-1) tail frames after
// its own dry content. Only the tail extending past the dry timeline grows
// the project duration; tails overlapping later span audio are mixed into
// the existing frames instead.
func TimelineProjectedFrames(plan Timeline, sources map[string]SourceAsset, impulses map[string]ImpulseAsset) int64 {
	dry := TimelineOutputFrames(plan, sources)
	if dry <= 0 {
		return 0
	}
	projected := dry
	cursor := int64(0)
	for index, span := range plan.Spans {
		source, ok := sources[span.SourceID]
		if !ok {
			return 0
		}
		outputFrames := ScaledFrames(span.SourceEndFrame-span.SourceStartFrame, source.SampleRate, plan.SampleRate)
		start := cursor
		if index > 0 {
			start -= span.CrossfadeFrames
		}
		cursor = start + outputFrames
		if span.Spatial == nil {
			continue
		}
		impulse, ok := impulses[span.Spatial.ImpulseID]
		if !ok {
			return 0
		}
		responseFrames := ScaledFrames(impulse.FrameCount, impulse.SampleRate, plan.SampleRate)
		if responseFrames < 1 {
			responseFrames = 1
		}
		// The span's convolution spans outputFrames+responseFrames-1 frames
		// starting at `start`, so it occupies that many output frames.
		spanWetFrames := start + outputFrames + responseFrames - 1
		if spanWetFrames > projected {
			projected = spanWetFrames
		}
	}
	return projected
}

func intString(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	position := len(digits)
	for value > 0 {
		position--
		digits[position] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[position:])
}
