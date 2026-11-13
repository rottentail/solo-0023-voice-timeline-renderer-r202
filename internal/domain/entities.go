package domain

import "time"

type SourceAsset struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Digest        string    `json:"digest"`
	ByteCount     int64     `json:"byte_count"`
	SampleRate    int       `json:"sample_rate"`
	Channels      int       `json:"channels"`
	BitsPerSample int       `json:"bits_per_sample"`
	FrameCount    int64     `json:"frame_count"`
	DurationMS    int64     `json:"duration_ms"`
	PeakDBFS      float64   `json:"peak_dbfs"`
	RMSDBFS       float64   `json:"rms_dbfs"`
	CreatedAt     time.Time `json:"created_at"`
}

// SpatialProcess configures convolution reverb for one span. The pointer is
// nil for spans without spatial processing, which keeps legacy timelines and
// their rendered bytes unchanged.
type SpatialProcess struct {
	ImpulseID string  `json:"impulse_id"`
	WetGainDB float64 `json:"wet_gain_db"`
	DryGainDB float64 `json:"dry_gain_db"`
}

type TimelineSpan struct {
	SourceID         string          `json:"source_id"`
	SourceStartFrame int64           `json:"source_start_frame"`
	SourceEndFrame   int64           `json:"source_end_frame"`
	GainDB           float64         `json:"gain_db"`
	CrossfadeFrames  int64           `json:"crossfade_frames"`
	Spatial          *SpatialProcess `json:"spatial,omitempty"`
}

type Timeline struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	SampleRate     int            `json:"sample_rate"`
	OutputChannels int            `json:"output_channels"`
	TargetPeakDBFS float64        `json:"target_peak_dbfs"`
	Spans          []TimelineSpan `json:"spans"`
	State          string         `json:"state"`
	OutputFrames   int64          `json:"output_frames"`
	// ProjectedFrames includes the convolution tail of the final processed
	// span. It is zero for legacy timelines persisted before spatial
	// processing existed.
	ProjectedFrames int64     `json:"projected_frames,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

// ImpulseAsset is a registered, immutable room impulse response. It never
// shares storage identity with a source: source recordings and response
// material remain separate, read-only assets.
type ImpulseAsset struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Digest        string    `json:"digest"`
	ByteCount     int64     `json:"byte_count"`
	SampleRate    int       `json:"sample_rate"`
	Channels      int       `json:"channels"`
	BitsPerSample int       `json:"bits_per_sample"`
	FrameCount    int64     `json:"frame_count"`
	DurationMS    int64     `json:"duration_ms"`
	PeakDBFS      float64   `json:"peak_dbfs"`
	RMSDBFS       float64   `json:"rms_dbfs"`
	CreatedAt     time.Time `json:"created_at"`
}

type RenderState string

const (
	RenderQueued     RenderState = "queued"
	RenderRendering  RenderState = "rendering"
	RenderFinalizing RenderState = "finalizing"
	RenderCompleted  RenderState = "completed"
	RenderFailed     RenderState = "failed"
)

type OutputAsset struct {
	Digest        string  `json:"digest"`
	ByteCount     int64   `json:"byte_count"`
	SampleRate    int     `json:"sample_rate"`
	Channels      int     `json:"channels"`
	BitsPerSample int     `json:"bits_per_sample"`
	FrameCount    int64   `json:"frame_count"`
	DurationMS    int64   `json:"duration_ms"`
	PeakDBFS      float64 `json:"peak_dbfs"`
	RMSDBFS       float64 `json:"rms_dbfs"`
}

type RenderJob struct {
	ID            string       `json:"id"`
	TimelineID    string       `json:"timeline_id"`
	State         RenderState  `json:"state"`
	Error         *ErrorDetail `json:"error,omitempty"`
	Output        *OutputAsset `json:"output,omitempty"`
	ReceiptDigest string       `json:"receipt_digest,omitempty"`
	CreatedAt     time.Time    `json:"created_at"`
	UpdatedAt     time.Time    `json:"updated_at"`
}

type ReceiptSpan struct {
	Index            int             `json:"index"`
	SourceID         string          `json:"source_id"`
	SourceDigest     string          `json:"source_digest"`
	SourceStartFrame int64           `json:"source_start_frame"`
	SourceEndFrame   int64           `json:"source_end_frame"`
	OutputStartFrame int64           `json:"output_start_frame"`
	OutputEndFrame   int64           `json:"output_end_frame"`
	OutputFrames     int64           `json:"output_frames"`
	CrossfadeFrames  int64           `json:"crossfade_frames"`
	GainDB           float64         `json:"gain_db"`
	ChannelAction    string          `json:"channel_action"`
	Spatial          *ReceiptSpatial `json:"spatial,omitempty"`
}

// ReceiptSpatial records exactly how one span was convolved so the spatial
// processing can be replayed and audited from the receipt.
type ReceiptSpatial struct {
	ImpulseID             string  `json:"impulse_id"`
	ImpulseDigest         string  `json:"impulse_digest"`
	ImpulseSourceRate     int     `json:"impulse_source_rate_hz"`
	ImpulseChannels       int     `json:"impulse_channels"`
	ImpulseSourceFrames   int64   `json:"impulse_source_frames"`
	ImpulseTimelineFrames int64   `json:"impulse_timeline_frames"`
	Resampled             bool    `json:"resampled"`
	ChannelAction         string  `json:"channel_action"`
	WetGainDB             float64 `json:"wet_gain_db"`
	DryGainDB             float64 `json:"dry_gain_db"`
	// TailStartFrame is the first frame where only this span's wet tail is
	// still being emitted (after its own dry span has ended).
	TailStartFrame int64 `json:"tail_start_frame"`
	// TailEndFrame is the frame count reached by the full convolution output
	// (one past the last written frame index), matching half-open timeline
	// placement semantics.
	TailEndFrame int64 `json:"tail_end_frame"`
	TailFrames   int64 `json:"tail_frames"`
}

// DurationSemantics makes the final project duration explicit and auditable.
type DurationSemantics struct {
	// DryTimelineFrames is the project length based solely on placed spans,
	// identical to a non-spatial render of the same timeline.
	DryTimelineFrames int64 `json:"dry_timeline_frames"`
	// TailExtensionFrames extends the project past the dry timeline so the
	// final span's convolution reverb tail is preserved in full.
	TailExtensionFrames int64 `json:"tail_extension_frames"`
	// TotalOutputFrames = DryTimelineFrames + TailExtensionFrames and equals
	// the encoded WAV frame count.
	TotalOutputFrames int64  `json:"total_output_frames"`
	Meaning           string `json:"meaning"`
}

type ConvolutionReceipt struct {
	Method         string `json:"method"`
	BlockFrames    int    `json:"block_frames"`
	FFTSize        int    `json:"fft_size"`
	PreparedCount  int    `json:"prepared_impulse_count"`
	MaxPartitions  int    `json:"max_partitions_per_impulse"`
	ConvolverLimit int    `json:"concurrent_span_convolvers"`
}

type NormalizationReceipt struct {
	RequestedPeakDBFS float64 `json:"requested_peak_dbfs"`
	BeforePeakDBFS    float64 `json:"before_peak_dbfs"`
	AppliedGainDB     float64 `json:"applied_gain_db"`
	OutputPeakDBFS    float64 `json:"output_peak_dbfs"`
}

type RenderReceipt struct {
	SchemaVersion int                  `json:"schema_version"`
	RenderID      string               `json:"render_id"`
	TimelineID    string               `json:"timeline_id"`
	Output        OutputAsset          `json:"output"`
	Normalization NormalizationReceipt `json:"normalization"`
	Spans         []ReceiptSpan        `json:"spans"`
	Warnings      []string             `json:"warnings"`
	// Duration is present on spatial renders (schema v2) and states exactly
	// what the final project frame count includes.
	Duration    *DurationSemantics  `json:"duration,omitempty"`
	Convolution *ConvolutionReceipt `json:"convolution,omitempty"`
	CreatedAt   time.Time           `json:"created_at"`
}

func ValidRenderTransition(from, to RenderState) bool {
	switch from {
	case RenderQueued:
		return to == RenderRendering || to == RenderFailed
	case RenderRendering:
		return to == RenderFinalizing || to == RenderFailed
	case RenderFinalizing:
		return to == RenderCompleted || to == RenderFailed
	default:
		return false
	}
}

func (job *RenderJob) Transition(to RenderState, detail *ErrorDetail, now time.Time) error {
	if job.State == to {
		return nil
	}
	if !ValidRenderTransition(job.State, to) {
		return E(
			KindConflict,
			"invalid_state_transition",
			"state",
			"render job cannot move from "+string(job.State)+" to "+string(to),
		)
	}
	if to == RenderFailed && detail == nil {
		return E(KindInternal, "missing_failure_detail", "error", "a failed render requires an error detail")
	}
	if to != RenderFailed {
		detail = nil
	}
	job.State = to
	job.Error = detail
	job.UpdatedAt = now.UTC()
	return nil
}
