// @vitest-environment jsdom

import { fireEvent, render, screen } from "@testing-library/react";
import { createElement } from "react";
import { describe, expect, it, vi } from "vitest";

import type { PlayerSubtitleInfo } from "../types";
import { SubtitleMenu } from "./SubtitleMenu";

// The appearance panel reads settings through react-query; the menu's own
// rendering does not need a provider.
vi.mock("./SubtitleAppearancePanel", () => ({
  SubtitleAppearancePanel: () => null,
}));

function subtitleTrack(overrides: Partial<PlayerSubtitleInfo> = {}): PlayerSubtitleInfo {
  return {
    index: 0,
    language: "en",
    codec: "pgs",
    label: "English",
    source: "embedded",
    url: "/stream/session-1/subtitles/0.sup",
    ...overrides,
  };
}

function renderMenu(tracks: PlayerSubtitleInfo[], onSelect = vi.fn()) {
  const view = render(
    createElement(SubtitleMenu, {
      tracks,
      activeIndex: null,
      onSelect,
      delayMs: 0,
      onDelayChange: () => {},
    }),
  );
  fireEvent.click(screen.getByRole("button", { name: "Enable captions" }));
  return { view, onSelect };
}

describe("SubtitleMenu", () => {
  it("collapses identical probed streams to one row and keeps the first ordinal", () => {
    const { onSelect } = renderMenu([
      subtitleTrack({ index: 13 }),
      subtitleTrack({ index: 14 }),
      subtitleTrack({ index: 23 }),
    ]);

    const rows = screen.getAllByRole("menuitem");
    // Off + one deduped track + Appearance.
    expect(rows).toHaveLength(3);
    const trackRow = screen.getByRole("menuitem", { name: /English/ });
    expect(trackRow).toBeTruthy();

    fireEvent.click(trackRow);
    expect(onSelect).toHaveBeenCalledWith(13);
  });

  it("keeps tracks that differ in forced or hearing-impaired flags", () => {
    renderMenu([
      subtitleTrack({ index: 0 }),
      subtitleTrack({ index: 1, forced: true }),
      subtitleTrack({ index: 2, hearing_impaired: true }),
    ]);

    // Off + three distinct tracks + Appearance.
    expect(screen.getAllByRole("menuitem")).toHaveLength(5);
  });
});
