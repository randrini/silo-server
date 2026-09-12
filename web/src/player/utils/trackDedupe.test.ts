import { describe, expect, it } from "vitest";

import type { PlayerAudioTrack, PlayerSubtitleInfo } from "../types";
import { dedupeAudioTracks, dedupeSubtitleTracks } from "./trackDedupe";

function audioTrack(overrides: Partial<PlayerAudioTrack> = {}): PlayerAudioTrack {
  return {
    title: "English AC3",
    codec: "ac3",
    channels: 6,
    layout: "5.1",
    language: "en",
    ...overrides,
  };
}

function subtitleTrack(overrides: Partial<PlayerSubtitleInfo> = {}): PlayerSubtitleInfo {
  return {
    index: 0,
    language: "en",
    codec: "pgs",
    label: "English",
    source: "embedded",
    url: "",
    ...overrides,
  };
}

describe("dedupeAudioTracks", () => {
  it("collapses identical descriptors and keeps original inventory positions", () => {
    const tracks = [
      // Same descriptor at two container indexes (7 and 9); the first wins.
      audioTrack({ index: 7 }),
      audioTrack({ index: 9 }),
      audioTrack({ language: "fr", title: "French AC3", codec: "aac", channels: 2, index: 10 }),
    ];

    const deduped = dedupeAudioTracks(tracks);

    expect(deduped).toHaveLength(2);
    expect(deduped[0]).toMatchObject({ index: 0, track: { title: "English AC3" } });
    // The French entry keeps position 2: collapsing the duplicate did not
    // renumber the entries after it.
    expect(deduped[1]).toMatchObject({ index: 2, track: { title: "French AC3" } });
  });

  it("does not collapse tracks whose language, codec or channel layout differs", () => {
    const deduped = dedupeAudioTracks([
      audioTrack(),
      audioTrack({ language: "es" }),
      audioTrack({ codec: "eac3" }),
      audioTrack({ channels: 2, layout: "2.0" }),
    ]);

    expect(deduped).toHaveLength(4);
  });

  it("collapses otherwise identical tracks that differ only by the default flag", () => {
    // `default` is a selection hint, not presentation identity: a container can
    // mark one of two otherwise identical streams default. They are one menu
    // row, and the retained track carries the badge.
    const deduped = dedupeAudioTracks([
      audioTrack({ default: true }),
      audioTrack({ default: false }),
    ]);

    expect(deduped).toHaveLength(1);
    expect(deduped[0]?.track.default).toBe(true);
  });

  it("keeps same-language tracks at different bitrates distinct", () => {
    const deduped = dedupeAudioTracks([audioTrack({ bitrate: 768 }), audioTrack({ bitrate: 384 })]);

    expect(deduped).toHaveLength(2);
  });

  it("collapses index-only duplicates that report the same bitrate", () => {
    const deduped = dedupeAudioTracks([
      audioTrack({ bitrate: 768, index: 4 }),
      audioTrack({ bitrate: 768, index: 6 }),
    ]);

    expect(deduped).toHaveLength(1);
    expect(deduped[0]?.index).toBe(0);
  });

  it("normalizes a languages list against the single language tag", () => {
    const deduped = dedupeAudioTracks([
      audioTrack({ languages: ["en"], language: undefined }),
      audioTrack({ language: "en" }),
    ]);

    expect(deduped).toHaveLength(1);
  });
});

describe("dedupeSubtitleTracks", () => {
  it("collapses identical descriptors and keeps the first ordinal", () => {
    const deduped = dedupeSubtitleTracks([
      subtitleTrack({ index: 13 }),
      subtitleTrack({ index: 14 }),
      subtitleTrack({ index: 23 }),
    ]);

    expect(deduped).toHaveLength(1);
    expect(deduped[0]?.index).toBe(13);
  });

  it("keeps tracks that differ in forced, hearing-impaired, title, language or codec", () => {
    const deduped = dedupeSubtitleTracks([
      subtitleTrack({ index: 0 }),
      subtitleTrack({ index: 1, forced: true }),
      subtitleTrack({ index: 2, hearing_impaired: true }),
      subtitleTrack({ index: 3, label: "English (SDH)" }),
      subtitleTrack({ index: 4, language: "fr" }),
      subtitleTrack({ index: 5, codec: "srt" }),
    ]);

    expect(deduped).toHaveLength(6);
  });

  it("keeps an embedded and an external track with the same descriptors distinct", () => {
    const deduped = dedupeSubtitleTracks([
      subtitleTrack({ index: 0, source: "embedded" }),
      subtitleTrack({ index: 1, source: "external" }),
    ]);

    expect(deduped).toHaveLength(2);
  });
});
