package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// virtualProbeGateCandidateHandler wires the minimum seams to drive
// resolveVirtualPlaybackSource down the candidate-declared upgrade path: a
// listed candidate that declares codecs/resolution/container, a stored row
// seeded by VirtualFileLookup, and a stub prober/saver.
func virtualProbeGateCandidateHandler(stored *models.MediaFile, lister VirtualPlaybackStreamLister, prober VirtualPlaybackSourceProber, saver VirtualFileMetadataSaver) *PlaybackHandler {
	return &PlaybackHandler{
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
			return "http://127.0.0.1:8080/stream?path=" + path, nil
		}),
		VirtualPlaybackStreamLister: lister,
		VirtualFileLookup: func(_ context.Context, _ string) (*models.MediaFile, error) {
			return stored, nil
		},
		VirtualPlaybackSourceProber: prober,
		VirtualFileMetadataSaver:    saver,
	}
}

func virtualProbeGateLister() VirtualPlaybackStreamListerFunc {
	return func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
		return []VirtualPlaybackStream{{
			ID:         "cand-1",
			URI:        "virtual://movie/tt-probe-gate?result=cand-1",
			Resolution: "1080p",
			CodecVideo: "h264",
			CodecAudio: "aac",
			Container:  "mkv",
			// A provider-declared language that the synthesis path folds in
			// before the probe. It must not survive as a duplicate track once
			// the real probed inventory replaces the synthesized one.
			AudioLanguages: []string{"deu"},
		}}, nil
	}
}

// A virtual row with no probe stamp (NULL tracks, NULL probe_updated_at) whose
// candidate declares enough metadata to synthesize complete-looking evidence
// must still run the real probe and persist its true inventory. The immediate
// plan keeps the synthesized tracks; persistence happens in the background.
func TestResolveProbesUnprobedVirtualRowDespiteCandidateDeclarations(t *testing.T) {
	stored := &models.MediaFile{
		ID:                         100,
		ContentID:                  "movie-1",
		FilePath:                   "virtual://movie/tt-probe-gate?result=cand-1",
		Container:                  "virtual",
		ProbeUpdatedAt:             nil,
		VirtualOwnerInstallationID: 5,
	}

	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	saverDone := make(chan struct{})
	var savedVideo, savedAudio, savedSubs []byte

	h := virtualProbeGateCandidateHandler(stored, virtualProbeGateLister(),
		func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
			close(probeStarted)
			<-releaseProbe
			// A real ffprobe inventory: different video codec and a track
			// inventory the candidate declarations cannot express.
			f.VideoTracks = []models.VideoTrack{{Codec: "hevc", Width: 3840, Height: 2160, FrameRate: "23.976"}}
			f.AudioTracks = []models.AudioTrack{
				{Codec: "eac3", Channels: 6, Language: "eng", Default: true},
				{Codec: "aac", Channels: 2, Language: "jpn"},
			}
			f.SubtitleTracks = []models.SubtitleTrack{{Codec: "srt", Language: "eng"}}
			f.CodecVideo = "hevc"
			f.CodecAudio = "eac3"
			f.Resolution = "2160p"
			f.Container = "mkv"
			return f, nil
		},
		func(_ context.Context, _ int, _ string, videoTracks, audioTracks, subtitleTracks []byte, _, _, _, _ string, _ bool, _ int, _ int) error {
			savedVideo = videoTracks
			savedAudio = audioTracks
			savedSubs = subtitleTracks
			close(saverDone)
			return nil
		})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	file := &models.MediaFile{
		ID:                         100,
		ContentID:                  "movie-1",
		FilePath:                   "virtual://movie/tt-probe-gate?result=cand-1",
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
	}
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}

	// The immediate plan is not blocked on the probe and still carries the
	// candidate-synthesized evidence so playback can start right away.
	if resolved.Provenance != ProbeProvenancePending || resolved.ProbeSucceeded {
		t.Fatalf("immediate result provenance=%q succeeded=%v, want pending/false", resolved.Provenance, resolved.ProbeSucceeded)
	}
	if resolved.File == nil || len(resolved.File.VideoTracks) != 1 || resolved.File.VideoTracks[0].Codec != "h264" {
		t.Fatalf("immediate plan video tracks = %#v, want synthesized h264", resolved.File)
	}
	if len(resolved.File.AudioTracks) != 1 || resolved.File.AudioTracks[0].Codec != "aac" {
		t.Fatalf("immediate plan audio tracks = %#v, want synthesized aac", resolved.File.AudioTracks)
	}
	if len(resolved.File.SubtitleTracks) != 0 {
		t.Fatalf("immediate plan subtitle tracks = %#v, want none", resolved.File.SubtitleTracks)
	}

	select {
	case <-probeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("prober was not invoked for an unprobed virtual row")
	}
	select {
	case <-saverDone:
		t.Fatal("metadata persisted before the probe finished")
	default:
	}

	close(releaseProbe)
	select {
	case <-saverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("probed inventory was not persisted")
	}

	// The persisted inventory must be the probe result, not the synthesized
	// candidate declaration.
	if !strings.Contains(string(savedVideo), "hevc") || strings.Contains(string(savedVideo), "h264") {
		t.Fatalf("persisted video tracks = %s, want probed hevc", savedVideo)
	}
	if !strings.Contains(string(savedAudio), "jpn") || !strings.Contains(string(savedAudio), "eac3") {
		t.Fatalf("persisted audio tracks = %s, want probed multi-track inventory", savedAudio)
	}
	// The candidate-declared German language was synthesized into the immediate
	// plan, but a real probe inventory must replace it rather than re-append it.
	if strings.Contains(string(savedAudio), "deu") {
		t.Fatalf("persisted audio tracks = %s, probe inventory must replace the synthesized track", savedAudio)
	}
	if !strings.Contains(string(savedSubs), "srt") {
		t.Fatalf("persisted subtitle tracks = %s, want probed srt", savedSubs)
	}
}

// A row that already carries a probe stamp and complete evidence keeps the fast
// skipProbe path: no provider round-trip and no re-probe on every play.
func TestResolveSkipsProbeForAlreadyProbedVirtualRow(t *testing.T) {
	probedAt := time.Now().Add(-time.Hour)
	stored := &models.MediaFile{
		ID:                         200,
		ContentID:                  "movie-1",
		FilePath:                   "virtual://movie/tt-probe-gate-b?result=cand-1",
		Container:                  "mkv",
		CodecVideo:                 "h264",
		CodecAudio:                 "aac",
		Resolution:                 "1080p",
		ProbeUpdatedAt:             &probedAt,
		VirtualOwnerInstallationID: 5,
		VideoTracks:                []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080, FrameRate: "24"}},
		AudioTracks:                []models.AudioTrack{{Codec: "aac", Channels: 2, Language: "eng", Default: true}},
	}

	probeCalls := 0
	h := virtualProbeGateCandidateHandler(stored, virtualProbeGateLister(),
		func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
			probeCalls++
			return f, nil
		}, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	file := *stored
	file.FilePath = stored.FilePath
	resolved, err := h.resolveVirtualPlaybackSource(req, &file, "profile-1", true, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if probeCalls != 0 {
		t.Fatalf("prober called %d times for an already-probed row, want 0", probeCalls)
	}
	if resolved.Provenance != ProbeProvenanceVerified || !resolved.ProbeSucceeded {
		t.Fatalf("provenance=%q succeeded=%v, want verified/true fast path", resolved.Provenance, resolved.ProbeSucceeded)
	}
}

// The probe gate can only converge if persisting a probe also stamps the row:
// without probe_updated_at a row that was just probed still looks unprobed on
// the next start and re-probes forever. virtual_collection rows keep their
// collection-owned stamp.
func TestVirtualFileMetadataUpdatePersistsProbeStamp(t *testing.T) {
	sql := VirtualFileMetadataUpdateSQL
	if !strings.Contains(sql, "probe_updated_at") {
		t.Fatalf("metadata update does not stamp probe_updated_at: %s", sql)
	}
	if !strings.Contains(sql, "probe_source=CASE WHEN media_files.probe_source='virtual_collection'") {
		t.Fatalf("metadata update does not preserve virtual_collection probe_source: %s", sql)
	}
	if !strings.Contains(sql, "probe_updated_at=CASE WHEN media_files.probe_source='virtual_collection'") {
		t.Fatalf("metadata update does not preserve virtual_collection probe_updated_at: %s", sql)
	}
	if !strings.Contains(sql, "ELSE 'virtual' END") {
		t.Fatalf("metadata update does not default probe_source to virtual: %s", sql)
	}
	if !strings.Contains(sql, "ELSE now() END") {
		t.Fatalf("metadata update does not stamp probe_updated_at with now(): %s", sql)
	}
}

// A failed probe consumed the whole probe budget; the next replan must not pay
// it again for the same candidate. The second call skips the prober and falls
// back to the candidate-declared metadata.
func TestResolveVirtualProbeFailureDamperSkipsRepeatProbe(t *testing.T) {
	uri := "virtual://movie/tt-negative-cache?result=cand-1"
	key := virtualProbeFailureKey(uri)
	virtualProbeFailures.clear(key)
	t.Cleanup(func() { virtualProbeFailures.clear(key) })

	stored := &models.MediaFile{
		ID:                         300,
		ContentID:                  "movie-1",
		FilePath:                   uri,
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
	}
	lister := VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
		return []VirtualPlaybackStream{{
			ID: "cand-neg", URI: uri, Resolution: "1080p",
			CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
		}}, nil
	})
	probeCalls := 0
	h := virtualProbeGateCandidateHandler(stored, lister,
		func(_ context.Context, _ string, _ *models.MediaFile) (*models.MediaFile, error) {
			probeCalls++
			return nil, errors.New("probe failed")
		}, nil)

	for call := 0; call < 2; call++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
		file := *stored
		resolved, err := h.resolveVirtualPlaybackSource(req, &file, "profile-1", false, nil, "", "", 0, false)
		if err != nil {
			t.Fatalf("call %d: resolveVirtualPlaybackSource error: %v", call, err)
		}
		if resolved.Provenance != ProbeProvenanceFailed {
			t.Fatalf("call %d: provenance=%q, want failed declared fallback", call, resolved.Provenance)
		}
	}
	if probeCalls != 1 {
		t.Fatalf("prober called %d times across two replans, want 1 after the failure damper engages", probeCalls)
	}
}

// A successful probe must leave no failure marker behind, so a later start is
// not damped by stale state.
func TestResolveVirtualProbeSuccessClearsFailureMarker(t *testing.T) {
	uri := "virtual://movie/tt-negative-cache-ok?result=cand-1"
	key := virtualProbeFailureKey(uri)
	virtualProbeFailures.clear(key)
	t.Cleanup(func() { virtualProbeFailures.clear(key) })

	stored := &models.MediaFile{
		ID:                         301,
		ContentID:                  "movie-1",
		FilePath:                   uri,
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
	}
	lister := VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
		return []VirtualPlaybackStream{{
			ID: "cand-neg-ok", URI: uri, Resolution: "1080p",
			CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
		}}, nil
	})
	probeCalls := 0
	h := virtualProbeGateCandidateHandler(stored, lister,
		func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
			probeCalls++
			f.VideoTracks = []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080}}
			f.AudioTracks = []models.AudioTrack{{Codec: "aac", Channels: 2, Language: "eng"}}
			f.CodecVideo, f.CodecAudio, f.Resolution, f.Container = "h264", "aac", "1080p", "mkv"
			return f, nil
		}, nil)

	// Seed a stale failure so the clear path is exercised after the probe.
	virtualProbeFailures.mark(key, time.Now().Add(-virtualProbeFailureTTL-time.Minute))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	file := *stored
	resolved, err := h.resolveVirtualPlaybackSource(req, &file, "profile-1", false, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if resolved.Provenance != ProbeProvenanceVerified {
		t.Fatalf("provenance=%q, want verified", resolved.Provenance)
	}
	if probeCalls != 1 {
		t.Fatalf("prober called %d times, want 1", probeCalls)
	}
	if virtualProbeFailures.recent(key, time.Now()) {
		t.Fatal("failure marker survived a successful probe")
	}
}

func TestVirtualProbeFailureCacheClearsMarker(t *testing.T) {
	key := "virtual://movie/tt-clear-marker"
	virtualProbeFailures.clear(key)
	t.Cleanup(func() { virtualProbeFailures.clear(key) })

	now := time.Now()
	virtualProbeFailures.mark(key, now)
	if !virtualProbeFailures.recent(key, now) {
		t.Fatal("fresh failure marker is not recent")
	}
	virtualProbeFailures.clear(key)
	if virtualProbeFailures.recent(key, now) {
		t.Fatal("cleared failure marker is still recent")
	}
}
