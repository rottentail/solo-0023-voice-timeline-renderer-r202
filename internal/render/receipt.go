package render

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/convolve"
	"voice-timeline-renderer/internal/domain"
)

// spatialReceiptData carries the auditable facts of one spatial render.
type spatialReceiptData struct {
	Facts       []spanSpatialFacts
	DryFrames   int64
	TotalFrames int64
	Impulses    map[string]*preparedImpulse
}

const durationMeaning = "total_output_frames is the dry timeline span length plus the tail extension needed to preserve the final span's full convolution reverb tail; tails that overlap later span audio are mixed into the dry timeline rather than extending it"

func buildReceipt(
	renderID string,
	plan domain.Timeline,
	encoded []byte,
	normalized normalizationResult,
	spans []preparedSpan,
	spatial *spatialReceiptData,
	now time.Time,
) (domain.RenderReceipt, domain.OutputAsset, error) {
	decoded, info, err := audiocodec.DecodeWAV(encoded)
	if err != nil {
		return domain.RenderReceipt{}, domain.OutputAsset{}, err
	}
	measurements := audiocodec.Measure(decoded.Samples)
	digest := sha256.Sum256(encoded)
	digestText := hex.EncodeToString(digest[:])
	output := domain.OutputAsset{
		Digest:        digestText,
		ByteCount:     int64(len(encoded)),
		SampleRate:    info.SampleRate,
		Channels:      info.Channels,
		BitsPerSample: info.BitsPerSample,
		FrameCount:    info.FrameCount,
		DurationMS:    framesToMilliseconds(info.FrameCount, info.SampleRate),
		PeakDBFS:      measurements.PeakDBFS,
		RMSDBFS:       measurements.RMSDBFS,
	}
	receiptSpans := make([]domain.ReceiptSpan, 0, len(spans))
	warnings := make([]string, 0)
	for spanPosition, span := range spans {
		receiptSpan := domain.ReceiptSpan{
			Index:            span.Index,
			SourceID:         span.SourceID,
			SourceDigest:     span.SourceDigest,
			SourceStartFrame: span.SourceStartFrame,
			SourceEndFrame:   span.SourceEndFrame,
			OutputStartFrame: span.OutputStartFrame,
			OutputEndFrame:   span.OutputEndFrame,
			OutputFrames:     span.OutputFrames,
			CrossfadeFrames:  span.CrossfadeFrames,
			GainDB:           span.GainDB,
			ChannelAction:    span.ChannelAction,
		}
		if spatial != nil && span.Spatial != nil {
			fact := spatial.Facts[spanPosition]
			receiptSpan.Spatial = fact.Receipt
			if fact.Resampled {
				warnings = append(warnings, fmt.Sprintf(
					"span %d impulse response resampled from %d Hz to timeline rate %d Hz",
					span.Index, fact.Receipt.ImpulseSourceRate, plan.SampleRate,
				))
			}
			if fact.ChannelAction != "unchanged" {
				warnings = append(warnings, fmt.Sprintf(
					"span %d impulse response applied %s to match output channels",
					span.Index, fact.ChannelAction,
				))
			}
		}
		if span.ChannelAction != "unchanged" {
			warnings = append(warnings, fmt.Sprintf("span %d applied %s", span.Index, span.ChannelAction))
		}
		receiptSpans = append(receiptSpans, receiptSpan)
	}
	receipt := domain.RenderReceipt{
		SchemaVersion: 1,
		RenderID:      renderID,
		TimelineID:    plan.ID,
		Output:        output,
		Normalization: domain.NormalizationReceipt{
			RequestedPeakDBFS: plan.TargetPeakDBFS,
			BeforePeakDBFS:    normalized.Before.PeakDBFS,
			AppliedGainDB:     normalized.GainDB,
			OutputPeakDBFS:    measurements.PeakDBFS,
		},
		Spans:     receiptSpans,
		Warnings:  warnings,
		CreatedAt: now.UTC(),
	}
	if spatial != nil {
		receipt.SchemaVersion = 2
		tailExtension := spatial.TotalFrames - spatial.DryFrames
		receipt.Duration = &domain.DurationSemantics{
			DryTimelineFrames:   spatial.DryFrames,
			TailExtensionFrames: tailExtension,
			TotalOutputFrames:   spatial.TotalFrames,
			Meaning:             durationMeaning,
		}
		receipt.Convolution = &domain.ConvolutionReceipt{
			Method:         "uniform_partition_overlap_save_fft",
			BlockFrames:    convolve.BlockFrames,
			FFTSize:        convolve.BlockFrames * 2,
			PreparedCount:  len(spatial.Impulses),
			MaxPartitions:  maxPartitions(spatial.Impulses),
			ConvolverLimit: 1,
		}
	}
	return receipt, output, nil
}

func maxPartitions(impulses map[string]*preparedImpulse) int {
	maximum := 0
	for _, impulse := range impulses {
		count := len(impulse.Prepared.ChannelPartitions[0])
		if count > maximum {
			maximum = count
		}
	}
	return maximum
}

func framesToMilliseconds(frames int64, sampleRate int) int64 {
	if frames <= 0 || sampleRate <= 0 {
		return 0
	}
	return int64(float64(frames) * 1000 / float64(sampleRate))
}
