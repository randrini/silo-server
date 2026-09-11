package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

func TestVisibleVirtualPlaybackStreamsHidesAlternatesOnlyWhenMarked(t *testing.T) {
	streams := []VirtualPlaybackStream{{URI: "virtual://movie/1?result=one", Visible: true, VisibilitySpecified: true}, {URI: "virtual://movie/1?result=two", Visible: false, VisibilitySpecified: true}}
	visible := visibleVirtualPlaybackStreams(streams)
	if len(visible) != 1 || visible[0].URI != streams[0].URI {
		t.Fatalf("visible streams = %#v, want only primary", visible)
	}
	legacy := visibleVirtualPlaybackStreams([]VirtualPlaybackStream{{URI: "one"}, {URI: "two"}})
	if len(legacy) != 2 {
		t.Fatalf("unmarked streams = %#v, want both candidates", legacy)
	}
}

func TestReorderVirtualCandidatesForQualityPrefersAtOrBelowRung(t *testing.T) {
	candidates := []VirtualPlaybackStream{
		{URI: "virtual://movie/1?result=4k", Resolution: "2160p"},
		{URI: "virtual://movie/1?result=1080p", Resolution: "1080p"},
		{URI: "virtual://movie/1?result=720p", Resolution: "720p"},
		{URI: "virtual://movie/1?result=480p", Resolution: "480p"},
	}
	// 720p preference: 720p and 480p move ahead of 4K/1080p, keeping device
	// order within each group.
	reordered := reorderVirtualCandidatesForQuality(candidates, "720p", 0)
	want := []string{"720p", "480p", "4k", "1080p"}
	got := make([]string, 0, len(reordered))
	for _, cand := range reordered {
		got = append(got, virtualResultCandidateID(cand.URI))
	}
	if len(got) != len(want) {
		t.Fatalf("reordered = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("reordered = %v, want %v", got, want)
		}
	}

	// auto/original keep the device ranking unchanged.
	if out := reorderVirtualCandidatesForQuality(candidates, "auto", 0); len(out) != len(candidates) || out[0].URI != candidates[0].URI {
		t.Fatalf("auto reorder changed the ranking: %#v", out)
	}
	if out := reorderVirtualCandidatesForQuality(candidates, "original", 0); len(out) != len(candidates) || out[0].URI != candidates[0].URI {
		t.Fatalf("original reorder changed the ranking: %#v", out)
	}

	// No candidate at or below the rung keeps the device ranking.
	only4K := []VirtualPlaybackStream{{URI: "virtual://movie/1?result=4k", Resolution: "2160p"}}
	if out := reorderVirtualCandidatesForQuality(only4K, "720p", 0); len(out) != 1 || out[0].URI != only4K[0].URI {
		t.Fatalf("no-match reorder changed the ranking: %#v", out)
	}

	// A bandwidth cap tightens the rung: 8Mbps cap on a 1080p preference
	// prefers 720p-or-below candidates.
	capped := reorderVirtualCandidatesForQuality(candidates, "1080p", 8_000)
	if capped[0].URI != "virtual://movie/1?result=720p" {
		t.Fatalf("capped reorder = %v, want 720p first", capped)
	}
}

func TestMergeVirtualCandidateLanguagesSynthesizesAudioTracksOnly(t *testing.T) {
	probed := &models.MediaFile{
		VideoTracks:    []models.VideoTrack{{Codec: "hevc", Width: 3840, Height: 2160}},
		SubtitleTracks: []models.SubtitleTrack{{Index: 0, Language: "eng", Codec: "subrip"}},
	}
	candidate := VirtualPlaybackStream{
		AudioLanguages:    []string{"ita", "ENG", "ita"},
		SubtitleLanguages: []string{"eng", "ger"},
	}

	mergeVirtualCandidateTracks(probed, candidate)

	if len(probed.AudioTracks) != 2 {
		t.Fatalf("audio tracks: got %d, want 2", len(probed.AudioTracks))
	}
	if got := probed.AudioTracks[0].Language; got != "ita" {
		t.Errorf("audio[0].language: got %q, want ita", got)
	}
	if got := probed.AudioTracks[1].Language; got != "ENG" {
		t.Errorf("audio[1].language: got %q, want ENG", got)
	}
	// Duplicate "ita" in the candidate must not append a third track.
	if !probed.AudioTracks[0].Default {
		t.Error("first synthesized audio track should be marked default")
	}

	// Subtitle tracks must NOT be synthesized from candidate metadata because
	// they carry invalid stream ordinals (0:s:N) that fail during extraction.
	// Only probed embedded subtitles should remain.
	if len(probed.SubtitleTracks) != 1 {
		t.Fatalf("subtitle tracks: got %d, want 1 (probed only)", len(probed.SubtitleTracks))
	}
	if got := probed.SubtitleTracks[0].Language; got != "eng" {
		t.Errorf("subtitle[0].language: got %q, want eng", got)
	}
}

func TestMergeVirtualCandidateTracksPreservesDVProfile(t *testing.T) {
	probed := &models.MediaFile{Resolution: "2160p", CodecVideo: "hevc"}
	mergeVirtualCandidateTracks(probed, VirtualPlaybackStream{HDR: "Dolby Vision Profile 5"})
	if len(probed.VideoTracks) != 1 || probed.VideoTracks[0].DVProfile != 5 {
		t.Fatalf("DV metadata = %#v", probed.VideoTracks)
	}
}

func TestMergeVirtualCandidateTracksDoesNotInferDVFromGenericHDR(t *testing.T) {
	probed := &models.MediaFile{Resolution: "2160p", CodecVideo: "hevc"}
	mergeVirtualCandidateTracks(probed, VirtualPlaybackStream{HDR: "true"})
	if len(probed.VideoTracks) != 1 || probed.VideoTracks[0].DVProfile != 0 || probed.VideoTracks[0].DolbyVision != "" {
		t.Fatalf("generic HDR became DV: %#v", probed.VideoTracks)
	}
}

func TestMergeVirtualCandidateTracksRepairsStaleSDRRange(t *testing.T) {
	probed := &models.MediaFile{
		HDR:        true,
		Resolution: "2160p",
		CodecVideo: "hevc",
		VideoTracks: []models.VideoTrack{{
			VideoRange:     "SDR",
			VideoRangeType: "SDR",
		}},
	}

	mergeVirtualCandidateTracks(probed, VirtualPlaybackStream{HDR: "true"})

	if got := probed.VideoTracks[0].VideoRange; got != "HDR" {
		t.Fatalf("video range = %q, want HDR", got)
	}
	if got := probed.VideoTracks[0].VideoRangeType; got != "HDR10" {
		t.Fatalf("video range type = %q, want HDR10", got)
	}
}

func TestMergeVirtualCandidateLanguagesKeepsProbedTracks(t *testing.T) {
	probed := &models.MediaFile{
		AudioTracks: []models.AudioTrack{{Language: "eng", Codec: "aac", Channels: 2}},
	}
	candidate := VirtualPlaybackStream{
		AudioLanguages: []string{"eng", "ita"},
	}

	mergeVirtualCandidateTracks(probed, candidate)

	if len(probed.AudioTracks) != 1 {
		t.Fatalf("audio tracks: got %d, want 1", len(probed.AudioTracks))
	}
	if got := probed.AudioTracks[0].Codec; got != "aac" {
		t.Errorf("probed track codec overwritten: got %q, want aac", got)
	}
	if got := probed.AudioTracks[0].Channels; got != 2 {
		t.Errorf("probed track channels overwritten: got %d, want 2", got)
	}
}

func TestMergeVirtualCandidateLanguagesAuthoritativeInventory(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tracks []models.AudioTrack
		want   int
	}{
		{"index zero", []models.AudioTrack{{Index: 0, Language: "en", Codec: "aac", Channels: 2, Default: true}}, 0},
		{"default beats hint", []models.AudioTrack{{Index: 1, Language: "en", Codec: "aac", Channels: 2}, {Index: 2, Language: "de", Codec: "aac", Channels: 2, Default: true}}, 1},
		{"audio first", []models.AudioTrack{{Index: 0, Language: "en", Codec: "aac", Channels: 2}, {Index: 1, Language: "de", Codec: "aac", Channels: 2, Default: true}}, 1},
		{"genuine multi", []models.AudioTrack{{Index: 0, Language: "en", Languages: []string{"en", "fr"}, Codec: "aac", Channels: 2}, {Index: 1, Language: "de", Codec: "aac", Channels: 2, Default: true}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, _ := json.Marshal(tc.tracks)
			file := &models.MediaFile{AudioTracks: tc.tracks}
			mergeVirtualCandidateTracks(file, VirtualPlaybackStream{AudioLanguages: []string{"ENG", "FRA"}})
			after, _ := json.Marshal(file.AudioTracks)
			if string(before) != string(after) {
				t.Fatalf("inventory changed: %s -> %s", before, after)
			}
			if got := playback.SelectAudioTrack(file.AudioTracks, "fr", nil); got != tc.want {
				t.Fatalf("selected %d, want %d", got, tc.want)
			}
			if got := playback.AudioStreamOrdinal(file.AudioTracks, tc.want); got != tc.want {
				t.Fatalf("ordinal %d, want %d", got, tc.want)
			}
		})
	}
}

func TestMergeVirtualCandidateLanguagesNilFile(t *testing.T) {
	mergeVirtualCandidateTracks(nil, VirtualPlaybackStream{AudioLanguages: []string{"eng"}})
	// Must not panic.
}

func TestMergeVirtualCandidateLanguagesSkipsReleaseMarkers(t *testing.T) {
	probed := &models.MediaFile{}
	candidate := VirtualPlaybackStream{
		AudioLanguages:    []string{"ITA", "ENG", "MULTI", "DUAL", "multi"},
		SubtitleLanguages: []string{"eng", "ger", "MULTI"},
	}

	mergeVirtualCandidateTracks(probed, candidate)

	gotAudio := make([]string, 0, len(probed.AudioTracks))
	for _, t := range probed.AudioTracks {
		gotAudio = append(gotAudio, t.Language)
	}
	for _, want := range []string{"ITA", "ENG"} {
		found := false
		for _, got := range gotAudio {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("audio languages missing %q: got %v", want, gotAudio)
		}
	}
	for _, got := range gotAudio {
		if got == "MULTI" || got == "DUAL" {
			t.Errorf("release marker %q leaked into audio tracks: %v", got, gotAudio)
		}
	}
	if len(probed.AudioTracks) != 2 {
		t.Errorf("audio tracks: got %d, want 2 (ITA, ENG)", len(probed.AudioTracks))
	}

	// Subtitles are never synthesized from release markers or candidate metadata.
	if len(probed.SubtitleTracks) != 0 {
		t.Errorf("subtitle tracks: got %d, want 0", len(probed.SubtitleTracks))
	}
}

func TestIsRealVirtualLanguageTag(t *testing.T) {
	for _, valid := range []string{"ENG", "eng", "ITA", "ger", "ara", "fil", "en", "zh-Hans"} {
		if !isRealVirtualLanguageTag(valid) {
			t.Errorf("isRealVirtualLanguageTag(%q) = false, want true", valid)
		}
	}
	for _, marker := range []string{"MULTI", "multi", "DUAL", "dual", "xx", "UNKNOWN", ""} {
		if isRealVirtualLanguageTag(marker) {
			t.Errorf("isRealVirtualLanguageTag(%q) = true, want false", marker)
		}
	}
}

func TestVirtualDVMetadataRobustProfileExtraction(t *testing.T) {
	cases := []struct {
		raw         string
		wantDV      bool
		wantProfile int
	}{
		{raw: "Dolby Vision Profile 8.1", wantDV: true, wantProfile: 8},
		{raw: "Dolby Vision Profile 5", wantDV: true, wantProfile: 5},
		{raw: "Dolby Vision Profile 7", wantDV: true, wantProfile: 7},
		{raw: "dv5", wantDV: true, wantProfile: 5},
		{raw: "dv 7", wantDV: true, wantProfile: 7},
		{raw: "dovi 08.06", wantDV: true, wantProfile: 8},
		{raw: "Dolby Vision 5", wantDV: true, wantProfile: 5},
		{raw: "4K Dolby Vision", wantDV: true, wantProfile: 0},
		{raw: "Dolby Vision 4K", wantDV: true, wantProfile: 0},
		{raw: "Dolby Vision 2160p", wantDV: true, wantProfile: 0},
		{raw: "DV 1080p", wantDV: true, wantProfile: 0},
		{raw: "Dolby Vision 10bit", wantDV: true, wantProfile: 0},
		{raw: "HDR10", wantDV: false, wantProfile: 0},
		{raw: "DVD", wantDV: false, wantProfile: 0},
	}
	for _, tc := range cases {
		gotDV, gotProf := virtualDVMetadata(tc.raw)
		if gotDV != tc.wantDV || gotProf != tc.wantProfile {
			t.Errorf("virtualDVMetadata(%q) = (%v, %d), want (%v, %d)", tc.raw, gotDV, gotProf, tc.wantDV, tc.wantProfile)
		}
	}
}

func TestMaxVirtualFailoverAttemptsConfigurable(t *testing.T) {
	ctx := context.Background()
	// Nil / default
	var h *PlaybackHandler
	if got := h.maxVirtualFailoverAttempts(ctx); got != 5 {
		t.Fatalf("nil handler max attempts = %d, want 5", got)
	}

	h = &PlaybackHandler{}
	if got := h.maxVirtualFailoverAttempts(ctx); got != 5 {
		t.Fatalf("empty handler max attempts = %d, want 5", got)
	}

	// From static config
	h = &PlaybackHandler{
		PlaybackConfig: func() config.PlaybackConfig {
			return config.PlaybackConfig{MaxVirtualFailoverAttempts: 12}
		},
	}
	if got := h.maxVirtualFailoverAttempts(ctx); got != 12 {
		t.Fatalf("config max attempts = %d, want 12", got)
	}

	// From dynamic settings repo
	h = &PlaybackHandler{
		SettingsRepo: &fakeServerSettingsStore{
			values: map[string]string{
				"playback.max_virtual_failover_attempts": "8",
			},
		},
		PlaybackConfig: func() config.PlaybackConfig {
			return config.PlaybackConfig{MaxVirtualFailoverAttempts: 12}
		},
	}
	if got := h.maxVirtualFailoverAttempts(ctx); got != 8 {
		t.Fatalf("dynamic settings max attempts = %d, want 8", got)
	}
}

func TestIsUnplayableVirtualURI(t *testing.T) {
	cases := []struct {
		uri  string
		want bool
	}{
		{"virtual://series/tt11198330", true},
		{"virtual://series/tvdb/371572", true},
		{"virtual://show/tt11198330", true},
		{"virtual://series/tt11198330/3/2", false},
		{"virtual://series/tvdb/371572/3/2", false},
		{"virtual://movie/tt11198330", false},
		{"virtual://movie/tmdb/12345", false},
		{"/var/media/movie.mkv", false},
	}
	for _, tc := range cases {
		if got := isUnplayableVirtualURI(tc.uri); got != tc.want {
			t.Errorf("isUnplayableVirtualURI(%q) = %v, want %v", tc.uri, got, tc.want)
		}
	}
}

func TestVirtualCandidateLookupUsesStableEpisodeIdentity(t *testing.T) {
	calledCandidateLookup := false
	calledContentLookup := false

	h := &PlaybackHandler{
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(ctx context.Context, path string, userID int, profileID string, ownerInstallationID int) (string, error) {
			return "https://provider.example/stream.mp4", nil
		}),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(ctx context.Context, path string, userID int, profileID string, ownerInstallationID int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{
				{URI: "virtual://series/tt11198330/3/2?result=fresh123", CodecVideo: "h264", Resolution: "1080p"},
			}, nil
		}),
		VirtualCandidateFileLookup: func(ctx context.Context, path, contentID, episodeID string, ownerInstallationID int) (*models.MediaFile, error) {
			calledCandidateLookup = true
			if path != "virtual://series/tt11198330/3/2" || episodeID != "episode-tvdb-371572-3-2" {
				t.Fatalf("VirtualCandidateFileLookup called with path=%q episode=%q", path, episodeID)
			}
			return &models.MediaFile{ID: 5323060, MediaFolderID: 32, FilePath: path, EpisodeID: episodeID}, nil
		},
		VirtualContentFileLookup: func(ctx context.Context, contentID string) (*models.MediaFile, error) {
			calledContentLookup = true
			return &models.MediaFile{ID: 5293604, MediaFolderID: 32, FilePath: "virtual://series/tt11198330", ContentID: contentID}, nil
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	episodeFile := &models.MediaFile{
		ID:        0,
		ContentID: "series-tvdb-371572",
		EpisodeID: "episode-tvdb-371572-3-2",
		FilePath:  "virtual://series/tt11198330/3/2",
	}

	resolved, err := h.resolveVirtualPlaybackSource(req, episodeFile, "profile-1", false, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource failed: %v", err)
	}

	if !calledCandidateLookup {
		t.Fatal("expected VirtualCandidateFileLookup to be called for episode file")
	}
	if calledContentLookup {
		t.Fatal("VirtualContentFileLookup should NOT be called when EpisodeID is present")
	}
	if resolved.File.ID != 5323060 {
		t.Fatalf("resolved file ID = %d, want 5323060 (candidate row)", resolved.File.ID)
	}
}

type fakeFileResolverForTransportTest struct {
	file *models.MediaFile
}

func (f fakeFileResolverForTransportTest) GetByID(ctx context.Context, id int) (*models.MediaFile, error) {
	return f.file, nil
}

func TestStartLocalPlaybackTransportPreservesEpisodeURIOverSeriesCatalogFile(t *testing.T) {
	var requestedTargetURI string
	sessionMgr := playback.NewSessionManager(0, 0)
	h := &PlaybackHandler{
		sessionMgr: sessionMgr,
		fileResolver: fakeFileResolverForTransportTest{
			file: &models.MediaFile{
				ID:        5293604,
				FilePath:  "virtual://series/tt11198330",
				ContentID: "series-tvdb-371572",
			},
		},
		VirtualMediaResolver: VirtualMediaResolverFunc(func(ctx context.Context, virtualURI string, ownerInstallationID int, userID int, profileID string) (string, error) {
			requestedTargetURI = virtualURI
			return "http://localhost:8080/stream.mp4", nil
		}),
	}

	opts := playback.TranscodeOpts{
		MediaFileID: 5293604,
		InputPath:   "virtual://series/tt11198330/3/2",
		SessionID:   "test-session",
	}

	_, _ = h.startLocalPlaybackTransportOnce(context.Background(), opts)

	if requestedTargetURI != "virtual://series/tt11198330/3/2" {
		t.Fatalf("VirtualMediaResolver received %q, want virtual://series/tt11198330/3/2", requestedTargetURI)
	}
}

func TestResolveVirtualPlaybackSourceKeepsUnprobedPinnedFallbackWhenOthersFail(t *testing.T) {
	pinnedStream := VirtualPlaybackStream{URI: "virtual://movie/1?result=pinned", Resolution: "1080p", CodecVideo: "h264"}
	alternateStream := VirtualPlaybackStream{URI: "virtual://movie/1?result=alt", Resolution: "1080p", CodecVideo: "h264"}

	h := &PlaybackHandler{
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(ctx context.Context, path string, userID int, profileID string, ownerInstallationID int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{pinnedStream, alternateStream}, nil
		}),
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(ctx context.Context, path string, userID int, profileID string, ownerInstallationID int) (string, error) {
			if path == pinnedStream.URI {
				return "http://localhost:8080/pinned.mp4", nil
			}
			return "", context.DeadlineExceeded
		}),
		VirtualPlaybackSourceProber: func(ctx context.Context, streamURL string, transient *models.MediaFile) (*models.MediaFile, error) {
			// Probing fails for the pinned stream.
			return nil, context.DeadlineExceeded
		},
	}

	stickyKey := "movie-1"
	h.pinVirtualSticky(stickyKey, pinnedStream.URI)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	file := &models.MediaFile{ID: 10, ContentID: "movie-1", FilePath: "virtual://movie/1"}

	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource returned error %v, want fallback to resolved pinned candidate", err)
	}
	if resolved.URI != pinnedStream.URI || resolved.URL != "http://localhost:8080/pinned.mp4" {
		t.Fatalf("resolved source = %#v, want pinned stream fallback", resolved)
	}
}

func TestResolveVirtualPlaybackSourceExplicitResultPreservesSelectedVersion(t *testing.T) {
	highestQualityStream := VirtualPlaybackStream{
		ID:         "stream-4k",
		URI:        "virtual://movie/1?result=stream-4k",
		Resolution: "4K",
		CodecVideo: "hevc",
		HDR:        "hdr10",
		Bitrate:    50000000,
	}
	selectedVersionStream := VirtualPlaybackStream{
		ID:         "stream-1080p",
		URI:        "virtual://movie/1?result=stream-1080p",
		Resolution: "1080p",
		CodecVideo: "h264",
		Bitrate:    8000000,
	}

	var attemptedPaths []string
	h := &PlaybackHandler{
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(ctx context.Context, path string, userID int, profileID string, ownerInstallationID int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{highestQualityStream, selectedVersionStream}, nil
		}),
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(ctx context.Context, path string, userID int, profileID string, ownerInstallationID int) (string, error) {
			attemptedPaths = append(attemptedPaths, path)
			if path == selectedVersionStream.URI {
				return "http://localhost:8080/1080p.mp4", nil
			}
			return "http://localhost:8080/4k.mp4", nil
		}),
		VirtualPlaybackSourceProber: func(ctx context.Context, streamURL string, transient *models.MediaFile) (*models.MediaFile, error) {
			transient.VideoTracks = []models.VideoTrack{{Codec: "h264"}}
			return transient, nil
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	file := &models.MediaFile{
		ID:        20,
		ContentID: "movie-1",
		FilePath:  "virtual://movie/1?result=stream-1080p",
	}

	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if resolved.URI != selectedVersionStream.URI {
		t.Fatalf("resolved URI = %q, want %q", resolved.URI, selectedVersionStream.URI)
	}
	if len(attemptedPaths) == 0 || attemptedPaths[0] != selectedVersionStream.URI {
		t.Fatalf("first attempted path = %v, want %q", attemptedPaths, selectedVersionStream.URI)
	}
}

func TestFallbackResolveStaleVirtualSourceRespectsMaxFailoverLimit(t *testing.T) {
	resolveAttempts := 0
	streams := make([]VirtualPlaybackStream, 0, 15)
	for i := 1; i <= 15; i++ {
		streams = append(streams, VirtualPlaybackStream{
			URI:        "virtual://movie/1?result=" + string(rune('a'+i)),
			Resolution: "1080p",
		})
	}

	h := &PlaybackHandler{
		PlaybackConfig: func() config.PlaybackConfig {
			return config.PlaybackConfig{
				MaxVirtualFailoverAttempts: 3,
			}
		},
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(ctx context.Context, path string, userID int, profileID string, ownerInstallationID int) ([]VirtualPlaybackStream, error) {
			return streams, nil
		}),
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(ctx context.Context, path string, userID int, profileID string, ownerInstallationID int) (string, error) {
			resolveAttempts++
			return "", context.DeadlineExceeded
		}),
	}

	file := &models.MediaFile{ID: 10, ContentID: "movie-1", FilePath: "virtual://movie/1?result=dead"}
	result := h.fallbackResolveStaleVirtualSource(context.Background(), file, 1, "profile-1")
	if result != nil {
		t.Fatalf("result = %#v, want nil after all attempts fail", result)
	}
	if resolveAttempts != 3 {
		t.Fatalf("resolve attempts = %d, want exactly max attempts (3)", resolveAttempts)
	}
}

// A stale result= pin that falls back to a substitute must persist the
// substitute's probed track inventory onto the media file row. Without it the
// row keeps advertising the dead candidate's tracks — wrong audio languages
// and phantom subtitle tracks — while the stream serves the substitute's
// real ones, so track switches appear to do nothing.
func TestFallbackResolveStaleVirtualSourcePersistsSubstituteMetadata(t *testing.T) {
	substitute := VirtualPlaybackStream{
		URI:        "virtual://movie/1?result=substitute",
		Resolution: "1080p",
	}
	updatedPath := ""
	var savedFileID int
	var savedPath string
	var savedAudio []byte
	var savedSubs []byte
	saved := make(chan struct{}, 1)

	h := &PlaybackHandler{
		PlaybackConfig: func() config.PlaybackConfig {
			return config.PlaybackConfig{MaxVirtualFailoverAttempts: 3}
		},
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(ctx context.Context, path string, userID int, profileID string, ownerInstallationID int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{substitute}, nil
		}),
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(ctx context.Context, path string, userID int, profileID string, ownerInstallationID int) (string, error) {
			return "http://localhost:8080/substitute.mp4", nil
		}),
		VirtualPlaybackSourceProber: func(ctx context.Context, streamURL string, transient *models.MediaFile) (*models.MediaFile, error) {
			transient.AudioTracks = []models.AudioTrack{{Language: "eng", Codec: "eac3", Channels: 6}}
			transient.SubtitleTracks = []models.SubtitleTrack{{Language: "eng", Codec: "subrip"}}
			return transient, nil
		},
		VirtualFileUpdater: func(ctx context.Context, fileID int, newFilePath string) error {
			updatedPath = newFilePath
			return nil
		},
		VirtualFileMetadataSaver: func(ctx context.Context, fileID int, expectedFilePath string, videoTracks, audioTracks, subtitleTracks []byte, resolution, codecVideo, codecAudio, container string, hdr bool, bitrate int, duration int) error {
			savedFileID = fileID
			savedPath = expectedFilePath
			savedAudio = append([]byte(nil), audioTracks...)
			savedSubs = append([]byte(nil), subtitleTracks...)
			saved <- struct{}{}
			return nil
		},
	}

	file := &models.MediaFile{ID: 10, ContentID: "movie-1", FilePath: "virtual://movie/1?result=dead"}
	result := h.fallbackResolveStaleVirtualSource(context.Background(), file, 1, "profile-1")
	if result == nil {
		t.Fatal("fallback returned nil, want resolved substitute")
	}
	if updatedPath != substitute.URI {
		t.Fatalf("updated path = %q, want %q", updatedPath, substitute.URI)
	}
	// The metadata saver runs on a bounded background goroutine; wait for it
	// before asserting what it captured.
	select {
	case <-saved:
	case <-time.After(2 * time.Second):
		t.Fatal("metadata saver was not called")
	}
	if savedFileID != file.ID {
		t.Fatalf("metadata saved for file %d, want %d", savedFileID, file.ID)
	}
	if savedPath != substitute.URI {
		t.Fatalf("metadata saved with path %q, want %q", savedPath, substitute.URI)
	}
	if !bytes.Contains(savedAudio, []byte("eac3")) || !bytes.Contains(savedAudio, []byte("eng")) {
		t.Fatalf("saved audio tracks = %s, want probed eac3/eng inventory", savedAudio)
	}
	if !bytes.Contains(savedSubs, []byte("subrip")) {
		t.Fatalf("saved subtitle tracks = %s, want probed subrip inventory", savedSubs)
	}
}

func TestMaybeTriggerSubtitleSearchDeduplicatesTransientFilesDistinctly(t *testing.T) {
	searchedContent := make(chan string, 2)
	h := &PlaybackHandler{
		SubtitleSearchInFlight: &sync.Map{},
		VirtualSubtitleSearcher: func(ctx context.Context, contentID, imdbID, title string, year, season, episode, mediaFileID int, subtitleLanguages []string) {
			searchedContent <- contentID
		},
	}

	fileA := &models.MediaFile{ID: 0, ContentID: "movie-a"}
	candA := VirtualPlaybackStream{URI: "virtual://movie/a?res=1", SubtitleLanguages: []string{"eng"}}

	fileB := &models.MediaFile{ID: 0, ContentID: "movie-b"}
	candB := VirtualPlaybackStream{URI: "virtual://movie/b?res=1", SubtitleLanguages: []string{"eng"}}

	// Both files are transient with ID 0. They must both trigger search because candidate URIs differ.
	h.maybeTriggerSubtitleSearch(context.Background(), fileA, candA)
	h.maybeTriggerSubtitleSearch(context.Background(), fileB, candB)

	var seen []string
	for i := 0; i < 2; i++ {
		select {
		case id := <-searchedContent:
			seen = append(seen, id)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for search %d; seen = %v", i+1, seen)
		}
	}

	if len(seen) != 2 {
		t.Fatalf("expected 2 distinct searches, got %d (%v)", len(seen), seen)
	}
}

func TestMaybeTriggerSubtitleSearchSuppressesDuplicatesForSameTransientCandidate(t *testing.T) {
	started := make(chan struct{})
	blockSearch := make(chan struct{})
	var callCount atomicInt
	h := &PlaybackHandler{
		SubtitleSearchInFlight: &sync.Map{},
		VirtualSubtitleSearcher: func(ctx context.Context, contentID, imdbID, title string, year, season, episode, mediaFileID int, subtitleLanguages []string) {
			callCount.add(1)
			close(started)
			<-blockSearch
		},
	}

	file := &models.MediaFile{ID: 0, ContentID: "movie-transient-1"}
	cand := VirtualPlaybackStream{URI: "virtual://movie/transient?res=1", SubtitleLanguages: []string{"eng"}}

	// First search begins and blocks.
	h.maybeTriggerSubtitleSearch(context.Background(), file, cand)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first search to begin")
	}

	// Second search with the exact same transient candidate while the first is in-flight.
	// It must be suppressed.
	h.maybeTriggerSubtitleSearch(context.Background(), file, cand)
	time.Sleep(50 * time.Millisecond)

	if got := callCount.get(); got != 1 {
		t.Fatalf("callCount while in-flight = %d, want 1 (duplicate suppressed)", got)
	}

	// Unblock first search and allow it to finish.
	close(blockSearch)
	time.Sleep(50 * time.Millisecond)

	// Now that the search finished, a subsequent call can trigger a search again.
	doneSearch := make(chan struct{})
	h.VirtualSubtitleSearcher = func(ctx context.Context, contentID, imdbID, title string, year, season, episode, mediaFileID int, subtitleLanguages []string) {
		callCount.add(1)
		close(doneSearch)
	}
	h.maybeTriggerSubtitleSearch(context.Background(), file, cand)
	select {
	case <-doneSearch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for subsequent search after completion")
	}

	if got := callCount.get(); got != 2 {
		t.Fatalf("callCount after completion = %d, want 2", got)
	}
}

type atomicInt struct {
	mu sync.Mutex
	v  int
}

func (a *atomicInt) add(delta int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.v += delta
}

func (a *atomicInt) get() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.v
}

// The user-reported scenario: probe fails (background probe deadline), the
// candidate is the only one ("show one stream with failover"), and the plugin
// declares languages with a leading release marker. The merged inventory must
// end up with one track per real language — no anonymous generic track, the
// marker filtered — so the player's audio menu shows labeled choices.
func TestMergeVirtualCandidateTracksProbeFailureYieldsLabeledPerLanguageTracks(t *testing.T) {
	probed := &models.MediaFile{Resolution: "2160p", CodecVideo: "hevc"}
	candidate := VirtualPlaybackStream{
		CodecAudio:     "eac3",
		AudioLanguages: []string{"MULTI", "eng", "fre"},
	}

	mergeVirtualCandidateTracks(probed, candidate)

	got := make([]string, 0, len(probed.AudioTracks))
	for _, track := range probed.AudioTracks {
		got = append(got, track.Language)
		if track.Language == "" {
			t.Fatalf("anonymous audio track leaked into the inventory: %#v", probed.AudioTracks)
		}
	}
	if len(got) != 2 {
		t.Fatalf("audio tracks = %v, want exactly [eng fre]", got)
	}
	if got[0] != "eng" || got[1] != "fre" {
		t.Fatalf("audio tracks = %v, want declaration order [eng fre]", got)
	}
	if !probed.AudioTracks[0].Default || probed.AudioTracks[1].Default {
		t.Fatalf("default must mark only the first declared language: %#v", probed.AudioTracks)
	}
	for i := range probed.AudioTracks {
		if probed.AudioTracks[i].Codec != "eac3" {
			t.Errorf("audio[%d].codec = %q, want eac3", i, probed.AudioTracks[i].Codec)
		}
		if probed.AudioTracks[i].Channels <= 0 {
			t.Errorf("audio[%d].channels = %d, want > 0", i, probed.AudioTracks[i].Channels)
		}
	}
}

// No declared languages (all filtered): the single anonymous generic track is
// the preserved fallback so a release without usable language metadata still
// has a playable default.
func TestMergeVirtualCandidateTracksProbeFailureWithoutLanguagesKeepsGenericTrack(t *testing.T) {
	probed := &models.MediaFile{Resolution: "2160p", CodecVideo: "hevc"}
	candidate := VirtualPlaybackStream{
		CodecAudio:     "aac",
		AudioLanguages: []string{"MULTI", "DUAL"},
	}

	mergeVirtualCandidateTracks(probed, candidate)

	if len(probed.AudioTracks) != 1 {
		t.Fatalf("audio tracks = %#v, want exactly the one generic fallback", probed.AudioTracks)
	}
	if probed.AudioTracks[0].Language != "" {
		t.Fatalf("generic track language = %q, want empty", probed.AudioTracks[0].Language)
	}
	if !probed.AudioTracks[0].Default {
		t.Fatal("generic track should be marked default")
	}
}

// The user-reported scenario (file 887387): the probe found real tracks with
// ISO 639-1 codes (it/ko/en), while the provider declares the same languages
// as 639-2/3 codes (ITA/KOR/ENG). Exact-string dedup appended all three again,
// producing a 6-row audio menu where the synthesized rows mapped to the same
// or out-of-range ffmpeg ordinals (same-ita-sound / silent rows). Dedup must
// be language-normalized: the declared list adds nothing the probe already has.
func TestMergeVirtualCandidateLanguagesDedupsByNormalizedLanguage(t *testing.T) {
	probed := &models.MediaFile{
		Resolution: "1080p",
		CodecVideo: "hevc",
		AudioTracks: []models.AudioTrack{
			{Index: 1, Language: "it", Codec: "aac", Channels: 2, Default: true},
			{Index: 2, Language: "ko", Codec: "aac", Channels: 2},
			{Index: 3, Language: "en", Codec: "aac", Channels: 2},
		},
	}
	candidate := VirtualPlaybackStream{
		CodecAudio:     "aac",
		AudioLanguages: []string{"ITA", "KOR", "ENG"},
	}

	mergeVirtualCandidateTracks(probed, candidate)

	if len(probed.AudioTracks) != 3 {
		t.Fatalf("audio tracks = %#v, want the 3 probed tracks with no synthesized duplicates", probed.AudioTracks)
	}
	got := make([]string, 0, len(probed.AudioTracks))
	for _, track := range probed.AudioTracks {
		got = append(got, track.Language)
		if track.Index <= 0 {
			t.Errorf("synthesized duplicate leaked into the inventory: %#v", probed.AudioTracks)
		}
	}
	want := []string{"it", "ko", "en"}
	for i, want := range want {
		if got[i] != want {
			t.Fatalf("audio[%d].language = %q, want %q (probed tracks untouched)", i, got[i], want)
		}
	}
}

// Mixed-script dedup: a provider language that resolves to a base subtag the
// probe already covers in a different code form ("en-US" vs "en") also dedups.
func TestMergeVirtualCandidateLanguagesDedupsRegionalVariants(t *testing.T) {
	probed := &models.MediaFile{
		AudioTracks: []models.AudioTrack{{Index: 1, Language: "en", Codec: "aac", Channels: 2, Default: true}},
	}
	candidate := VirtualPlaybackStream{
		CodecAudio:     "aac",
		AudioLanguages: []string{"EN", "en-US", "FRA"},
	}

	mergeVirtualCandidateTracks(probed, candidate)

	if len(probed.AudioTracks) != 1 {
		t.Fatalf("audio tracks = %#v, want the single probed track — EN and en-US dedup to the same base subtag and FRA is metadata, not a fabricated stream", probed.AudioTracks)
	}
	track := probed.AudioTracks[0]
	if track.Language != "en" {
		t.Fatalf("audio[0].language = %q, want the probed en untouched", track.Language)
	}
	if len(track.Languages) != 0 {
		t.Fatalf("provider hints changed real track languages: %#v", track.Languages)
	}
}

func TestMergeVirtualCandidateLanguagesHintsStayOnCandidate(t *testing.T) {
	probed := &models.MediaFile{
		AudioTracks: []models.AudioTrack{
			{Index: 1, Language: "en", Codec: "aac", Channels: 2, Default: true},
		},
	}
	candidate := VirtualPlaybackStream{
		CodecAudio:     "aac",
		AudioLanguages: []string{"ENG", "FRA"},
	}

	mergeVirtualCandidateTracks(probed, candidate)

	if len(probed.AudioTracks) != 1 {
		t.Fatalf("audio tracks = %#v, want exactly the probed English track (FRA is metadata, not a stream)", probed.AudioTracks)
	}
	// The merged inventory ranks through the real ordinal math: English keeps
	// its audio ordinal 0 (emitted as 0:a:0?), and there is no ordinal 1.
	if ordinal := playback.AudioStreamOrdinal(probed.AudioTracks, 0); ordinal != 0 {
		t.Fatalf("English ordinal = %d, want 0", ordinal)
	}
	if len(probed.AudioTracks) > 1 {
		t.Fatal("a French ordinal would exist — the hint became a stream")
	}
}

// completeEvidenceVirtualMovieFile is a virtual result= row carrying the full
// video/audio/container evidence a prior probe would have persisted, so the
// resolver's needsCandidateMetadata gate is false and only force_relist can
// make it list again.
func completeEvidenceVirtualMovieFile(path string) *models.MediaFile {
	return &models.MediaFile{
		ID:         30,
		ContentID:  "movie-1",
		FilePath:   path,
		Container:  "mkv",
		CodecVideo: "h264",
		Resolution: "1080p",
		VideoTracks: []models.VideoTrack{{
			Codec: "h264", Width: 1920, Height: 1080, FrameRate: "24000/1001",
		}},
		AudioTracks: []models.AudioTrack{{Codec: "aac", Channels: 2}},
	}
}

// A forced relist on an explicitly-selected version must query the provider
// even though the pinned row already carries complete evidence, and must keep
// the pinned candidate at index 0 when the fresh list still contains it.
func TestResolveVirtualPlaybackSourceForceRelistListsAndKeepsPin(t *testing.T) {
	pinned := VirtualPlaybackStream{ID: "pin", URI: "virtual://movie/1?result=pin", Resolution: "1080p", CodecVideo: "h264"}
	alternate := VirtualPlaybackStream{ID: "alt", URI: "virtual://movie/1?result=alt", Resolution: "1080p", CodecVideo: "h264"}

	listCalls := 0
	var resolvedPaths []string
	var forceRefreshArgs []bool
	h := &PlaybackHandler{
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
			return path, nil
		}),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			listCalls++
			return []VirtualPlaybackStream{pinned, alternate}, nil
		}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, virtualURI string, _ int, _ int, _ string, forceRefresh bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			resolvedPaths = append(resolvedPaths, virtualURI)
			forceRefreshArgs = append(forceRefreshArgs, forceRefresh)
			return ResolvedVirtualMedia{URL: "http://localhost:8080/stream.mp4", URI: virtualURI}, nil
		}),
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	file := completeEvidenceVirtualMovieFile(pinned.URI)

	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "", 0, true)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if listCalls != 1 {
		t.Fatalf("lister calls = %d, want 1 when forceRelist is set on complete evidence", listCalls)
	}
	if resolved.URI != pinned.URI {
		t.Fatalf("resolved URI = %q, want the still-listed pinned candidate %q", resolved.URI, pinned.URI)
	}
	if len(resolvedPaths) == 0 || resolvedPaths[0] != pinned.URI {
		t.Fatalf("first resolved path = %v, want pinned %q", resolvedPaths, pinned.URI)
	}
	if len(forceRefreshArgs) == 0 || !forceRefreshArgs[0] {
		t.Fatalf("forceRefresh args = %v, want true on a forced relist", forceRefreshArgs)
	}
}

// Without force_relist, a pinned result= row with complete evidence must skip
// the provider listing entirely — the behavior force_relist exists to override.
func TestResolveVirtualPlaybackSourceCompleteEvidenceSkipsListerWithoutForceRelist(t *testing.T) {
	pinned := VirtualPlaybackStream{ID: "pin", URI: "virtual://movie/1?result=pin", Resolution: "1080p", CodecVideo: "h264"}

	listCalls := 0
	h := &PlaybackHandler{
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
			return path, nil
		}),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			listCalls++
			return []VirtualPlaybackStream{pinned}, nil
		}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, virtualURI string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{URL: "http://localhost:8080/stream.mp4", URI: virtualURI}, nil
		}),
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	file := completeEvidenceVirtualMovieFile(pinned.URI)

	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if listCalls != 0 {
		t.Fatalf("lister calls = %d, want 0 for complete evidence without forceRelist", listCalls)
	}
	if resolved.URI != pinned.URI {
		t.Fatalf("resolved URI = %q, want pinned %q", resolved.URI, pinned.URI)
	}
}

// When the forced fresh listing no longer contains the pinned version, the
// stale pin must be dropped so resolution moves to the current candidate.
func TestResolveVirtualPlaybackSourceForceRelistDropsGonePin(t *testing.T) {
	pinnedURI := "virtual://movie/1?result=pin"
	fresh := VirtualPlaybackStream{ID: "fresh", URI: "virtual://movie/1?result=fresh", Resolution: "1080p", CodecVideo: "h264"}

	var resolvedPaths []string
	h := &PlaybackHandler{
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
			return path, nil
		}),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{fresh}, nil
		}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, virtualURI string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			resolvedPaths = append(resolvedPaths, virtualURI)
			return ResolvedVirtualMedia{URL: "http://localhost:8080/stream.mp4", URI: virtualURI}, nil
		}),
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	file := completeEvidenceVirtualMovieFile(pinnedURI)

	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "", 0, true)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if resolved.URI != fresh.URI {
		t.Fatalf("resolved URI = %q, want the fresh candidate %q after the pin disappeared", resolved.URI, fresh.URI)
	}
	if len(resolvedPaths) == 0 || resolvedPaths[0] != fresh.URI {
		t.Fatalf("first resolved path = %v, want fresh %q", resolvedPaths, fresh.URI)
	}
}
