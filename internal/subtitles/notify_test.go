package subtitles

import (
	"context"
	"errors"
	"testing"
)

type fakeSubtitleDownloader struct {
	searchResp  *SearchResponse
	searchErr   error
	searchCalls int
	downloads   []DownloadRequest
	downloadErr map[string]error
	rows        map[string]*DownloadedSubtitle
}

func (f *fakeSubtitleDownloader) Search(context.Context, SearchRequest) (*SearchResponse, error) {
	f.searchCalls++
	return f.searchResp, f.searchErr
}

func (f *fakeSubtitleDownloader) Download(_ context.Context, req DownloadRequest) (*DownloadedSubtitle, error) {
	f.downloads = append(f.downloads, req)
	if err := f.downloadErr[req.SubtitleID]; err != nil {
		return nil, err
	}
	if row, ok := f.rows[req.SubtitleID]; ok {
		return row, nil
	}
	return &DownloadedSubtitle{ID: len(f.downloads), MediaFileID: req.MediaFileID, Language: req.Language, ReleaseName: req.ReleaseName}, nil
}

type readyCall struct {
	mediaFileID int
	subtitleID  int
	language    string
	label       string
}

type recordingReadyNotifier struct {
	calls []readyCall
}

func (n *recordingReadyNotifier) SubtitleReady(_ context.Context, mediaFileID, subtitleID int, language, label string) {
	n.calls = append(n.calls, readyCall{mediaFileID, subtitleID, language, label})
}

func TestSubtitleDownloadBestMatchesNotifiesEachDownloadedLanguage(t *testing.T) {
	downloader := &fakeSubtitleDownloader{
		searchResp: &SearchResponse{Results: []SubtitleResult{
			{ID: "en-best", Provider: "opensubtitles", Language: "en", ReleaseName: "Movie.2020.en"},
			{ID: "en-alt", Provider: "opensubtitles", Language: "en", ReleaseName: "Movie.2020.en.alt"},
			{ID: "es-best", Provider: "subdl", Language: "es", ReleaseName: "Movie.2020.es"},
		}},
		rows: map[string]*DownloadedSubtitle{
			"en-best": {ID: 11, MediaFileID: 42, Language: "en", ReleaseName: "Movie.2020.en", Provider: "opensubtitles"},
			"es-best": {ID: 22, MediaFileID: 42, Language: "es", ReleaseName: "Movie.2020.es", Provider: "subdl"},
		},
	}
	notifier := &recordingReadyNotifier{}

	DownloadBestMatches(context.Background(), downloader, notifier, 42, SearchRequest{Languages: []string{"en", "es"}})

	if len(notifier.calls) != 2 {
		t.Fatalf("notifier calls = %d, want one per downloaded language", len(notifier.calls))
	}
	if got := notifier.calls[0]; got != (readyCall{42, 11, "en", "Movie.2020.en (opensubtitles)"}) {
		t.Errorf("first notification = %+v, want file 42 row 11 en", got)
	}
	if got := notifier.calls[1]; got != (readyCall{42, 22, "es", "Movie.2020.es (subdl)"}) {
		t.Errorf("second notification = %+v, want file 42 row 22 es", got)
	}
	// One match per language is enough: the second English candidate is skipped.
	if len(downloader.downloads) != 2 {
		t.Fatalf("downloads = %d, want 2 (one per language)", len(downloader.downloads))
	}
	if downloader.downloads[0].SubtitleID != "en-best" || downloader.downloads[1].SubtitleID != "es-best" {
		t.Errorf("downloaded %q then %q, want the best match per language", downloader.downloads[0].SubtitleID, downloader.downloads[1].SubtitleID)
	}
	if downloader.downloads[0].MediaFileID != 42 {
		t.Errorf("download media_file_id = %d, want the requested file 42", downloader.downloads[0].MediaFileID)
	}
}

func TestSubtitleDownloadBestMatchesFallsThroughDownloadFailureWithoutNotifying(t *testing.T) {
	downloader := &fakeSubtitleDownloader{
		searchResp: &SearchResponse{Results: []SubtitleResult{
			{ID: "bad", Provider: "opensubtitles", Language: "en", ReleaseName: "bad"},
			{ID: "good", Provider: "opensubtitles", Language: "en", ReleaseName: "good"},
		}},
		downloadErr: map[string]error{"bad": errors.New("provider down")},
		rows: map[string]*DownloadedSubtitle{
			"good": {ID: 5, MediaFileID: 7, Language: "en", ReleaseName: "good", Provider: "opensubtitles"},
		},
	}
	notifier := &recordingReadyNotifier{}

	DownloadBestMatches(context.Background(), downloader, notifier, 7, SearchRequest{Languages: []string{"en"}})

	if len(downloader.downloads) != 2 {
		t.Fatalf("downloads = %d, want the failed candidate then the working one", len(downloader.downloads))
	}
	if len(notifier.calls) != 1 {
		t.Fatalf("notifier calls = %d, want only the successful download", len(notifier.calls))
	}
	if got := notifier.calls[0]; got != (readyCall{7, 5, "en", "good (opensubtitles)"}) {
		t.Errorf("notification = %+v, want file 7 row 5", got)
	}
}

func TestSubtitleDownloadBestMatchesDoesNotNotifyWhenEveryDownloadFails(t *testing.T) {
	downloader := &fakeSubtitleDownloader{
		searchResp: &SearchResponse{Results: []SubtitleResult{
			{ID: "one", Provider: "opensubtitles", Language: "en", ReleaseName: "one"},
			{ID: "two", Provider: "opensubtitles", Language: "en", ReleaseName: "two"},
		}},
		downloadErr: map[string]error{"one": errors.New("gone"), "two": errors.New("gone")},
	}
	notifier := &recordingReadyNotifier{}

	DownloadBestMatches(context.Background(), downloader, notifier, 7, SearchRequest{Languages: []string{"en"}})

	if len(notifier.calls) != 0 {
		t.Fatalf("notifier calls = %d, want 0 when no download succeeded", len(notifier.calls))
	}
}

func TestSubtitleDownloadBestMatchesDoesNotNotifyWithoutSearchResults(t *testing.T) {
	notifier := &recordingReadyNotifier{}

	empty := &fakeSubtitleDownloader{searchResp: &SearchResponse{}}
	DownloadBestMatches(context.Background(), empty, notifier, 7, SearchRequest{Languages: []string{"en"}})

	failed := &fakeSubtitleDownloader{searchErr: errors.New("all providers down")}
	DownloadBestMatches(context.Background(), failed, notifier, 7, SearchRequest{Languages: []string{"en"}})

	if len(notifier.calls) != 0 {
		t.Fatalf("notifier calls = %d, want 0", len(notifier.calls))
	}
	if len(failed.downloads) != 0 {
		t.Fatalf("downloads = %d, want 0 after a search error", len(failed.downloads))
	}
}

func TestSubtitleDownloadBestMatchesSkipsSearchWhenNoLanguagesRequested(t *testing.T) {
	downloader := &fakeSubtitleDownloader{searchResp: &SearchResponse{Results: []SubtitleResult{
		{ID: "en", Provider: "opensubtitles", Language: "en", ReleaseName: "en"},
	}}}
	notifier := &recordingReadyNotifier{}

	DownloadBestMatches(context.Background(), downloader, notifier, 7, SearchRequest{})

	if downloader.searchCalls != 0 {
		t.Fatalf("search calls = %d, want 0 with no requested languages", downloader.searchCalls)
	}
	if len(notifier.calls) != 0 {
		t.Fatalf("notifier calls = %d, want 0", len(notifier.calls))
	}
}

func TestNotifyDownloadedSubtitleIgnoresUnpersistedRowsAndNilNotifier(t *testing.T) {
	notifier := &recordingReadyNotifier{}
	NotifyDownloadedSubtitle(context.Background(), notifier, nil)
	NotifyDownloadedSubtitle(context.Background(), notifier, &DownloadedSubtitle{ID: 0, MediaFileID: 7})
	NotifyDownloadedSubtitle(context.Background(), notifier, &DownloadedSubtitle{ID: 5, MediaFileID: 0})
	NotifyDownloadedSubtitle(context.Background(), nil, &DownloadedSubtitle{ID: 5, MediaFileID: 7})

	if len(notifier.calls) != 0 {
		t.Fatalf("notifier calls = %d, want 0 for unpersisted rows", len(notifier.calls))
	}
}

func TestDownloadedSubtitleLabelFallsBackToEmptyWhenProviderAndReleaseMissing(t *testing.T) {
	if got := DownloadedSubtitleLabel(&DownloadedSubtitle{Language: "en"}); got != "" {
		t.Errorf("label = %q, want empty when release and provider are both missing", got)
	}
	if got := DownloadedSubtitleLabel(&DownloadedSubtitle{ReleaseName: "release", Provider: "subdl"}); got != "release (subdl)" {
		t.Errorf("label = %q, want %q", got, "release (subdl)")
	}
}
