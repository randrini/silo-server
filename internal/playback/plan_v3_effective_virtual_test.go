package playback

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

// A virtual candidate substituted for the neutral requested row must be
// visible on the plan, or a client's version menu keeps showing the neutral
// label after a first play never adopts the version that actually played.
func TestPlanPlaybackV3ExposesEffectiveVirtualURI(t *testing.T) {
	candidate := detailedFixtureFileV3()
	candidate.FilePath = "virtual://movie/tt1234567?result=working"
	requested := &models.MediaFile{
		ID:        41,
		ContentID: candidate.ContentID,
		Container: candidate.Container,
		FilePath:  "virtual://movie/tt1234567",
	}
	req := validStartRequestV3()
	req.Capabilities.VideoDecode = []VideoDecodeCapabilityV3{{Codec: "hevc", Profiles: []string{"main 10"}, Levels: []int{153}, BitDepths: []int{10}, MaxWidth: 3840, MaxHeight: 2160, MaxFrameRate: 60, MaxBitrateKbps: 80_000, Hardware: true}}
	req.Capabilities.HDRDetails = &HDRCapabilitiesV3{HDR10: true}

	result := PlanPlaybackV3(PlannerInputV3{
		Request: req, RequestedFile: requested, EffectiveFile: candidate, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: true, Allow4KTranscode: true},
	})
	if result.Plan == nil {
		t.Fatalf("result = %#v, want a plan", result)
	}
	if result.Plan.EffectiveVirtualURI != candidate.FilePath {
		t.Fatalf("effective virtual URI = %q, want %q", result.Plan.EffectiveVirtualURI, candidate.FilePath)
	}
}

// A non-virtual effective source has no virtual URI to expose.
func TestPlanPlaybackV3OmitsEffectiveVirtualURIForLocalSource(t *testing.T) {
	file := detailedFixtureFileV3()
	req := validStartRequestV3()
	req.Capabilities.VideoDecode = []VideoDecodeCapabilityV3{{Codec: "hevc", Profiles: []string{"main 10"}, Levels: []int{153}, BitDepths: []int{10}, MaxWidth: 3840, MaxHeight: 2160, MaxFrameRate: 60, MaxBitrateKbps: 80_000, Hardware: true}}
	req.Capabilities.HDRDetails = &HDRCapabilitiesV3{HDR10: true}

	result := PlanPlaybackV3(PlannerInputV3{
		Request: req, RequestedFile: file, EffectiveFile: file, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: true, Allow4KTranscode: true},
	})
	if result.Plan == nil {
		t.Fatalf("result = %#v, want a plan", result)
	}
	if result.Plan.EffectiveVirtualURI != "" {
		t.Fatalf("effective virtual URI = %q, want empty for a local source", result.Plan.EffectiveVirtualURI)
	}
}

// The audio-only planner builds its own plan; it must expose the substituted
// virtual URI too.
func TestPlanPlaybackV3AudioOnlyExposesEffectiveVirtualURI(t *testing.T) {
	candidate := audioOnlyFixtureFileV3()
	candidate.FilePath = "virtual://audiobook/tt7654321?result=working"
	requested := &models.MediaFile{
		ID:        76,
		ContentID: candidate.ContentID,
		Container: candidate.Container,
		FilePath:  "virtual://audiobook/tt7654321",
	}
	req := validStartRequestV3()
	req.FileID = requested.ID
	req.Capabilities.Containers = []string{"mp4"}

	result := PlanPlaybackV3(PlannerInputV3{
		Request: req, RequestedFile: requested, EffectiveFile: candidate, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: true},
	})
	if result.Plan == nil {
		t.Fatalf("result = %#v, want a plan", result)
	}
	if result.Plan.EffectiveVirtualURI != candidate.FilePath {
		t.Fatalf("effective virtual URI = %q, want %q", result.Plan.EffectiveVirtualURI, candidate.FilePath)
	}
}

// The URI is a UI hint, not a route input: attaching it must not perturb plan
// identity, or replans would miss the cache and clients would see spurious new
// attempts.
func TestPlanAttemptKeyV3IgnoresEffectiveVirtualURI(t *testing.T) {
	candidate := detailedFixtureFileV3()
	candidate.FilePath = "virtual://movie/tt1234567?result=working"
	requested := &models.MediaFile{ID: 41, ContentID: candidate.ContentID, Container: candidate.Container, FilePath: "virtual://movie/tt1234567"}
	req := validStartRequestV3()
	req.Capabilities.VideoDecode = []VideoDecodeCapabilityV3{{Codec: "hevc", Profiles: []string{"main 10"}, Levels: []int{153}, BitDepths: []int{10}, MaxWidth: 3840, MaxHeight: 2160, MaxFrameRate: 60, MaxBitrateKbps: 80_000, Hardware: true}}
	req.Capabilities.HDRDetails = &HDRCapabilitiesV3{HDR10: true}

	result := PlanPlaybackV3(PlannerInputV3{
		Request: req, RequestedFile: requested, EffectiveFile: candidate, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: true, Allow4KTranscode: true},
	})
	if result.Plan == nil {
		t.Fatalf("result = %#v, want a plan", result)
	}

	before := PlanAttemptKeyV3(*result.Plan, "output-1", nil)
	mutated := *result.Plan
	mutated.EffectiveVirtualURI = "virtual://movie/tt1234567?result=other"
	if after := PlanAttemptKeyV3(mutated, "output-1", nil); after != before {
		t.Errorf("attempt key changed with the effective virtual URI: %q -> %q", before, after)
	}
	if mutated.PlanID != result.Plan.PlanID {
		// PlanID is computed before mutation, but guard the intent explicitly.
		t.Errorf("plan ID changed: %q -> %q", result.Plan.PlanID, mutated.PlanID)
	}
}
