package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Silo-Server/silo-server/internal/logredact"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/nodepool"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/tonemap"
	"github.com/Silo-Server/silo-server/internal/transcodenode"
)

// startLocalPlaybackTransport is the shared local ffmpeg launch primitive for
// legacy and protocol-v3 orchestration. Callers retain ownership of lifecycle
// locking and decide whether registration is immediate or transactionally
// staged.
type localPlaybackStartupError struct {
	cause   error
	running bool
}

func (e *localPlaybackStartupError) Error() string {
	return e.cause.Error()
}

func (e *localPlaybackStartupError) Unwrap() error {
	return e.cause
}

func (h *PlaybackHandler) startLocalPlaybackTransport(ctx context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
	// Tone-map hardware/software selection and retries are owned by the v3
	// planner (softwareToneMapRetryOptsV3) and the compat recipe resolver; this
	// primitive starts exactly the executor it was given.
	return h.startLocalPlaybackTransportOnce(ctx, opts)
}

func (h *PlaybackHandler) startTranscodeSession(ctx context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
	if h != nil && h.StartTranscodeFunc != nil {
		return h.StartTranscodeFunc(ctx, opts)
	}
	return playback.StartTranscode(ctx, opts)
}

func (h *PlaybackHandler) startLocalPlaybackTransportOnce(ctx context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
	// Repeated input demux failures stamp the virtual candidate known-bad; the
	// manager owns the callback so every local start (fresh or reconstructed)
	// reaches the same marker without the transcode package importing handlers.
	if opts.OnDemuxFailure == nil && h.tm != nil {
		opts.OnDemuxFailure = h.tm.OnDemuxFailure
	}
	if !strings.HasPrefix(strings.ToLower(opts.InputPath), virtualPlaybackPrefix) {
		session, startErr := h.startTranscodeSession(context.WithoutCancel(ctx), opts)
		if startErr != nil {
			return nil, startErr
		}
		if _, readyErr := session.WaitForManifest(8 * time.Second); readyErr != nil {
			startupErr := &localPlaybackStartupError{cause: readyErr, running: session.IsRunning()}
			_ = session.Close()
			return nil, startupErr
		}
		return session, nil
	}
	// The catalog candidate can be replaced while playback is starting. Do not
	// make a just-selected virtual row a second hard dependency; the canonical
	// URI and owner carried in opts are sufficient to resolve the provider URL.
	file := &models.MediaFile{ID: opts.MediaFileID, FilePath: opts.InputPath, VirtualOwnerInstallationID: opts.VirtualSourceOwnerInstallationID}
	if h.fileResolver != nil && opts.MediaFileID > 0 {
		if catalogFile, lookupErr := h.fileResolver.GetByID(ctx, opts.MediaFileID); lookupErr == nil && catalogFile != nil {
			origPath := file.FilePath
			file = catalogFile
			if isUnplayableVirtualURI(file.FilePath) && !isUnplayableVirtualURI(origPath) {
				file.FilePath = origPath
			}
		}
	}
	userID, profileID := 0, ""
	ownerInstallationID := file.VirtualOwnerInstallationID
	sessionVirtualURI := ""
	if session, sessionErr := h.sessionMgr.GetSession(opts.SessionID); sessionErr == nil && session != nil {
		userID, profileID = session.UserID, session.ProfileID
		sessionVirtualURI = session.VirtualSourceURI
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(session.VirtualSourceURI)), virtualPlaybackPrefix) && (!isUnplayableVirtualURI(session.VirtualSourceURI) || isUnplayableVirtualURI(file.FilePath)) {
			copy := *file
			copy.FilePath = session.VirtualSourceURI
			if session.VirtualSourceOwnerInstallationID > 0 {
				copy.VirtualOwnerInstallationID = session.VirtualSourceOwnerInstallationID
			}
			file = &copy
			ownerInstallationID = file.VirtualOwnerInstallationID
			opts.InputPath = file.FilePath
		}
	}
	canonicalPath := opts.InputPath
	neutralPath := virtualPlaybackNeutralKey(file.FilePath)
	if neutralPath == "" || strings.HasSuffix(neutralPath, "://") {
		neutralPath = virtualPlaybackNeutralKey(opts.InputPath)
	}
	initialPinnedPath := ""
	initialPinnedResultID := ""
	if file != nil && strings.Contains(file.FilePath, "?result=") {
		initialPinnedPath = file.FilePath
		if parsed, err := url.Parse(file.FilePath); err == nil {
			initialPinnedResultID = parsed.Query().Get("result")
		}
	}
	opts.CanonicalInputPath = canonicalPath
	opts.VirtualSourceOwnerInstallationID = ownerInstallationID
	opts.RefreshInput = func(refreshCtx context.Context) (string, func(), error) {
		// A restart renews the exact candidate pinned to this session, never a
		// provider-neutral re-selection: re-resolving through the neutral path
		// can silently swap to a differently-ranked candidate mid-stream. The
		// canonical path still carries the ?result= identity the session bound
		// to during planning.
		res, cleanup, err := h.resolveVirtualInputURI(
			refreshCtx, canonicalPath, ownerInstallationID,
			userID, profileID, true, nil, "",
		)
		return res.URL, cleanup, err
	}
	var lastErr error
	startupCtx, startupCancel := context.WithTimeout(context.WithoutCancel(ctx), virtualStartupBudget)
	defer startupCancel()
	maxAttempts := h.maxVirtualFailoverAttempts(ctx)
	if sessionVirtualURI != "" {
		maxAttempts = min(2, maxAttempts)
	}
	failedCandidateIDs := make([]string, 0)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		targetURI := canonicalPath
		if attempt > 0 {
			targetURI = neutralPath
		}
		preferredID := initialPinnedResultID
		for _, failed := range failedCandidateIDs {
			if preferredID == failed {
				preferredID = ""
				break
			}
		}
		resolvedMedia, cleanup, resolveErr := h.resolveVirtualInputURI(
			startupCtx, targetURI, ownerInstallationID, userID, profileID, attempt > 0, failedCandidateIDs, preferredID,
		)
		if resolveErr != nil {
			lastErr = resolveErr
			failedID := resolvedMedia.CandidateID
			if failedID == "" {
				if parsed, err := url.Parse(targetURI); err == nil {
					failedID = parsed.Query().Get("result")
				}
			}
			if failedID != "" {
				failedCandidateIDs = append(failedCandidateIDs, failedID)
			}
			if h.BestResultCache != nil && file != nil {
				neutralURI := virtualPlaybackNeutralKey(canonicalPath)
				failedURI := targetURI
				if failedID != "" {
					failedURI = withVirtualResultKey(neutralURI, failedID)
				}
				h.BestResultCache.RemoveCandidate(bestResultCacheKey(file.ContentID, neutralURI, ownerInstallationID), failedURI)
				h.BestResultCache.RemoveCandidateForContent(file.ContentID, neutralURI, ownerInstallationID, failedURI)
			}
			if targetURI == canonicalPath {
				canonicalPath = neutralPath
				if file != nil {
					file.FilePath = neutralPath
				}
			}
			continue
		}
		transcodeCtx, transcodeCancel := context.WithCancel(context.Background())
		timer := time.AfterFunc(4*time.Hour, transcodeCancel)
		cleanupWithCancel := func() {
			timer.Stop()
			transcodeCancel()
			if cleanup != nil {
				cleanup()
			}
		}
		attemptOpts := opts
		attemptOpts.InputPath = resolvedMedia.URL
		attemptOpts.InputCleanup = cleanupWithCancel
		session, startErr := h.startTranscodeSession(transcodeCtx, attemptOpts)
		if startErr == nil {
			if _, readyErr := session.WaitForManifest(playback.ManifestStartupTimeout); readyErr == nil {
				winningURI := resolvedMedia.URI
				if winningURI == "" || winningURI == neutralPath {
					if resolvedMedia.CandidateID != "" {
						winningURI = withVirtualResultKey(neutralPath, resolvedMedia.CandidateID)
					} else {
						winningURI = targetURI
					}
				}
				canonicalPath = winningURI
				if file != nil {
					file.FilePath = winningURI
				}
				// Transport successfully ready. If this was a fallback attempt from a dead pin,
				// update the persisted pin compare-and-swap so future sessions use the live source.
				if replacer, ok := h.fileResolver.(interface {
					ReplaceVirtualResultPin(context.Context, int, string, string) (bool, error)
				}); ok && file != nil && file.ID > 0 && initialPinnedPath != "" {
					if winningURI != "" && winningURI != initialPinnedPath {
						unpinCtx, unpinCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
						if _, err := replacer.ReplaceVirtualResultPin(unpinCtx, file.ID, initialPinnedPath, winningURI); err != nil {
							slog.WarnContext(ctx, "failed to update virtual result pin after fallback success", "component", "api", "file_id", file.ID, "error", err)
						}
						unpinCancel()
					}
				}
				return session, nil
			} else {
				startErr = readyErr
			}
			_ = session.Close()
		} else if cleanup != nil {
			cleanupWithCancel()
		}
		if resolvedMedia.CandidateID != "" {
			failedCandidateIDs = append(failedCandidateIDs, resolvedMedia.CandidateID)
		}
		if h.BestResultCache != nil && file != nil {
			neutralURI := virtualPlaybackNeutralKey(canonicalPath)
			failedURI := targetURI
			if resolvedMedia.CandidateID != "" {
				failedURI = withVirtualResultKey(neutralURI, resolvedMedia.CandidateID)
			}
			h.BestResultCache.RemoveCandidate(bestResultCacheKey(file.ContentID, neutralURI, ownerInstallationID), failedURI)
			h.BestResultCache.RemoveCandidateForContent(file.ContentID, neutralURI, ownerInstallationID, failedURI)
		}
		if targetURI == canonicalPath {
			canonicalPath = neutralPath
			if file != nil {
				file.FilePath = neutralPath
			}
		}
		lastErr = startErr
	}
	if lastErr == nil {
		lastErr = errors.New("virtual transcode provider returned no usable stream")
	}
	// All attempts failed: conditionally clear the initial dead pin so future plays re-list
	if file != nil && file.ID > 0 {
		unpinCtx, unpinCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		if replacer, ok := h.fileResolver.(interface {
			ReplaceVirtualResultPin(context.Context, int, string, string) (bool, error)
		}); ok && initialPinnedPath != "" {
			_, _ = replacer.ReplaceVirtualResultPin(unpinCtx, file.ID, initialPinnedPath, neutralPath)
		} else if cleaner, ok := h.fileResolver.(interface {
			ClearVirtualResultPin(context.Context, int) error
		}); ok {
			_ = cleaner.ClearVirtualResultPin(unpinCtx, file.ID)
		}
		unpinCancel()
	}
	return nil, lastErr
}

func (h *PlaybackHandler) resolveVirtualInputURI(
	ctx context.Context,
	virtualURI string,
	ownerInstallationID int,
	userID int,
	profileID string,
	forceRefresh bool,
	excludedCandidateIDs []string,
	preferredCandidateID string,
) (ResolvedVirtualMedia, func(), error) {
	var res ResolvedVirtualMedia
	var err error
	if h.VirtualMediaDetailedResolver != nil {
		res, err = h.VirtualMediaDetailedResolver.ResolveVirtualMediaDetailed(
			ctx, virtualURI, ownerInstallationID, userID, profileID, forceRefresh, excludedCandidateIDs, preferredCandidateID,
		)
	} else if forceRefresh && h.VirtualMediaRefreshResolver != nil {
		var inputPath string
		inputPath, err = h.VirtualMediaRefreshResolver.RefreshVirtualMedia(
			ctx, virtualURI, ownerInstallationID, userID, profileID,
		)
		res = ResolvedVirtualMedia{URL: inputPath, URI: virtualURI}
	} else {
		var inputPath string
		inputPath, err = resolveVirtualMediaPath(
			ctx, h.VirtualMediaResolver, virtualURI,
			ownerInstallationID, userID, profileID,
		)
		res = ResolvedVirtualMedia{URL: inputPath, URI: virtualURI}
	}
	if err != nil {
		return res, nil, fmt.Errorf("resolve virtual input: %w", err)
	}
	if h.RemoteStreamRelay == nil {
		return res, nil, nil
	}
	var relayURL string
	var cleanup func()
	if h.AllowInsecureVirtual != nil && h.AllowInsecureVirtual(ownerInstallationID) {
		relayURL, cleanup, err = h.RemoteStreamRelay.RegisterInsecureWithHeaders(ctx, res.URL, res.RequestHeaders)
	} else {
		relayURL, cleanup, err = h.RemoteStreamRelay.RegisterWithHeaders(ctx, res.URL, res.RequestHeaders)
	}
	if err != nil {
		return ResolvedVirtualMedia{}, nil, err
	}
	res.URL = relayURL
	return res, cleanup, nil
}

// startRemotePlaybackTransport is the shared remote-node launch primitive.
// It returns the node's HTTP status separately so legacy and v3 can preserve
// their existing public error envelopes while executing identical transport
// startup and response parsing.
func (h *PlaybackHandler) startRemotePlaybackTransport(ctx context.Context, nodeURL string, request transcodenode.TranscodeStartRequest) (transcodenode.TranscodeStartResponse, int, error) {
	if strings.HasPrefix(strings.ToLower(request.InputPath), virtualPlaybackPrefix) {
		return transcodenode.TranscodeStartResponse{}, 0, errors.New("virtual sources require an integrated transcode transport")
	}
	body, err := json.Marshal(request)
	if err != nil {
		return transcodenode.TranscodeStartResponse{}, 0, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, h.remotePlaybackTransportTimeout(nodeURL, request))
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(requestCtx, http.MethodPost, nodepool.NodeEndpoint(nodeURL, "/transcode/start"), bytes.NewReader(body))
	if err != nil {
		return transcodenode.TranscodeStartResponse{}, 0, logredact.SanitizeURLError(err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+h.JWTSecret)
	response, err := http.DefaultClient.Do(httpRequest)
	if err != nil {
		return transcodenode.TranscodeStartResponse{}, 0, logredact.SanitizeURLError(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusAccepted {
		// Drain the (small) error body so the transport can reuse the
		// connection instead of tearing it down on every failed start.
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		if request.ToneMapMode != "" {
			if validationErr := transcodenode.ToneMapExecutionErrorForResponse(
				response.StatusCode,
				response.Header.Get(transcodenode.ToneMapExecutionErrorHeader),
			); validationErr != nil {
				return transcodenode.TranscodeStartResponse{}, response.StatusCode, validationErr
			}
		}
		return transcodenode.TranscodeStartResponse{}, response.StatusCode, nil
	}
	var result transcodenode.TranscodeStartResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		// Older nodes returned an empty 202 response; accept that for ordinary
		// transcodes while treating any other malformed 202 body as a failed
		// start instead of fabricating a success from a zero-value response.
		if errors.Is(err, io.EOF) && request.ToneMapMode == "" {
			return transcodenode.TranscodeStartResponse{}, response.StatusCode, nil
		}
		slog.WarnContext(ctx, "remote transcode start response decode failed", "component", "api", "node", logredact.SanitizeURL(nodeURL), "error", err)
		return transcodenode.TranscodeStartResponse{}, response.StatusCode, fmt.Errorf("decode remote transcode start response: %w", err)
	}
	return result, response.StatusCode, nil
}

func (h *PlaybackHandler) remotePlaybackTransportTimeout(nodeURL string, request transcodenode.TranscodeStartRequest) time.Duration {
	if request.ToneMapMode == "" {
		return 20 * time.Second
	}
	timeout := h.remoteToneMapProbeTimeoutV3(nodeURL) + playback.ManifestStartupTimeout
	if request.ToneMapPreflightRequired {
		timeout += tonemap.SourcePreflightTimeout(request.TotalDuration)
	}
	if request.RequireReady {
		timeout += transcodenode.TranscodeStartReadinessTimeout
	}
	return timeout
}

func fetchRemoteTranscodeCapabilities(ctx context.Context, nodeURL, jwtSecret string) (playback.HWAccelInfo, error) {
	info, status, err := transcodenode.FetchHWCapabilities(ctx, http.DefaultClient, nodeURL, jwtSecret)
	if err != nil {
		return playback.HWAccelInfo{}, err
	}
	if status != http.StatusOK {
		return playback.HWAccelInfo{}, fmt.Errorf("node returned %d", status)
	}
	info.Source = "transcode_node"
	info.NodeURL = nodeURL
	return info, nil
}
