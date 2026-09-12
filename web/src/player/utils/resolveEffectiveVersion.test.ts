import { describe, expect, it } from "vitest";

import { resolveEffectiveVersion } from "./resolveEffectiveVersion";

const versions = [
  { file_id: 100, file_path: "virtual://movie/tt1?result=all" },
  { file_id: 7, file_path: "/media/Movies/Example (2024)/Example.1080p.mkv" },
  { file_id: 8, file_path: "/media/Movies/Example (2024)/Example.2160p.mkv" },
];

describe("resolveEffectiveVersion", () => {
  it("matches the effective candidate by path when the plan resolved a virtual row", () => {
    expect(
      resolveEffectiveVersion(versions, {
        mediaFileId: 100,
        effectiveVirtualUri: "/media/Movies/Example (2024)/Example.1080p.mkv",
      })?.file_id,
    ).toBe(7);
  });

  it("prefers the path match over the collapsed id", () => {
    expect(
      resolveEffectiveVersion(versions, {
        mediaFileId: 8,
        effectiveVirtualUri: "/media/Movies/Example (2024)/Example.1080p.mkv",
      })?.file_id,
    ).toBe(7);
  });

  it("falls back to the collapsed id when the path matches no version", () => {
    expect(
      resolveEffectiveVersion(versions, {
        mediaFileId: 100,
        effectiveVirtualUri: "/media/Movies/Example (2024)/Example.missing.mkv",
      })?.file_id,
    ).toBe(100);
  });

  it("matches by file_id when no effective virtual URI is published", () => {
    expect(
      resolveEffectiveVersion(versions, { mediaFileId: 8, effectiveVirtualUri: null })?.file_id,
    ).toBe(8);
  });

  it("returns undefined when neither identity matches", () => {
    expect(
      resolveEffectiveVersion(versions, { mediaFileId: 999, effectiveVirtualUri: null }),
    ).toBeUndefined();
  });
});
