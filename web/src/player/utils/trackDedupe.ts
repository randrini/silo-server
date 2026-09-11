import type { PlayerAudioTrack, PlayerSubtitleInfo } from "../types";

/**
 * A probed audio track paired with the position it occupied in the inventory
 * the menu was given.
 *
 * Selection is by inventory position, not by the track's absolute container
 * stream `index`: the v3 protocol's `selected_tracks.audio.index` is the track's
 * array position, and the server resolves it against its own audio-tracks list.
 * Deduping compacts a copy of the list for display only, so the retained entry
 * keeps its original position rather than the position it happens to occupy
 * after entries ahead of it collapsed.
 */
export interface DedupedAudioTrack {
  track: PlayerAudioTrack;
  /** Position in the original inventory; the menu echoes this for selection. */
  index: number;
}

function normalize(value: string | undefined | null): string {
  return (value ?? "").trim().toLowerCase();
}

/**
 * Canonical language identity for a track. The full advertised list and the
 * single language tag describe the same track, so both shapes normalize to one
 * sorted set ("en", "en+es") and a MULTi track is not split from its duplicate.
 */
function normalizeLanguages(track: PlayerAudioTrack): string {
  const languages = track.languages?.length
    ? track.languages
    : track.language
      ? [track.language]
      : [];
  const normalized = [...new Set(languages.map(normalize).filter(Boolean))];
  normalized.sort();
  return normalized.join("+");
}

/** Stable presentation identity for an audio track, excluding the container index. */
function audioIdentity(track: PlayerAudioTrack): string {
  return [
    normalize(track.codec),
    track.channels ?? "",
    normalize(track.layout),
    normalizeLanguages(track),
    normalize(track.title) || normalize(track.embedded_title),
    track.default ? "default" : "",
  ].join("|");
}

/**
 * Collapses probed audio streams whose menu descriptors are identical.
 *
 * A container can carry several streams that differ only by ffprobe index —
 * the same codec, layout, channels, language and title repeated at distinct
 * positions. They are real, distinct streams but one menu row of noise. The
 * first (lowest) occurrence wins and keeps its inventory position; later
 * duplicates are dropped. Tracks that differ in any descriptor are kept.
 */
export function dedupeAudioTracks(tracks: PlayerAudioTrack[]): DedupedAudioTrack[] {
  const seen = new Set<string>();
  const deduped: DedupedAudioTrack[] = [];
  tracks.forEach((track, index) => {
    const identity = audioIdentity(track);
    if (seen.has(identity)) return;
    seen.add(identity);
    deduped.push({ track, index });
  });
  return deduped;
}

/** Stable presentation identity for a subtitle track, excluding its ordinal. */
function subtitleIdentity(track: PlayerSubtitleInfo): string {
  return [
    normalize(track.source),
    normalize(track.codec),
    normalize(track.language),
    track.forced ? "forced" : "",
    track.hearing_impaired ? "hearing-impaired" : "",
    // The player only carries the resolved label; `embedded_title` is folded
    // into it when the inventory is mapped, so the label is the title here.
    normalize(track.label),
  ].join("|");
}

/**
 * Collapses probed subtitle streams whose menu rows are identical.
 *
 * The plan's `index` (the combined ordinal the server assigned) is the
 * selection identity and is preserved on the retained entry. Tracks that differ
 * in forced, hearing-impaired, source, codec, language or title are legitimately
 * distinct options and are never collapsed.
 */
export function dedupeSubtitleTracks(tracks: PlayerSubtitleInfo[]): PlayerSubtitleInfo[] {
  const seen = new Set<string>();
  const deduped: PlayerSubtitleInfo[] = [];
  for (const track of tracks) {
    const identity = subtitleIdentity(track);
    if (seen.has(identity)) continue;
    seen.add(identity);
    deduped.push(track);
  }
  return deduped;
}
