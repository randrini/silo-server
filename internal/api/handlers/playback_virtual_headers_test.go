package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

func TestProbeVirtualSourcePrefersHeaderAwareProber(t *testing.T) {
	h := &PlaybackHandler{
		VirtualPlaybackSourceProber: func(_ context.Context, _ string, _ *models.MediaFile) (*models.MediaFile, error) {
			t.Error("legacy prober should not be called when header-aware prober is set")
			return nil, context.DeadlineExceeded
		},
		VirtualPlaybackSourceProberWithHeaders: func(_ context.Context, _ string, f *models.MediaFile, headers map[string]string) (*models.MediaFile, error) {
			if headers["Referer"] != "https://new.example/" {
				t.Fatalf("headers = %v, want new Referer", headers)
			}
			return f, nil
		},
	}
	file := &models.MediaFile{ID: 1, FilePath: "virtual://movie/1?result=a"}
	if _, err := h.probeVirtualSource(context.Background(), "http://example.com/v.mp4", file, map[string]string{"Referer": "https://new.example/"}); err != nil {
		t.Fatalf("probeVirtualSource error: %v", err)
	}
}

func TestResolveUsesResolutionTimeHeadersAuthoritatively(t *testing.T) {
	var probedHeaders map[string]string
	h := &PlaybackHandler{
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			return "http://localhost:8080/list.mp4", nil
		}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{
				URL:            "http://localhost:8080/resolved.mp4",
				URI:            "virtual://movie/1?result=cand-1",
				RequestHeaders: map[string]string{"Referer": "https://new.example/"},
			}, nil
		}),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{{
				ID:             "cand-1",
				URI:            "virtual://movie/1?result=cand-1",
				RequestHeaders: map[string]string{"Referer": "https://old.example/"},
			}}, nil
		}),
		VirtualPlaybackSourceProberWithHeaders: func(_ context.Context, _ string, f *models.MediaFile, headers map[string]string) (*models.MediaFile, error) {
			probedHeaders = headers
			f.VideoTracks = []models.VideoTrack{{Codec: "h264"}}
			f.AudioTracks = []models.AudioTrack{{Codec: "aac"}}
			f.Container = "mp4"
			return f, nil
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	file := &models.MediaFile{ID: 10, ContentID: "movie-1", FilePath: "virtual://movie/1?result=cand-1"}
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if resolved.Provenance != ProbeProvenanceVerified {
		t.Fatalf("provenance = %q, want verified", resolved.Provenance)
	}
	if probedHeaders["Referer"] != "https://new.example/" {
		t.Fatalf("probed headers = %v, want new Referer", probedHeaders)
	}
}

func TestResolveClearedHeadersDoNotRetainStaleHeaders(t *testing.T) {
	var probedHeaders map[string]string
	h := &PlaybackHandler{
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			return "http://localhost:8080/list.mp4", nil
		}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{
				URL: "http://localhost:8080/resolved.mp4",
				URI: "virtual://movie/1?result=cand-1",
			}, nil
		}),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{{
				ID:             "cand-1",
				URI:            "virtual://movie/1?result=cand-1",
				RequestHeaders: map[string]string{"Referer": "https://old.example/"},
			}}, nil
		}),
		VirtualPlaybackSourceProberWithHeaders: func(_ context.Context, _ string, f *models.MediaFile, headers map[string]string) (*models.MediaFile, error) {
			probedHeaders = headers
			f.VideoTracks = []models.VideoTrack{{Codec: "h264"}}
			f.AudioTracks = []models.AudioTrack{{Codec: "aac"}}
			f.Container = "mp4"
			return f, nil
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	file := &models.MediaFile{ID: 11, ContentID: "movie-1", FilePath: "virtual://movie/1?result=cand-1"}
	if _, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "", 0, false); err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if len(probedHeaders) != 0 {
		t.Fatalf("probed headers = %v, want empty after resolution cleared them", probedHeaders)
	}
}

func TestPersistVirtualMetadataBoundedForwardsExpectedPath(t *testing.T) {
	done := make(chan struct{}, 1)
	var gotID int
	var gotPath string
	h := &PlaybackHandler{
		VirtualFileMetadataSaver: func(_ context.Context, fileID int, expectedFilePath string, _, _, _ []byte, _, _, _, _ string, _ bool, _ int, _ int) error {
			gotID = fileID
			gotPath = expectedFilePath
			done <- struct{}{}
			return nil
		},
	}
	file := &models.MediaFile{ID: 42, FilePath: "virtual://movie/1?result=cand-1"}
	h.persistVirtualMetadataBounded(context.Background(), 42, file.FilePath, file)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("saver was not called")
	}
	if gotID != 42 || gotPath != "virtual://movie/1?result=cand-1" {
		t.Fatalf("saver got (%d, %q), want (42, cand-1 URI)", gotID, gotPath)
	}
}
