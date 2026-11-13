package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"voice-timeline-renderer/internal/domain"
)

// fakeRepository is an in-memory Repository for service lifecycle tests.
type fakeRepository struct {
	jobs      map[string]domain.RenderJob
	timelines map[string]domain.Timeline
	sources   map[string]struct {
		asset domain.SourceAsset
		wav   []byte
	}
	impulses map[string]struct {
		asset domain.ImpulseAsset
		wav   []byte
	}
	committed map[string]bool
	saved     []domain.RenderJob
	// onImpulse, when set, runs during GetImpulse so a render can be
	// canceled after the durable queued/rendering states were written.
	onImpulse func()
}

func newFakeRepository() *fakeRepository {
	return &fakeRepository{
		jobs:      map[string]domain.RenderJob{},
		timelines: map[string]domain.Timeline{},
		sources: map[string]struct {
			asset domain.SourceAsset
			wav   []byte
		}{},
		impulses: map[string]struct {
			asset domain.ImpulseAsset
			wav   []byte
		}{},
		committed: map[string]bool{},
	}
}

func (repo *fakeRepository) SaveSource(asset domain.SourceAsset, wav []byte) error {
	repo.sources[asset.ID] = struct {
		asset domain.SourceAsset
		wav   []byte
	}{asset, wav}
	return nil
}
func (repo *fakeRepository) GetSourceMeta(id string) (domain.SourceAsset, error) {
	entry, ok := repo.sources[id]
	if !ok {
		return domain.SourceAsset{}, domain.E(domain.KindNotFound, "source_not_found", "source_id", "missing")
	}
	return entry.asset, nil
}
func (repo *fakeRepository) GetSource(id string) (domain.SourceAsset, []byte, error) {
	entry, ok := repo.sources[id]
	if !ok {
		return domain.SourceAsset{}, nil, domain.E(domain.KindNotFound, "source_not_found", "source_id", "missing")
	}
	return entry.asset, entry.wav, nil
}
func (repo *fakeRepository) SaveImpulse(asset domain.ImpulseAsset, wav []byte) error {
	repo.impulses[asset.ID] = struct {
		asset domain.ImpulseAsset
		wav   []byte
	}{asset, wav}
	return nil
}
func (repo *fakeRepository) GetImpulseMeta(id string) (domain.ImpulseAsset, error) {
	entry, ok := repo.impulses[id]
	if !ok {
		return domain.ImpulseAsset{}, domain.E(domain.KindNotFound, "impulse_not_found", "impulse_id", "missing")
	}
	return entry.asset, nil
}
func (repo *fakeRepository) GetImpulse(id string) (domain.ImpulseAsset, []byte, error) {
	if repo.onImpulse != nil {
		repo.onImpulse()
	}
	entry, ok := repo.impulses[id]
	if !ok {
		return domain.ImpulseAsset{}, nil, domain.E(domain.KindNotFound, "impulse_not_found", "impulse_id", "missing")
	}
	return entry.asset, entry.wav, nil
}
func (repo *fakeRepository) SaveTimeline(plan domain.Timeline) error {
	repo.timelines[plan.ID] = plan
	return nil
}
func (repo *fakeRepository) GetTimeline(id string) (domain.Timeline, error) {
	plan, ok := repo.timelines[id]
	if !ok {
		return domain.Timeline{}, domain.E(domain.KindNotFound, "timeline_not_found", "timeline_id", "missing")
	}
	return plan, nil
}
func (repo *fakeRepository) SaveJob(job domain.RenderJob) error {
	repo.jobs[job.ID] = job
	repo.saved = append(repo.saved, job)
	return nil
}
func (repo *fakeRepository) GetJob(id string) (domain.RenderJob, error) {
	job, ok := repo.jobs[id]
	if !ok {
		return domain.RenderJob{}, domain.E(domain.KindNotFound, "render_not_found", "render_id", "missing")
	}
	return job, nil
}
func (repo *fakeRepository) CommitRenderArtifacts(id string, _ []byte, _ domain.RenderReceipt) error {
	repo.committed[id] = true
	return nil
}
func (repo *fakeRepository) GetRenderOutput(id string) ([]byte, domain.RenderReceipt, error) {
	if !repo.committed[id] {
		return nil, domain.RenderReceipt{}, domain.E(domain.KindUnready, "render_not_completed", "render_id", "missing")
	}
	return []byte("wav"), domain.RenderReceipt{RenderID: id}, nil
}

// TestCanceledRenderFailsWithoutArtifacts proves a canceled spatial render
// reaches the terminal failed state with a stable error code and never
// publishes output.
func TestCanceledRenderFailsWithoutArtifacts(t *testing.T) {
	repo := newFakeRepository()
	serviceLayer := New(repo, DefaultConfig())

	// Pre-seed a source and a long impulse directly through storage.
	sourceWAV := makeTestWAV(t, 48000, 1, 4800)
	impulseWAV := makeTestWAV(t, 48000, 1, 4800)
	if err := repo.SaveSource(domain.SourceAsset{
		ID: "src", Name: "src", Digest: "src-digest", SampleRate: 48000, Channels: 1,
		BitsPerSample: 16, FrameCount: 4800, CreatedAt: time.Now().UTC(),
	}, sourceWAV); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveImpulse(domain.ImpulseAsset{
		ID: "ir", Name: "ir", Digest: "ir-digest", SampleRate: 48000, Channels: 1,
		BitsPerSample: 16, FrameCount: 4800, CreatedAt: time.Now().UTC(),
	}, impulseWAV); err != nil {
		t.Fatal(err)
	}
	plan := domain.Timeline{
		ID: "plan", Name: "plan", SampleRate: 48000, OutputChannels: 1, TargetPeakDBFS: -1,
		Spans: []domain.TimelineSpan{{
			SourceID: "src", SourceStartFrame: 0, SourceEndFrame: 4800, GainDB: 0,
			Spatial: &domain.SpatialProcess{ImpulseID: "ir", WetGainDB: 0, DryGainDB: 0},
		}},
		State: "ready",
	}
	if err := repo.SaveTimeline(plan); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel once the render has already persisted its queued/rendering
	// states and starts loading the impulse, which is after publication
	// became observable but before any output is written.
	repo.onImpulse = cancel
	job, _, err := serviceLayer.CreateRender(ctx, "render-cancel", RenderWriteRequest{TimelineID: "plan"})
	if err == nil {
		t.Fatal("expected an error from a canceled render")
	}
	var appErr *domain.AppError
	if !errors.As(err, &appErr) || appErr.Detail.Code != "render_canceled" {
		t.Fatalf("expected render_canceled stable code, got %v", err)
	}
	if job.State != domain.RenderFailed {
		t.Fatalf("canceled render state = %s, want failed", job.State)
	}
	if repo.committed["render-cancel"] {
		t.Fatal("canceled render must never commit artifacts")
	}
	persisted, getErr := repo.GetJob("render-cancel")
	if getErr != nil || persisted.State != domain.RenderFailed || persisted.Error == nil || persisted.Error.Code != "render_canceled" {
		t.Fatalf("persisted job must record the terminal failure, got %+v err=%v", persisted, getErr)
	}
}
