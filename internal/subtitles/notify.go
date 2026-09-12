package subtitles

import (
	"context"
	"strings"
)

// SubtitleReadyNotifier receives newly available subtitle tracks. playback's
// SubtitleReadyNotifier implements it, but the interface lives here so the
// provider search/download path can be unit-tested without a playback
// dependency.
type SubtitleReadyNotifier interface {
	SubtitleReady(ctx context.Context, mediaFileID, subtitleID int, language, label string)
}

// SubtitleDownloader is the subset of Manager the provider subtitle path needs:
// find candidates, then fetch and persist the chosen one. *Manager implements
// it.
type SubtitleDownloader interface {
	Search(ctx context.Context, req SearchRequest) (*SearchResponse, error)
	Download(ctx context.Context, req DownloadRequest) (*DownloadedSubtitle, error)
}

// DownloadBestMatches searches the configured providers and downloads the best
// match for each requested language. Every successfully stored track is
// announced through notifier so playback sessions watching the file fold it in
// without a reload.
//
// A nil downloader, an empty language list, a search failure, a download
// failure, or a nil notifier is a no-op for the affected language: a realtime
// delivery problem must never fail the search. One successful match per
// language is enough; later candidates for that language are skipped.
func DownloadBestMatches(
	ctx context.Context,
	downloader SubtitleDownloader,
	notifier SubtitleReadyNotifier,
	fileID int,
	req SearchRequest,
) {
	if downloader == nil || len(req.Languages) == 0 {
		return
	}
	results, err := downloader.Search(ctx, req)
	if err != nil || results == nil || len(results.Results) == 0 {
		return
	}
	for _, lang := range req.Languages {
		for _, r := range results.Results {
			if !strings.EqualFold(r.Language, lang) {
				continue
			}
			sub, dlErr := downloader.Download(ctx, DownloadRequest{
				ProviderName:    r.Provider,
				SubtitleID:      r.ID,
				Language:        r.Language,
				ReleaseName:     r.ReleaseName,
				MediaFileID:     fileID,
				HearingImpaired: r.HearingImpaired,
			})
			if dlErr != nil {
				continue // try the next candidate for this language
			}
			NotifyDownloadedSubtitle(ctx, notifier, sub)
			break // one good match per language is enough
		}
	}
}

// NotifyDownloadedSubtitle announces one stored subtitle row to the notifier.
// A nil notifier, or a row that was not persisted (no ID or no owning file),
// is a no-op. The notifier's own delivery failures are logged inside it, so a
// realtime problem cannot fail the search that triggered it.
func NotifyDownloadedSubtitle(ctx context.Context, notifier SubtitleReadyNotifier, sub *DownloadedSubtitle) {
	if notifier == nil || sub == nil || sub.ID <= 0 || sub.MediaFileID <= 0 {
		return
	}
	notifier.SubtitleReady(ctx, sub.MediaFileID, sub.ID, sub.Language, DownloadedSubtitleLabel(sub))
}

// DownloadedSubtitleLabel returns the user-facing label for a stored subtitle.
// It mirrors the inventory label the v3 plan publishes so a realtime event and
// the next plan name the same row identically.
func DownloadedSubtitleLabel(sub *DownloadedSubtitle) string {
	if sub == nil || (sub.ReleaseName == "" && sub.Provider == "") {
		return ""
	}
	return sub.ReleaseName + " (" + sub.Provider + ")"
}
