package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// recordingChapterQueuer counts how many times the watch surfaces enqueue
// chapter-thumbnail work, so a repeated fetch can be asserted to be a no-op.
type recordingChapterQueuer struct {
	calls [][]int
}

func (q *recordingChapterQueuer) QueueFileIDs(_ context.Context, fileIDs []int) {
	q.calls = append(q.calls, append([]int(nil), fileIDs...))
}

func (q *recordingChapterQueuer) QueuePriorityFileIDs(context.Context, []int) {}

func (q *recordingChapterQueuer) QueuePriorityFileAtPosition(context.Context, int, float64) {}

// A multi-version item whose files carry no duration must resolve the runtime
// fallback once, not once per version. The item row supplies the duration for
// all three versions from a single lookup.
func TestBuildPlaybackInfoDurationFallbackOneLookupPerItem(t *testing.T) {
	f := newVersionsFixture(t)
	movieID := f.ids["movie"]
	if _, err := f.svc.itemRepo.pool.Exec(t.Context(), `UPDATE media_items SET runtime=120 WHERE content_id=$1`, movieID); err != nil {
		t.Fatal(err)
	}
	files := []*models.MediaFile{
		{ID: 1, ContentID: movieID, Duration: 0, FilePath: "/media/a.mkv", Chapters: []models.MediaChapter{}},
		{ID: 2, ContentID: movieID, Duration: 0, FilePath: "/media/b.mkv", Chapters: []models.MediaChapter{}},
		{ID: 3, ContentID: movieID, Duration: 0, FilePath: "/media/c.mkv", Chapters: []models.MediaChapter{}},
	}

	f.queries.calls.Store(0)
	versions, _, _, _, _, _, _ := f.svc.buildPlaybackInfo(t.Context(), files, AccessFilter{}, movieID)
	if got := f.queries.calls.Load(); got != 1 {
		t.Fatalf("runtime fallback issued %d queries for three durationless versions, want 1 (one item lookup)", got)
	}
	for i, version := range versions {
		if want := 120 * 60; version.Duration != want {
			t.Fatalf("version %d duration = %d, want %d", i, version.Duration, want)
		}
	}
}

// Episode versions share one episode ID, so the episode runtime fallback must
// also collapse to a single lookup and leave the item fallback untouched.
func TestBuildPlaybackInfoDurationFallbackOneLookupPerEpisode(t *testing.T) {
	f := newVersionsFixture(t)
	episodeID := f.ids["episode"]
	if _, err := f.svc.episodeRepo.pool.Exec(t.Context(), `UPDATE episodes SET runtime=45 WHERE content_id=$1`, episodeID); err != nil {
		t.Fatal(err)
	}
	files := []*models.MediaFile{
		{ID: 1, ContentID: "episode-owner", EpisodeID: episodeID, Duration: 0, FilePath: "/media/a.mkv", Chapters: []models.MediaChapter{}},
		{ID: 2, ContentID: "episode-owner", EpisodeID: episodeID, Duration: 0, FilePath: "/media/b.mkv", Chapters: []models.MediaChapter{}},
		{ID: 3, ContentID: "episode-owner", EpisodeID: episodeID, Duration: 0, FilePath: "/media/c.mkv", Chapters: []models.MediaChapter{}},
	}

	f.queries.calls.Store(0)
	versions, _, _, _, _, _, _ := f.svc.buildPlaybackInfo(t.Context(), files, AccessFilter{}, "episode-owner")
	if got := f.queries.calls.Load(); got != 1 {
		t.Fatalf("runtime fallback issued %d queries for three durationless episode versions, want 1 (one episode lookup)", got)
	}
	for i, version := range versions {
		if want := 45 * 60; version.Duration != want {
			t.Fatalf("version %d duration = %d, want %d", i, version.Duration, want)
		}
	}
}

// A repeated watch-detail fetch of an unchanged file set must not re-enqueue
// chapter-thumbnail work; the first fetch still prepares everything, and a
// changed probe fingerprint lets the next fetch prepare again.
func TestGetWatchDetailRepeatedFetchDoesNotRequeueChapterThumbs(t *testing.T) {
	f := newVersionsFixture(t)
	queuer := &recordingChapterQueuer{}
	f.svc.SetChapterThumbnailQueuer(queuer)

	if _, err := f.svc.GetWatchDetail(t.Context(), f.ids["movie"], AccessFilter{}); err != nil {
		t.Fatal(err)
	}
	if len(queuer.calls) != 1 {
		t.Fatalf("first watch-detail fetch queued %d chapter-thumb batches, want 1", len(queuer.calls))
	}

	if _, err := f.svc.GetWatchDetail(t.Context(), f.ids["movie"], AccessFilter{}); err != nil {
		t.Fatal(err)
	}
	if len(queuer.calls) != 1 {
		t.Fatalf("repeated watch-detail fetch re-queued chapter thumbs: %d batches, want 1", len(queuer.calls))
	}

	// A probe repair or rescan changes the fingerprint, so the next fetch must
	// not be suppressed forever.
	probedAt := time.Now().Add(time.Second)
	for _, file := range f.files.files[f.ids["movie"]] {
		file.ProbeUpdatedAt = &probedAt
	}
	if _, err := f.svc.GetWatchDetail(t.Context(), f.ids["movie"], AccessFilter{}); err != nil {
		t.Fatal(err)
	}
	if len(queuer.calls) != 2 {
		t.Fatalf("fetch after fingerprint change queued %d chapter-thumb batches total, want 2", len(queuer.calls))
	}
}
