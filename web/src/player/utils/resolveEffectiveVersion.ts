/**
 * Resolves which catalog version row a session's plan is actually playing
 * against.
 *
 * The server can collapse a neutral `VIRTUAL` requested row into a concrete
 * candidate row (for example a 1080p profile) without changing
 * `effective_media_file_id`, which stays the collapsed id. When the plan
 * publishes `effective_virtual_uri`, the candidate is matched by its file path;
 * that is the identity the version, audio and subtitle menus must adopt.
 *
 * When the plan names no virtual URI (ordinary files, older plans) this falls
 * back to matching `mediaFileId`, exactly as the menus did before.
 */
export interface EffectiveVersionIdentity {
  /** The collapsed catalog row the plan was planned against. */
  mediaFileId: number | null;
  /** The effective candidate's file path when a virtual row was resolved. */
  effectiveVirtualUri: string | null;
}

export function resolveEffectiveVersion<T extends { file_id: number; file_path?: string }>(
  versions: readonly T[],
  identity: EffectiveVersionIdentity,
): T | undefined {
  const effectiveUri = identity.effectiveVirtualUri;
  if (effectiveUri) {
    const byPath = versions.find((version) => version.file_path === effectiveUri);
    if (byPath) return byPath;
  }
  if (identity.mediaFileId == null) return undefined;
  return versions.find((version) => version.file_id === identity.mediaFileId);
}
