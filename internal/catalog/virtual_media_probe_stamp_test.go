package catalog

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestVirtualMediaRegistrationDoesNotStampProbe requires a migrated database
// via SILO_TEST_DATABASE_URL. Registration stores declared display hints
// (provider-declared languages) but it is not an ffprobe: it must leave
// probe_updated_at NULL on a new row and preserve whatever a real probe already
// recorded when a rotated result hash adopts the row. Stamping now() made the
// row look probed while its track inventory was still only a language hint, so
// the stream was never repaired with real ffprobe data.
func TestVirtualMediaRegistrationDoesNotStampProbe(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(999,'TestVirtualProbe','movies',true)`); err != nil {
		t.Fatalf("seed virtual media folder: %v", err)
	}
	reg := NewVirtualMediaRegistrar(pool)

	movie := VirtualMedia{
		LibraryID:      "999",
		MediaType:      "movie",
		Title:          "Probe Stamp Film",
		IMDbID:         "tt70000001",
		TMDBID:         "70000001",
		Source:         "provider-probe-stamp",
		RuntimeMinutes: 100,
		VirtualURI:     "virtual://movie/tt70000001?result=hash-one",
		Container:      "mkv",
		Resolution:     "1080p",
		CodecVideo:     "h264",
		AudioLanguages: []string{"eng", "fra"},
	}

	// A fresh registration inserts the row with no probe timestamp.
	res, err := reg.UpsertVirtualMedia(ctx, 11, movie)
	if err != nil {
		t.Fatalf("register virtual media: %v", err)
	}
	probeSource, probeUpdatedAt, audioTracks := loadVirtualProbeState(t, ctx, pool, res.MediaID, movie.VirtualURI)
	if probeSource != "virtual" {
		t.Fatalf("probe_source = %q, want virtual", probeSource)
	}
	if probeUpdatedAt != nil {
		t.Fatalf("fresh registration stamped probe_updated_at = %v, want NULL", probeUpdatedAt)
	}
	// Declared languages remain display hints stored in audio_tracks; only the
	// probe stamping changes.
	var declared []struct {
		Language string `json:"language"`
	}
	if err := json.Unmarshal([]byte(audioTracks), &declared); err != nil {
		t.Fatalf("decode declared audio tracks %q: %v", audioTracks, err)
	}
	if len(declared) != 2 || declared[0].Language != "eng" || declared[1].Language != "fra" {
		t.Fatalf("declared audio languages = %s, want eng+fra", audioTracks)
	}

	// Rotating to a new result hash adopts the unprobed row; it must stay NULL.
	movie.VirtualURI = "virtual://movie/tt70000001?result=hash-two"
	if _, err := reg.UpsertVirtualMedia(ctx, 11, movie); err != nil {
		t.Fatalf("rotate unprobed virtual media: %v", err)
	}
	if _, probeUpdatedAt, _ = loadVirtualProbeState(t, ctx, pool, res.MediaID, movie.VirtualURI); probeUpdatedAt != nil {
		t.Fatalf("adoption of an unprobed row stamped probe_updated_at = %v, want NULL", probeUpdatedAt)
	}

	// A row that a real probe already stamped keeps its timestamp on adoption.
	probedAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx,
		`UPDATE media_files SET probe_source='local', probe_updated_at=$1 WHERE content_id=$2 AND file_path=$3`,
		probedAt, res.MediaID, movie.VirtualURI); err != nil {
		t.Fatalf("simulate real probe: %v", err)
	}
	movie.VirtualURI = "virtual://movie/tt70000001?result=hash-three"
	if _, err := reg.UpsertVirtualMedia(ctx, 11, movie); err != nil {
		t.Fatalf("rotate probed virtual media: %v", err)
	}
	if _, probeUpdatedAt, _ = loadVirtualProbeState(t, ctx, pool, res.MediaID, movie.VirtualURI); probeUpdatedAt == nil || !probeUpdatedAt.Equal(probedAt) {
		t.Fatalf("adoption of a probed row changed probe_updated_at to %v, want %v", probeUpdatedAt, probedAt)
	}
}

// TestVirtualMediaVariantRegistrationDoesNotStampProbe covers the variant insert
// path, which shares the same registration semantics as the base file path.
func TestVirtualMediaVariantRegistrationDoesNotStampProbe(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(999,'TestVirtualProbe','movies',true)`); err != nil {
		t.Fatalf("seed virtual media folder: %v", err)
	}
	reg := NewVirtualMediaRegistrar(pool)

	uri := "virtual://movie/tt70000002?profile=1080p&result=hash-one"
	in := VirtualMedia{
		LibraryID:      "999",
		MediaType:      "movie",
		Title:          "Probe Stamp Variant Film",
		IMDbID:         "tt70000002",
		TMDBID:         "70000002",
		Source:         "provider-probe-stamp",
		RuntimeMinutes: 100,
		Variants: []VirtualMediaVariant{{
			VirtualURI:     uri,
			Label:          "1080p",
			Resolution:     "1080p",
			CodecVideo:     "h264",
			AudioLanguages: []string{"eng"},
		}},
	}

	res, err := reg.UpsertVirtualMedia(ctx, 11, in)
	if err != nil {
		t.Fatalf("register virtual media variant: %v", err)
	}
	if _, probeUpdatedAt, _ := loadVirtualProbeState(t, ctx, pool, res.MediaID, uri); probeUpdatedAt != nil {
		t.Fatalf("variant registration stamped probe_updated_at = %v, want NULL", probeUpdatedAt)
	}

	in.Variants[0].VirtualURI = "virtual://movie/tt70000002?profile=1080p&result=hash-two"
	if _, err := reg.UpsertVirtualMedia(ctx, 11, in); err != nil {
		t.Fatalf("rotate virtual media variant: %v", err)
	}
	if _, probeUpdatedAt, _ := loadVirtualProbeState(t, ctx, pool, res.MediaID, in.Variants[0].VirtualURI); probeUpdatedAt != nil {
		t.Fatalf("variant adoption stamped probe_updated_at = %v, want NULL", probeUpdatedAt)
	}
}

func loadVirtualProbeState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, contentID, filePath string) (string, *time.Time, string) {
	t.Helper()
	var (
		probeSource    string
		probeUpdatedAt *time.Time
		audioTracks    string
	)
	if err := pool.QueryRow(ctx,
		`SELECT probe_source, probe_updated_at, audio_tracks::text FROM media_files WHERE content_id=$1 AND file_path=$2`,
		contentID, filePath).Scan(&probeSource, &probeUpdatedAt, &audioTracks); err != nil {
		t.Fatalf("load probe state for %s: %v", filePath, err)
	}
	return probeSource, probeUpdatedAt, audioTracks
}
