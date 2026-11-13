package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"time"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
)

// CreateImpulse registers an immutable room impulse response. The response
// is a distinct resource from source recordings: registering it never
// alters any source, and the stored bytes are never rewritten.
func (s *Service) CreateImpulse(ctx context.Context, id string, request ImpulseWriteRequest) (domain.ImpulseAsset, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return domain.ImpulseAsset{}, false, err
	}
	if err := domain.ValidateResourceID("impulse_id", id); err != nil {
		return domain.ImpulseAsset{}, false, err
	}
	request.Name = strings.TrimSpace(request.Name)
	if err := domain.ValidateName(request.Name); err != nil {
		return domain.ImpulseAsset{}, false, err
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(request.WAVBase64)
	if err != nil {
		return domain.ImpulseAsset{}, false, domain.E(domain.KindInvalid, "invalid_base64", "wav_base64", "media payload must be valid base64")
	}
	if int64(len(payload)) > s.config.MaxImpulseBytes {
		return domain.ImpulseAsset{}, false, domain.E(domain.KindTooLarge, "impulse_too_large", "wav_base64", "impulse payload exceeds the configured byte limit")
	}
	digestBytes := sha256.Sum256(payload)
	digest := hex.EncodeToString(digestBytes[:])

	existing, getErr := s.repo.GetImpulseMeta(id)
	if getErr == nil {
		if existing.Digest != digest || existing.Name != request.Name {
			return domain.ImpulseAsset{}, false, domain.E(domain.KindConflict, "impulse_conflict", "impulse_id", "identifier is already bound to different impulse content")
		}
		return existing, false, nil
	} else if appErr := domain.AsAppError(getErr); appErr.Kind != domain.KindNotFound {
		return domain.ImpulseAsset{}, false, getErr
	}

	clip, info, decodeErr := audiocodec.DecodeWAV(payload)
	if decodeErr != nil {
		return domain.ImpulseAsset{}, false, domain.Wrap(
			domain.KindInvalid,
			"invalid_wav",
			"wav_base64",
			"payload must be a RIFF/WAVE file containing 16-bit integer PCM with one or two channels",
			decodeErr,
		)
	}
	if info.FrameCount > s.config.MaxImpulseFrames {
		return domain.ImpulseAsset{}, false, domain.E(domain.KindTooLarge, "impulse_too_long", "wav_base64", "impulse response exceeds the configured frame limit")
	}
	facts := domain.SourceFacts{
		SampleRate:    info.SampleRate,
		Channels:      info.Channels,
		BitsPerSample: info.BitsPerSample,
		FrameCount:    info.FrameCount,
	}
	if err := domain.ValidateSourceFacts("impulse", facts); err != nil {
		return domain.ImpulseAsset{}, false, err
	}
	measurements := audiocodec.Measure(clip.Samples)
	asset := domain.ImpulseAsset{
		ID:            id,
		Name:          request.Name,
		Digest:        digest,
		ByteCount:     info.ByteCount,
		SampleRate:    info.SampleRate,
		Channels:      info.Channels,
		BitsPerSample: info.BitsPerSample,
		FrameCount:    info.FrameCount,
		DurationMS:    durationMilliseconds(info.FrameCount, info.SampleRate),
		PeakDBFS:      measurements.PeakDBFS,
		RMSDBFS:       measurements.RMSDBFS,
		CreatedAt:     time.Now().UTC(),
	}
	if err := s.repo.SaveImpulse(asset, payload); err != nil {
		return domain.ImpulseAsset{}, false, err
	}
	return asset, true, nil
}
