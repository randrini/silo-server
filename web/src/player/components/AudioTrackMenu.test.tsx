// @vitest-environment jsdom

import { fireEvent, render, screen } from "@testing-library/react";
import { createElement } from "react";
import { describe, expect, it, vi } from "vitest";
import { AudioTrackMenu } from "./AudioTrackMenu";

function renderMenu(tracks: Parameters<typeof AudioTrackMenu>[0]["tracks"]) {
  return render(
    createElement(AudioTrackMenu, {
      tracks,
      activeIndex: 0,
      onSelect: () => {},
      currentPosition: 0,
      open: true,
      onOpenChange: () => {},
      hideTrigger: true,
    }),
  );
}

describe("AudioTrackMenu", () => {
  it("joins the languages array into a multi-language descriptor", () => {
    renderMenu([
      {
        title: "English / French / Spanish",
        languages: ["en", "fr", "es"],
        codec: "eac3",
        layout: "5.1",
      },
    ]);
    expect(screen.getByText("English/French/Spanish · 5.1")).toBeTruthy();
  });

  it("falls back to the single language tag when languages is absent", () => {
    renderMenu([{ title: "French DTS", language: "fra", codec: "dts" }]);
    expect(screen.getByText("French")).toBeTruthy();
  });

  it("omits the language segment when no language resolves", () => {
    renderMenu([{ title: "Commentary", codec: "ac3", layout: "2.0" }]);
    expect(screen.getByText("2.0")).toBeTruthy();
  });

  it("keeps the trigger enabled with a single track and opens to that entry", () => {
    render(
      createElement(AudioTrackMenu, {
        tracks: [{ title: "English", codec: "eac3", channels: 6, default: true }],
        activeIndex: 0,
        onSelect: () => {},
        currentPosition: 0,
      }),
    );

    const trigger = screen.getByRole("button", { name: "Audio tracks" });
    expect(trigger).not.toHaveAttribute("aria-disabled");
    expect(trigger).not.toHaveClass("cursor-default");
    expect(trigger).not.toHaveClass("opacity-40");
    expect(trigger).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("menu")).toBeNull();

    fireEvent.click(trigger);

    expect(trigger).toHaveAttribute("aria-expanded", "true");
    const entry = screen.getByRole("menuitem");
    expect(entry).toHaveClass("text-blue-400");
    expect(entry).toHaveTextContent("English");
    expect(entry).toHaveTextContent("EAC3");
    expect(entry).toHaveTextContent("5.1");
    expect(entry).toHaveTextContent("Default");
    expect(entry).toHaveTextContent("\u2713");
  });

  it("renders nothing when there are no tracks", () => {
    const { container } = render(
      createElement(AudioTrackMenu, {
        tracks: [],
        activeIndex: -1,
        onSelect: () => {},
        currentPosition: 0,
      }),
    );
    expect(container.firstChild).toBeNull();
    expect(screen.queryByRole("button", { name: "Audio tracks" })).toBeNull();
  });

  it("keeps multiple-track selection behavior unchanged", () => {
    const onSelect = vi.fn();
    render(
      createElement(AudioTrackMenu, {
        tracks: [
          { title: "English", codec: "eac3", channels: 6, default: true },
          { title: "French", codec: "aac", channels: 2 },
        ],
        activeIndex: 0,
        onSelect,
        currentPosition: 0,
      }),
    );

    fireEvent.click(screen.getByRole("button", { name: "Audio tracks" }));
    const entries = screen.getAllByRole("menuitem");
    expect(entries).toHaveLength(2);
    const [firstEntry, secondEntry] = entries;
    if (!firstEntry || !secondEntry) {
      throw new Error("expected two menu entries");
    }
    expect(firstEntry).toHaveClass("text-blue-400");
    expect(secondEntry).not.toHaveClass("text-blue-400");

    fireEvent.click(secondEntry);
    expect(onSelect).toHaveBeenCalledWith(1, 0);
  });
});
