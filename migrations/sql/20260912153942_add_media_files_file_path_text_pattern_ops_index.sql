-- +goose NO TRANSACTION

-- +goose Up
-- Left-anchored prefix lookups on file_path (virtual neutral-path matching in
-- internal/scanner/file_repo.go GetVirtualCandidateByNeutralPath) cannot use
-- the existing folder-leading pattern index (media_folder_id, file_path
-- text_pattern_ops); give the bare file_path prefix its own index so the
-- lookup is index-backed.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_media_files_file_path_pattern
    ON media_files (file_path text_pattern_ops);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS idx_media_files_file_path_pattern;
