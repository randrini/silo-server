package plugins

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-server/internal/pluginhost"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const testVirtualCapabilityID = "test-stream-provider"

func TestVirtualCandidateHDRIncludesSeparateDolbyVisionMetadata(t *testing.T) {
	result := &pluginv1.VirtualStreamResult{Candidates: []*pluginv1.VirtualStreamCandidate{{
		CandidateId: "dv-8",
		Hdr: &pluginv1.VirtualStreamHDR{
			IsHdr:              true,
			Format:             "HDR10",
			HasDolbyVision:     true,
			DolbyVisionProfile: "Profile 8.1",
		},
	}}}
	streams := streamsFromVirtualResult("virtual://movie/1", result, 1)
	if len(streams) != 1 || streams[0].HDR != "Dolby Vision Profile 8.1" {
		t.Fatalf("HDR metadata = %#v", streams)
	}
}

type fakeVirtualStreamGRPCClient struct {
	pluginv1.VirtualStreamProviderClient
	resolveFunc  func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error)
	profilesFunc func(context.Context, *pluginv1.ListVirtualStreamProfilesRequest) (*pluginv1.ListVirtualStreamProfilesResponse, error)
}

func (f *fakeVirtualStreamGRPCClient) ResolveVirtualStream(
	ctx context.Context,
	request *pluginv1.ResolveVirtualStreamRequest,
	_ ...grpc.CallOption,
) (*pluginv1.ResolveVirtualStreamResponse, error) {
	if f.resolveFunc == nil {
		return nil, errors.New("not implemented")
	}
	return f.resolveFunc(ctx, request)
}

func (f *fakeVirtualStreamGRPCClient) ListVirtualStreamProfiles(
	ctx context.Context,
	request *pluginv1.ListVirtualStreamProfilesRequest,
	_ ...grpc.CallOption,
) (*pluginv1.ListVirtualStreamProfilesResponse, error) {
	if f.profilesFunc == nil {
		return nil, errors.New("not implemented")
	}
	return f.profilesFunc(ctx, request)
}

type fakeVirtualPluginHost struct {
	clients map[int]pluginClient
}

func (h *fakeVirtualPluginHost) Client(id int) (pluginClient, error) {
	if c, ok := h.clients[id]; ok {
		return c, nil
	}
	return nil, pluginhost.ErrClientNotFound
}

func (h *fakeVirtualPluginHost) Start(context.Context, pluginhost.StartRequest) (pluginClient, error) {
	return nil, errors.New("not implemented")
}

func (h *fakeVirtualPluginHost) Stop(int) error { return nil }

func (h *fakeVirtualPluginHost) Shutdown(context.Context) error { return nil }

func TestListVirtualPlaybackStreamsRoutesOwnerThenExplicitFallback(t *testing.T) {
	service, calls := newVirtualPlaybackTestService(t,
		func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
			return virtualResponse(), nil
		},
		func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
			return virtualResponse(virtualCandidate("fallback", "https://1.1.1.1/fallback")), nil
		},
	)
	streams, err := service.ListVirtualPlaybackStreamsWithRouting(
		context.Background(),
		"virtual://movie/tt1234",
		7,
		"profile-1",
		VirtualPlaybackRouting{OwnerInstallationID: 101, AllowFallback: true},
	)
	if err != nil {
		t.Fatalf("ListVirtualPlaybackStreamsWithRouting failed: %v", err)
	}
	if len(streams) != 1 || streams[0].ID != "fallback" || streams[0].OwnerInstallationID != 102 {
		t.Fatalf("unexpected fallback streams: %+v", streams)
	}
	if calls[101].Load() != 1 || calls[102].Load() != 1 {
		t.Fatalf("provider calls = owner:%d fallback:%d, want 1 each", calls[101].Load(), calls[102].Load())
	}
}

func TestVirtualPlaybackProfileMissTriesFallbackProvider(t *testing.T) {
	owner := virtualCandidate("owner-hd", "https://1.1.1.1/hd")
	owner.Resolution.Label = "1080p"
	fallback := virtualCandidate("fallback-4k", "https://8.8.8.8/4k")
	fallback.Resolution.Label = "2160p"
	service, calls := newVirtualPlaybackTestService(t,
		func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
			return virtualResponse(owner), nil
		},
		func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
			return virtualResponse(fallback), nil
		},
	)
	resolved, err := service.ResolveVirtualPlaybackWithRouting(
		context.Background(), "virtual://movie/tt1234?profile=2160p", 7, "p1",
		VirtualPlaybackRouting{OwnerInstallationID: 101, AllowFallback: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != "https://8.8.8.8/4k" || calls[101].Load() != 1 || calls[102].Load() != 1 {
		t.Fatalf("resolved=%q calls owner=%d fallback=%d", resolved, calls[101].Load(), calls[102].Load())
	}
}

func TestVirtualPlaybackDoesNotFallbackWithoutOptIn(t *testing.T) {
	service, calls := newVirtualPlaybackTestService(t,
		func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
			return virtualResponse(), nil
		},
		func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
			return virtualResponse(virtualCandidate("other", "https://1.1.1.1/other")), nil
		},
	)
	_, err := service.ListVirtualPlaybackStreamsWithRouting(
		context.Background(),
		"virtual://movie/tt1234",
		0,
		"",
		VirtualPlaybackRouting{OwnerInstallationID: 101},
	)
	if err == nil {
		t.Fatal("owner-only resolution succeeded with no owner candidates")
	}
	if calls[101].Load() != 1 || calls[102].Load() != 0 {
		t.Fatalf("provider calls = owner:%d other:%d, want 1 and 0", calls[101].Load(), calls[102].Load())
	}
}

func TestVirtualPlaybackCachesTypedResponseAcrossListAndResolve(t *testing.T) {
	service, calls := newVirtualPlaybackTestService(t,
		func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
			return virtualResponse(virtualCandidate("chosen", "https://1.1.1.1/chosen?token=secret")), nil
		},
	)
	routing := VirtualPlaybackRouting{OwnerInstallationID: 101}
	streams, err := service.ListVirtualPlaybackStreamsWithRouting(context.Background(), "virtual://movie/tt1234", 7, "p1", routing)
	if err != nil || len(streams) != 1 {
		t.Fatalf("list streams = %+v, %v", streams, err)
	}
	resolved, err := service.ResolveVirtualPlaybackWithRouting(context.Background(), streams[0].URI, 7, "p1", routing)
	if err != nil {
		t.Fatalf("resolve selected stream: %v", err)
	}
	if resolved != "https://1.1.1.1/chosen?token=secret" {
		t.Fatalf("resolved URL = %q", resolved)
	}
	if calls[101].Load() != 1 {
		t.Fatalf("provider called %d times, want cached single call", calls[101].Load())
	}
}

func TestVirtualPlaybackForceRefreshBypassesAndReplacesHostCache(t *testing.T) {
	var sequence atomic.Int32
	var forced atomic.Bool
	service, calls := newVirtualPlaybackTestService(t,
		func(_ context.Context, request *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
			number := sequence.Add(1)
			if virtualStreamForceRefresh(request) {
				forced.Store(true)
			}
			return virtualResponse(virtualCandidate(
				fmt.Sprintf("candidate-%d", number),
				fmt.Sprintf("https://1.1.1.1/stream-%d", number),
			)), nil
		},
	)
	routing := VirtualPlaybackRouting{OwnerInstallationID: 101}
	first, err := service.ResolveVirtualPlaybackWithRouting(context.Background(), "virtual://movie/tt1234", 7, "p1", routing)
	if err != nil {
		t.Fatal(err)
	}
	cached, err := service.ResolveVirtualPlaybackWithRouting(context.Background(), "virtual://movie/tt1234", 7, "p1", routing)
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err := service.RefreshVirtualPlaybackForInstallation(
		context.Background(), "virtual://movie/tt1234", 7, "p1", 101, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	afterRefresh, err := service.ResolveVirtualPlaybackWithRouting(context.Background(), "virtual://movie/tt1234", 7, "p1", routing)
	if err != nil {
		t.Fatal(err)
	}
	if first != "https://1.1.1.1/stream-1" || cached != first ||
		refreshed != "https://1.1.1.1/stream-2" || afterRefresh != refreshed {
		t.Fatalf("cache sequence = first %q cached %q refreshed %q after %q", first, cached, refreshed, afterRefresh)
	}
	if calls[101].Load() != 2 || !forced.Load() {
		t.Fatalf("provider calls=%d force_refresh=%v", calls[101].Load(), forced.Load())
	}
}

func TestVirtualPlaybackCacheTTLUsesBoundedProviderMetadata(t *testing.T) {
	tests := []struct {
		name    string
		seconds any
		want    time.Duration
	}{
		{name: "default", seconds: nil, want: virtualStreamsCacheTTL},
		{name: "minimum clamp", seconds: float64(1), want: minVirtualStreamsCacheTTL},
		{name: "provider value", seconds: float64(3600), want: time.Hour},
		{name: "maximum clamp", seconds: float64(9999999), want: maxVirtualStreamsCacheTTL},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := virtualResponse(virtualCandidate("ttl", "https://1.1.1.1/ttl")).GetResult()
			if test.seconds != nil {
				result.Metadata, _ = structpb.NewStruct(map[string]any{"cache_ttl_seconds": test.seconds})
			}
			if got := virtualStreamCacheTTL(result); got != test.want {
				t.Fatalf("virtualStreamCacheTTL = %v, want %v", got, test.want)
			}
			service := &Service{}
			now := time.Now()
			service.storeVirtualStreamResult("ttl", result, now)
			if got := service.virtualStreamsCache["ttl"].expiresAt.Sub(now); got != test.want {
				t.Fatalf("stored TTL = %v, want %v", got, test.want)
			}
		})
	}
}

func TestVirtualPlaybackRejectsInvalidProviderCacheTTL(t *testing.T) {
	for _, ttl := range []any{"3600", 60.5} {
		result := virtualResponse(virtualCandidate("ttl", "https://1.1.1.1/ttl"))
		result.Result.Metadata, _ = structpb.NewStruct(map[string]any{"cache_ttl_seconds": ttl})
		if _, err := validateVirtualStreamResponse(result); err == nil {
			t.Fatalf("invalid cache_ttl_seconds %#v was accepted", ttl)
		}
	}
}

func TestConfiguredVirtualProfilesCachesConfigurationOnlyRPC(t *testing.T) {
	var calls atomic.Int32
	client := pluginhost.NewVirtualStreamProviderClientForTest(&fakeVirtualStreamGRPCClient{
		profilesFunc: func(_ context.Context, request *pluginv1.ListVirtualStreamProfilesRequest) (*pluginv1.ListVirtualStreamProfilesResponse, error) {
			calls.Add(1)
			if request.GetCapabilityId() != testVirtualCapabilityID || request.GetMediaType() != "movie" {
				t.Fatalf("request = %#v", request)
			}
			return &pluginv1.ListVirtualStreamProfilesResponse{Profiles: []*pluginv1.VirtualStreamProfile{{Label: "1080p"}}}, nil
		},
	}, time.Second)
	service := &Service{}

	for range 2 {
		response, err := service.configuredVirtualProfiles(context.Background(), 101, testVirtualCapabilityID, "movie", client)
		if err != nil {
			t.Fatalf("configuredVirtualProfiles: %v", err)
		}
		if len(response.GetProfiles()) != 1 || response.GetProfiles()[0].GetLabel() != "1080p" {
			t.Fatalf("response = %#v", response)
		}
		response.Profiles[0].Label = "mutated"
	}
	if calls.Load() != 1 {
		t.Fatalf("profile RPC calls = %d, want 1", calls.Load())
	}
	service.invalidateInstallationCache()
	if _, err := service.configuredVirtualProfiles(context.Background(), 101, testVirtualCapabilityID, "movie", client); err != nil {
		t.Fatalf("configuredVirtualProfiles after invalidation: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("profile RPC calls after invalidation = %d, want 2", calls.Load())
	}
}

func TestVirtualPlaybackRejectsOversizedCandidateSet(t *testing.T) {
	candidates := make([]*pluginv1.VirtualStreamCandidate, maxVirtualPlaybackStreams+1)
	for i := range candidates {
		candidates[i] = virtualCandidate(fmt.Sprintf("candidate-%d", i), fmt.Sprintf("https://1.1.1.1/%d", i))
	}
	service, _ := newVirtualPlaybackTestService(t,
		func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
			return virtualResponse(candidates...), nil
		},
	)
	_, err := service.ListVirtualPlaybackStreamsWithRouting(
		context.Background(), "virtual://movie/tt1234", 0, "", VirtualPlaybackRouting{OwnerInstallationID: 101},
	)
	if err == nil {
		t.Fatal("oversized candidate set was accepted")
	}
}

func TestVirtualPlaybackRejectsUnsafeCandidateURL(t *testing.T) {
	for idx, raw := range []string{
		"file:///etc/passwd",
		"https://user:secret@1.1.1.1/stream",
		"https://1.1.1.1/stream\ninjected",
	} {
		t.Run(raw, func(t *testing.T) {
			service, _ := newVirtualPlaybackTestService(t,
				func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
					return virtualResponse(virtualCandidate("unsafe", raw)), nil
				},
			)
			vpath := fmt.Sprintf("virtual://movie/tt1234-%d", idx)
			_, err := service.ResolveVirtualPlaybackForInstallation(
				context.Background(), vpath, 0, "", 101, false,
			)
			if err == nil {
				t.Fatal("unsafe candidate URL was accepted")
			}
		})
	}
}

func TestVirtualPlaybackRejectsPrivateCandidateWithoutInsecureOptIn(t *testing.T) {
	// Private hosts are structurally valid candidates (allow_insecure_http may
	// be enabled for the owning plugin), so the listing accepts them. The
	// resolved-URL path must still refuse to fetch them without the opt-in.
	private := "http://127.0.0.1/admin"
	service, _ := newVirtualPlaybackTestService(t,
		func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
			return virtualResponse(virtualCandidate("unsafe", private)), nil
		},
	)
	streams, err := service.ListVirtualPlaybackStreamsWithRouting(
		context.Background(), "virtual://movie/tt1234", 0, "", VirtualPlaybackRouting{OwnerInstallationID: 101},
	)
	if err != nil || len(streams) != 1 {
		t.Fatalf("private candidate listing = %+v, %v; want one listed stream", streams, err)
	}
	if _, err := service.ResolveVirtualPlaybackWithRouting(
		context.Background(), streams[0].URI, 0, "", VirtualPlaybackRouting{OwnerInstallationID: 101},
	); err == nil {
		t.Fatal("private host resolved without allow_insecure_http opt-in")
	}
}

func TestVirtualPlaybackResolvesPrivateHostWithInsecureOptIn(t *testing.T) {
	private := "http://127.0.0.1/admin"
	service, _ := newVirtualPlaybackTestService(t,
		func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
			return virtualResponse(virtualCandidate("private", private)), nil
		},
	)
	streams, err := service.ListVirtualPlaybackStreamsWithRouting(
		context.Background(), "virtual://movie/tt1234", 0, "", VirtualPlaybackRouting{OwnerInstallationID: 101},
	)
	if err != nil || len(streams) != 1 {
		t.Fatalf("private candidate listing = %+v, %v", streams, err)
	}
	resolved, err := service.ResolveVirtualPlaybackWithRouting(
		context.Background(), streams[0].URI, 0, "", VirtualPlaybackRouting{OwnerInstallationID: 101, AllowInsecure: true},
	)
	if err != nil {
		t.Fatalf("private host rejected with allow_insecure_http opt-in: %v", err)
	}
	if resolved != private {
		t.Fatalf("resolved URL = %q, want %q", resolved, private)
	}
}

func TestVirtualPlaybackDefersDNSUntilCandidateResolution(t *testing.T) {
	service, _ := newVirtualPlaybackTestService(t,
		func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
			return virtualResponse(virtualCandidate("unresolved", "https://provider.invalid/stream")), nil
		},
	)
	streams, err := service.ListVirtualPlaybackStreamsWithRouting(
		context.Background(), "virtual://movie/tt1234", 0, "", VirtualPlaybackRouting{OwnerInstallationID: 101},
	)
	if err != nil || len(streams) != 1 {
		t.Fatalf("candidate list = %+v, %v; DNS should be deferred", streams, err)
	}
	if _, err := service.ResolveVirtualPlaybackWithRouting(
		context.Background(), streams[0].URI, 0, "", VirtualPlaybackRouting{OwnerInstallationID: 101},
	); err == nil {
		t.Fatal("selected candidate with an unresolvable host was accepted")
	}
}

func TestVirtualPlaybackUsesBoundedCandidateDisplayMetadata(t *testing.T) {
	candidate := virtualCandidate("display", "https://1.1.1.1/display")
	candidate.Metadata, _ = structpb.NewStruct(map[string]any{
		"display_name": "Release Name · 4K",
		"source_type":  "Alt source",
	})
	service, _ := newVirtualPlaybackTestService(t,
		func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
			return virtualResponse(candidate), nil
		},
	)
	streams, err := service.ListVirtualPlaybackStreamsWithRouting(
		context.Background(), "virtual://movie/tt1234", 0, "", VirtualPlaybackRouting{OwnerInstallationID: 101},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(streams) != 1 || streams[0].Label != "Release Name · 4K" || streams[0].SourceType != "Alt source" {
		t.Fatalf("display metadata not applied: %+v", streams)
	}
}

func TestVirtualPlaybackMapsVisibleCandidateMetadata(t *testing.T) {
	first := virtualCandidate("first", "https://provider.invalid/first")
	first.Metadata, _ = structpb.NewStruct(map[string]any{"visible": true})
	second := virtualCandidate("second", "https://provider.invalid/second")
	second.Metadata, _ = structpb.NewStruct(map[string]any{"visible": false})
	service, _ := newVirtualPlaybackTestService(t, func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
		return virtualResponse(first, second), nil
	})
	streams, err := service.ListVirtualPlaybackStreamsWithRouting(context.Background(), "virtual://movie/tt1234", 0, "", VirtualPlaybackRouting{OwnerInstallationID: 101})
	if err != nil || len(streams) != 2 {
		t.Fatalf("streams = %+v, err = %v", streams, err)
	}
	if !streams[0].Visible || streams[1].Visible {
		t.Fatalf("visibility = [%v, %v], want [true, false]", streams[0].Visible, streams[1].Visible)
	}
}

func TestVirtualPlaybackRejectsInvalidCandidateDisplayMetadata(t *testing.T) {
	for _, metadata := range []map[string]any{
		{"display_name": 42},
		{"source_type": "bad\nsource"},
		{"display_name": "spoof\u202ename"},
		{"display_name": strings.Repeat("x", maxVirtualLabelLen+1)},
	} {
		candidate := virtualCandidate("display", "https://1.1.1.1/display")
		candidate.Metadata, _ = structpb.NewStruct(metadata)
		service, _ := newVirtualPlaybackTestService(t,
			func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
				return virtualResponse(candidate), nil
			},
		)
		if _, err := service.ListVirtualPlaybackStreamsWithRouting(
			context.Background(), "virtual://movie/tt1234", 0, "", VirtualPlaybackRouting{OwnerInstallationID: 101},
		); err == nil {
			t.Fatalf("invalid display metadata was accepted: %#v", metadata)
		}
	}
}

func TestVirtualPlaybackFallsBackWhenSelectedCandidateDisappearsOrExpires(t *testing.T) {
	expired := virtualCandidate("expired", "https://1.1.1.1/expired")
	expired.ExpiresAt = timestamppb.New(time.Now().Add(-time.Minute))
	fallback := virtualCandidate("fallback", "https://1.1.1.1/fallback")
	fallback.Rank = 2
	service, calls := newVirtualPlaybackTestService(t,
		func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
			return virtualResponse(expired, fallback), nil
		},
	)
	routing := VirtualPlaybackRouting{OwnerInstallationID: 101}
	for _, selected := range []string{"expired", "no-longer-returned"} {
		resolved, err := service.ResolveVirtualPlaybackWithRouting(
			context.Background(), "virtual://movie/tt1234?result="+selected, 7, "p1", routing,
		)
		if err != nil {
			t.Fatalf("resolve stale selection %q: %v", selected, err)
		}
		if resolved != "https://1.1.1.1/fallback" {
			t.Fatalf("resolved stale selection %q to %q", selected, resolved)
		}
	}
	if calls[101].Load() != 1 {
		t.Fatalf("provider called %d times, want one cached resolution", calls[101].Load())
	}
}

func TestVirtualPlaybackStaleSelectionIsRejected(t *testing.T) {
	best := virtualCandidate("new-4k", "https://1.1.1.1/4k")
	best.Resolution.Label = "2160p"
	profileMatch := virtualCandidate("new-1080p", "https://1.1.1.1/1080p")
	profileMatch.Resolution.Label = "1080p"
	profileMatch.Rank = 2
	service, _ := newVirtualPlaybackTestService(t,
		func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
			return virtualResponse(best, profileMatch), nil
		},
	)
	resolved, err := service.ResolveVirtualPlaybackWithRouting(
		context.Background(),
		"virtual://movie/tt1234?profile=1080p&result=rotated-away",
		7,
		"p1",
		VirtualPlaybackRouting{OwnerInstallationID: 101},
	)
	if err == nil {
		t.Fatalf("stale result unexpectedly resolved to %q", resolved)
	}
}

func TestVirtualPlaybackProfileOnlySelectionMatchesProfile(t *testing.T) {
	best4K := virtualCandidate("candidate-4k", "https://1.1.1.1/4k")
	best4K.Resolution.Label = "2160p"
	match1080p := virtualCandidate("candidate-1080p", "https://1.1.1.1/1080p")
	match1080p.Resolution.Label = "1080p"
	match1080p.Rank = 2

	service, _ := newVirtualPlaybackTestService(t,
		func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
			return virtualResponse(best4K, match1080p), nil
		},
	)
	res, err := service.ResolveVirtualPlaybackDetailedWithRouting(
		context.Background(),
		"virtual://movie/tt1234?profile=1080p",
		7,
		"p1",
		VirtualPlaybackRouting{OwnerInstallationID: 101},
		false,
		nil,
		"",
	)
	if err != nil {
		t.Fatalf("unexpected resolve error: %v", err)
	}
	if res.CandidateID != "candidate-1080p" {
		t.Fatalf("candidateID = %q, want candidate-1080p", res.CandidateID)
	}
	if res.URL != "https://1.1.1.1/1080p" {
		t.Fatalf("url = %q, want https://1.1.1.1/1080p", res.URL)
	}
}

func TestVirtualPlaybackNamedProfileSynonymMatches(t *testing.T) {
	cand4K := virtualCandidate("cand-4k", "https://1.1.1.1/4k")
	cand4K.Resolution.Label = "2160p"
	cand1080p := virtualCandidate("cand-1080p", "https://1.1.1.1/1080p")
	cand1080p.Resolution.Label = "1080p"

	service, _ := newVirtualPlaybackTestService(t,
		func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
			return virtualResponse(cand1080p, cand4K), nil
		},
	)
	res, err := service.ResolveVirtualPlaybackDetailedWithRouting(
		context.Background(),
		"virtual://movie/tt1234?profile=4K",
		7,
		"p1",
		VirtualPlaybackRouting{OwnerInstallationID: 101},
		false,
		nil,
		"",
	)
	if err != nil {
		t.Fatalf("unexpected resolve error: %v", err)
	}
	if res.CandidateID != "cand-4k" {
		t.Fatalf("candidateID = %q, want cand-4k", res.CandidateID)
	}
}

func TestVirtualPlaybackProfileCandidateFallbackOnInvalidURL(t *testing.T) {
	badURLCand := virtualCandidate("cand-bad", "ftp://invalid-scheme/4k")
	badURLCand.Resolution.Label = "2160p"
	goodURLCand := virtualCandidate("cand-good", "https://1.1.1.1/4k")
	goodURLCand.Resolution.Label = "2160p"

	service, _ := newVirtualPlaybackTestService(t,
		func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
			return virtualResponse(badURLCand, goodURLCand), nil
		},
	)
	res, err := service.ResolveVirtualPlaybackDetailedWithRouting(
		context.Background(),
		"virtual://movie/tt1234?profile=4K",
		7,
		"p1",
		VirtualPlaybackRouting{OwnerInstallationID: 101},
		false,
		nil,
		"",
	)
	if err != nil {
		t.Fatalf("unexpected resolve error: %v", err)
	}
	if res.CandidateID != "cand-good" {
		t.Fatalf("candidateID = %q, want cand-good", res.CandidateID)
	}
}

func TestVirtualPlaybackCompoundProfileMatches4KHDR(t *testing.T) {
	cand1080p := virtualCandidate("cand-1080p", "https://1.1.1.1/1080p")
	cand1080p.Resolution.Label = "1080p"
	cand4KHDR := virtualCandidate("cand-4k-hdr", "https://1.1.1.1/4k-hdr")
	cand4KHDR.Resolution.Label = "2160p"
	cand4KHDR.Hdr = &pluginv1.VirtualStreamHDR{IsHdr: true, Format: "hdr10"}

	service, _ := newVirtualPlaybackTestService(t,
		func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
			return virtualResponse(cand1080p, cand4KHDR), nil
		},
	)
	res, err := service.ResolveVirtualPlaybackDetailedWithRouting(
		context.Background(),
		"virtual://series/tt22202452/1/1?profile=4K+HDR",
		7,
		"p1",
		VirtualPlaybackRouting{OwnerInstallationID: 101},
		false,
		nil,
		"",
	)
	if err != nil {
		t.Fatalf("unexpected resolve error: %v", err)
	}
	if res.CandidateID != "cand-4k-hdr" {
		t.Fatalf("candidateID = %q, want cand-4k-hdr", res.CandidateID)
	}
}

func TestVirtualPlaybackCompoundProfileRefinesDolbyVision(t *testing.T) {
	candHDR10 := virtualCandidate("cand-hdr10", "https://1.1.1.1/hdr10")
	candHDR10.Resolution.Label = "2160p"
	candHDR10.Hdr = &pluginv1.VirtualStreamHDR{IsHdr: true, Format: "hdr10"}

	candDV := virtualCandidate("cand-dv", "https://1.1.1.1/dv")
	candDV.Resolution.Label = "2160p"
	candDV.Hdr = &pluginv1.VirtualStreamHDR{IsHdr: true, Format: "dv", HasDolbyVision: true}

	service, _ := newVirtualPlaybackTestService(t,
		func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
			return virtualResponse(candHDR10, candDV), nil
		},
	)
	res, err := service.ResolveVirtualPlaybackDetailedWithRouting(
		context.Background(),
		"virtual://movie/tt1234?profile=4K+Dolby+Vision",
		7,
		"p1",
		VirtualPlaybackRouting{OwnerInstallationID: 101},
		false,
		nil,
		"",
	)
	if err != nil {
		t.Fatalf("unexpected resolve error: %v", err)
	}
	if res.CandidateID != "cand-dv" {
		t.Fatalf("candidateID = %q, want cand-dv", res.CandidateID)
	}
}

func TestVirtualPlaybackProfileFallsBackToAvailableStreams(t *testing.T) {
	cand1080p := virtualCandidate("cand-1080p", "https://1.1.1.1/1080p")
	cand1080p.Resolution.Label = "1080p"

	service, _ := newVirtualPlaybackTestService(t,
		func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
			return virtualResponse(cand1080p), nil
		},
	)
	res, err := service.ResolveVirtualPlaybackDetailedWithRouting(
		context.Background(),
		"virtual://series/tt22202452/1/1?profile=4K+HDR",
		7,
		"p1",
		VirtualPlaybackRouting{OwnerInstallationID: 101},
		false,
		nil,
		"",
	)
	if err != nil {
		t.Fatalf("expected fallback resolution, got error: %v", err)
	}
	if res.CandidateID != "cand-1080p" {
		t.Fatalf("candidateID = %q, want cand-1080p", res.CandidateID)
	}
}

func TestVirtualPlaybackListStreamsReturnsAllCandidatesEvenWithProfile(t *testing.T) {
	cand1 := virtualCandidate("c1", "https://1.1.1.1/1")
	cand1.Resolution.Label = "2160p"
	cand2 := virtualCandidate("c2", "https://1.1.1.1/2")
	cand2.Resolution.Label = "1080p"

	service, _ := newVirtualPlaybackTestService(t,
		func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
			return virtualResponse(cand1, cand2), nil
		},
	)
	streams, err := service.ListVirtualPlaybackStreamsWithRouting(
		context.Background(),
		"virtual://series/tt22202452/1/1?profile=4K+HDR",
		7,
		"p1",
		VirtualPlaybackRouting{OwnerInstallationID: 101},
	)
	if err != nil {
		t.Fatalf("ListVirtualPlaybackStreamsWithRouting failed: %v", err)
	}
	if len(streams) != 2 {
		t.Fatalf("got %d streams, want 2", len(streams))
	}
}

func TestVirtualPlaybackMissingOwnerFallsBackToReplacementProvider(t *testing.T) {
	service, calls := newVirtualPlaybackTestService(t,
		func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
			return virtualResponse(virtualCandidate("old", "https://1.1.1.1/old")), nil
		},
		func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
			return virtualResponse(virtualCandidate("replacement", "https://1.1.1.1/replacement")), nil
		},
	)
	installationStore, ok := service.installations.(*fakeServiceInstallationStore)
	if !ok {
		t.Fatal("test service did not use the fake installation store")
	}
	delete(installationStore.byID, 101)
	resolved, err := service.ResolveVirtualPlaybackWithRouting(
		context.Background(),
		"virtual://movie/tt1234?result=old",
		7,
		"p1",
		VirtualPlaybackRouting{OwnerInstallationID: 101, AllowFallback: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != "https://1.1.1.1/replacement" {
		t.Fatalf("replacement provider resolved %q", resolved)
	}
	if calls[101].Load() != 0 || calls[102].Load() != 1 {
		t.Fatalf("provider calls = old:%d replacement:%d", calls[101].Load(), calls[102].Load())
	}
}

func TestVirtualPlaybackResolvesWithoutOwner(t *testing.T) {
	service, calls := newVirtualPlaybackTestService(t,
		func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
			return virtualResponse(virtualCandidate("p101", "https://1.1.1.1/p101")), nil
		},
	)
	resolved, err := service.ResolveVirtualPlaybackWithRouting(
		context.Background(),
		"virtual://movie/tt1234?result=p101",
		7,
		"p1",
		VirtualPlaybackRouting{OwnerInstallationID: 0, AllowFallback: false},
	)
	if err != nil {
		t.Fatalf("expected resolution without owner to succeed, got: %v", err)
	}
	if resolved != "https://1.1.1.1/p101" {
		t.Fatalf("got %q, want https://1.1.1.1/p101", resolved)
	}
	if calls[101].Load() != 1 {
		t.Fatalf("expected call to provider 101, got %d", calls[101].Load())
	}
}

func TestVirtualPlaybackCacheIsBoundedAndLifecycleInvalidated(t *testing.T) {
	service := &Service{}
	now := time.Now()
	for i := 0; i < maxVirtualPlaybackCacheEntries+25; i++ {
		service.storeVirtualStreamResult(
			fmt.Sprintf("key-%d", i),
			virtualResponse(virtualCandidate(fmt.Sprintf("candidate-%d", i), "https://1.1.1.1/stream")).GetResult(),
			now.Add(time.Duration(i)*time.Millisecond),
		)
	}
	if got := len(service.virtualStreamsCache); got != maxVirtualPlaybackCacheEntries {
		t.Fatalf("cache entries = %d, want %d", got, maxVirtualPlaybackCacheEntries)
	}
	if got := virtualStreamsCacheSize(service.virtualStreamsCache); got > maxVirtualPlaybackCacheBytes {
		t.Fatalf("cache size = %d, limit %d", got, maxVirtualPlaybackCacheBytes)
	}
	service.storeResolvedURL("virtual://movie/tt000001", 1, "kid", 7, "https://1.1.1.1/stream")
	service.invalidateInstallationCache()
	if service.virtualStreamsCache != nil {
		t.Fatalf("lifecycle invalidation retained %d cache entries", len(service.virtualStreamsCache))
	}
	if service.resolvedURLs != nil {
		t.Fatalf("lifecycle invalidation retained %d resolved URL entries", len(service.resolvedURLs))
	}
}

func TestVirtualPlaybackCacheIsBoundedByAggregateBytes(t *testing.T) {
	service := &Service{}
	largeCandidates := make([]*pluginv1.VirtualStreamCandidate, 4)
	for index := range largeCandidates {
		candidate := virtualCandidate(fmt.Sprintf("large-%d", index), fmt.Sprintf("https://1.1.1.1/large-%d", index))
		candidate.Metadata, _ = structpb.NewStruct(map[string]any{
			"payload": strings.Repeat("x", 60<<10),
		})
		largeCandidates[index] = candidate
	}
	result := virtualResponse(largeCandidates...).GetResult()
	now := time.Now()
	for index := 0; index < 100; index++ {
		service.storeVirtualStreamResult(fmt.Sprintf("large-key-%d", index), result, now.Add(time.Duration(index)*time.Millisecond))
	}
	if got := virtualStreamsCacheSize(service.virtualStreamsCache); got > maxVirtualPlaybackCacheBytes {
		t.Fatalf("cache size = %d, limit %d", got, maxVirtualPlaybackCacheBytes)
	}
	if got := len(service.virtualStreamsCache); got >= 100 {
		t.Fatalf("byte ceiling did not evict entries: retained %d", got)
	}
}

func TestResolvedURLMemoIsBoundedAndExpires(t *testing.T) {
	service := &Service{}
	// Bounded at the configured ceiling.
	for i := 0; i < resolvedURLMemoMax+32; i++ {
		service.storeResolvedURL(fmt.Sprintf("virtual://movie/tt%06d", i), 1, "kid", 7, "https://1.1.1.1/stream")
		if got := len(service.resolvedURLs); got > resolvedURLMemoMax {
			t.Fatalf("memo entries = %d, want at most %d", got, resolvedURLMemoMax)
		}
	}
	if got := len(service.resolvedURLs); got != resolvedURLMemoMax {
		t.Fatalf("memo entries = %d, want %d", got, resolvedURLMemoMax)
	}

	// A stored entry is served within its TTL and dropped once it is past the
	// bounded stale grace (a bare Service cannot refresh, so no in-flight
	// refresh keeps it alive).
	service.storeResolvedURL("virtual://movie/tt000001", 1, "kid", 7, "https://1.1.1.1/fresh")
	if got := service.lookupResolvedURL("virtual://movie/tt000001", 1, "kid", 7); got != "https://1.1.1.1/fresh" {
		t.Fatalf("lookup = %q, want freshly stored URL", got)
	}
	entry := service.resolvedURLs[resolvedURLMemoKey("virtual://movie/tt000001", 1, "kid", 7)]
	entry.resolvedAt = entry.resolvedAt.Add(-(resolvedURLMemoTTL + resolvedURLMemoStaleGrace + time.Second))
	service.resolvedURLs[resolvedURLMemoKey("virtual://movie/tt000001", 1, "kid", 7)] = entry
	if got := service.lookupResolvedURL("virtual://movie/tt000001", 1, "kid", 7); got != "" {
		t.Fatalf("lookup = %q, want entry past stale grace dropped", got)
	}
	if _, ok := service.resolvedURLs[resolvedURLMemoKey("virtual://movie/tt000001", 1, "kid", 7)]; ok {
		t.Fatal("entry past stale grace remained after lookup")
	}

	// Clear flushes memoized resolved URLs instantly.
	service.storeResolvedURL("virtual://movie/tt000001", 1, "kid", 7, "https://1.1.1.1/fresh")
	service.Clear()
	if got := service.lookupResolvedURL("virtual://movie/tt000001", 1, "kid", 7); got != "" {
		t.Fatalf("lookup = %q, want empty after Clear", got)
	}
}

// seedResolvedURLEntry installs a memo entry directly so a test can control its
// age without waiting on the TTL or starting a warm-refresh timer.
func seedResolvedURLEntry(service *Service, key, url string, age time.Duration) {
	service.resolvedURLsMu.Lock()
	defer service.resolvedURLsMu.Unlock()
	if service.resolvedURLs == nil {
		service.resolvedURLs = make(map[string]resolvedURLEntry)
	}
	service.resolvedURLs[key] = resolvedURLEntry{
		url:        url,
		uri:        "virtual://movie/tt1234",
		resolvedAt: time.Now().Add(-age),
	}
}

func waitForResolvedURLValue(t *testing.T, service *Service, virtualPath string, userID int, profileID string, ownerInstallationID int, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := service.lookupResolvedURL(virtualPath, userID, profileID, ownerInstallationID); got == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("resolved URL = %q, want %q", service.lookupResolvedURL(virtualPath, userID, profileID, ownerInstallationID), want)
}

func waitForResolvedURLRefreshFailure(t *testing.T, service *Service, key string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		service.resolvedURLsMu.Lock()
		entry, ok := service.resolvedURLs[key]
		failed := ok && entry.refreshFailed
		service.resolvedURLsMu.Unlock()
		if failed {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("background refresh failure was not recorded")
}

// A memo entry past its TTL but inside resolvedURLMemoStaleGrace is served
// while exactly one background refresh replaces it.
func TestResolvedURLMemoServesStaleWithinGraceAndRefreshes(t *testing.T) {
	service, calls := newVirtualPlaybackTestService(t,
		func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
			return virtualResponse(virtualCandidate("fresh", "https://1.1.1.1/fresh")), nil
		},
	)
	const path = "virtual://movie/tt1234"
	key := resolvedURLMemoKey(path, 7, "p1", 101)
	seedResolvedURLEntry(service, key, "https://1.1.1.1/stale", resolvedURLMemoTTL+time.Minute)

	res, ok := service.lookupResolvedStream(path, 7, "p1", 101)
	if !ok || res.URL != "https://1.1.1.1/stale" {
		t.Fatalf("stale lookup = (%#v, %v), want cached stale URL served", res, ok)
	}

	waitForResolvedURLValue(t, service, path, 7, "p1", 101, "https://1.1.1.1/fresh")
	if got := calls[101].Load(); got != 1 {
		t.Fatalf("provider calls = %d, want 1 refresh", got)
	}
}

// A provider-declared expiry is a hard bound: the entry is never served stale
// even though it is inside the TTL-based grace window.
func TestResolvedURLMemoNeverServesPastProviderExpiry(t *testing.T) {
	service, _ := newVirtualPlaybackTestService(t,
		func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
			return virtualResponse(virtualCandidate("fresh", "https://1.1.1.1/fresh")), nil
		},
	)
	const path = "virtual://movie/tt1234"
	key := resolvedURLMemoKey(path, 7, "p1", 101)
	seedResolvedURLEntry(service, key, "https://1.1.1.1/dead", resolvedURLMemoTTL+time.Minute)
	entry := service.resolvedURLs[key]
	entry.expiresAt = time.Now().Add(-time.Second)
	service.resolvedURLsMu.Lock()
	service.resolvedURLs[key] = entry
	service.resolvedURLsMu.Unlock()

	if _, ok := service.lookupResolvedStream(path, 7, "p1", 101); ok {
		t.Fatal("entry past provider expiry was served")
	}
	if _, exists := service.resolvedURLs[key]; exists {
		t.Fatal("entry past provider expiry was not deleted")
	}
}

// A definitive refresh failure must not have the stale URL pinned: the entry is
// dropped instead of extended.
func TestResolvedURLMemoDropsStaleAfterDefinitiveRefreshFailure(t *testing.T) {
	service, _ := newVirtualPlaybackTestService(t,
		func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
			return nil, errors.New("provider unavailable")
		},
	)
	const path = "virtual://movie/tt1234"
	key := resolvedURLMemoKey(path, 7, "p1", 101)
	seedResolvedURLEntry(service, key, "https://1.1.1.1/stale", resolvedURLMemoTTL+time.Minute)

	if res, ok := service.lookupResolvedStream(path, 7, "p1", 101); !ok || res.URL != "https://1.1.1.1/stale" {
		t.Fatalf("stale lookup = (%#v, %v), want cached stale URL served", res, ok)
	}
	waitForResolvedURLRefreshFailure(t, service, key)
	if _, ok := service.lookupResolvedStream(path, 7, "p1", 101); ok {
		t.Fatal("stale entry served after a definitive refresh failure")
	}
	if _, exists := service.resolvedURLs[key]; exists {
		t.Fatal("failed stale entry was not deleted")
	}
}

// Concurrent stale lookups must not each spawn a refresh: the first caller
// marks the refresh in flight and the provider is contacted once.
func TestResolvedURLMemoStaleRefreshSingleFlight(t *testing.T) {
	release := make(chan struct{})
	service, calls := newVirtualPlaybackTestService(t,
		func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
			<-release
			return virtualResponse(virtualCandidate("fresh", "https://1.1.1.1/fresh")), nil
		},
	)
	const path = "virtual://movie/tt1234"
	key := resolvedURLMemoKey(path, 7, "p1", 101)
	seedResolvedURLEntry(service, key, "https://1.1.1.1/stale", resolvedURLMemoTTL+time.Minute)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, ok := service.lookupResolvedStream(path, 7, "p1", 101)
			if !ok || res.URL != "https://1.1.1.1/stale" {
				t.Errorf("stale lookup = (%#v, %v), want cached stale URL served", res, ok)
			}
		}()
	}
	wg.Wait()

	deadline := time.Now().Add(2 * time.Second)
	for calls[101].Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := calls[101].Load(); got != 1 {
		close(release)
		t.Fatalf("provider calls = %d, want 1", got)
	}
	close(release)
	waitForResolvedURLValue(t, service, path, 7, "p1", 101, "https://1.1.1.1/fresh")
}

func TestVirtualStreamRequestRejectsTraversalAndMalformedEpisodes(t *testing.T) {
	for _, raw := range []string{
		"virtual://series/tt123/1",
		"virtual://series/tt123/0/1",
		"virtual://series/tt123/%2e%2e/1",
		"virtual://movie/tt123/extra",
		"virtual://movie/%2fetc",
	} {
		if _, _, err := virtualStreamRequest(raw, 0, ""); err == nil {
			t.Fatalf("virtualStreamRequest(%q) succeeded", raw)
		}
	}
}

func TestVirtualStreamRequestAcceptsNamespacedFallbackIDs(t *testing.T) {
	tests := []struct {
		name      string
		uri       string
		mediaType string
		ids       map[string]string
		season    int32
		episode   int32
	}{
		{
			name:      "tmdb movie",
			uri:       "virtual://movie/tmdb/603",
			mediaType: "movie",
			ids:       map[string]string{"tmdb": "603"},
		},
		{
			name:      "tvdb episode",
			uri:       "virtual://series/tvdb/393159/3/1",
			mediaType: "episode",
			ids:       map[string]string{"tvdb": "393159"},
			season:    3,
			episode:   1,
		},
		{
			name:      "tmdb episode",
			uri:       "virtual://series/tmdb/202555/1/2",
			mediaType: "episode",
			ids:       map[string]string{"tmdb": "202555"},
			season:    1,
			episode:   2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request, _, err := virtualStreamRequest(tt.uri, 0, "")
			if err != nil {
				t.Fatalf("virtualStreamRequest(): %v", err)
			}
			if request.GetMediaType() != tt.mediaType ||
				request.GetSeasonNumber() != tt.season ||
				request.GetEpisodeNumber() != tt.episode {
				t.Fatalf("request = %+v", request)
			}
			if len(request.GetExternalIds()) != len(tt.ids) {
				t.Fatalf("external IDs = %v, want %v", request.GetExternalIds(), tt.ids)
			}
			for provider, want := range tt.ids {
				if got := request.GetExternalIds()[provider]; got != want {
					t.Fatalf("%s ID = %q, want %q", provider, got, want)
				}
			}
		})
	}
}

func newVirtualPlaybackTestService(
	t *testing.T,
	resolvers ...func(context.Context, *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error),
) (*Service, map[int]*atomic.Int32) {
	t.Helper()
	manifest := testPluginManifest(t, "test.virtual", "1.0.0")
	manifest.Capabilities = []*pluginv1.CapabilityDescriptor{{Type: virtualStreamProviderCapabilityType, Id: testVirtualCapabilityID}}
	installPath := writeInstalledPluginManifest(t, manifest)
	store := newFakeServiceInstallationStore()
	store.listCapabilities = []*Capability{{Type: virtualStreamProviderCapabilityType, ID: testVirtualCapabilityID}}
	host := &fakeVirtualPluginHost{clients: make(map[int]pluginClient)}
	calls := make(map[int]*atomic.Int32)
	for index, resolver := range resolvers {
		id := 101 + index
		installation := &Installation{ID: id, PluginID: "test.virtual", Version: "1.0.0", InstallPath: installPath, Enabled: true}
		store.byID[id] = installation
		store.byPluginID[installation.PluginID] = append(store.byPluginID[installation.PluginID], installation)
		counter := &atomic.Int32{}
		calls[id] = counter
		resolve := resolver
		host.clients[id] = &fakePluginClient{
			manifest: manifest,
			virtualStreamClient: pluginhost.NewVirtualStreamProviderClientForTest(&fakeVirtualStreamGRPCClient{
				resolveFunc: func(ctx context.Context, request *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
					counter.Add(1)
					if request.GetCapabilityId() != testVirtualCapabilityID {
						t.Errorf("capability ID = %q", request.GetCapabilityId())
					}
					return resolve(ctx, request)
				},
			}, time.Second),
		}
	}
	return &Service{installations: store, host: host}, calls
}

func virtualResponse(candidates ...*pluginv1.VirtualStreamCandidate) *pluginv1.ResolveVirtualStreamResponse {
	return &pluginv1.ResolveVirtualStreamResponse{Result: &pluginv1.VirtualStreamResult{
		ResultId: "result", ProviderId: "test.provider", Candidates: candidates,
	}}
}

func virtualCandidate(id, streamURL string) *pluginv1.VirtualStreamCandidate {
	return &pluginv1.VirtualStreamCandidate{
		CandidateId: id, ProviderId: "test.provider", TemporaryUri: streamURL, Rank: 1,
		Resolution: &pluginv1.VirtualStreamResolution{Label: "1080p"},
		VideoCodec: "h264", AudioCodec: "aac", Container: "mp4",
	}
}

func TestInstallationAllowsInsecure(t *testing.T) {
	cases := []struct {
		name        string
		configKey   string
		configValue map[string]any
		want        bool
	}{
		{name: "bool under schema key", configKey: "allow_insecure_http", configValue: map[string]any{"allow_insecure_http": true}, want: true},
		{name: "bool false under schema key", configKey: "allow_insecure_http", configValue: map[string]any{"allow_insecure_http": false}, want: false},
		{name: "bool under enabled key", configKey: "allow_insecure_http", configValue: map[string]any{"enabled": true}, want: true},
		{name: "bool under value key", configKey: "allow_insecure_http", configValue: map[string]any{"value": true}, want: true},
		{name: "string TRUE under value key", configKey: "allow_insecure_http", configValue: map[string]any{"value": "TRUE"}, want: true},
		{name: "string false under enabled key", configKey: "allow_insecure_http", configValue: map[string]any{"enabled": "false"}, want: false},
		{name: "unrelated key is ignored", configKey: "allow_insecure_http", configValue: map[string]any{"other": true}, want: false},
		{name: "schema key inside streaming config group", configKey: "streaming", configValue: map[string]any{"aiostreams_url": "http://192.168.1.50:3000", "allow_insecure_http": true}, want: true},
		{name: "string yes under streaming config group", configKey: "streaming", configValue: map[string]any{"allow_insecure_http": "yes"}, want: true},
		// Hardened: alias and look-alike keys must never relax SSRF validation.
		{name: "alias allow_insecure is not honored", configKey: "general", configValue: map[string]any{"allow_insecure": true}, want: false},
		{name: "alias insecure is not honored", configKey: "settings", configValue: map[string]any{"insecure": "true"}, want: false},
		{name: "alias allow_http is not honored", configKey: "general", configValue: map[string]any{"allow_http": true}, want: false},
		{name: "nested maps are not walked", configKey: "settings", configValue: map[string]any{"playback": map[string]any{"allow_insecure_http": true}}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := tc.configKey
			if key == "" {
				key = "allow_insecure_http"
			}
			configs := &fakeServiceConfigStore{
				configsByInstallation: map[int][]*RuntimeConfig{
					7: {{Key: key, Value: tc.configValue}},
				},
			}
			service := &Service{configs: configs}
			if got := service.InstallationAllowsInsecure(context.Background(), 7); got != tc.want {
				t.Fatalf("InstallationAllowsInsecure = %v, want %v", got, tc.want)
			}
		})
	}
}

type failingConfigStore struct {
	serviceConfigStore
}

func (failingConfigStore) ListGlobalConfigs(context.Context, int) ([]*RuntimeConfig, error) {
	return nil, errors.New("config read failed")
}

func TestInstallationAllowsInsecureUnknownOwnerFailsClosed(t *testing.T) {
	configs := &fakeServiceConfigStore{
		configsByInstallation: map[int][]*RuntimeConfig{
			7: {{Key: "streaming", Value: map[string]any{"allow_insecure_http": true}}},
		},
	}
	service := &Service{configs: configs}
	if service.InstallationAllowsInsecure(context.Background(), 0) {
		t.Fatal("unknown owner (0) must fail closed even when some installation opts in")
	}
	if service.InstallationAllowsInsecure(context.Background(), -5) {
		t.Fatal("negative owner must fail closed")
	}
}

func TestInstallationAllowsInsecureFailClosed(t *testing.T) {
	service := &Service{}
	if service.InstallationAllowsInsecure(context.Background(), 7) {
		t.Fatal("nil configs must fail closed")
	}
	failing := &Service{configs: failingConfigStore{}}
	if failing.InstallationAllowsInsecure(context.Background(), 7) {
		t.Fatal("config read error must fail closed")
	}
	okConfig := &Service{configs: &fakeServiceConfigStore{
		configsByInstallation: map[int][]*RuntimeConfig{7: {{Key: "allow_insecure_http", Value: map[string]any{"allow_insecure_http": true}}}},
	}}
	if okConfig.InstallationAllowsInsecure(context.Background(), 0) {
		t.Fatal("invalid installation ID must fail closed")
	}
}

func TestResolvedURLMemoHonorsDynamicExpiresAt(t *testing.T) {
	service := &Service{}
	now := time.Now()
	// Stored with an explicit short expiry (in the past relative to check)
	shortExpiry := now.Add(-10 * time.Second)
	service.storeResolvedURL("virtual://movie/tt000002", 1, "profile1", 7, "https://1.1.1.1/short-lived", shortExpiry)

	// Lookup must recognize the passed expiry and drop the entry
	if got := service.lookupResolvedURL("virtual://movie/tt000002", 1, "profile1", 7); got != "" {
		t.Fatalf("lookupResolvedURL returned %q for expired candidate URL", got)
	}

	// Stored with future expiry
	futureExpiry := now.Add(2 * time.Hour)
	service.storeResolvedURL("virtual://movie/tt000003", 1, "profile1", 7, "https://1.1.1.1/future-lived", futureExpiry)
	if got := service.lookupResolvedURL("virtual://movie/tt000003", 1, "profile1", 7); got != "https://1.1.1.1/future-lived" {
		t.Fatalf("lookupResolvedURL = %q, want https://1.1.1.1/future-lived", got)
	}
}

func TestVirtualStreamResponseRejectsInvalidRequestHeaders(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
	}{
		{name: "too many", headers: map[string]string{"A": "1", "B": "2", "C": "3", "D": "4", "E": "5", "F": "6", "G": "7", "H": "8", "I": "9"}},
		{name: "invalid name", headers: map[string]string{"Bad\nName": "value"}},
		{name: "oversized name", headers: map[string]string{strings.Repeat("x", maxVirtualRequestHeaderKeyLen+1): "value"}},
		{name: "disallowed cookie", headers: map[string]string{"Cookie": "secret"}},
		{name: "disallowed authorization", headers: map[string]string{"Authorization": "secret"}},
		{name: "invalid value", headers: map[string]string{"Referer": "bad\nvalue"}},
		{name: "oversized value", headers: map[string]string{"Referer": strings.Repeat("x", maxVirtualRequestHeaderValueLen+1)}},
		{name: "duplicate case", headers: map[string]string{"Referer": "one", "referer": "two"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := virtualCandidate("header-test", "https://1.1.1.1/video.mp4")
			candidate.RequestHeaders = tc.headers
			if _, err := validateVirtualStreamResponse(virtualResponse(candidate)); err == nil {
				t.Fatal("expected invalid request headers to be rejected")
			}
		})
	}
}

func TestVirtualStreamResponseAcceptsAllowedRequestHeaders(t *testing.T) {
	candidate := virtualCandidate("header-valid", "https://1.1.1.1/video.mp4")
	candidate.RequestHeaders = map[string]string{
		"Referer":    "https://stream.example/player",
		"Origin":     "https://stream.example",
		"User-Agent": "Silo/1.0",
	}
	if _, err := validateVirtualStreamResponse(virtualResponse(candidate)); err != nil {
		t.Fatalf("validateVirtualStreamResponse rejected allowed headers: %v", err)
	}
}

func TestResolveVirtualPlaybackDetailedPropagatesHeadersAndExclusions(t *testing.T) {
	var capturedExcluded []string
	var capturedPreferred string
	cand := virtualCandidate("cand-1", "https://1.1.1.1/video.mp4")
	cand.RequestHeaders = map[string]string{"Referer": "https://stream.example/player"}
	cand.HasAtmos = true
	cand.QualityScore = 25
	futureExp := time.Now().Add(30 * time.Minute)
	cand.ExpiresAt = timestamppb.New(futureExp)

	service, _ := newVirtualPlaybackTestService(t, func(ctx context.Context, req *pluginv1.ResolveVirtualStreamRequest) (*pluginv1.ResolveVirtualStreamResponse, error) {
		capturedExcluded = req.GetExcludedCandidateIds()
		capturedPreferred = req.GetPreferredCandidateId()
		return virtualResponse(cand), nil
	})

	res, err := service.ResolveVirtualPlaybackDetailedForInstallation(
		context.Background(),
		"virtual://movie/tt1234567",
		1, "profile1", 101,
		false, false,
		[]string{"dead-1", "dead-2"},
		"cand-1",
	)
	if err != nil {
		t.Fatalf("ResolveVirtualPlaybackDetailedForInstallation error: %v", err)
	}

	if len(capturedExcluded) != 2 || capturedExcluded[0] != "dead-1" || capturedExcluded[1] != "dead-2" {
		t.Fatalf("capturedExcluded = %#v, want [dead-1, dead-2]", capturedExcluded)
	}
	if capturedPreferred != "cand-1" {
		t.Fatalf("capturedPreferred = %q, want cand-1", capturedPreferred)
	}
	if res.URL != "https://1.1.1.1/video.mp4" {
		t.Fatalf("res.URL = %q, want https://1.1.1.1/video.mp4", res.URL)
	}
	if res.RequestHeaders["Referer"] != "https://stream.example/player" {
		t.Fatalf("res.RequestHeaders = %#v", res.RequestHeaders)
	}
	if res.CandidateID != "cand-1" {
		t.Fatalf("res.CandidateID = %q, want cand-1", res.CandidateID)
	}
	if res.ExpiresAt.IsZero() {
		t.Fatal("res.ExpiresAt is zero")
	}

	// Second resolution hits memo and must preserve RequestHeaders, CandidateID, and ExpiresAt
	memoHit, err := service.ResolveVirtualPlaybackDetailedForInstallation(
		context.Background(),
		"virtual://movie/tt1234567",
		1, "profile1", 101,
		false, false,
		nil, "",
	)
	if err != nil {
		t.Fatalf("memo hit error: %v", err)
	}
	if memoHit.URL != "https://1.1.1.1/video.mp4" {
		t.Fatalf("memoHit.URL = %q", memoHit.URL)
	}
	if memoHit.RequestHeaders["Referer"] != "https://stream.example/player" {
		t.Fatalf("memoHit.RequestHeaders = %#v, want Referer preserved", memoHit.RequestHeaders)
	}
	if memoHit.CandidateID != "cand-1" {
		t.Fatalf("memoHit.CandidateID = %q, want cand-1", memoHit.CandidateID)
	}
	if memoHit.ExpiresAt.IsZero() {
		t.Fatal("memoHit.ExpiresAt is zero")
	}
}

func TestStoreConfiguredVirtualVariantsDoesNotCacheCancellation(t *testing.T) {
	service := &Service{}
	service.storeConfiguredVirtualVariants("movie", nil, context.Canceled, time.Now())
	if _, _, ok := service.cachedConfiguredVirtualVariants("movie", time.Now()); ok {
		t.Fatal("storeConfiguredVirtualVariants cached context.Canceled error")
	}

	service.storeConfiguredVirtualVariants("movie", nil, context.DeadlineExceeded, time.Now())
	if _, _, ok := service.cachedConfiguredVirtualVariants("movie", time.Now()); ok {
		t.Fatal("storeConfiguredVirtualVariants cached context.DeadlineExceeded error")
	}

	service.storeConfiguredVirtualVariants("movie", nil, status.Error(codes.Canceled, "grpc canceled"), time.Now())
	if _, _, ok := service.cachedConfiguredVirtualVariants("movie", time.Now()); ok {
		t.Fatal("storeConfiguredVirtualVariants cached grpc codes.Canceled error")
	}

	service.storeConfiguredVirtualVariants("movie", nil, status.Error(codes.DeadlineExceeded, "grpc timeout"), time.Now())
	if _, _, ok := service.cachedConfiguredVirtualVariants("movie", time.Now()); ok {
		t.Fatal("storeConfiguredVirtualVariants cached grpc codes.DeadlineExceeded error")
	}

	service.storeConfiguredVirtualVariants("movie", nil, fmt.Errorf("outer wrap: %w", context.Canceled), time.Now())
	if _, _, ok := service.cachedConfiguredVirtualVariants("movie", time.Now()); ok {
		t.Fatal("storeConfiguredVirtualVariants cached wrapped context.Canceled error")
	}

	sentinelErr := errors.New("provider down")
	service.storeConfiguredVirtualVariants("movie", nil, sentinelErr, time.Now())
	if variants, err, ok := service.cachedConfiguredVirtualVariants("movie", time.Now()); !ok || !errors.Is(err, sentinelErr) || variants != nil {
		t.Fatalf("expected cached ordinary error %v, got variants=%v, err=%v, ok=%v", sentinelErr, variants, err, ok)
	}
}

func TestConfiguredVirtualVariantsNilContextDoesNotPanic(t *testing.T) {
	service := &Service{}
	variants, err := service.ConfiguredVirtualVariants(nil, "virtual://movie/test", "movie")
	if err != nil {
		t.Fatalf("unexpected error with nil context: %v", err)
	}
	if len(variants) != 0 {
		t.Fatalf("expected no variants without installations, got: %v", variants)
	}
}

func TestConfiguredVirtualVariantsIsolatesLeaderCancellation(t *testing.T) {
	manifest := testPluginManifest(t, "test.virtual", "1.0.0")
	manifest.Capabilities = []*pluginv1.CapabilityDescriptor{{Type: virtualStreamProviderCapabilityType, Id: testVirtualCapabilityID}}
	installPath := writeInstalledPluginManifest(t, manifest)
	store := newFakeServiceInstallationStore()
	store.listCapabilities = []*Capability{{Type: virtualStreamProviderCapabilityType, ID: testVirtualCapabilityID}}
	host := &fakeVirtualPluginHost{clients: make(map[int]pluginClient)}

	installation := &Installation{ID: 101, PluginID: "test.virtual", Version: "1.0.0", InstallPath: installPath, Enabled: true}
	store.byID[101] = installation
	store.byPluginID[installation.PluginID] = []*Installation{installation}

	blockProfiles := make(chan struct{})
	enteredProfiles := make(chan struct{})

	host.clients[101] = &fakePluginClient{
		manifest: manifest,
		virtualStreamClient: pluginhost.NewVirtualStreamProviderClientForTest(&fakeVirtualStreamGRPCClient{
			profilesFunc: func(ctx context.Context, req *pluginv1.ListVirtualStreamProfilesRequest) (*pluginv1.ListVirtualStreamProfilesResponse, error) {
				select {
				case <-enteredProfiles:
				default:
					close(enteredProfiles)
				}
				<-blockProfiles
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return &pluginv1.ListVirtualStreamProfilesResponse{
					Profiles: []*pluginv1.VirtualStreamProfile{
						{Label: "1080p", Resolution: "1080p"},
					},
				}, nil
			},
		}, time.Second),
	}

	service := &Service{installations: store, host: host}

	leaderCtx, leaderCancel := context.WithCancel(context.Background())
	type result struct {
		variants []VirtualPlaybackVariant
		err      error
	}
	leaderCh := make(chan result, 1)
	go func() {
		v, err := service.ConfiguredVirtualVariants(leaderCtx, "virtual://movie/tt100", "movie")
		leaderCh <- result{variants: v, err: err}
	}()

	<-enteredProfiles
	leaderCancel() // Cancel leader while in flight

	// Follower calls with active context
	followerCtx := context.Background()
	followerCh := make(chan result, 1)
	go func() {
		v, err := service.ConfiguredVirtualVariants(followerCtx, "virtual://movie/tt100", "movie")
		followerCh <- result{variants: v, err: err}
	}()

	// Allow profilesFunc to complete
	close(blockProfiles)

	fRes := <-followerCh
	if fRes.err != nil {
		t.Fatalf("follower failed due to leader cancellation: %v", fRes.err)
	}
	if len(fRes.variants) != 1 || fRes.variants[0].Label != "1080p" {
		t.Fatalf("follower got unexpected variants: %#v", fRes.variants)
	}
}
