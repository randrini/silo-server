package scanner

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestClearVirtualCandidateFailed verifies the failed_at stamp lifecycle for
// the batched version liveness check: ClearVirtualCandidateFailed clears the
// stamp on a virtual row, is a no-op for local rows, and is a no-op for
// vanished rows.
func TestClearVirtualCandidateFailed(t *testing.T) {
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
	contentID := fmt.Sprintf("clear-virtual-failed-%d", suffix)

	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Clear Virtual Failed %d", suffix)).Scan(&folderID); err != nil {
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
		VALUES($1,'movie','Clear Virtual Failed','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_item_libraries(content_id,media_folder_id)
		VALUES($1,$2)`, contentID, folderID); err != nil {
		t.Fatalf("seed item library: %v", err)
	}

	repo := NewFileRepository(pool)

	// Virtual candidate row (container='virtual', virtual:// path).
	virtualPath := fmt.Sprintf("virtual://movie/tt%d?result=dead", suffix)
	var virtualID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id,failed_at)
		VALUES($1,$2,$3,1000,'virtual',7,NOW()) RETURNING id`,
		contentID, folderID, virtualPath).Scan(&virtualID); err != nil {
		t.Fatalf("seed virtual file: %v", err)
	}
	var failedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT failed_at FROM media_files WHERE id=$1`, virtualID).Scan(&failedAt); err != nil {
		t.Fatalf("read virtual failed_at: %v", err)
	}

	// Local row with a failed_at stamp (should never be touched by the clear).
	localPath := fmt.Sprintf("/media/clear-virtual-failed-%d.mkv", suffix)
	var localID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,failed_at)
		VALUES($1,$2,$3,1000,NOW()) RETURNING id`,
		contentID, folderID, localPath).Scan(&localID); err != nil {
		t.Fatalf("seed local file: %v", err)
	}
	var localFailedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT failed_at FROM media_files WHERE id=$1`, localID).Scan(&localFailedAt); err != nil {
		t.Fatalf("read local failed_at: %v", err)
	}

	assertFailedAt := func(fileID int, want bool) {
		t.Helper()
		var failedAt *time.Time
		if err := pool.QueryRow(ctx, `SELECT failed_at FROM media_files WHERE id=$1`, fileID).Scan(&failedAt); err != nil {
			t.Fatalf("read failed_at for file %d: %v", fileID, err)
		}
		if (failedAt != nil) != want {
			t.Fatalf("file %d failed_at set=%v, want %v", fileID, failedAt != nil, want)
		}
	}

	// Clear on the virtual row removes the stamp.
	if err := repo.ClearVirtualCandidateFailed(ctx, virtualID, virtualPath, failedAt); err != nil {
		t.Fatalf("clear virtual candidate failed: %v", err)
	}
	assertFailedAt(virtualID, false)

	// Clear on a local row is a no-op: the stamp survives.
	if err := repo.ClearVirtualCandidateFailed(ctx, localID, localPath, localFailedAt); err != nil {
		t.Fatalf("clear local candidate failed: %v", err)
	}
	assertFailedAt(localID, true)

	// Clear on a vanished row is a no-op (no error).
	if err := repo.ClearVirtualCandidateFailed(ctx, 999999999, "virtual://gone?result=x", nil); err != nil {
		t.Fatalf("clear vanished candidate failed: %v", err)
	}

	// MarkVirtualCandidateFailed still stamps the virtual row (round-trip).
	if err := repo.MarkVirtualCandidateFailed(ctx, virtualID, virtualPath, nil); err != nil {
		t.Fatalf("mark virtual candidate failed: %v", err)
	}
	assertFailedAt(virtualID, true)

	// The model round-trips the stamp through the repository.
	file, err := repo.GetByID(ctx, virtualID)
	if err != nil {
		t.Fatalf("get virtual file: %v", err)
	}
	if file == nil || file.FailedAt == nil {
		t.Fatalf("virtual file failed_at not loaded: %#v", file)
	}
	if file.MissingSince != nil {
		t.Fatalf("virtual file must never carry missing_since: %#v", file)
	}
}

// TestVirtualCandidateFailedFencing verifies the failed_at stamp/clear writes
// are fenced on the candidate identity the liveness check inspected: when the
// row's file_path rotates (candidate replaced) or failed_at changes while a
// resolution is in flight, the stale write is a no-op. The happy paths still
// write.
func TestVirtualCandidateFailedFencing(t *testing.T) {
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
	contentID := fmt.Sprintf("virtual-failed-fencing-%d", suffix)
	originalPath := fmt.Sprintf("virtual://movie/tt%d?result=original", suffix)
	replacementPath := fmt.Sprintf("virtual://movie/tt%d?result=replacement", suffix)

	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Failed Fencing %d", suffix)).Scan(&folderID); err != nil {
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
		VALUES($1,'movie','Failed Fencing Item','matched','{}'::text[])`, contentID); err != nil {
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
		VALUES($1,$2,$3,1000,'virtual',7) RETURNING id`,
		contentID, folderID, originalPath).Scan(&fileID); err != nil {
		t.Fatalf("seed virtual file: %v", err)
	}

	repo := NewFileRepository(pool)
	readFailedAt := func() *time.Time {
		t.Helper()
		var failedAt *time.Time
		if err := pool.QueryRow(ctx, `SELECT failed_at FROM media_files WHERE id=$1`, fileID).Scan(&failedAt); err != nil {
			t.Fatalf("read failed_at: %v", err)
		}
		return failedAt
	}
	readPath := func() string {
		t.Helper()
		var path string
		if err := pool.QueryRow(ctx, `SELECT file_path FROM media_files WHERE id=$1`, fileID).Scan(&path); err != nil {
			t.Fatalf("read file_path: %v", err)
		}
		return path
	}

	t.Run("stale mark after candidate rotation is a no-op", func(t *testing.T) {
		// The check inspected originalPath with failed_at nil; while its
		// resolution was in flight the provider rotated the candidate.
		if _, err := pool.Exec(ctx, `UPDATE media_files SET file_path=$1 WHERE id=$2`, replacementPath, fileID); err != nil {
			t.Fatal(err)
		}
		if err := repo.MarkVirtualCandidateFailed(ctx, fileID, originalPath, nil); err != nil {
			t.Fatalf("stale mark: %v", err)
		}
		if readFailedAt() != nil {
			t.Fatal("stale mark stamped the rotated replacement candidate")
		}
		if readPath() != replacementPath {
			t.Fatalf("stale mark modified file_path: %q", readPath())
		}
	})

	t.Run("stale clear after newer failure is a no-op", func(t *testing.T) {
		// The check inspected replacementPath with failed_at nil; while its
		// resolution was in flight another session stamped the candidate dead.
		if _, err := pool.Exec(ctx, `UPDATE media_files SET failed_at=NOW() WHERE id=$1`, fileID); err != nil {
			t.Fatal(err)
		}
		if err := repo.ClearVirtualCandidateFailed(ctx, fileID, replacementPath, nil); err != nil {
			t.Fatalf("stale clear: %v", err)
		}
		if readFailedAt() == nil {
			t.Fatal("stale clear erased a newer failure stamp")
		}
	})

	t.Run("stale mark after newer failure is a no-op", func(t *testing.T) {
		// The check inspected replacementPath with failed_at nil; a newer
		// failure arrived before the stale dead-pin verdict landed.
		if err := repo.MarkVirtualCandidateFailed(ctx, fileID, replacementPath, nil); err != nil {
			t.Fatalf("stale mark: %v", err)
		}
		if readFailedAt() == nil {
			t.Fatal("stale mark cleared a newer failure stamp")
		}
	})

	t.Run("matching mark writes", func(t *testing.T) {
		observed := readFailedAt()
		if err := repo.MarkVirtualCandidateFailed(ctx, fileID, replacementPath, observed); err != nil {
			t.Fatalf("matching mark: %v", err)
		}
		if readFailedAt() == nil {
			t.Fatal("matching mark did not stamp failed_at")
		}
	})

	t.Run("matching clear writes", func(t *testing.T) {
		observed := readFailedAt()
		if err := repo.ClearVirtualCandidateFailed(ctx, fileID, replacementPath, observed); err != nil {
			t.Fatalf("matching clear: %v", err)
		}
		if readFailedAt() != nil {
			t.Fatal("matching clear did not remove failed_at")
		}
	})
}

// The transport-delivery recovery path: a session retains the candidate it is
// serving even after the catalog row rotates. A late delivery signal must be
// fenced on the DELIVERED identity and the health state observed at transport
// start — a rotation to B or a newer failure on A is never cleared by a late
// delivery of A.
func TestMarkVirtualCandidateRecoveredFencing(t *testing.T) {
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	prefix := fmt.Sprintf("recovered-fencing-%d-", time.Now().UnixNano())
	var library int
	if err := pool.QueryRow(t.Context(), `INSERT INTO media_folders (type,name) VALUES ('movies',$1) RETURNING id`, prefix).Scan(&library); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if _, err := pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, library); err != nil {
			t.Error(err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM media_items WHERE content_id LIKE $1`, prefix+"%"); err != nil {
			t.Error(err)
		}
	})
	exec(`INSERT INTO media_items (content_id,type,title,genres,default_metadata_language) VALUES ($1,'movie','Recovered Fencing','{}','en')`, prefix+"movie")
	exec(`INSERT INTO media_item_libraries (content_id,media_folder_id) VALUES ($1,$2)`, prefix+"movie", library)

	originalPath := fmt.Sprintf("virtual://movie/tt%d?result=original", time.Now().UnixNano())
	replacementPath := fmt.Sprintf("virtual://movie/tt%d?result=replacement", time.Now().UnixNano())
	var fileID int
	if err := pool.QueryRow(t.Context(), `
		INSERT INTO media_files (content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id,failed_at)
		VALUES ($1,$2,$3,1000,'virtual',7,NOW()) RETURNING id`,
		prefix+"movie", library, originalPath).Scan(&fileID); err != nil {
		t.Fatal(err)
	}

	repo := NewFileRepository(pool)
	readRow := func() (path string, failedAt *time.Time) {
		t.Helper()
		if err := pool.QueryRow(t.Context(), `SELECT file_path, failed_at FROM media_files WHERE id=$1`, fileID).Scan(&path, &failedAt); err != nil {
			t.Fatal(err)
		}
		return path, failedAt
	}

	t.Run("late delivery of A does not clear a rotation to B", func(t *testing.T) {
		exec(`UPDATE media_files SET file_path=$1, failed_at=NOW() WHERE id=$2`, originalPath, fileID)
		path, _ := readRow()
		if path != originalPath {
			t.Fatalf("precondition: path = %q", path)
		}
		// The session delivers A; the row then rotates to B (a newer failure
		// stamp is also set on the rotated row by another session). The late
		// delivery signal for A must not clear B's failure.
		exec(`UPDATE media_files SET file_path=$1, failed_at=NOW() WHERE id=$2`, replacementPath, fileID)
		deliveredObserved := time.Now().Add(-time.Minute) // the health state at transport start (A, stamped)
		if err := repo.MarkVirtualCandidateRecovered(t.Context(), fileID, originalPath, &deliveredObserved); err != nil {
			t.Fatal(err)
		}
		path, failedAt := readRow()
		if path != replacementPath {
			t.Fatalf("recovery modified file_path: %q, want %q", path, replacementPath)
		}
		if failedAt == nil {
			t.Fatal("late delivery of A cleared the rotation to B (and its failure)")
		}
	})

	t.Run("late delivery of A does not clear a newer failure on A", func(t *testing.T) {
		exec(`UPDATE media_files SET file_path=$1, failed_at=NOW() WHERE id=$2`, originalPath, fileID)
		// The delivery started with A unstamped; a newer failure lands on A
		// before the delivery completes. The late signal must not clear it.
		firstObserved := (*time.Time)(nil)
		exec(`UPDATE media_files SET failed_at=NOW() WHERE id=$1`, fileID)
		if err := repo.MarkVirtualCandidateRecovered(t.Context(), fileID, originalPath, firstObserved); err != nil {
			t.Fatal(err)
		}
		_, failedAt := readRow()
		if failedAt == nil {
			t.Fatal("late delivery erased a newer failure on the same candidate")
		}
	})

	t.Run("resolved B cannot recover requested A", func(t *testing.T) {
		exec(`UPDATE media_files SET file_path=$1, failed_at=NOW() WHERE id=$2`, originalPath, fileID)
		_, observed := readRow()
		if err := repo.MarkVirtualCandidateRecovered(t.Context(), fileID, replacementPath, observed); err != nil {
			t.Fatal(err)
		}
		path, failedAt := readRow()
		if path != originalPath || failedAt == nil || !failedAt.Equal(*observed) {
			t.Fatalf("B changed A: %s %v", path, failedAt)
		}
	})

	t.Run("newer stamped failure survives", func(t *testing.T) {
		exec(`UPDATE media_files SET file_path=$1, failed_at=NOW() WHERE id=$2`, originalPath, fileID)
		_, observed := readRow()
		exec(`UPDATE media_files SET failed_at=failed_at + INTERVAL '1 second' WHERE id=$1`, fileID)
		if err := repo.MarkVirtualCandidateRecovered(t.Context(), fileID, originalPath, observed); err != nil {
			t.Fatal(err)
		}
		_, failedAt := readRow()
		if failedAt == nil || !failedAt.After(*observed) {
			t.Fatalf("newer failure erased: %v", failedAt)
		}
	})

	t.Run("matching identity and health state recovers", func(t *testing.T) {
		exec(`UPDATE media_files SET file_path=$1, failed_at=NOW() WHERE id=$2`, originalPath, fileID)
		path, failedAt := readRow()
		if failedAt == nil {
			t.Fatal("precondition: candidate must start stamped failed")
		}
		if err := repo.MarkVirtualCandidateRecovered(t.Context(), fileID, path, failedAt); err != nil {
			t.Fatal(err)
		}
		path, failedAt = readRow()
		if failedAt != nil {
			t.Fatalf("matching recovery did not clear failed_at: path=%q failed_at=%v", path, failedAt)
		}
	})
}

// TestMarkVirtualCandidateRecoveredStampsLastDelivered verifies that a real
// delivery records durable last_delivered_at evidence in the same UPDATE that
// clears failed_at, and that the write stays fenced: a late delivery of a
// candidate the row no longer describes stamps nothing.
func TestMarkVirtualCandidateRecoveredStampsLastDelivered(t *testing.T) {
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
	contentID := fmt.Sprintf("recovered-delivered-%d", suffix)
	originalPath := fmt.Sprintf("virtual://movie/tt%d?result=original", suffix)

	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Recovered Delivered %d", suffix)).Scan(&folderID); err != nil {
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
		VALUES($1,'movie','Recovered Delivered','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}

	var fileID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id,failed_at)
		VALUES($1,$2,$3,1000,'virtual',7,NOW()) RETURNING id`,
		contentID, folderID, originalPath).Scan(&fileID); err != nil {
		t.Fatalf("seed virtual file: %v", err)
	}

	repo := NewFileRepository(pool)
	readRow := func() (failedAt, lastDeliveredAt *time.Time) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT failed_at, last_delivered_at FROM media_files WHERE id=$1`, fileID).
			Scan(&failedAt, &lastDeliveredAt); err != nil {
			t.Fatalf("read recovery row: %v", err)
		}
		return failedAt, lastDeliveredAt
	}

	// Never delivered yet.
	if _, lastDeliveredAt := readRow(); lastDeliveredAt != nil {
		t.Fatalf("precondition: last_delivered_at = %v, want NULL", lastDeliveredAt)
	}

	// A late delivery of a candidate the row no longer describes is a no-op:
	// neither failed_at nor last_delivered_at may change.
	if _, err := pool.Exec(ctx, `UPDATE media_files SET file_path=$1 WHERE id=$2`, originalPath+"-rotated", fileID); err != nil {
		t.Fatalf("rotate row: %v", err)
	}
	failedAt, _ := readRow()
	if err := repo.MarkVirtualCandidateRecovered(ctx, fileID, originalPath, failedAt); err != nil {
		t.Fatalf("stale recovery: %v", err)
	}
	if _, lastDeliveredAt := readRow(); lastDeliveredAt != nil {
		t.Fatal("stale recovery stamped last_delivered_at on a rotated row")
	}

	// Matching identity and observed health state: both columns move together.
	if _, err := pool.Exec(ctx, `UPDATE media_files SET file_path=$1, failed_at=NOW(), last_delivered_at=NULL WHERE id=$2`, originalPath, fileID); err != nil {
		t.Fatalf("reset row: %v", err)
	}
	failedAt, _ = readRow()
	if failedAt == nil {
		t.Fatal("precondition: row must start stamped failed")
	}
	if err := repo.MarkVirtualCandidateRecovered(ctx, fileID, originalPath, failedAt); err != nil {
		t.Fatalf("recover: %v", err)
	}
	failedAt, lastDeliveredAt := readRow()
	if failedAt != nil {
		t.Fatalf("recovery did not clear failed_at: %v", failedAt)
	}
	if lastDeliveredAt == nil {
		t.Fatal("recovery did not stamp last_delivered_at")
	}
}

// TestMarkVirtualCandidateFailedDefersKnownGood verifies the delivered-grace
// rule on the failed_at stamp: a known-good row inside
// VirtualCandidateDeliveryGrace is not branded dead by a later failure, while a
// row that never delivered (and a known-good row whose delivery is older than
// the grace) stamps immediately.
func TestMarkVirtualCandidateFailedDefersKnownGood(t *testing.T) {
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
	contentID := fmt.Sprintf("failed-grace-%d", suffix)

	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Failed Grace %d", suffix)).Scan(&folderID); err != nil {
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
		VALUES($1,'movie','Failed Grace','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}

	repo := NewFileRepository(pool)
	seed := func(path string, lastDeliveredAt *time.Time) int {
		var id int
		if err := pool.QueryRow(ctx, `
			INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id,last_delivered_at)
			VALUES($1,$2,$3,1000,'virtual',7,$4) RETURNING id`,
			contentID, folderID, path, lastDeliveredAt).Scan(&id); err != nil {
			t.Fatalf("seed virtual file %q: %v", path, err)
		}
		return id
	}
	readFailedAt := func(id int) *time.Time {
		t.Helper()
		var failedAt *time.Time
		if err := pool.QueryRow(ctx, `SELECT failed_at FROM media_files WHERE id=$1`, id).Scan(&failedAt); err != nil {
			t.Fatalf("read failed_at: %v", err)
		}
		return failedAt
	}

	freshly := time.Now().Add(-time.Hour)
	knownGoodPath := fmt.Sprintf("virtual://movie/tt%d?result=known-good", suffix)
	knownGoodID := seed(knownGoodPath, &freshly)

	// First failure within the grace window: no stamp, so the auto-pick keeps
	// preferring the known-good candidate for a re-verify.
	if err := repo.MarkVirtualCandidateFailed(ctx, knownGoodID, knownGoodPath, nil); err != nil {
		t.Fatalf("mark known-good failed: %v", err)
	}
	if readFailedAt(knownGoodID) != nil {
		t.Fatal("known-good candidate was branded dead inside the delivery grace")
	}

	// Once the delivery evidence is older than the grace, a failure stamps.
	if _, err := pool.Exec(ctx, `UPDATE media_files SET last_delivered_at = NOW() - INTERVAL '8 days' WHERE id=$1`, knownGoodID); err != nil {
		t.Fatalf("age delivery evidence: %v", err)
	}
	if err := repo.MarkVirtualCandidateFailed(ctx, knownGoodID, knownGoodPath, nil); err != nil {
		t.Fatalf("mark aged known-good failed: %v", err)
	}
	if readFailedAt(knownGoodID) == nil {
		t.Fatal("aged known-good candidate was not stamped after the grace expired")
	}

	// A candidate that never delivered stamps on the first failure.
	neverDeliveredPath := fmt.Sprintf("virtual://movie/tt%d?result=never-delivered", suffix)
	neverDeliveredID := seed(neverDeliveredPath, nil)
	if err := repo.MarkVirtualCandidateFailed(ctx, neverDeliveredID, neverDeliveredPath, nil); err != nil {
		t.Fatalf("mark never-delivered failed: %v", err)
	}
	if readFailedAt(neverDeliveredID) == nil {
		t.Fatal("never-delivered candidate was not stamped")
	}
}
