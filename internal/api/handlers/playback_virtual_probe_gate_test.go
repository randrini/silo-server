package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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
	virtualProbeFailures.mu.Lock()
	virtualProbeFailures.marks[key] = virtualProbeFailureMark{
		ttl:       virtualProbeFailureTTL,
		expiresAt: time.Now().Add(-time.Minute),
	}
	virtualProbeFailures.mu.Unlock()

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
	if virtualProbeFailures.recent(key) {
		t.Fatal("failure marker survived a successful probe")
	}
}

func TestVirtualProbeFailureCacheClearsMarker(t *testing.T) {
	key := "virtual://movie/tt-clear-marker"
	virtualProbeFailures.clear(key)
	t.Cleanup(func() { virtualProbeFailures.clear(key) })

	virtualProbeFailures.mark(key)
	if !virtualProbeFailures.recent(key) {
		t.Fatal("fresh failure marker is not recent")
	}
	virtualProbeFailures.clear(key)
	if virtualProbeFailures.recent(key) {
		t.Fatal("cleared failure marker is still recent")
	}
}

// virtualRepeatPlayFile is a virtual row that already carries a probe stamp and
// complete probed evidence: the state the repeat-play fast path recognizes.
func virtualRepeatPlayFile(path string) *models.MediaFile {
	probedAt := time.Now().Add(-time.Hour)
	return &models.MediaFile{
		ID:                         700,
		ContentID:                  "movie-repeat",
		FilePath:                   path,
		Container:                  "mkv",
		CodecVideo:                 "h264",
		CodecAudio:                 "aac",
		Resolution:                 "1080p",
		ProbeUpdatedAt:             &probedAt,
		VirtualOwnerInstallationID: 5,
		VideoTracks:                []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080, FrameRate: "24"}},
		AudioTracks:                []models.AudioTrack{{Codec: "aac", Channels: 2, Language: "eng"}},
	}
}

// virtualRepeatPlayHandler wires detailed- and legacy-resolver spies. detailed
// counts every ResolveVirtualMediaDetailed invocation; the repeat-play fast
// path must leave it at zero on a replay. The legacy resolver is present only
// because resolveVirtualPlaybackSource requires one to be configured; the
// detailed resolver always takes precedence when both are set.
func virtualRepeatPlayHandler(detailedCalls, legacyCalls *int) *PlaybackHandler {
	return &PlaybackHandler{
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(
			func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
				*legacyCalls++
				return "http://provider.example/legacy?path=" + path, nil
			}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(
			func(_ context.Context, virtualURI string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
				*detailedCalls++
				return ResolvedVirtualMedia{URL: "http://provider.example/stream.mkv", URI: virtualURI}, nil
			}),
	}
}

// A replay of a virtual row that already owns an adopted result= candidate,
// complete evidence, and a probe stamp must not call the provider resolver:
// the serve relay re-resolves and owns failover, so the start path only needs
// the persisted URI.
func TestResolveVirtualRepeatPlaySkipsProviderResolve(t *testing.T) {
	detailedCalls, legacyCalls := 0, 0
	h := virtualRepeatPlayHandler(&detailedCalls, &legacyCalls)
	file := virtualRepeatPlayFile("virtual://movie/tt-repeat?result=cand-1")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if detailedCalls != 0 || legacyCalls != 0 {
		t.Fatalf("resolvers called detailed=%d legacy=%d, want 0/0 on a repeat play", detailedCalls, legacyCalls)
	}
	if resolved.URL != "" {
		t.Fatalf("resolved URL = %q, want empty (serve relay owns resolution)", resolved.URL)
	}
	if resolved.URI != file.FilePath {
		t.Fatalf("resolved URI = %q, want persisted %q", resolved.URI, file.FilePath)
	}
	if resolved.Provenance != ProbeProvenancePending || resolved.ProbeSucceeded {
		t.Fatalf("provenance=%q succeeded=%v, want pending/false", resolved.Provenance, resolved.ProbeSucceeded)
	}
	if resolved.File == nil || resolved.File.FilePath != file.FilePath {
		t.Fatalf("resolved file = %#v, want persisted candidate", resolved.File)
	}
}

// A sticky pin plus a best-result cache hit clears noResult; the repeat-play
// fast path must then skip the resolver while returning the pinned URI.
func TestResolveVirtualRepeatPlaySkipsProviderResolveWithStickyPin(t *testing.T) {
	detailedCalls, legacyCalls := 0, 0
	h := virtualRepeatPlayHandler(&detailedCalls, &legacyCalls)
	h.BestResultCache = NewVirtualBestResultCache(time.Hour, 16)

	file := virtualRepeatPlayFile("virtual://movie/tt-repeat-pin")
	pinned := "virtual://movie/tt-repeat-pin?result=cand-9"
	neutral := virtualPlaybackNeutralKey(file.FilePath)
	stickyKey := bestResultCacheKey(file.ContentID, neutral, file.VirtualOwnerInstallationID, "")
	h.pinVirtualSticky(stickyKey, pinned)
	h.BestResultCache.set(bestResultCacheKey(file.ContentID, neutral, file.VirtualOwnerInstallationID, ""), []VirtualPlaybackStream{{
		URI: pinned, Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
	}}, time.Now())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if detailedCalls != 0 || legacyCalls != 0 {
		t.Fatalf("resolvers called detailed=%d legacy=%d, want 0/0 for a pinned candidate", detailedCalls, legacyCalls)
	}
	if resolved.URI != pinned {
		t.Fatalf("resolved URI = %q, want pinned %q", resolved.URI, pinned)
	}
}

// The fast path is start-only. forceRelist (explicit re-selection), noResult
// (neutral row with no adopted pick), incomplete evidence, and a missing probe
// stamp must all keep resolving synchronously.
func TestResolveVirtualRepeatPlayStillResolvesWhenRequired(t *testing.T) {
	cases := []struct {
		name        string
		path        string
		deferProbe  bool
		forceRelist bool
		mutate      func(*models.MediaFile)
		wantCalls   int
	}{
		{
			name: "forceRelist", path: "virtual://movie/tt-repeat?result=cand-1",
			deferProbe: true, forceRelist: true, wantCalls: 1,
		},
		{
			name: "noResult", path: "virtual://movie/tt-repeat",
			deferProbe: true, wantCalls: 1,
		},
		{
			name: "evidenceIncomplete", path: "virtual://movie/tt-repeat?result=cand-1",
			deferProbe: true, mutate: func(f *models.MediaFile) { f.AudioTracks = nil }, wantCalls: 1,
		},
		{
			name: "probeMissing", path: "virtual://movie/tt-repeat?result=cand-1",
			deferProbe: true, mutate: func(f *models.MediaFile) { f.ProbeUpdatedAt = nil }, wantCalls: 1,
		},
		{
			name: "synchronousDeferProbeFalse", path: "virtual://movie/tt-repeat?result=cand-1",
			deferProbe: false, wantCalls: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			detailedCalls, legacyCalls := 0, 0
			h := virtualRepeatPlayHandler(&detailedCalls, &legacyCalls)
			file := virtualRepeatPlayFile(tc.path)
			if tc.mutate != nil {
				tc.mutate(file)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
			if _, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", tc.deferProbe, nil, "", "", 0, tc.forceRelist); err != nil {
				t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
			}
			if detailedCalls != tc.wantCalls {
				t.Fatalf("detailed resolver called %d times, want %d", detailedCalls, tc.wantCalls)
			}
			if legacyCalls != 0 {
				t.Fatalf("legacy resolver called %d times, want 0", legacyCalls)
			}
		})
	}
}

// The damper backs off exponentially: each consecutive failure for the same
// key doubles the window, capped at virtualProbeFailureMaxTTL. The stored
// expiry must be derived from the injected clock, not the wall clock.
func TestVirtualProbeFailureCacheBackoffGrowsAndCaps(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cache := &virtualProbeFailureCache{
		marks: make(map[string]virtualProbeFailureMark),
		now:   func() time.Time { return now },
	}
	key := "virtual://movie/tt-backoff"

	cache.mark(key)
	first := cache.marks[key]
	if first.ttl != virtualProbeFailureTTL {
		t.Fatalf("first failure ttl=%s, want %s", first.ttl, virtualProbeFailureTTL)
	}
	if want := now.Add(virtualProbeFailureTTL); !first.expiresAt.Equal(want) {
		t.Fatalf("first failure expiresAt=%s, want %s", first.expiresAt, want)
	}

	// A second failure, after the first window lapses but while the marker is
	// still retained, extends the stored expiry to 10 minutes.
	now = now.Add(virtualProbeFailureTTL + time.Minute)
	cache.mark(key)
	second := cache.marks[key]
	if second.ttl != 2*virtualProbeFailureTTL {
		t.Fatalf("second failure ttl=%s, want %s", second.ttl, 2*virtualProbeFailureTTL)
	}
	if !second.expiresAt.After(first.expiresAt) {
		t.Fatalf("second failure expiresAt=%s did not grow past first %s", second.expiresAt, first.expiresAt)
	}

	// Keep failing: the window doubles until it hits the cap and stays there.
	for i := 0; i < 6; i++ {
		now = now.Add(cache.marks[key].ttl)
		cache.mark(key)
	}
	if got := cache.marks[key].ttl; got != virtualProbeFailureMaxTTL {
		t.Fatalf("capped ttl=%s, want %s", got, virtualProbeFailureMaxTTL)
	}
}

// recent honors the per-key backoff window: it is true inside the window and
// false once the stored expiry passes, dropping the marker on read.
func TestVirtualProbeFailureCacheRecentHonorsBackoff(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cache := &virtualProbeFailureCache{
		marks: make(map[string]virtualProbeFailureMark),
		now:   func() time.Time { return now },
	}
	key := "virtual://movie/tt-backoff-recent"

	cache.mark(key)
	if !cache.recent(key) {
		t.Fatal("fresh marker is not recent")
	}
	now = now.Add(virtualProbeFailureTTL - time.Second)
	if !cache.recent(key) {
		t.Fatal("marker inside its window is not recent")
	}
	now = now.Add(2 * time.Second)
	if cache.recent(key) {
		t.Fatal("marker past its window is still recent")
	}
	if _, ok := cache.marks[key]; ok {
		t.Fatal("expired marker was not pruned on read")
	}
}

// virtualOptimisticFile is a virtual row that has never been probed (no probe
// stamp, no stored tracks) but may carry recent delivery evidence: the state
// the optimistic-start gate recognizes.
func virtualOptimisticFile(path string, deliveredAt *time.Time) *models.MediaFile {
	return &models.MediaFile{
		ID:                         901,
		ContentID:                  "movie-optimistic",
		FilePath:                   path,
		Container:                  "virtual",
		LastDeliveredAt:            deliveredAt,
		VirtualOwnerInstallationID: 5,
	}
}

// virtualOptimisticGateHandler wires a non-blocking resolver spy and a
// permissive prober so the synchronous-resolve fallback is observable by call
// count.
func virtualOptimisticGateHandler(detailedCalls *int32) *PlaybackHandler {
	return &PlaybackHandler{
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(
			func(_ context.Context, virtualURI string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
				atomic.AddInt32(detailedCalls, 1)
				return ResolvedVirtualMedia{URL: "http://provider.example/stream.mkv", URI: virtualURI}, nil
			}),
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(
			func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
				return "http://provider.example/legacy?path=" + path, nil
			}),
		VirtualPlaybackSourceProber: func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
			f.VideoTracks = []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080}}
			f.AudioTracks = []models.AudioTrack{{Codec: "aac", Channels: 2, Language: "eng"}}
			f.CodecVideo, f.CodecAudio, f.Resolution, f.Container = "h264", "aac", "1080p", "mkv"
			return f, nil
		},
	}
}

// A row that delivered bytes recently starts optimistically even when the
// probe stamp is missing: the start path returns the persisted URI without
// waiting on the provider, and the background chain resolves and probes so the
// next start takes the P0 fast path. The resolver is held open, so the start
// can only return if it never called it synchronously.
func TestResolveVirtualOptimisticStartWithinDeliveryGrace(t *testing.T) {
	uri := "virtual://movie/tt-optimistic?result=cand-1"
	deliveredAt := time.Now().Add(-time.Hour)
	stored := virtualOptimisticFile(uri, &deliveredAt)

	saverDone := make(chan string, 1)
	resolverStarted := make(chan struct{}, 1)
	releaseResolver := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseResolver) }) }
	t.Cleanup(release)

	var detailedCalls int32
	h := &PlaybackHandler{
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(
			func(_ context.Context, virtualURI string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
				atomic.AddInt32(&detailedCalls, 1)
				resolverStarted <- struct{}{}
				<-releaseResolver
				return ResolvedVirtualMedia{URL: "http://provider.example/stream.mkv", URI: virtualURI}, nil
			}),
		// resolveVirtualPlaybackSource requires the legacy resolver to be
		// configured even when the detailed resolver takes precedence.
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(
			func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
				return "http://provider.example/legacy?path=" + path, nil
			}),
		VirtualFileLookup: func(_ context.Context, _ string) (*models.MediaFile, error) {
			return stored, nil
		},
		VirtualPlaybackSourceProber: func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
			f.VideoTracks = []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080}}
			f.AudioTracks = []models.AudioTrack{{Codec: "aac", Channels: 2, Language: "eng"}}
			f.CodecVideo, f.CodecAudio, f.Resolution, f.Container = "h264", "aac", "1080p", "mkv"
			return f, nil
		},
		VirtualFileMetadataSaver: func(_ context.Context, _ int, expectedFilePath string, _, _, _ []byte, _, _, _, _ string, _ bool, _ int, _ int) error {
			saverDone <- expectedFilePath
			return nil
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	file := *stored
	type startResult struct {
		src resolvedVirtualPlaybackSource
		err error
	}
	started := make(chan startResult, 1)
	go func() {
		src, err := h.resolveVirtualPlaybackSource(req, &file, "profile-1", true, nil, "", "", 0, false)
		started <- startResult{src, err}
	}()

	var resolved resolvedVirtualPlaybackSource
	select {
	case r := <-started:
		if r.err != nil {
			t.Fatalf("resolveVirtualPlaybackSource error: %v", r.err)
		}
		resolved = r.src
	case <-time.After(2 * time.Second):
		t.Fatal("start path blocked on the synchronous resolver despite recent delivery")
	}

	if resolved.URL != "" {
		t.Fatalf("resolved URL = %q, want empty optimistic start", resolved.URL)
	}
	if resolved.URI != uri {
		t.Fatalf("resolved URI = %q, want persisted %q", resolved.URI, uri)
	}
	if resolved.Provenance != ProbeProvenancePending || resolved.ProbeSucceeded {
		t.Fatalf("provenance=%q succeeded=%v, want pending/false", resolved.Provenance, resolved.ProbeSucceeded)
	}
	if resolved.File == nil || resolved.File.FilePath != uri {
		t.Fatalf("resolved file = %#v, want persisted candidate %q", resolved.File, uri)
	}

	// The optimistic return must still kick the background resolve+probe chain.
	select {
	case <-resolverStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("optimistic start did not kick the background resolver")
	}
	release()
	select {
	case got := <-saverDone:
		if got != uri {
			t.Fatalf("background persist path = %q, want %q", got, uri)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("background revalidation did not persist probed metadata")
	}
}

// No adopted result= and no sticky pin means no persisted candidate: the
// optimistic gate must not apply even inside the delivery grace, so the start
// still resolves synchronously.
func TestResolveVirtualOptimisticRequiresPinnedCandidate(t *testing.T) {
	deliveredAt := time.Now().Add(-time.Hour)
	file := virtualOptimisticFile("virtual://movie/tt-no-pin", &deliveredAt)
	var detailedCalls int32
	h := virtualOptimisticGateHandler(&detailedCalls)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	if _, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false); err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if got := atomic.LoadInt32(&detailedCalls); got != 1 {
		t.Fatalf("detailed resolver called %d times, want 1 without a pinned/adopted candidate", got)
	}
}

// Delivery older than the grace window is not evidence the candidate is still
// good, so the start must resolve synchronously instead of starting blind.
func TestResolveVirtualOptimisticRequiresDeliveryGrace(t *testing.T) {
	stale := time.Now().Add(-8 * 24 * time.Hour)
	file := virtualOptimisticFile("virtual://movie/tt-stale-delivery?result=cand-1", &stale)
	var detailedCalls int32
	h := virtualOptimisticGateHandler(&detailedCalls)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	if _, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false); err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if got := atomic.LoadInt32(&detailedCalls); got != 1 {
		t.Fatalf("detailed resolver called %d times, want 1 once delivery grace lapsed", got)
	}
}

// The optimistic gate is start-only. A synchronous replan/alternate caller
// (deferProbe=false) still resolves even with recent delivery evidence.
func TestResolveVirtualOptimisticOnlyOnDeferredStart(t *testing.T) {
	deliveredAt := time.Now().Add(-time.Hour)
	file := virtualOptimisticFile("virtual://movie/tt-sync-start?result=cand-1", &deliveredAt)
	var detailedCalls int32
	h := virtualOptimisticGateHandler(&detailedCalls)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	if _, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "", 0, false); err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if got := atomic.LoadInt32(&detailedCalls); got != 1 {
		t.Fatalf("detailed resolver called %d times, want 1 for a synchronous caller", got)
	}
}
