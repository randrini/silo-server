package scanner

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/models"
)

func TestReplaceVirtualCandidatesIsScopedToLibrary(t *testing.T) {
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)

	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("virtual-candidate-scope-%d", suffix)
	basePath := fmt.Sprintf("virtual://movie/tt%d?profile=1080p", suffix)
	firstPath := basePath + "&result=first"
	secondPath := basePath + "&result=second"
	var folderA, folderB int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Candidate A %d", suffix)).Scan(&folderA); err != nil {
		t.Fatalf("seed first folder: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Candidate B %d", suffix)).Scan(&folderB); err != nil {
		t.Fatalf("seed second folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_item_libraries WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=ANY($1::int[])`, []int{folderA, folderB})
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,status,genres)
		VALUES($1,'movie','Candidate Scope','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed candidate item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_item_libraries(content_id,media_folder_id)
		VALUES($1,$2),($1,$3)`, contentID, folderA, folderB); err != nil {
		t.Fatalf("seed candidate library links: %v", err)
	}

	repo := NewFileRepository(pool)
	source := func(folderID int) *models.MediaFile {
		return &models.MediaFile{
			ContentID:                  contentID,
			MediaFolderID:              folderID,
			FilePath:                   basePath,
			VirtualOwnerInstallationID: 91,
		}
	}
	first := []VirtualCandidate{{URI: firstPath, Label: "1080p"}}
	if err := repo.ReplaceVirtualCandidates(ctx, source(folderA), first); err != nil {
		t.Fatalf("replace first-library candidates: %v", err)
	}
	if err := repo.ReplaceVirtualCandidates(ctx, source(folderB), first); err != nil {
		t.Fatalf("replace second-library candidates: %v", err)
	}
	if err := repo.ReplaceVirtualCandidates(ctx, source(folderA), []VirtualCandidate{{URI: secondPath, Label: "1080p"}}); err != nil {
		t.Fatalf("refresh first-library candidates: %v", err)
	}

	var firstA, firstB, secondA int
	if err := pool.QueryRow(ctx, `
		SELECT
		  count(*) FILTER(WHERE media_folder_id=$2 AND file_path=$4),
		  count(*) FILTER(WHERE media_folder_id=$3 AND file_path=$4),
		  count(*) FILTER(WHERE media_folder_id=$2 AND file_path=$5)
		FROM media_files
		WHERE content_id=$1 AND virtual_owner_installation_id=91`,
		contentID, folderA, folderB, firstPath, secondPath,
	).Scan(&firstA, &firstB, &secondA); err != nil {
		t.Fatalf("inspect library-scoped candidates: %v", err)
	}
	if firstA != 0 || firstB != 1 || secondA != 1 {
		t.Fatalf("candidate rows firstA=%d firstB=%d secondA=%d, want 0/1/1", firstA, firstB, secondA)
	}
}

// TestReplaceVirtualCandidatesRetainsLastPlayed covers the retention rule in
// ReplaceVirtualCandidates: provider re-lists churn result ids, so a stale
// candidate that is some user's user_watch_progress.last_file_id must survive,
// an unplayed stale candidate must still be deleted, and a retained row that is
// re-listed with the same URI must be updated in place rather than duplicated.
func TestReplaceVirtualCandidatesRetainsLastPlayed(t *testing.T) {
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)

	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("virtual-retain-last-played-%d", suffix)
	basePath := fmt.Sprintf("virtual://movie/tt%d?profile=1080p", suffix)
	playedPath := basePath + "&result=played"
	unplayedPath := basePath + "&result=unplayed"
	relistedPath := basePath + "&result=relisted"

	var folderID, userID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Retain Last Played %d", suffix)).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO users(username, role) VALUES($1,'user') RETURNING id`,
		fmt.Sprintf("retain-last-played-%d", suffix)).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM user_watch_progress WHERE user_id=$1`, userID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_item_libraries WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, folderID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, userID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,status,genres)
		VALUES($1,'movie','Retain Last Played','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_item_libraries(content_id,media_folder_id)
		VALUES($1,$2)`, contentID, folderID); err != nil {
		t.Fatalf("seed item library: %v", err)
	}

	seed := func(path string, failed bool) int {
		var failedAt *time.Time
		if failed {
			now := time.Now()
			failedAt = &now
		}
		var id int
		if err := pool.QueryRow(ctx, `
			INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id,failed_at)
			VALUES($1,$2,$3,0,'mkv',5,$4) RETURNING id`,
			contentID, folderID, path, failedAt).Scan(&id); err != nil {
			t.Fatalf("seed virtual file %q: %v", path, err)
		}
		return id
	}
	playedID := seed(playedPath, false)
	_ = seed(unplayedPath, false)
	// The re-listed row is seeded failed to prove a fresh listing clears it.
	relistedID := seed(relistedPath, true)

	if _, err := pool.Exec(ctx, `
		INSERT INTO user_watch_progress(user_id, profile_id, media_item_id, position_seconds, duration_seconds, last_file_id)
		VALUES($1,'default',$2,100,7200,$3)`, userID, contentID, playedID); err != nil {
		t.Fatalf("seed watch progress: %v", err)
	}

	repo := NewFileRepository(pool)
	source := &models.MediaFile{
		ContentID:                  contentID,
		MediaFolderID:              folderID,
		FilePath:                   basePath,
		VirtualOwnerInstallationID: 5,
	}
	// The re-list drops played and unplayed; only relisted is offered again.
	if err := repo.ReplaceVirtualCandidates(ctx, source, []VirtualCandidate{{URI: relistedPath, Label: "1080p"}}); err != nil {
		t.Fatalf("replace virtual candidates: %v", err)
	}

	var playedCount, unplayedCount, relistedCount, relistedSameID int
	if err := pool.QueryRow(ctx, `
		SELECT
		  count(*) FILTER(WHERE file_path=$2),
		  count(*) FILTER(WHERE file_path=$3),
		  count(*) FILTER(WHERE file_path=$4),
		  count(*) FILTER(WHERE file_path=$4 AND id=$5)
		FROM media_files
		WHERE content_id=$1 AND virtual_owner_installation_id=5`,
		contentID, playedPath, unplayedPath, relistedPath, relistedID,
	).Scan(&playedCount, &unplayedCount, &relistedCount, &relistedSameID); err != nil {
		t.Fatalf("inspect retained candidates: %v", err)
	}
	if playedCount != 1 {
		t.Fatalf("last-played stale candidate was deleted: count=%d, want 1", playedCount)
	}
	if unplayedCount != 0 {
		t.Fatalf("unplayed stale candidate survived: count=%d, want 0", unplayedCount)
	}
	if relistedCount != 1 || relistedSameID != 1 {
		t.Fatalf("relisted candidate duplicated or replaced: count=%d sameID=%d, want 1/1", relistedCount, relistedSameID)
	}

	// The retained last-played row is untouched.
	var playedPathNow string
	var playedFailed *time.Time
	if err := pool.QueryRow(ctx, `SELECT file_path, failed_at FROM media_files WHERE id=$1`, playedID).
		Scan(&playedPathNow, &playedFailed); err != nil {
		t.Fatalf("fetch retained played row: %v", err)
	}
	if playedPathNow != playedPath || playedFailed != nil {
		t.Fatalf("retained played row changed: path=%q failed=%v, want %q/NULL", playedPathNow, playedFailed, playedPath)
	}

	// The re-listed row keeps its id and has failed_at cleared by the re-list.
	var relistedPathNow string
	var relistedFailed *time.Time
	if err := pool.QueryRow(ctx, `SELECT file_path, failed_at FROM media_files WHERE id=$1`, relistedID).
		Scan(&relistedPathNow, &relistedFailed); err != nil {
		t.Fatalf("fetch relisted row: %v", err)
	}
	if relistedPathNow != relistedPath || relistedFailed != nil {
		t.Fatalf("relisted row not updated: path=%q failed=%v, want %q/NULL", relistedPathNow, relistedFailed, relistedPath)
	}
}

func TestReplaceVirtualCandidatesPreservesBarePlaceholder(t *testing.T) {
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)

	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("virtual-bare-placeholder-%d", suffix)
	barePath := fmt.Sprintf("virtual://movie/tt%d", suffix)
	candidatePath := barePath + "?result=beststream123"

	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Placeholder Folder %d", suffix)).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_item_libraries WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, folderID)
	})

	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,status,genres)
		VALUES($1,'movie','Placeholder Scope','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	// Seed initial bare placeholder file
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id)
		VALUES($1,$2,$3,0,'mkv',5)`, contentID, folderID, barePath); err != nil {
		t.Fatalf("seed bare placeholder file: %v", err)
	}

	repo := NewFileRepository(pool)
	source := &models.MediaFile{
		ContentID:                  contentID,
		MediaFolderID:              folderID,
		FilePath:                   barePath,
		VirtualOwnerInstallationID: 5,
	}

	if err := repo.ReplaceVirtualCandidates(ctx, source, []VirtualCandidate{{URI: candidatePath, Label: "2160p"}}); err != nil {
		t.Fatalf("replace virtual candidates: %v", err)
	}

	var bareCount, candidateCount int
	if err := pool.QueryRow(ctx, `
		SELECT
		  count(*) FILTER(WHERE file_path=$2),
		  count(*) FILTER(WHERE file_path=$3)
		FROM media_files
		WHERE content_id=$1 AND virtual_owner_installation_id=5`,
		contentID, barePath, candidatePath,
	).Scan(&bareCount, &candidateCount); err != nil {
		t.Fatalf("inspect files: %v", err)
	}

	if bareCount != 1 || candidateCount != 1 {
		t.Fatalf("expected bareCount=1 candidateCount=1, got bare=%d candidate=%d", bareCount, candidateCount)
	}
}

func TestReplaceVirtualResultPin_PostgresCAS(t *testing.T) {
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)

	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("virtual-pin-cas-%d", suffix)
	deadPath := fmt.Sprintf("virtual://movie/tt%d?result=dead-candidate", suffix)
	livePath := fmt.Sprintf("virtual://movie/tt%d?result=live-winner", suffix)
	stalePath := fmt.Sprintf("virtual://movie/tt%d?result=stale-loser", suffix)

	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Pin CAS %d", suffix)).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_item_libraries WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, folderID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,status,genres)
		VALUES($1,'movie','Pin CAS Item','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_item_libraries(content_id,media_folder_id)
		VALUES($1,$2)`, contentID, folderID); err != nil {
		t.Fatalf("seed item library: %v", err)
	}

	var fileID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id)
		VALUES($1,$2,$3,0,'mkv',5) RETURNING id`, contentID, folderID, deadPath).Scan(&fileID); err != nil {
		t.Fatalf("seed pinned virtual file: %v", err)
	}

	repo := NewFileRepository(pool)

	// 1. CAS with wrong expected path should return (false, nil) and not modify row
	replaced, err := repo.ReplaceVirtualResultPin(ctx, fileID, "virtual://movie/tt1234?result=wrong", livePath)
	if err != nil {
		t.Fatalf("ReplaceVirtualResultPin error on mismatch: %v", err)
	}
	if replaced {
		t.Fatal("ReplaceVirtualResultPin returned true on mismatched expectedPath")
	}

	var currentPath string
	if err := pool.QueryRow(ctx, `SELECT file_path FROM media_files WHERE id=$1`, fileID).Scan(&currentPath); err != nil {
		t.Fatalf("fetch current path: %v", err)
	}
	if currentPath != deadPath {
		t.Fatalf("file path was modified on failed CAS: got %q, want %q", currentPath, deadPath)
	}

	// 2. CAS with matching expected path should return (true, nil) and update row
	replaced, err = repo.ReplaceVirtualResultPin(ctx, fileID, deadPath, livePath)
	if err != nil {
		t.Fatalf("ReplaceVirtualResultPin error on match: %v", err)
	}
	if !replaced {
		t.Fatal("ReplaceVirtualResultPin returned false on matching expectedPath")
	}

	if err := pool.QueryRow(ctx, `SELECT file_path FROM media_files WHERE id=$1`, fileID).Scan(&currentPath); err != nil {
		t.Fatalf("fetch current path: %v", err)
	}
	if currentPath != livePath {
		t.Fatalf("file path was not updated on successful CAS: got %q, want %q", currentPath, livePath)
	}

	// 3. Subsequent CAS with original deadPath should fail because row is now livePath
	replaced, err = repo.ReplaceVirtualResultPin(ctx, fileID, deadPath, stalePath)
	if err != nil {
		t.Fatalf("ReplaceVirtualResultPin error on stale: %v", err)
	}
	if replaced {
		t.Fatal("ReplaceVirtualResultPin returned true on stale expectedPath")
	}
}

func TestReplaceVirtualResultPin_CollisionUnpinsInsteadOfErroring(t *testing.T) {
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)

	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("virtual-pin-collision-%d", suffix)
	deadPath := fmt.Sprintf("virtual://movie/tt%d?result=dead-candidate", suffix)
	livePath := fmt.Sprintf("virtual://movie/tt%d?result=live-winner", suffix)
	neutralPath := fmt.Sprintf("virtual://movie/tt%d", suffix)

	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Pin Collision %d", suffix)).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_item_libraries WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, folderID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,status,genres)
		VALUES($1,'movie','Pin Collision Item','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_item_libraries(content_id,media_folder_id)
		VALUES($1,$2)`, contentID, folderID); err != nil {
		t.Fatalf("seed item library: %v", err)
	}

	// Row A: the dead-pinned row that playback is trying to repoint.
	var deadFileID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id)
		VALUES($1,$2,$3,0,'mkv',5) RETURNING id`, contentID, folderID, deadPath).Scan(&deadFileID); err != nil {
		t.Fatalf("seed dead-pinned file: %v", err)
	}
	// Row B: the sibling live row that already owns the winning (file_path, owner, folder) tuple.
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id)
		VALUES($1,$2,$3,0,'mkv',5)`, contentID, folderID, livePath); err != nil {
		t.Fatalf("seed sibling live file: %v", err)
	}

	repo := NewFileRepository(pool)

	// Repointing the dead-pinned row at the live path collides with the sibling row:
	// the pin must be stripped from this row and (false, nil) returned, not an error.
	replaced, err := repo.ReplaceVirtualResultPin(ctx, deadFileID, deadPath, livePath)
	if err != nil {
		t.Fatalf("ReplaceVirtualResultPin error on collision: %v", err)
	}
	if replaced {
		t.Fatal("ReplaceVirtualResultPin returned true on collision; pin should be stripped instead")
	}

	var deadPathNow, livePathNow string
	if err := pool.QueryRow(ctx, `SELECT file_path FROM media_files WHERE id=$1`, deadFileID).Scan(&deadPathNow); err != nil {
		t.Fatalf("fetch dead-pinned row path: %v", err)
	}
	if deadPathNow != neutralPath {
		t.Fatalf("dead-pinned row was not unpinned on collision: got %q, want %q", deadPathNow, neutralPath)
	}
	if err := pool.QueryRow(ctx, `SELECT file_path FROM media_files WHERE file_path=$1 AND virtual_owner_installation_id=5 AND media_folder_id=$2`, livePath, folderID).Scan(&livePathNow); err != nil {
		t.Fatalf("fetch sibling live row path: %v", err)
	}
	if livePathNow != livePath {
		t.Fatalf("sibling live row was modified: got %q, want %q", livePathNow, livePath)
	}
}

func TestClearVirtualResultPin_StripsResultPreservingOtherParams(t *testing.T) {
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)

	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("virtual-pin-clear-%d", suffix)

	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Pin Clear %d", suffix)).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_item_libraries WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, folderID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,status,genres)
		VALUES($1,'movie','Pin Clear Item','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_item_libraries(content_id,media_folder_id)
		VALUES($1,$2)`, contentID, folderID); err != nil {
		t.Fatalf("seed item library: %v", err)
	}

	repo := NewFileRepository(pool)

	seed := func(path string) int {
		var id int
		if err := pool.QueryRow(ctx, `
			INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id)
			VALUES($1,$2,$3,0,'mkv',5) RETURNING id`, contentID, folderID, path).Scan(&id); err != nil {
			t.Fatalf("seed pinned virtual file %q: %v", path, err)
		}
		return id
	}
	fetch := func(id int) string {
		var path string
		if err := pool.QueryRow(ctx, `SELECT file_path FROM media_files WHERE id=$1`, id).Scan(&path); err != nil {
			t.Fatalf("fetch path for file %d: %v", id, err)
		}
		return path
	}

	// result-first order: ?result=abc&profile=1080p -> ?profile=1080p
	resultFirst := fmt.Sprintf("virtual://movie/tt%d?result=abc&profile=1080p", suffix)
	resultFirstID := seed(resultFirst)
	if err := repo.ClearVirtualResultPin(ctx, resultFirstID); err != nil {
		t.Fatalf("ClearVirtualResultPin error on result-first: %v", err)
	}
	if got := fetch(resultFirstID); got != fmt.Sprintf("virtual://movie/tt%d?profile=1080p", suffix) {
		t.Fatalf("result-first order: got %q, want %q", got, fmt.Sprintf("virtual://movie/tt%d?profile=1080p", suffix))
	}

	// profile-first order: ?profile=1080p&result=abc -> ?profile=1080p
	profileFirst := fmt.Sprintf("virtual://movie/tt%d?profile=1080p&result=abc", suffix+1)
	profileFirstID := seed(profileFirst)
	if err := repo.ClearVirtualResultPin(ctx, profileFirstID); err != nil {
		t.Fatalf("ClearVirtualResultPin error on profile-first: %v", err)
	}
	if got := fetch(profileFirstID); got != fmt.Sprintf("virtual://movie/tt%d?profile=1080p", suffix+1) {
		t.Fatalf("profile-first order: got %q, want %q", got, fmt.Sprintf("virtual://movie/tt%d?profile=1080p", suffix+1))
	}

	// trailing-only: ?result=abc -> bare path
	trailingOnly := fmt.Sprintf("virtual://movie/tt%d?result=abc", suffix+2)
	trailingOnlyID := seed(trailingOnly)
	if err := repo.ClearVirtualResultPin(ctx, trailingOnlyID); err != nil {
		t.Fatalf("ClearVirtualResultPin error on trailing-only: %v", err)
	}
	if got := fetch(trailingOnlyID); got != fmt.Sprintf("virtual://movie/tt%d", suffix+2) {
		t.Fatalf("trailing-only: got %q, want %q", got, fmt.Sprintf("virtual://movie/tt%d", suffix+2))
	}

	// no-pin -> no-op
	noPin := fmt.Sprintf("virtual://movie/tt%d?profile=1080p", suffix+3)
	noPinID := seed(noPin)
	if err := repo.ClearVirtualResultPin(ctx, noPinID); err != nil {
		t.Fatalf("ClearVirtualResultPin error on no-pin: %v", err)
	}
	if got := fetch(noPinID); got != noPin {
		t.Fatalf("no-pin: got %q, want %q", got, noPin)
	}

	// vanished row -> no-op (no error)
	if err := repo.ClearVirtualResultPin(ctx, 999999999); err != nil {
		t.Fatalf("ClearVirtualResultPin error on vanished row: %v", err)
	}
}

func TestReplaceVirtualResultPin_CollisionUnpinsProfileQualified(t *testing.T) {
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)

	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("virtual-pin-collision-profile-%d", suffix)
	deadPath := fmt.Sprintf("virtual://movie/tt%d?profile=1080p&result=dead-candidate", suffix)
	livePath := fmt.Sprintf("virtual://movie/tt%d?profile=1080p&result=live-winner", suffix)
	neutralPath := fmt.Sprintf("virtual://movie/tt%d?profile=1080p", suffix)

	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Pin Collision Profile %d", suffix)).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_item_libraries WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, folderID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,status,genres)
		VALUES($1,'movie','Pin Collision Profile Item','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_item_libraries(content_id,media_folder_id)
		VALUES($1,$2)`, contentID, folderID); err != nil {
		t.Fatalf("seed item library: %v", err)
	}

	// Row A: the dead-pinned row that playback is trying to repoint.
	var deadFileID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id)
		VALUES($1,$2,$3,0,'mkv',5) RETURNING id`, contentID, folderID, deadPath).Scan(&deadFileID); err != nil {
		t.Fatalf("seed dead-pinned file: %v", err)
	}
	// Row B: the sibling live row that already owns the winning (file_path, owner, folder) tuple.
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id)
		VALUES($1,$2,$3,0,'mkv',5)`, contentID, folderID, livePath); err != nil {
		t.Fatalf("seed sibling live file: %v", err)
	}

	repo := NewFileRepository(pool)

	replaced, err := repo.ReplaceVirtualResultPin(ctx, deadFileID, deadPath, livePath)
	if err != nil {
		t.Fatalf("ReplaceVirtualResultPin error on collision: %v", err)
	}
	if replaced {
		t.Fatal("ReplaceVirtualResultPin returned true on collision; pin should be stripped instead")
	}

	var deadPathNow, livePathNow string
	if err := pool.QueryRow(ctx, `SELECT file_path FROM media_files WHERE id=$1`, deadFileID).Scan(&deadPathNow); err != nil {
		t.Fatalf("fetch dead-pinned row path: %v", err)
	}
	if deadPathNow != neutralPath {
		t.Fatalf("dead-pinned row was not unpinned preserving profile: got %q, want %q", deadPathNow, neutralPath)
	}
	if err := pool.QueryRow(ctx, `SELECT file_path FROM media_files WHERE file_path=$1 AND virtual_owner_installation_id=5 AND media_folder_id=$2`, livePath, folderID).Scan(&livePathNow); err != nil {
		t.Fatalf("fetch sibling live row path: %v", err)
	}
	if livePathNow != livePath {
		t.Fatalf("sibling live row was modified: got %q, want %q", livePathNow, livePath)
	}
}

func TestReplaceVirtualResultPin_NoCollisionReplaces(t *testing.T) {
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)

	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("virtual-pin-nocollision-%d", suffix)
	deadPath := fmt.Sprintf("virtual://movie/tt%d?result=dead-candidate", suffix)
	livePath := fmt.Sprintf("virtual://movie/tt%d?result=live-winner", suffix)

	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Pin NoCollision %d", suffix)).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_item_libraries WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, folderID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,status,genres)
		VALUES($1,'movie','Pin NoCollision Item','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_item_libraries(content_id,media_folder_id)
		VALUES($1,$2)`, contentID, folderID); err != nil {
		t.Fatalf("seed item library: %v", err)
	}

	var fileID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id)
		VALUES($1,$2,$3,0,'mkv',5) RETURNING id`, contentID, folderID, deadPath).Scan(&fileID); err != nil {
		t.Fatalf("seed pinned virtual file: %v", err)
	}

	repo := NewFileRepository(pool)

	replaced, err := repo.ReplaceVirtualResultPin(ctx, fileID, deadPath, livePath)
	if err != nil {
		t.Fatalf("ReplaceVirtualResultPin error on no collision: %v", err)
	}
	if !replaced {
		t.Fatal("ReplaceVirtualResultPin returned false on no collision")
	}

	var currentPath string
	if err := pool.QueryRow(ctx, `SELECT file_path FROM media_files WHERE id=$1`, fileID).Scan(&currentPath); err != nil {
		t.Fatalf("fetch current path: %v", err)
	}
	if currentPath != livePath {
		t.Fatalf("file path was not replaced on no collision: got %q, want %q", currentPath, livePath)
	}
}
