package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-server/internal/pluginhost"
)

const (
	virtualStreamProviderCapabilityType = "virtual_stream_provider.v1"
	maxVirtualPlaybackStreams           = 50
	maxVirtualPlaybackResponseLen       = 1 << 20
	maxVirtualPlaybackCacheEntries      = 256
	maxVirtualPlaybackCacheBytes        = 16 << 20
	maxVirtualProfileCacheEntries       = 128
	virtualStreamsCacheTTL              = 1 * time.Minute
	virtualProfilesCacheTTL             = time.Minute
	minVirtualStreamsCacheTTL           = 10 * time.Second
	maxVirtualStreamsCacheTTL           = 7 * 24 * time.Hour
	maxVirtualIDLen                     = 256
	maxVirtualLabelLen                  = 256
	maxVirtualMetadataStringLen         = 1024
	maxVirtualMetadataLen               = 64 << 10
	maxVirtualRequestHeaders            = 8
	maxVirtualRequestHeaderKeyLen       = 128
	maxVirtualRequestHeaderValueLen     = 4096
	maxVirtualLanguages                 = 32
	maxVirtualLanguageLen               = 35
)

type virtualStreamsCacheEntry struct {
	result    *pluginv1.VirtualStreamResult
	expiresAt time.Time
	createdAt time.Time
	size      int
}

type virtualProfilesCacheEntry struct {
	response  *pluginv1.ListVirtualStreamProfilesResponse
	expiresAt time.Time
	createdAt time.Time
}

type virtualVariantsCacheEntry struct {
	variants  []VirtualPlaybackVariant
	err       error
	expiresAt time.Time
}

// VirtualPlaybackRouting makes plugin ownership and fallback an explicit
// decision. OwnerInstallationID should come from the virtual MediaFile.
// Fallback is disabled unless the caller opts in.
type VirtualPlaybackRouting struct {
	OwnerInstallationID int
	AllowFallback       bool
	AllowInsecure       bool // skip SSRF validation when plugin allows local URLs
}

// VirtualPlaybackVariant is a provider-neutral profile placeholder returned
// by a virtual playback plugin. It is safe to request during collection sync:
// no upstream streaming provider is contacted until playback.
type VirtualPlaybackVariant struct {
	VirtualURI          string `json:"virtual_uri"`
	Label               string `json:"label"`
	Resolution          string `json:"resolution,omitempty"`
	CodecVideo          string `json:"codec_video,omitempty"`
	CodecAudio          string `json:"codec_audio,omitempty"`
	HDR                 string `json:"hdr,omitempty"`
	OwnerInstallationID int    `json:"-"`
}

// VirtualPlaybackStream is a provider result exposed as a temporary, stable
// virtual file. The provider URL is deliberately not returned or persisted;
// it is resolved again when the selected file is played.
type VirtualPlaybackStream struct {
	ID                  string            `json:"id"`
	Label               string            `json:"label"`
	URI                 string            `json:"uri"`
	Resolution          string            `json:"resolution,omitempty"`
	CodecVideo          string            `json:"codec_video,omitempty"`
	CodecAudio          string            `json:"codec_audio,omitempty"`
	HDR                 string            `json:"hdr,omitempty"`
	SourceType          string            `json:"source_type,omitempty"`
	FileSize            int64             `json:"file_size,omitempty"`
	Container           string            `json:"container,omitempty"`
	Bitrate             int               `json:"bitrate,omitempty"`
	FrameRate           string            `json:"frame_rate,omitempty"`
	AudioLanguages      []string          `json:"audio_languages,omitempty"`
	SubtitleLanguages   []string          `json:"subtitle_languages,omitempty"`
	HasAtmos            bool              `json:"has_atmos,omitempty"`
	QualityScore        int               `json:"quality_score,omitempty"`
	RequestHeaders      map[string]string `json:"-"`
	ExpiresAt           time.Time         `json:"-"`
	OwnerInstallationID int               `json:"-"`
	Visible             bool              `json:"-"`
	VisibilitySpecified bool              `json:"-"`
}

// Get* accessors satisfy plugins.VirtualStreamMetadata so the shared device
// ranker can score both this type and the handler-layer candidate shape.
func (s VirtualPlaybackStream) GetCodecVideo() string { return s.CodecVideo }
func (s VirtualPlaybackStream) GetCodecAudio() string { return s.CodecAudio }
func (s VirtualPlaybackStream) GetHDR() string        { return s.HDR }
func (s VirtualPlaybackStream) GetContainer() string  { return s.Container }
func (s VirtualPlaybackStream) GetResolution() string { return s.Resolution }
func (s VirtualPlaybackStream) GetHasAtmos() bool     { return s.HasAtmos }
func (s VirtualPlaybackStream) GetQualityScore() int  { return s.QualityScore }

var ErrVirtualPlaybackResolverNotInstalled = errors.New("virtual playback resolver is not installed")

// ListVirtualPlaybackStreams is the compatibility entry point for virtual
// files that predate persisted owner IDs. New callers should use
// ListVirtualPlaybackStreamsWithRouting and pass the file owner.
func (s *Service) ListVirtualPlaybackStreams(ctx context.Context, virtualPath string) ([]VirtualPlaybackStream, error) {
	return s.ListVirtualPlaybackStreamsWithRouting(ctx, virtualPath, 0, "", VirtualPlaybackRouting{AllowFallback: true})
}

// ListVirtualPlaybackStreamsWithRouting asks the owning typed virtual stream
// provider for current candidates. Collection sync must not call this method.
func (s *Service) ListVirtualPlaybackStreamsWithRouting(
	ctx context.Context,
	virtualPath string,
	userID int,
	profileID string,
	routing VirtualPlaybackRouting,
) ([]VirtualPlaybackStream, error) {
	request, selection, err := virtualStreamRequest(virtualPath, userID, profileID)
	if err != nil {
		return nil, err
	}
	result, installationID, err := s.resolveVirtualStreamResult(ctx, request, selection, routing)
	if err != nil {
		return nil, err
	}
	return streamsFromVirtualResult(virtualPath, result, installationID), nil
}

func (s *Service) ListVirtualPlaybackStreamsForInstallation(
	ctx context.Context,
	virtualPath string,
	userID int,
	profileID string,
	ownerInstallationID int,
	allowFallback bool,
) ([]VirtualPlaybackStream, error) {
	return s.ListVirtualPlaybackStreamsWithRouting(ctx, virtualPath, userID, profileID, VirtualPlaybackRouting{
		OwnerInstallationID: ownerInstallationID,
		AllowFallback:       allowFallback,
	})
}

// ResolvedVirtualStream contains the complete outcome of resolving a virtual media stream,
// including the provider stream URL, canonical URI, request headers, candidate ID, and expiration.
type ResolvedVirtualStream struct {
	URL            string
	URI            string
	CandidateID    string
	RequestHeaders map[string]string
	ExpiresAt      time.Time
}

// ResolveVirtualPlayback is the compatibility entry point for callers that do
// not yet pass the MediaFile owner. It explicitly opts into provider fallback.
func (s *Service) ResolveVirtualPlayback(ctx context.Context, virtualPath string, userID int, profileID string) (string, error) {
	return s.ResolveVirtualPlaybackWithRouting(ctx, virtualPath, userID, profileID, VirtualPlaybackRouting{
		AllowFallback: true,
		AllowInsecure: s.InstallationAllowsInsecure(ctx, 0),
	})
}

// ResolveVirtualPlaybackWithRouting resolves the selected candidate through
// the typed SDK capability. It routes to the file owner first and only tries
// another provider when AllowFallback is true.
func (s *Service) ResolveVirtualPlaybackWithRouting(
	ctx context.Context,
	virtualPath string,
	userID int,
	profileID string,
	routing VirtualPlaybackRouting,
) (string, error) {
	return s.resolveVirtualPlaybackWithRouting(ctx, virtualPath, userID, profileID, routing, false)
}

func (s *Service) resolveVirtualPlaybackWithRouting(
	ctx context.Context,
	virtualPath string,
	userID int,
	profileID string,
	routing VirtualPlaybackRouting,
	forceRefresh bool,
) (string, error) {
	res, err := s.ResolveVirtualPlaybackDetailedWithRouting(ctx, virtualPath, userID, profileID, routing, forceRefresh, nil, "")
	if err != nil {
		return "", err
	}
	return res.URL, nil
}

func cloneHeaderMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func withVirtualResultKey(virtualPath, candID string) string {
	if candID == "" {
		return virtualPath
	}
	parsed, err := url.Parse(virtualPath)
	if err != nil {
		return virtualPath + "?result=" + candID
	}
	q := parsed.Query()
	q.Set("result", candID)
	parsed.RawQuery = q.Encode()
	return parsed.String()
}

// ResolveVirtualPlaybackDetailedWithRouting resolves a virtual stream and returns full stream details including headers and candidate identity.
func (s *Service) ResolveVirtualPlaybackDetailedWithRouting(
	ctx context.Context,
	virtualPath string,
	userID int,
	profileID string,
	routing VirtualPlaybackRouting,
	forceRefresh bool,
	excludedCandidateIDs []string,
	preferredCandidateID string,
) (ResolvedVirtualStream, error) {
	// Short-lived memo: the same virtual URI is resolved once for probing and
	// again when the transport opens. Skip the provider RPC on the second call
	// within a few seconds — provider URLs rotate, so this is never long-lived.
	if !forceRefresh {
		if memo, ok := s.lookupResolvedStream(virtualPath, userID, profileID, routing.OwnerInstallationID); ok && memo.URL != "" {
			// Do not synchronously fetch the memoized URL here. The relay
			// revalidates and fetches it when playback opens, so a slow provider
			// cannot consume the startup budget before a session is returned.
			return memo, nil
		}
	}
	request, selection, err := virtualStreamRequestWithRefresh(virtualPath, userID, profileID, forceRefresh, excludedCandidateIDs, preferredCandidateID)
	if err != nil {
		return ResolvedVirtualStream{}, err
	}
	result, _, err := s.resolveVirtualStreamResult(ctx, request, selection, routing)
	if err != nil {
		return ResolvedVirtualStream{}, err
	}
	candidatesToTry := result.GetCandidates()
	if selection.resultID != "" || selection.profile != "" {
		filtered := filterCandidatesBySelection(candidatesToTry, selection)
		if len(filtered) > 0 {
			candidatesToTry = filtered
		}
	}

	var lastValidateErr error
	for _, candidate := range candidatesToTry {
		if candidate == nil {
			continue
		}
		raw := candidate.GetTemporaryUri()
		var validated string
		var validateErr error
		if routing.AllowInsecure {
			validated, validateErr = validateProviderStreamURLSyntax(raw)
		} else {
			validated, validateErr = validateProviderStreamURL(ctx, raw)
		}
		if validateErr != nil {
			lastValidateErr = validateErr
			continue
		}
		var exp time.Time
		if candidate.GetExpiresAt() != nil && candidate.GetExpiresAt().IsValid() {
			exp = candidate.GetExpiresAt().AsTime()
		}
		candID := candidate.GetCandidateId()
		resURI := withVirtualResultKey(virtualPath, candID)
		stream := ResolvedVirtualStream{
			URL:            validated,
			URI:            resURI,
			CandidateID:    candID,
			RequestHeaders: cloneHeaderMap(candidate.GetRequestHeaders()),
			ExpiresAt:      exp,
		}
		s.storeResolvedStream(virtualPath, userID, profileID, routing.OwnerInstallationID, stream)
		return stream, nil
	}

	// A pinned candidate that passed URL validation is returned immediately;
	// relay/FFmpeg owns first-byte failure and bounded stream failover.
	if selection.resultID != "" && selection.profile == "" && len(result.GetCandidates()) > 1 {
		for _, candidate := range result.GetCandidates() {
			if candidate == nil {
				continue
			}
			raw := candidate.GetTemporaryUri()
			var validated string
			var validateErr error
			if routing.AllowInsecure {
				validated, validateErr = validateProviderStreamURLSyntax(raw)
			} else {
				validated, validateErr = validateProviderStreamURL(ctx, raw)
			}
			if validateErr != nil {
				continue
			}
			var exp time.Time
			if candidate.GetExpiresAt() != nil && candidate.GetExpiresAt().IsValid() {
				exp = candidate.GetExpiresAt().AsTime()
			}
			candID := candidate.GetCandidateId()
			resURI := withVirtualResultKey(virtualPath, candID)
			stream := ResolvedVirtualStream{
				URL:            validated,
				URI:            resURI,
				CandidateID:    candID,
				RequestHeaders: cloneHeaderMap(candidate.GetRequestHeaders()),
				ExpiresAt:      exp,
			}
			s.storeResolvedStream(virtualPath, userID, profileID, routing.OwnerInstallationID, stream)
			return stream, nil
		}
	}

	if lastValidateErr != nil {
		return ResolvedVirtualStream{}, fmt.Errorf("virtual playback provider returned an unsafe stream URL: %w", lastValidateErr)
	}
	return ResolvedVirtualStream{}, fmt.Errorf("virtual playback provider returned no usable stream (%d candidates)", len(result.GetCandidates()))
}

func (s *Service) ResolveVirtualPlaybackForInstallation(
	ctx context.Context,
	virtualPath string,
	userID int,
	profileID string,
	ownerInstallationID int,
	allowFallback bool,
) (string, error) {
	return s.ResolveVirtualPlaybackWithRouting(ctx, virtualPath, userID, profileID, VirtualPlaybackRouting{
		OwnerInstallationID: ownerInstallationID,
		AllowFallback:       allowFallback,
		AllowInsecure:       s.InstallationAllowsInsecure(ctx, ownerInstallationID),
	})
}

func (s *Service) RefreshVirtualPlaybackForInstallation(
	ctx context.Context,
	virtualPath string,
	userID int,
	profileID string,
	ownerInstallationID int,
	allowFallback bool,
) (string, error) {
	return s.resolveVirtualPlaybackWithRouting(ctx, virtualPath, userID, profileID, VirtualPlaybackRouting{
		OwnerInstallationID: ownerInstallationID,
		AllowFallback:       allowFallback,
		AllowInsecure:       s.InstallationAllowsInsecure(ctx, ownerInstallationID),
	}, true)
}

func (s *Service) ResolveVirtualPlaybackDetailedForInstallation(
	ctx context.Context,
	virtualPath string,
	userID int,
	profileID string,
	ownerInstallationID int,
	allowFallback bool,
	forceRefresh bool,
	excludedCandidateIDs []string,
	preferredCandidateID string,
) (ResolvedVirtualStream, error) {
	return s.ResolveVirtualPlaybackDetailedWithRouting(ctx, virtualPath, userID, profileID, VirtualPlaybackRouting{
		OwnerInstallationID: ownerInstallationID,
		AllowFallback:       allowFallback,
		AllowInsecure:       s.InstallationAllowsInsecure(ctx, ownerInstallationID),
	}, forceRefresh, excludedCandidateIDs, preferredCandidateID)
}

type virtualStreamSelection struct {
	resultID     string
	profile      string
	forceRefresh bool
}

func virtualStreamRequest(virtualPath string, userID int, profileID string) (*pluginv1.ResolveVirtualStreamRequest, virtualStreamSelection, error) {
	return virtualStreamRequestWithRefresh(virtualPath, userID, profileID, false, nil, "")
}

func virtualStreamRequestWithRefresh(
	virtualPath string,
	userID int,
	profileID string,
	forceRefresh bool,
	excludedCandidateIDs []string,
	preferredCandidateID string,
) (*pluginv1.ResolveVirtualStreamRequest, virtualStreamSelection, error) {
	raw := strings.TrimSpace(virtualPath)
	if raw == "" || len(raw) > maxVirtualMetadataStringLen {
		return nil, virtualStreamSelection{}, errors.New("virtual media URI is empty or too long")
	}
	if !strings.Contains(raw, "://") {
		raw = "virtual://" + strings.TrimPrefix(raw, "/")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "virtual" || parsed.Host == "" {
		return nil, virtualStreamSelection{}, errors.New("invalid virtual media URI")
	}
	if parsed.User != nil || parsed.Fragment != "" {
		return nil, virtualStreamSelection{}, errors.New("invalid virtual media URI")
	}
	mediaType := strings.ToLower(parsed.Host)
	segments := splitVirtualPath(parsed.EscapedPath())
	if len(segments) == 0 {
		return nil, virtualStreamSelection{}, errors.New("virtual media URI has no external ID")
	}

	externalIDs := make(map[string]string, 1)
	season, episode := int32(0), int32(0)
	switch mediaType {
	case "movie":
		idIndex := 0
		if len(segments) == 2 && (segments[0] == "tmdb" || segments[0] == "imdb") {
			putVirtualExternalID(externalIDs, segments[0]+":"+segments[1], "tmdb")
			idIndex = 1
		}
		if len(segments)-idIndex != 1 {
			return nil, virtualStreamSelection{}, errors.New("movie virtual URI has an invalid path")
		}
		if idIndex == 0 {
			putVirtualExternalID(externalIDs, segments[0], "tmdb")
		}
	case "series", "episode":
		mediaType = "episode"
		idIndex := 0
		if len(segments) == 4 && (segments[0] == "tvdb" || segments[0] == "tmdb" || segments[0] == "imdb") {
			putVirtualExternalID(externalIDs, segments[0]+":"+segments[1], "tvdb")
			idIndex = 1
		}
		if len(segments)-idIndex != 3 {
			return nil, virtualStreamSelection{}, errors.New("episode virtual URI must include an external ID, season, and episode")
		}
		if idIndex == 0 {
			putVirtualExternalID(externalIDs, segments[0], "tvdb")
		}
		season64, seasonErr := strconv.ParseInt(segments[idIndex+1], 10, 32)
		episode64, episodeErr := strconv.ParseInt(segments[idIndex+2], 10, 32)
		if seasonErr != nil || episodeErr != nil || season64 <= 0 || episode64 <= 0 {
			return nil, virtualStreamSelection{}, errors.New("episode virtual URI has invalid season or episode")
		}
		season, episode = int32(season64), int32(episode64)
	default:
		return nil, virtualStreamSelection{}, fmt.Errorf("unsupported virtual media type %q", mediaType)
	}
	if len(externalIDs) == 0 {
		return nil, virtualStreamSelection{}, errors.New("virtual media URI has an invalid external ID")
	}

	query := parsed.Query()
	selection := virtualStreamSelection{
		resultID:     boundedQueryValue(query.Get("result")),
		profile:      boundedQueryValue(query.Get("profile")),
		forceRefresh: forceRefresh,
	}
	query.Del("result")
	parsed.RawQuery = query.Encode()
	requestMetadata := map[string]any{
		"virtual_uri": parsed.String(),
		"user_id":     userID,
		"profile_id":  boundedQueryValue(profileID),
	}
	if forceRefresh {
		requestMetadata["force_refresh"] = true
	}
	metadata, err := structpb.NewStruct(requestMetadata)
	if err != nil {
		return nil, virtualStreamSelection{}, fmt.Errorf("build virtual stream request metadata: %w", err)
	}
	return &pluginv1.ResolveVirtualStreamRequest{
		MediaType:            mediaType,
		ExternalIds:          externalIDs,
		SeasonNumber:         season,
		EpisodeNumber:        episode,
		Metadata:             metadata,
		ExcludedCandidateIds: excludedCandidateIDs,
		PreferredCandidateId: preferredCandidateID,
	}, selection, nil
}

func splitVirtualPath(escapedPath string) []string {
	raw := strings.Trim(escapedPath, "/")
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, "/")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		decoded, err := url.PathUnescape(part)
		if err != nil || decoded == "" || decoded == "." || decoded == ".." ||
			strings.ContainsAny(decoded, "/\\\x00\r\n\t") {
			return nil
		}
		result = append(result, strings.ToLower(decoded))
	}
	return result
}

func putVirtualExternalID(ids map[string]string, raw, defaultProvider string) {
	raw = strings.TrimSpace(strings.ToLower(raw))
	provider, value, found := strings.Cut(raw, ":")
	if !found {
		provider, value = defaultProvider, raw
		if strings.HasPrefix(raw, "tt") {
			provider = "imdb"
		}
	}
	if (provider != "imdb" && provider != "tvdb" && provider != "tmdb") ||
		value == "" || len(value) > maxVirtualIDLen {
		return
	}
	if provider == "imdb" && !strings.HasPrefix(value, "tt") {
		return
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (provider != "imdb" || r != 't') {
			return
		}
	}
	ids[provider] = value
}

func boundedQueryValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > maxVirtualLabelLen {
		return ""
	}
	return value
}

func (s *Service) resolveVirtualStreamResult(
	ctx context.Context,
	request *pluginv1.ResolveVirtualStreamRequest,
	selection virtualStreamSelection,
	routing VirtualPlaybackRouting,
) (*pluginv1.VirtualStreamResult, int, error) {
	if s == nil || s.installations == nil {
		return nil, 0, ErrVirtualPlaybackResolverNotInstalled
	}
	providers, err := s.virtualStreamProviders(ctx, routing)
	if err != nil {
		return nil, 0, err
	}
	var providerErr error
	var firstCandidateResult *pluginv1.VirtualStreamResult
	var firstCandidateInstallationID int

	for _, provider := range providers {
		result, err := s.resolveVirtualStreamProvider(ctx, provider.installationID, provider.capabilityID, request)
		if err != nil {
			providerErr = errors.Join(providerErr, err)
			continue
		}
		if len(result.GetCandidates()) == 0 {
			providerErr = errors.Join(providerErr, errors.New("no streams available from provider"))
			continue
		}
		if firstCandidateResult == nil {
			firstCandidateResult = result
			firstCandidateInstallationID = provider.installationID
		}
		if selectVirtualCandidate(result.GetCandidates(), selection) != nil {
			return result, provider.installationID, nil
		}
		providerErr = errors.Join(providerErr, errors.New("virtual stream provider returned no matching candidate"))
		if selection.resultID != "" {
			break
		}
	}

	if firstCandidateResult != nil && selection.resultID == "" {
		return firstCandidateResult, firstCandidateInstallationID, nil
	}

	if providerErr != nil {
		return nil, 0, fmt.Errorf("resolve virtual playback: %w", providerErr)
	}
	return nil, 0, ErrVirtualPlaybackResolverNotInstalled
}

type virtualStreamProviderRoute struct {
	installationID int
	capabilityID   string
}

func (s *Service) virtualStreamProviders(ctx context.Context, routing VirtualPlaybackRouting) ([]virtualStreamProviderRoute, error) {
	var routes []virtualStreamProviderRoute
	seen := make(map[int]struct{})
	if routing.OwnerInstallationID > 0 {
		capabilityID, err := s.virtualStreamCapability(ctx, routing.OwnerInstallationID)
		if err == nil {
			routes = append(routes, virtualStreamProviderRoute{installationID: routing.OwnerInstallationID, capabilityID: capabilityID})
			seen[routing.OwnerInstallationID] = struct{}{}
		} else if !routing.AllowFallback {
			return nil, fmt.Errorf("load owning virtual stream provider: %w", err)
		}
	}
	// When an owner is explicitly specified and fallback is disabled, use only the owner.
	// Otherwise (including when no owner is specified), query all enabled providers so
	// virtual playback always plays without requiring ownership.
	if routing.OwnerInstallationID > 0 && !routing.AllowFallback {
		return routes, nil
	}
	installations, err := s.installations.ListEnabled(ctx)
	if err != nil {
		if len(routes) > 0 {
			return routes, nil
		}
		return nil, fmt.Errorf("list enabled virtual stream providers: %w", err)
	}
	for _, installation := range installations {
		if installation == nil {
			continue
		}
		if _, ok := seen[installation.ID]; ok {
			continue
		}
		capabilityID, err := s.virtualStreamCapability(ctx, installation.ID)
		if err != nil {
			continue
		}
		routes = append(routes, virtualStreamProviderRoute{installationID: installation.ID, capabilityID: capabilityID})
	}
	if len(routes) == 0 {
		return nil, ErrVirtualPlaybackResolverNotInstalled
	}
	return routes, nil
}

func (s *Service) virtualStreamCapability(ctx context.Context, installationID int) (string, error) {
	if _, err := s.loadInstallation(ctx, installationID, true); err != nil {
		return "", err
	}
	capabilities, err := s.cachedCapabilities(ctx, installationID)
	if err != nil {
		return "", err
	}
	for _, capability := range capabilities {
		if capability != nil && capability.Type == virtualStreamProviderCapabilityType && strings.TrimSpace(capability.ID) != "" {
			return capability.ID, nil
		}
	}
	return "", ErrVirtualPlaybackResolverNotInstalled
}

func (s *Service) resolveVirtualStreamProvider(
	ctx context.Context,
	installationID int,
	capabilityID string,
	request *pluginv1.ResolveVirtualStreamRequest,
) (*pluginv1.VirtualStreamResult, error) {
	callRequest := proto.CloneOf(request)
	if callRequest == nil {
		return nil, errors.New("virtual stream request is empty")
	}
	callRequest.CapabilityId = capabilityID
	forceRefresh := virtualStreamForceRefresh(callRequest)
	cacheKey, err := virtualStreamCacheKey(installationID, callRequest)
	if err != nil {
		return nil, err
	}
	if !forceRefresh {
		if cached := s.cachedVirtualStreamResult(cacheKey, time.Now()); cached != nil {
			return cached, nil
		}
	}
	flightKey := "virtual-stream:" + cacheKey
	if forceRefresh {
		flightKey += ":refresh"
	}
	value, err, _ := s.launchGroup.Do(flightKey, func() (any, error) {
		if !forceRefresh {
			if cached := s.cachedVirtualStreamResult(cacheKey, time.Now()); cached != nil {
				return cached, nil
			}
		}
		client, err := s.VirtualStreamProviderClient(ctx, installationID, capabilityID)
		if err != nil {
			return nil, err
		}
		response, err := client.ResolveVirtualStream(ctx, callRequest)
		if err != nil {
			// Plugin errors are untrusted and may embed the provider request or
			// tokenized response URL. Keep the public error provider-neutral.
			return nil, fmt.Errorf("virtual stream provider %d request failed", installationID)
		}
		result, err := validateVirtualStreamResponse(response)
		if err != nil {
			return nil, err
		}
		s.storeVirtualStreamResult(cacheKey, result, time.Now())
		return result, nil
	})
	if err != nil {
		return nil, err
	}
	result, ok := value.(*pluginv1.VirtualStreamResult)
	if !ok || result == nil {
		return nil, errors.New("virtual stream provider returned an invalid result")
	}
	return proto.CloneOf(result), nil
}

func virtualStreamForceRefresh(request *pluginv1.ResolveVirtualStreamRequest) bool {
	if request == nil || request.GetMetadata() == nil {
		return false
	}
	value, ok := request.GetMetadata().GetFields()["force_refresh"]
	return ok && value.GetBoolValue()
}

func requestForVirtualStreamCache(request *pluginv1.ResolveVirtualStreamRequest) *pluginv1.ResolveVirtualStreamRequest {
	cacheRequest := proto.CloneOf(request)
	if cacheRequest.Metadata == nil {
		return cacheRequest
	}
	delete(cacheRequest.Metadata.Fields, "force_refresh")
	return cacheRequest
}

func virtualStreamCacheKey(installationID int, request *pluginv1.ResolveVirtualStreamRequest) (string, error) {
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(requestForVirtualStreamCache(request))
	if err != nil {
		return "", fmt.Errorf("marshal virtual stream cache key: %w", err)
	}
	sum := sha256.Sum256(data)
	return strconv.Itoa(installationID) + ":" + hex.EncodeToString(sum[:]), nil
}

func (s *Service) cachedVirtualStreamResult(key string, now time.Time) *pluginv1.VirtualStreamResult {
	s.virtualStreamsMu.Lock()
	defer s.virtualStreamsMu.Unlock()
	s.evictExpiredVirtualStreamsLocked(now)
	entry, ok := s.virtualStreamsCache[key]
	if !ok || !now.Before(entry.expiresAt) {
		return nil
	}
	return proto.CloneOf(entry.result)
}

func virtualStreamCacheTTL(result *pluginv1.VirtualStreamResult) time.Duration {
	if result == nil || result.GetMetadata() == nil {
		return virtualStreamsCacheTTL
	}
	value, ok := result.GetMetadata().GetFields()["cache_ttl_seconds"]
	if !ok {
		return virtualStreamsCacheTTL
	}
	seconds := value.GetNumberValue()
	switch {
	case seconds < minVirtualStreamsCacheTTL.Seconds():
		return minVirtualStreamsCacheTTL
	case seconds > maxVirtualStreamsCacheTTL.Seconds():
		return maxVirtualStreamsCacheTTL
	default:
		return time.Duration(seconds) * time.Second
	}
}

func (s *Service) storeVirtualStreamResult(key string, result *pluginv1.VirtualStreamResult, now time.Time) {
	resultSize := proto.Size(result)
	if resultSize <= 0 || resultSize > maxVirtualPlaybackCacheBytes {
		return
	}
	expiresAt := now.Add(virtualStreamCacheTTL(result))
	for _, candidate := range result.GetCandidates() {
		if candidate.GetExpiresAt() != nil && candidate.GetExpiresAt().IsValid() {
			candidateExpiry := candidate.GetExpiresAt().AsTime()
			if candidateExpiry.Before(expiresAt) {
				expiresAt = candidateExpiry
			}
		}
	}
	if !expiresAt.After(now) {
		return
	}
	s.virtualStreamsMu.Lock()
	defer s.virtualStreamsMu.Unlock()
	s.evictExpiredVirtualStreamsLocked(now)
	if s.virtualStreamsCache == nil {
		s.virtualStreamsCache = make(map[string]virtualStreamsCacheEntry)
	}
	for len(s.virtualStreamsCache) >= maxVirtualPlaybackCacheEntries ||
		virtualStreamsCacheSize(s.virtualStreamsCache)+resultSize > maxVirtualPlaybackCacheBytes {
		var oldestKey string
		var oldestAt time.Time
		for candidateKey, entry := range s.virtualStreamsCache {
			if oldestKey == "" || entry.createdAt.Before(oldestAt) {
				oldestKey, oldestAt = candidateKey, entry.createdAt
			}
		}
		delete(s.virtualStreamsCache, oldestKey)
	}
	s.virtualStreamsCache[key] = virtualStreamsCacheEntry{
		result:    proto.CloneOf(result),
		expiresAt: expiresAt,
		createdAt: now,
		size:      resultSize,
	}
}

func virtualStreamsCacheSize(entries map[string]virtualStreamsCacheEntry) int {
	total := 0
	for _, entry := range entries {
		total += entry.size
	}
	return total
}

func (s *Service) evictExpiredVirtualStreamsLocked(now time.Time) {
	for key, entry := range s.virtualStreamsCache {
		if !now.Before(entry.expiresAt) {
			delete(s.virtualStreamsCache, key)
		}
	}
}

func validateVirtualStreamResponse(response *pluginv1.ResolveVirtualStreamResponse) (*pluginv1.VirtualStreamResult, error) {
	if response == nil || response.GetResult() == nil {
		return nil, errors.New("virtual stream provider returned an empty response")
	}
	if proto.Size(response) > maxVirtualPlaybackResponseLen {
		return nil, errors.New("virtual stream provider response exceeded size limit")
	}
	result := proto.CloneOf(response.GetResult())
	if len(result.GetProviderId()) > maxVirtualIDLen || len(result.GetResultId()) > maxVirtualIDLen {
		return nil, errors.New("virtual stream provider returned an oversized identifier")
	}
	if err := validateVirtualStatus(result.GetAvailability(), result.GetError(), result.GetMetadata()); err != nil {
		return nil, err
	}
	if err := validateVirtualResultCacheMetadata(result.GetMetadata()); err != nil {
		return nil, err
	}
	if len(result.GetCandidates()) > maxVirtualPlaybackStreams {
		return nil, fmt.Errorf("virtual stream provider returned %d candidates; maximum is %d", len(result.GetCandidates()), maxVirtualPlaybackStreams)
	}
	now := time.Now()
	seen := make(map[string]struct{}, len(result.GetCandidates()))
	active := result.Candidates[:0]
	for idx, candidate := range result.GetCandidates() {
		if candidate != nil && candidate.GetExpiresAt() != nil {
			if !candidate.GetExpiresAt().IsValid() {
				return nil, errors.New("virtual stream provider returned an invalid candidate expiry")
			}
			if !candidate.GetExpiresAt().AsTime().After(now) {
				continue
			}
		}
		if candidate != nil {
			rawID := strings.TrimSpace(candidate.GetCandidateId())
			if rawID != "" {
				if _, dup := seen[rawID]; dup {
					candidate.CandidateId = fmt.Sprintf("%s-%d", rawID, idx)
				}
			}
		}
		if err := validateVirtualStreamCandidate(candidate, seen); err != nil {
			return nil, err
		}
		active = append(active, candidate)
	}
	result.Candidates = active
	sort.SliceStable(result.Candidates, func(i, j int) bool {
		left, right := result.Candidates[i].GetRank(), result.Candidates[j].GetRank()
		if left <= 0 {
			left = math.MaxInt32
		}
		if right <= 0 {
			right = math.MaxInt32
		}
		return left < right
	})
	return result, nil
}

func validateVirtualStreamCandidate(candidate *pluginv1.VirtualStreamCandidate, seen map[string]struct{}) error {
	if candidate == nil {
		return errors.New("virtual stream provider returned a nil candidate")
	}
	id := strings.TrimSpace(candidate.GetCandidateId())
	if id == "" || len(id) > maxVirtualIDLen || strings.ContainsAny(id, "\x00\r\n\t") {
		return errors.New("virtual stream provider returned an invalid candidate ID")
	}
	if _, duplicate := seen[id]; duplicate {
		return errors.New("virtual stream provider returned duplicate candidate IDs")
	}
	seen[id] = struct{}{}
	for _, value := range []string{
		candidate.GetProviderId(), candidate.GetVideoCodec(), candidate.GetAudioCodec(),
		candidate.GetContainer(), candidate.GetResolution().GetLabel(),
		candidate.GetHdr().GetFormat(), candidate.GetHdr().GetDolbyVisionProfile(),
	} {
		if len(value) > maxVirtualLabelLen || strings.ContainsAny(value, "\x00\r\n") {
			return errors.New("virtual stream provider returned an oversized or invalid field")
		}
	}
	if candidate.GetBitrate() < 0 || candidate.GetFileSizeBytes() < 0 ||
		candidate.GetResolution().GetWidth() < 0 || candidate.GetResolution().GetHeight() < 0 ||
		candidate.GetResolution().GetWidth() > 32768 || candidate.GetResolution().GetHeight() > 32768 ||
		candidate.GetResolution().GetFrameRate() < 0 || candidate.GetResolution().GetFrameRate() > 1000 {
		return errors.New("virtual stream provider returned invalid media metadata")
	}
	if err := validateVirtualStatus(candidate.GetAvailability(), candidate.GetError(), candidate.GetMetadata()); err != nil {
		return err
	}
	if err := validateVirtualRequestHeaders(candidate.GetRequestHeaders()); err != nil {
		return err
	}
	if err := validateVirtualCandidateDisplayMetadata(candidate.GetMetadata()); err != nil {
		return err
	}
	if err := validateLanguages(candidate.GetAudioLanguages()); err != nil {
		return err
	}
	if err := validateLanguages(candidate.GetSubtitleLanguages()); err != nil {
		return err
	}
	return nil
}

func validateVirtualRequestHeaders(headers map[string]string) error {
	if len(headers) > maxVirtualRequestHeaders {
		return errors.New("virtual stream provider returned too many request headers")
	}
	normalized := make(map[string]struct{}, len(headers))
	for key, value := range headers {
		if len(key) == 0 || len(key) > maxVirtualRequestHeaderKeyLen || strings.ContainsAny(key, "\x00\r\n\t") {
			return errors.New("virtual stream provider returned an invalid request header name")
		}
		canonical := ""
		switch {
		case strings.EqualFold(key, "Referer"):
			canonical = "referer"
		case strings.EqualFold(key, "Origin"):
			canonical = "origin"
		case strings.EqualFold(key, "User-Agent"):
			canonical = "user-agent"
		default:
			return errors.New("virtual stream provider returned a disallowed request header")
		}
		if _, duplicate := normalized[canonical]; duplicate {
			return errors.New("virtual stream provider returned duplicate request headers")
		}
		normalized[canonical] = struct{}{}
		if len(value) > maxVirtualRequestHeaderValueLen || strings.ContainsAny(value, "\x00\r\n") {
			return errors.New("virtual stream provider returned an invalid request header value")
		}
	}
	return nil
}

func validateVirtualCandidateDisplayMetadata(metadata *structpb.Struct) error {
	if metadata == nil {
		return nil
	}
	for _, key := range []string{"display_name", "source_type", "visible"} {
		value, ok := metadata.GetFields()[key]
		if !ok {
			continue
		}
		if key == "visible" {
			if _, ok := value.GetKind().(*structpb.Value_BoolValue); !ok {
				return fmt.Errorf("virtual stream provider metadata %q must be a boolean", key)
			}
			continue
		}
		if _, ok := value.GetKind().(*structpb.Value_StringValue); !ok {
			return fmt.Errorf("virtual stream provider metadata %q must be a string", key)
		}
		text := strings.TrimSpace(value.GetStringValue())
		if len(text) > maxVirtualLabelLen || strings.IndexFunc(text, func(char rune) bool {
			return unicode.IsControl(char) || unicode.Is(unicode.Cf, char)
		}) >= 0 {
			return fmt.Errorf("virtual stream provider metadata %q is invalid", key)
		}
	}
	return nil
}

func validateVirtualResultCacheMetadata(metadata *structpb.Struct) error {
	if metadata == nil {
		return nil
	}
	value, ok := metadata.GetFields()["cache_ttl_seconds"]
	if !ok {
		return nil
	}
	if _, ok := value.GetKind().(*structpb.Value_NumberValue); !ok {
		return errors.New("virtual stream provider metadata \"cache_ttl_seconds\" must be a number")
	}
	seconds := value.GetNumberValue()
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || math.Trunc(seconds) != seconds {
		return errors.New("virtual stream provider metadata \"cache_ttl_seconds\" must be a finite integer")
	}
	return nil
}

func validateLanguages(values []string) error {
	if len(values) > maxVirtualLanguages {
		return errors.New("virtual stream provider returned too many language tags")
	}
	for _, value := range values {
		if len(value) > maxVirtualLanguageLen || strings.ContainsAny(value, "\x00\r\n\t") {
			return errors.New("virtual stream provider returned an invalid language tag")
		}
	}
	return nil
}

func validateVirtualStatus(
	availability *pluginv1.VirtualStreamAvailability,
	streamError *pluginv1.VirtualStreamError,
	metadata *structpb.Struct,
) error {
	if availability != nil {
		if len(availability.GetMessage()) > maxVirtualMetadataStringLen ||
			availability.GetProgressPercent() < 0 || availability.GetProgressPercent() > 100 {
			return errors.New("virtual stream provider returned invalid availability metadata")
		}
		if availability.GetEstimatedReadyAt() != nil && !availability.GetEstimatedReadyAt().IsValid() {
			return errors.New("virtual stream provider returned an invalid availability timestamp")
		}
	}
	if streamError != nil {
		if len(streamError.GetMessage()) > maxVirtualMetadataStringLen {
			return errors.New("virtual stream provider returned an oversized error message")
		}
		if streamError.GetRetryAfter() != nil && streamError.GetRetryAfter().CheckValid() != nil {
			return errors.New("virtual stream provider returned an invalid retry delay")
		}
	}
	if metadata != nil && proto.Size(metadata) > maxVirtualMetadataLen {
		return errors.New("virtual stream provider returned oversized metadata")
	}
	return nil
}

func candidateMatchesProfile(candidate *pluginv1.VirtualStreamCandidate, profile string) bool {
	if candidate == nil || profile == "" {
		return false
	}
	profLower := strings.ToLower(strings.TrimSpace(profile))
	if profLower == "" {
		return false
	}
	resLabel := candidate.GetResolution().GetLabel()
	if strings.EqualFold(resLabel, profile) {
		return true
	}
	resLower := strings.ToLower(strings.TrimSpace(resLabel))

	candTier := normalizeVirtualResolutionTier(resLower)
	profTier := normalizeVirtualResolutionTier(profLower)
	if candTier != "" && profTier != "" && candTier == profTier {
		return true
	}
	if resLower != "" && (strings.Contains(resLower, profLower) || strings.Contains(profLower, resLower)) {
		return true
	}
	if candidate.GetMetadata() != nil {
		for _, k := range []string{"profile", "quality", "label", "display_name"} {
			if f, ok := candidate.GetMetadata().GetFields()[k]; ok {
				val := strings.ToLower(strings.TrimSpace(f.GetStringValue()))
				if val == profLower || (val != "" && (strings.Contains(val, profLower) || strings.Contains(profLower, val))) {
					return true
				}
			}
		}
	}
	return false
}

func normalizeVirtualResolutionTier(val string) string {
	val = strings.ToLower(strings.TrimSpace(val))
	switch {
	case strings.Contains(val, "2160") || strings.Contains(val, "4k") || strings.Contains(val, "uhd"):
		return "2160p"
	case strings.Contains(val, "1080") || strings.Contains(val, "fhd"):
		return "1080p"
	case strings.Contains(val, "720") || containsExactVirtualWord(val, "hd"):
		return "720p"
	case strings.Contains(val, "480") || strings.Contains(val, "576") || containsExactVirtualWord(val, "sd"):
		return "480p"
	default:
		return ""
	}
}

func containsExactVirtualWord(s, word string) bool {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	for _, f := range fields {
		if strings.EqualFold(f, word) {
			return true
		}
	}
	return false
}

func isCandidateHDR(candidate *pluginv1.VirtualStreamCandidate) bool {
	if candidate == nil {
		return false
	}
	if hdr := candidate.GetHdr(); hdr != nil {
		if hdr.GetIsHdr() || hdr.GetHasDolbyVision() || strings.TrimSpace(hdr.GetFormat()) != "" || strings.TrimSpace(hdr.GetDolbyVisionProfile()) != "" {
			return true
		}
	}
	if candidate.GetMetadata() != nil {
		for _, k := range []string{"hdr", "hdr_format", "format"} {
			if f, ok := candidate.GetMetadata().GetFields()[k]; ok {
				val := strings.ToLower(strings.TrimSpace(f.GetStringValue()))
				if val != "" && val != "sdr" && val != "none" && val != "false" {
					return true
				}
			}
		}
	}
	return false
}

func isCandidateDolbyVision(candidate *pluginv1.VirtualStreamCandidate) bool {
	if candidate == nil {
		return false
	}
	if hdr := candidate.GetHdr(); hdr != nil {
		if hdr.GetHasDolbyVision() || strings.TrimSpace(hdr.GetDolbyVisionProfile()) != "" {
			return true
		}
		fmtLower := strings.ToLower(strings.TrimSpace(hdr.GetFormat()))
		if fmtLower == "dv" || strings.Contains(fmtLower, "dolby") || strings.Contains(fmtLower, "dovi") {
			return true
		}
	}
	if candidate.GetMetadata() != nil {
		for _, k := range []string{"hdr", "hdr_format", "format"} {
			if f, ok := candidate.GetMetadata().GetFields()[k]; ok {
				val := strings.ToLower(strings.TrimSpace(f.GetStringValue()))
				if val == "dv" || strings.Contains(val, "dolby") || strings.Contains(val, "dovi") {
					return true
				}
			}
		}
	}
	return false
}

func filterCandidatesByHDR(candidates []*pluginv1.VirtualStreamCandidate, profile string) []*pluginv1.VirtualStreamCandidate {
	profLower := strings.ToLower(strings.TrimSpace(profile))
	wantsDV := strings.Contains(profLower, "dolby") || strings.Contains(profLower, "vision") || containsExactVirtualWord(profLower, "dv") || containsExactVirtualWord(profLower, "dovi")
	wantsSDR := containsExactVirtualWord(profLower, "sdr")
	wantsHDR := strings.Contains(profLower, "hdr") || wantsDV

	if wantsDV {
		var dv []*pluginv1.VirtualStreamCandidate
		for _, c := range candidates {
			if isCandidateDolbyVision(c) {
				dv = append(dv, c)
			}
		}
		if len(dv) > 0 {
			return dv
		}
	}

	if wantsHDR {
		var hdr []*pluginv1.VirtualStreamCandidate
		for _, c := range candidates {
			if isCandidateHDR(c) {
				hdr = append(hdr, c)
			}
		}
		if len(hdr) > 0 {
			return hdr
		}
	} else if wantsSDR {
		var sdr []*pluginv1.VirtualStreamCandidate
		for _, c := range candidates {
			if !isCandidateHDR(c) {
				sdr = append(sdr, c)
			}
		}
		if len(sdr) > 0 {
			return sdr
		}
	}

	return nil
}

func filterCandidatesBySelection(candidates []*pluginv1.VirtualStreamCandidate, selection virtualStreamSelection) []*pluginv1.VirtualStreamCandidate {
	if selection.resultID != "" {
		for _, candidate := range candidates {
			if candidate.GetCandidateId() == selection.resultID {
				return []*pluginv1.VirtualStreamCandidate{candidate}
			}
		}
		if !selection.forceRefresh && selection.profile == "" && len(candidates) > 0 {
			return []*pluginv1.VirtualStreamCandidate{candidates[0]}
		}
		return nil
	}
	if selection.profile != "" {
		var matched []*pluginv1.VirtualStreamCandidate
		for _, candidate := range candidates {
			if candidateMatchesProfile(candidate, selection.profile) {
				matched = append(matched, candidate)
			}
		}
		if len(matched) > 1 {
			if refined := filterCandidatesByHDR(matched, selection.profile); len(refined) > 0 {
				return refined
			}
		}
		return matched
	}
	return candidates
}

func selectVirtualCandidate(candidates []*pluginv1.VirtualStreamCandidate, selection virtualStreamSelection) *pluginv1.VirtualStreamCandidate {
	filtered := filterCandidatesBySelection(candidates, selection)
	if len(filtered) > 0 {
		return filtered[0]
	}
	return nil
}

func streamsFromVirtualResult(virtualPath string, result *pluginv1.VirtualStreamResult, installationID int) []VirtualPlaybackStream {
	streams := make([]VirtualPlaybackStream, 0, len(result.GetCandidates()))
	for _, candidate := range result.GetCandidates() {
		visible := true
		if value, ok := virtualCandidateMetadataBool(candidate, "visible"); ok {
			visible = value
		}
		bitrate := int(candidate.GetBitrate())
		if candidate.GetBitrate() > int64(math.MaxInt) {
			bitrate = math.MaxInt
		}
		label := virtualCandidateMetadataString(candidate, "display_name")
		if label == "" {
			label = virtualCandidateLabel(candidate)
		}
		sourceType := virtualCandidateMetadataString(candidate, "source_type")
		if sourceType == "" {
			sourceType = candidate.GetProviderId()
		}
		var exp time.Time
		if candidate.GetExpiresAt() != nil && candidate.GetExpiresAt().IsValid() {
			exp = candidate.GetExpiresAt().AsTime()
		}
		streams = append(streams, VirtualPlaybackStream{
			ID:                  candidate.GetCandidateId(),
			Label:               label,
			URI:                 virtualCandidateURI(virtualPath, candidate.GetCandidateId()),
			Resolution:          candidate.GetResolution().GetLabel(),
			CodecVideo:          candidate.GetVideoCodec(),
			CodecAudio:          candidate.GetAudioCodec(),
			HasAtmos:            candidate.GetHasAtmos(),
			QualityScore:        int(candidate.GetQualityScore()),
			RequestHeaders:      cloneHeaderMap(candidate.GetRequestHeaders()),
			ExpiresAt:           exp,
			HDR:                 virtualCandidateHDR(candidate),
			SourceType:          sourceType,
			FileSize:            candidate.GetFileSizeBytes(),
			Container:           candidate.GetContainer(),
			Bitrate:             bitrate,
			FrameRate:           formatVirtualFrameRate(candidate.GetResolution().GetFrameRate()),
			AudioLanguages:      append([]string(nil), candidate.GetAudioLanguages()...),
			SubtitleLanguages:   append([]string(nil), candidate.GetSubtitleLanguages()...),
			OwnerInstallationID: installationID,
			Visible:             visible,
			VisibilitySpecified: virtualCandidateVisibilitySpecified(candidate),
		})
	}
	return streams
}

func virtualCandidateHDR(candidate *pluginv1.VirtualStreamCandidate) string {
	if candidate == nil || candidate.GetHdr() == nil {
		return ""
	}
	hdr := strings.TrimSpace(candidate.GetHdr().GetFormat())
	profile := strings.TrimSpace(candidate.GetHdr().GetDolbyVisionProfile())
	if candidate.GetHdr().GetHasDolbyVision() || profile != "" {
		if profile == "" {
			return "Dolby Vision"
		}
		return "Dolby Vision " + profile
	}
	return hdr
}

func formatVirtualFrameRate(rate float64) string {
	if rate <= 0 {
		return ""
	}
	return strconv.FormatFloat(rate, 'f', -1, 64)
}

func virtualCandidateMetadataString(candidate *pluginv1.VirtualStreamCandidate, key string) string {
	if candidate == nil || candidate.GetMetadata() == nil {
		return ""
	}
	value, ok := candidate.GetMetadata().GetFields()[key]
	if !ok {
		return ""
	}
	return strings.TrimSpace(value.GetStringValue())
}

func virtualCandidateMetadataBool(candidate *pluginv1.VirtualStreamCandidate, key string) (bool, bool) {
	if candidate == nil || candidate.GetMetadata() == nil {
		return false, false
	}
	value, ok := candidate.GetMetadata().GetFields()[key]
	if !ok {
		return false, false
	}
	boolValue, ok := value.GetKind().(*structpb.Value_BoolValue)
	if !ok {
		return false, false
	}
	return boolValue.BoolValue, true
}

func virtualCandidateVisibilitySpecified(candidate *pluginv1.VirtualStreamCandidate) bool {
	_, ok := virtualCandidateMetadataBool(candidate, "visible")
	return ok
}

func virtualCandidateLabel(candidate *pluginv1.VirtualStreamCandidate) string {
	parts := make([]string, 0, 4)
	for _, value := range []string{
		candidate.GetResolution().GetLabel(),
		candidate.GetVideoCodec(),
		candidate.GetHdr().GetFormat(),
		candidate.GetProviderId(),
	} {
		value = strings.TrimSpace(value)
		if value != "" {
			parts = append(parts, value)
		}
	}
	if len(parts) == 0 {
		return candidate.GetCandidateId()
	}
	return strings.Join(parts, " · ")
}

func virtualCandidateURI(virtualPath, candidateID string) string {
	raw := virtualPath
	if !strings.Contains(raw, "://") {
		raw = "virtual://" + strings.TrimPrefix(raw, "/")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	query := parsed.Query()
	query.Set("result", candidateID)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// ConfiguredVirtualVariants uses the typed SDK configuration-only method. A
// compliant provider must not contact its upstream streaming source here.
func (s *Service) ConfiguredVirtualVariants(ctx context.Context, virtualPath, mediaType string) ([]VirtualPlaybackVariant, error) {
	if s == nil || s.installations == nil {
		return nil, nil
	}
	key := strings.ToLower(strings.TrimSpace(mediaType))
	if variants, err, ok := s.cachedConfiguredVirtualVariants(key, time.Now()); ok {
		return applyVirtualVariantPath(virtualPath, variants), err
	}
	value, err, _ := s.launchGroup.Do("configured-virtual-variants:"+key, func() (any, error) {
		if variants, cachedErr, ok := s.cachedConfiguredVirtualVariants(key, time.Now()); ok {
			return virtualVariantsCacheEntry{variants: variants, err: cachedErr}, nil
		}
		baseCtx := ctx
		if baseCtx == nil {
			baseCtx = context.Background()
		}
		loadCtx, cancel := context.WithTimeout(context.WithoutCancel(baseCtx), 15*time.Second)
		defer cancel()
		variants, loadErr := s.configuredVirtualVariantsUncached(loadCtx, "virtual://movie/placeholder", key)
		s.storeConfiguredVirtualVariants(key, variants, loadErr, time.Now())
		return virtualVariantsCacheEntry{variants: variants, err: loadErr}, nil
	})
	if err != nil {
		return nil, err
	}
	entry, ok := value.(virtualVariantsCacheEntry)
	if !ok {
		return nil, errors.New("virtual profile provider returned an invalid cache entry")
	}
	return applyVirtualVariantPath(virtualPath, entry.variants), entry.err
}

func (s *Service) configuredVirtualVariantsUncached(ctx context.Context, virtualPath, mediaType string) ([]VirtualPlaybackVariant, error) {
	installations, err := s.installations.ListEnabled(ctx)
	if err != nil {
		return nil, err
	}
	for _, installation := range installations {
		if installation == nil {
			continue
		}
		capabilityID, err := s.virtualStreamCapability(ctx, installation.ID)
		if err != nil {
			continue
		}
		client, err := s.VirtualStreamProviderClient(ctx, installation.ID, capabilityID)
		if err != nil {
			continue
		}
		response, err := s.configuredVirtualProfiles(ctx, installation.ID, capabilityID, mediaType, client)
		if err != nil {
			continue
		}
		if proto.Size(response) > maxVirtualMetadataLen || len(response.GetProfiles()) > maxVirtualPlaybackStreams {
			return nil, errors.New("virtual profile provider response exceeded its limit")
		}
		if len(response.GetProfiles()) == 0 {
			// The base placeholder still needs an installation owner even when
			// the administrator has not configured quality profiles. Carry the
			// owner in a sentinel variant; catalog materialization skips the
			// duplicate URI while retaining its ownership.
			return []VirtualPlaybackVariant{{
				VirtualURI:          virtualPath,
				OwnerInstallationID: installation.ID,
			}}, nil
		}
		variants := make([]VirtualPlaybackVariant, 0, len(response.GetProfiles()))
		for _, profile := range response.GetProfiles() {
			if profile == nil {
				return nil, errors.New("virtual profile provider returned an empty profile")
			}
			for _, value := range []string{
				profile.GetLabel(), profile.GetResolution(), profile.GetVideoCodec(),
				profile.GetAudioCodec(), profile.GetHdrFormat(),
			} {
				if len(value) > maxVirtualLabelLen || strings.ContainsAny(value, "\x00\r\n") {
					return nil, errors.New("virtual profile provider returned an invalid field")
				}
			}
			label := strings.TrimSpace(profile.GetLabel())
			if label == "" {
				return nil, errors.New("virtual profile provider returned an unlabeled profile")
			}
			parsed, err := url.Parse(virtualPath)
			if err != nil || parsed.Scheme != "virtual" {
				return nil, errors.New("invalid virtual profile base URI")
			}
			query := parsed.Query()
			if profile.GetAllResults() {
				query.Set("results", "all")
				query.Del("profile")
			} else {
				query.Set("profile", label)
				query.Del("results")
			}
			query.Del("result")
			parsed.RawQuery = query.Encode()
			variants = append(variants, VirtualPlaybackVariant{
				VirtualURI: parsed.String(), Label: label,
				Resolution: profile.GetResolution(), CodecVideo: profile.GetVideoCodec(),
				CodecAudio: profile.GetAudioCodec(), HDR: profile.GetHdrFormat(),
				OwnerInstallationID: installation.ID,
			})
		}
		return variants, nil
	}
	return nil, nil
}

func (s *Service) cachedConfiguredVirtualVariants(key string, now time.Time) ([]VirtualPlaybackVariant, error, bool) {
	s.virtualVariantsMu.Lock()
	defer s.virtualVariantsMu.Unlock()
	entry, ok := s.virtualVariantsCache[key]
	if !ok || !now.Before(entry.expiresAt) {
		if ok {
			delete(s.virtualVariantsCache, key)
		}
		return nil, nil, false
	}
	return append([]VirtualPlaybackVariant(nil), entry.variants...), entry.err, true
}

func isContextCanceledOrTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if st, ok := status.FromError(err); ok {
		if st.Code() == codes.Canceled || st.Code() == codes.DeadlineExceeded {
			return true
		}
	}
	msg := err.Error()
	return strings.Contains(msg, "context canceled") || strings.Contains(msg, "context deadline exceeded")
}

func (s *Service) storeConfiguredVirtualVariants(key string, variants []VirtualPlaybackVariant, err error, now time.Time) {
	if isContextCanceledOrTimeout(err) {
		return
	}
	ttl := virtualProfilesCacheTTL
	if err != nil {
		ttl = 10 * time.Second
	}
	s.virtualVariantsMu.Lock()
	defer s.virtualVariantsMu.Unlock()
	if s.virtualVariantsCache == nil {
		s.virtualVariantsCache = make(map[string]virtualVariantsCacheEntry)
	}
	s.virtualVariantsCache[key] = virtualVariantsCacheEntry{
		variants: append([]VirtualPlaybackVariant(nil), variants...), err: err, expiresAt: now.Add(ttl),
	}
}

func applyVirtualVariantPath(virtualPath string, variants []VirtualPlaybackVariant) []VirtualPlaybackVariant {
	result := append([]VirtualPlaybackVariant(nil), variants...)
	for index := range result {
		parsed, err := url.Parse(virtualPath)
		if err != nil || parsed.Scheme != "virtual" {
			result[index].VirtualURI = virtualPath
			continue
		}
		query := parsed.Query()
		template, _ := url.Parse(result[index].VirtualURI)
		if template != nil {
			if template.Query().Get("results") == "all" {
				query.Set("results", "all")
				query.Del("profile")
			} else if profile := template.Query().Get("profile"); profile != "" {
				query.Set("profile", profile)
				query.Del("results")
			}
		}
		query.Del("result")
		parsed.RawQuery = query.Encode()
		result[index].VirtualURI = parsed.String()
	}
	return result
}

func (s *Service) configuredVirtualProfiles(
	ctx context.Context,
	installationID int,
	capabilityID string,
	mediaType string,
	client *pluginhost.VirtualStreamProviderClient,
) (*pluginv1.ListVirtualStreamProfilesResponse, error) {
	if client == nil {
		return nil, errors.New("virtual stream provider client is unavailable")
	}
	key := fmt.Sprintf("%d\x00%s\x00%s", installationID, capabilityID, strings.ToLower(strings.TrimSpace(mediaType)))
	if cached := s.cachedVirtualProfiles(key, time.Now()); cached != nil {
		return cached, nil
	}
	value, err, _ := s.launchGroup.Do("virtual-profiles:"+key, func() (any, error) {
		if cached := s.cachedVirtualProfiles(key, time.Now()); cached != nil {
			return cached, nil
		}
		baseCtx := ctx
		if baseCtx == nil {
			baseCtx = context.Background()
		}
		callCtx, cancel := context.WithTimeout(context.WithoutCancel(baseCtx), 15*time.Second)
		defer cancel()
		response, err := client.ListVirtualStreamProfiles(callCtx, &pluginv1.ListVirtualStreamProfilesRequest{
			CapabilityId: capabilityID,
			MediaType:    mediaType,
		})
		if err != nil {
			return nil, err
		}
		if response == nil {
			return nil, errors.New("virtual profile provider returned an empty response")
		}
		if proto.Size(response) > maxVirtualMetadataLen || len(response.GetProfiles()) > maxVirtualPlaybackStreams {
			return nil, errors.New("virtual profile provider response exceeded its limit")
		}
		s.storeVirtualProfiles(key, response, time.Now())
		return proto.CloneOf(response), nil
	})
	if err != nil {
		return nil, err
	}
	response, ok := value.(*pluginv1.ListVirtualStreamProfilesResponse)
	if !ok || response == nil {
		return nil, errors.New("virtual profile provider returned an invalid response")
	}
	return response, nil
}

func (s *Service) cachedVirtualProfiles(key string, now time.Time) *pluginv1.ListVirtualStreamProfilesResponse {
	s.virtualProfilesMu.Lock()
	defer s.virtualProfilesMu.Unlock()
	entry, ok := s.virtualProfilesCache[key]
	if !ok || !now.Before(entry.expiresAt) {
		if ok {
			delete(s.virtualProfilesCache, key)
		}
		return nil
	}
	return proto.CloneOf(entry.response)
}

func (s *Service) storeVirtualProfiles(key string, response *pluginv1.ListVirtualStreamProfilesResponse, now time.Time) {
	s.virtualProfilesMu.Lock()
	defer s.virtualProfilesMu.Unlock()
	if s.virtualProfilesCache == nil {
		s.virtualProfilesCache = make(map[string]virtualProfilesCacheEntry)
	}
	for candidateKey, entry := range s.virtualProfilesCache {
		if !now.Before(entry.expiresAt) {
			delete(s.virtualProfilesCache, candidateKey)
		}
	}
	for len(s.virtualProfilesCache) >= maxVirtualProfileCacheEntries {
		oldestKey := ""
		var oldest time.Time
		for candidateKey, entry := range s.virtualProfilesCache {
			if oldestKey == "" || entry.createdAt.Before(oldest) {
				oldestKey, oldest = candidateKey, entry.createdAt
			}
		}
		delete(s.virtualProfilesCache, oldestKey)
	}
	s.virtualProfilesCache[key] = virtualProfilesCacheEntry{
		response:  proto.CloneOf(response),
		expiresAt: now.Add(virtualProfilesCacheTTL),
		createdAt: now,
	}
}

// InstallationAllowsInsecure checks whether a specific plugin installation
// has the documented allow_insecure_http toggle enabled, permitting
// private/local IP stream URLs that would otherwise be blocked by the SSRF
// guard. Unknown or unscoped installations (id <= 0) always fail closed:
// one plugin's local-network opt-in must never relax URL validation for
// unrelated providers.
func (s *Service) InstallationAllowsInsecure(ctx context.Context, installationID int) bool {
	if s == nil || s.configs == nil || installationID <= 0 {
		return false
	}
	configs, err := s.configs.ListGlobalConfigs(ctx, installationID)
	if err != nil {
		return false
	}
	for _, cfg := range configs {
		if cfg == nil {
			continue
		}
		if isConfigMapInsecure(cfg.Key, cfg.Value) {
			return true
		}
	}
	return false
}

// isConfigMapInsecure honors ONLY the exact documented schema key
// allow_insecure_http. Generic look-alikes ("insecure", "allow_http",
// "allow_private_hosts", …) must never weaken SSRF validation for an
// installation, so alias keys and arbitrary nested config maps are ignored.
func isConfigMapInsecure(configKey string, values map[string]any) bool {
	if values == nil {
		return false
	}
	if val, ok := values["allow_insecure_http"]; ok && isTruthy(val) {
		return true
	}
	if strings.EqualFold(configKey, "allow_insecure_http") {
		for _, k := range []string{"enabled", "value"} {
			if val, ok := values[k]; ok && isTruthy(val) {
				return true
			}
		}
	}
	return false
}

func isTruthy(val any) bool {
	switch v := val.(type) {
	case bool:
		return v
	case string:
		trimmed := strings.ToLower(strings.TrimSpace(v))
		return trimmed == "true" || trimmed == "1" || trimmed == "yes" || trimmed == "on"
	case int:
		return v == 1
	case int64:
		return v == 1
	case float64:
		return v == 1
	}
	return false
}

// Virtual playback caching policy, from shortest to longest lifetime so a
// stale resolved transport URL can never outlive the listing that produced it:
//   - resolvedURLMemoTTL: transient provider URLs (rotation-safe), 5m.
//   - virtualStreamsCacheTTL: bounded stream-listing cache, 1m (clamped by
//     provider metadata to [minVirtualStreamsCacheTTL, maxVirtualStreamsCacheTTL]).
//
// The memo is shorter than the listing cache: a resolved URL must be
// re-fetched before the listing used to reach it goes stale. The 5-minute
// TTL keeps repeat plays warm across brief idle windows while a background
// refresh chain refreshes the URL ~10s before expiry.
const (
	resolvedURLMemoTTL           = 5 * time.Minute
	resolvedURLMemoMax           = 256
	resolvedURLMemoSweepInterval = 5 * time.Second
	// resolvedURLMemoStaleGrace lets an expired memo entry keep serving while a
	// background refresh replaces it, so a playback start that lands during a
	// provider URL rotation is not forced into a synchronous re-resolve. It is
	// capped at one TTL so a stale entry is never served more than 2×
	// resolvedURLMemoTTL after it was resolved.
	resolvedURLMemoStaleGrace = resolvedURLMemoTTL
	// maxResolvedRefreshChain bounds background warm-refresh cycles per
	// resolved URL: ~6 × 290s ≈ 29 minutes of warmth, enough to cover
	// extended browsing sessions without an immortal goroutine/timer chain.
	maxResolvedRefreshChain = 6
)

func resolvedURLMemoKey(virtualPath string, userID int, profileID string, ownerInstallationID int) string {
	return fmt.Sprintf("%s\x00%d\x00%s\x00%d", virtualPath, userID, profileID, ownerInstallationID)
}

func resolvedStreamFromEntry(entry resolvedURLEntry) ResolvedVirtualStream {
	return ResolvedVirtualStream{
		URL:            entry.url,
		URI:            entry.uri,
		CandidateID:    entry.candidateID,
		RequestHeaders: cloneHeaderMap(entry.requestHeaders),
		ExpiresAt:      entry.expiresAt,
	}
}

func (s *Service) lookupResolvedStream(virtualPath string, userID int, profileID string, ownerInstallationID int) (ResolvedVirtualStream, bool) {
	if s == nil {
		return ResolvedVirtualStream{}, false
	}
	key := resolvedURLMemoKey(virtualPath, userID, profileID, ownerInstallationID)
	s.resolvedURLsMu.Lock()
	entry, ok := s.resolvedURLs[key]
	if !ok {
		s.resolvedURLsMu.Unlock()
		return ResolvedVirtualStream{}, false
	}
	now := time.Now()
	// A provider-declared expiry is a hard bound: the URL is dead, so it is
	// never served even inside the TTL stale grace.
	if !entry.expiresAt.IsZero() && now.After(entry.expiresAt) {
		if entry.cancel != nil {
			entry.cancel()
		}
		delete(s.resolvedURLs, key)
		s.resolvedURLsMu.Unlock()
		return ResolvedVirtualStream{}, false
	}
	age := now.Sub(entry.resolvedAt)
	if age <= resolvedURLMemoTTL {
		stream := resolvedStreamFromEntry(entry)
		s.resolvedURLsMu.Unlock()
		return stream, true
	}
	// Expired. Serve stale within the bounded grace while exactly one
	// background refresh replaces it, unless the last refresh definitively
	// failed. Concurrent callers join the in-flight refresh instead of each
	// spawning their own.
	if age <= resolvedURLMemoTTL+resolvedURLMemoStaleGrace && !entry.refreshFailed {
		kickRefresh := false
		if !entry.refreshInFlight {
			entry.refreshInFlight = true
			s.resolvedURLs[key] = entry
			kickRefresh = true
		}
		stream := resolvedStreamFromEntry(entry)
		depth := entry.refreshes
		s.resolvedURLsMu.Unlock()
		if kickRefresh {
			go s.refreshResolvedURL(context.Background(), virtualPath, userID, profileID, ownerInstallationID, depth)
		}
		return stream, true
	}
	if entry.cancel != nil {
		entry.cancel()
	}
	delete(s.resolvedURLs, key)
	s.resolvedURLsMu.Unlock()
	return ResolvedVirtualStream{}, false
}

func (s *Service) lookupResolvedURL(virtualPath string, userID int, profileID string, ownerInstallationID int) string {
	res, ok := s.lookupResolvedStream(virtualPath, userID, profileID, ownerInstallationID)
	if !ok {
		return ""
	}
	return res.URL
}

func (s *Service) storeResolvedStream(virtualPath string, userID int, profileID string, ownerInstallationID int, stream ResolvedVirtualStream) {
	s.storeResolvedStreamDepth(virtualPath, userID, profileID, ownerInstallationID, stream, 0)
}

func (s *Service) storeResolvedURL(virtualPath string, userID int, profileID string, ownerInstallationID int, url string, expiresAt ...time.Time) {
	var exp time.Time
	if len(expiresAt) > 0 {
		exp = expiresAt[0]
	}
	s.storeResolvedStream(virtualPath, userID, profileID, ownerInstallationID, ResolvedVirtualStream{
		URL:       url,
		URI:       virtualPath,
		ExpiresAt: exp,
	})
}

// storeResolvedStreamDepth memoizes a resolved provider stream. depth counts how
// many consecutive background warm-refreshes produced it; once the chain cap
// is reached the entry is stored WITHOUT re-arming the refresh timer and
// simply ages out at the memo TTL. This bounds each memo's lifetime to an
// active playback startup window instead of the process lifetime.
func (s *Service) storeResolvedStreamDepth(virtualPath string, userID int, profileID string, ownerInstallationID int, stream ResolvedVirtualStream, depth int) {
	if s == nil || stream.URL == "" {
		return
	}
	if depth < 0 {
		depth = 0
	}
	if depth > maxResolvedRefreshChain {
		return
	}
	key := resolvedURLMemoKey(virtualPath, userID, profileID, ownerInstallationID)
	s.resolvedURLsMu.Lock()
	defer s.resolvedURLsMu.Unlock()
	if s.resolvedURLs == nil {
		s.resolvedURLs = make(map[string]resolvedURLEntry)
	}
	now := time.Now()
	// Evict expired entries on a throttled cadence rather than on every write,
	// so the steady-state store path stays cheap. Lookup still deletes the
	// single stale key it touches, and the capacity guard below re-bounds the
	// map, so entries are never kept past their provider expiry or the bounded
	// stale grace and the map never grows without bound.
	if now.After(s.resolvedURLsNextSweep) {
		for k, entry := range s.resolvedURLs {
			hardExpired := !entry.expiresAt.IsZero() && now.After(entry.expiresAt)
			pastGrace := now.Sub(entry.resolvedAt) > resolvedURLMemoTTL+resolvedURLMemoStaleGrace
			if hardExpired || pastGrace {
				if entry.cancel != nil {
					entry.cancel()
				}
				delete(s.resolvedURLs, k)
			}
		}
		s.resolvedURLsNextSweep = now.Add(resolvedURLMemoSweepInterval)
	}
	if len(s.resolvedURLs) >= resolvedURLMemoMax {
		var oldestKey string
		var oldest time.Time
		for k, entry := range s.resolvedURLs {
			if oldestKey == "" || entry.resolvedAt.Before(oldest) {
				oldestKey, oldest = k, entry.resolvedAt
			}
		}
		if entry, ok := s.resolvedURLs[oldestKey]; ok && entry.cancel != nil {
			entry.cancel()
		}
		delete(s.resolvedURLs, oldestKey)
	}
	// Cancel any existing background refresh for this key.
	if existing, ok := s.resolvedURLs[key]; ok && existing.cancel != nil {
		existing.cancel()
	}
	bgCtx, bgCancel := context.WithCancel(context.Background())
	memoTTL := resolvedURLMemoTTL
	if !stream.ExpiresAt.IsZero() && stream.ExpiresAt.Before(now.Add(memoTTL)) {
		if d := time.Until(stream.ExpiresAt); d > 0 {
			memoTTL = d
		}
	}
	if depth >= maxResolvedRefreshChain || memoTTL <= 15*time.Second {
		// Terminal store: no timer goroutine; natural TTL expiry applies.
		bgCancel()
		s.resolvedURLs[key] = resolvedURLEntry{
			url:            stream.URL,
			uri:            stream.URI,
			candidateID:    stream.CandidateID,
			requestHeaders: cloneHeaderMap(stream.RequestHeaders),
			resolvedAt:     now,
			expiresAt:      stream.ExpiresAt,
		}
		return
	}
	s.resolvedURLs[key] = resolvedURLEntry{
		url:            stream.URL,
		uri:            stream.URI,
		candidateID:    stream.CandidateID,
		requestHeaders: cloneHeaderMap(stream.RequestHeaders),
		resolvedAt:     now,
		expiresAt:      stream.ExpiresAt,
		cancel:         bgCancel,
		refreshes:      depth + 1,
	}
	// Spawn a background refresh that wakes 10s before the TTL expires,
	// keeping the memo warm for active playback sessions.
	depth += 1
	go func() {
		timer := time.NewTimer(memoTTL - 10*time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
			s.refreshResolvedURL(bgCtx, virtualPath, userID, profileID, ownerInstallationID, depth)
		case <-bgCtx.Done():
		}
	}()
}

// refreshResolvedURL re-resolves a provider URL in the background to keep the
// memo warm before the TTL expires. If the re-resolution fails the existing
// entry stays until natural expiry, but the failure is recorded so the entry is
// not served as stale: a provider that definitively refused to renew the URL
// must not have it pinned past its memo TTL.
func (s *Service) refreshResolvedURL(ctx context.Context, virtualPath string, userID int, profileID string, ownerInstallationID int, depth int) {
	if s == nil || ctx.Err() != nil {
		return
	}
	if s.installations == nil || s.host == nil {
		// Not fully wired (e.g. a bare Service in a memo unit test); clear any
		// in-flight marker so a later stale lookup can retry.
		s.noteResolvedURLRefreshFailure(virtualPath, userID, profileID, ownerInstallationID)
		return
	}
	// Bypass the memo: this refresh exists to obtain a FRESH provider URL.
	// Reading our own nearly-expired memo would be a no-op that pins potentially
	// stale URLs past their rotation lifetime.
	res, err := s.ResolveVirtualPlaybackDetailedWithRouting(ctx, virtualPath, userID, profileID, VirtualPlaybackRouting{OwnerInstallationID: ownerInstallationID}, true, nil, "")
	if err != nil || res.URL == "" {
		slog.DebugContext(ctx, "virtual URL background refresh failed", "component", "plugins", "virtual_path", virtualPath, "error", err)
		s.noteResolvedURLRefreshFailure(virtualPath, userID, profileID, ownerInstallationID)
		return
	}
	s.storeResolvedStreamDepth(virtualPath, userID, profileID, ownerInstallationID, res, depth)
}

// noteResolvedURLRefreshFailure records a definitive background-refresh failure
// for the key and clears the in-flight marker. A stale entry carrying the
// failure flag is dropped on the next lookup rather than served.
func (s *Service) noteResolvedURLRefreshFailure(virtualPath string, userID int, profileID string, ownerInstallationID int) {
	if s == nil {
		return
	}
	key := resolvedURLMemoKey(virtualPath, userID, profileID, ownerInstallationID)
	s.resolvedURLsMu.Lock()
	defer s.resolvedURLsMu.Unlock()
	entry, ok := s.resolvedURLs[key]
	if !ok {
		return
	}
	entry.refreshFailed = true
	entry.refreshInFlight = false
	s.resolvedURLs[key] = entry
}

// Clear flushes all in-memory virtual playback caches (streams, profiles,
// variants, and resolved URLs) so provider configuration changes instantly
// take effect.
func (s *Service) Clear() {
	if s == nil {
		return
	}
	s.virtualStreamsMu.Lock()
	s.virtualStreamsCache = nil
	s.virtualStreamsMu.Unlock()
	s.virtualProfilesMu.Lock()
	s.virtualProfilesCache = nil
	s.virtualProfilesMu.Unlock()
	s.virtualVariantsMu.Lock()
	s.virtualVariantsCache = nil
	s.virtualVariantsMu.Unlock()
	s.resolvedURLsMu.Lock()
	for _, entry := range s.resolvedURLs {
		if entry.cancel != nil {
			entry.cancel()
		}
	}
	s.resolvedURLs = nil
	s.resolvedURLsMu.Unlock()
}
