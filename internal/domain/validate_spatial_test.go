package domain

import (
	"math"
	"testing"
)

var nanSentinel = math.NaN()

func TestValidateSpatialProcess(t *testing.T) {
	impulses := map[string]ImpulseAsset{
		"irr": {ID: "irr", SampleRate: 44100, Channels: 2, FrameCount: 1000},
	}
	cases := []struct {
		name     string
		process  *SpatialProcess
		wantCode string
	}{
		{"nil is allowed", nil, ""},
		{"valid", &SpatialProcess{ImpulseID: "irr", WetGainDB: -3, DryGainDB: 0}, ""},
		{"missing impulse", &SpatialProcess{ImpulseID: "ghost01", WetGainDB: 0, DryGainDB: 0}, "impulse_not_found"},
		{"bad wet gain", &SpatialProcess{ImpulseID: "irr", WetGainDB: 99, DryGainDB: 0}, "invalid_gain"},
		{"nan dry gain", &SpatialProcess{ImpulseID: "irr", WetGainDB: 0, DryGainDB: nanSentinel}, "invalid_gain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSpatialProcess("spans[0].spatial", tc.process, impulses)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("expected valid, got %v", err)
				}
				return
			}
			appErr := AsAppError(err)
			if appErr.Detail.Code != tc.wantCode {
				t.Fatalf("expected %s, got %v", tc.wantCode, err)
			}
		})
	}
}

func TestTimelineProjectedFrames(t *testing.T) {
	sources := map[string]SourceAsset{
		"src": {ID: "src", SampleRate: 48000, Channels: 1, FrameCount: 48000},
	}
	impulses := map[string]ImpulseAsset{
		// 24000 frames at 24 kHz resample to 48000 at 48 kHz.
		"irr": {ID: "irr", SampleRate: 24000, Channels: 1, FrameCount: 24000},
	}
	plan := Timeline{
		ID: "ttl", Name: "ttl", SampleRate: 48000, OutputChannels: 1, TargetPeakDBFS: -1,
		Spans: []TimelineSpan{
			{SourceID: "src", SourceStartFrame: 0, SourceEndFrame: 48000, GainDB: 0,
				Spatial: &SpatialProcess{ImpulseID: "irr", WetGainDB: 0, DryGainDB: 0}},
			{SourceID: "src", SourceStartFrame: 0, SourceEndFrame: 48000, GainDB: 0},
		},
	}
	dry := TimelineOutputFrames(plan, sources)
	if dry != 96000 {
		t.Fatalf("dry frames = %d, want 96000", dry)
	}
	projected := TimelineProjectedFrames(plan, sources, impulses)
	// First span's full wet output ends at 48000+48000-1 = 95999, inside the
	// 96000-frame dry timeline, so it must not extend the project.
	if projected != 96000 {
		t.Fatalf("overlapping tail must not extend duration, got %d", projected)
	}

	plan.Spans[1].Spatial = &SpatialProcess{ImpulseID: "irr", WetGainDB: 0, DryGainDB: 0}
	projected = TimelineProjectedFrames(plan, sources, impulses)
	// Final span ends at frame 96000; its tail adds 48000-1 frames.
	if projected != 143999 {
		t.Fatalf("final tail extension = %d, want 143999", projected)
	}
}

func TestValidateTimelineRejectsProjectedOverflow(t *testing.T) {
	sources := map[string]SourceAsset{
		"src": {ID: "src", SampleRate: 48000, Channels: 1, FrameCount: 48000},
	}
	impulses := map[string]ImpulseAsset{
		"irr": {ID: "irr", SampleRate: 48000, Channels: 1, FrameCount: 40000},
	}
	plan := Timeline{
		ID: "ttl", Name: "ttl", SampleRate: 48000, OutputChannels: 1, TargetPeakDBFS: -1,
		Spans: []TimelineSpan{
			{SourceID: "src", SourceStartFrame: 0, SourceEndFrame: 48000, GainDB: 0,
				Spatial: &SpatialProcess{ImpulseID: "irr", WetGainDB: 0, DryGainDB: 0}},
		},
	}
	err := ValidateTimeline(plan, sources, impulses, 64, 60000)
	if appErr := AsAppError(err); appErr.Detail.Code != "timeline_too_long" {
		t.Fatalf("expected timeline_too_long including tail, got %v", err)
	}
}
