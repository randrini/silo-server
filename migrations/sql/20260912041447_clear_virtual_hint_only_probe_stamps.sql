-- +goose Up
-- Rows registered before the virtual registration fix were stamped with
-- probe_updated_at=NOW() while carrying hint-only audio_tracks (language tags
-- with no codec). The playback probe gate reads the stamp as evidence the row
-- was really probed, so those rows never re-probe and keep advertising an
-- inventory that cannot drive track selection. Clear the stamp so the next
-- play performs a real probe. Rows that already carry a codec are real probe
-- results and keep their stamp.
UPDATE media_files SET probe_updated_at = NULL
WHERE probe_source IN ('virtual', 'virtual_collection')
  AND probe_updated_at IS NOT NULL
  AND NOT EXISTS (
    SELECT 1 FROM jsonb_array_elements(audio_tracks) AS t WHERE t ? 'codec'
  );

-- +goose Down
-- Irreversible: repopulating the cleared stamps requires real probes, which
-- cannot be synthesized here. Leaving the stamps NULL is safe: the rows are
-- simply re-probed on the next play.
SELECT 1;
