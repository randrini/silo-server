// @vitest-environment jsdom

import { fireEvent, renderHook } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { KEYBOARD_SKIP_SECONDS, useKeyboardShortcuts } from "./useKeyboardShortcuts";

function renderShortcuts() {
  const video = { currentTime: 100, duration: 300, volume: 1, muted: false } as HTMLVideoElement;
  const videoRef = { current: video };
  const containerRef = { current: null };
  const handleSeek = vi.fn();
  renderHook(() =>
    useKeyboardShortcuts(videoRef, containerRef, vi.fn(), handleSeek, vi.fn(), undefined),
  );
  return { handleSeek, video };
}

describe("useKeyboardShortcuts", () => {
  it("nudges the keyboard arrow seek by 10s", () => {
    expect(KEYBOARD_SKIP_SECONDS).toBe(10);

    const { handleSeek } = renderShortcuts();

    fireEvent.keyDown(document, { key: "ArrowLeft" });
    expect(handleSeek).toHaveBeenLastCalledWith(90);

    fireEvent.keyDown(document, { key: "ArrowRight" });
    expect(handleSeek).toHaveBeenLastCalledWith(110);
  });

  it("clamps the arrow seek to the media bounds", () => {
    const video = { currentTime: 5, duration: 300, volume: 1, muted: false } as HTMLVideoElement;
    const handleSeek = vi.fn();
    renderHook(() =>
      useKeyboardShortcuts({ current: video }, { current: null }, vi.fn(), handleSeek, vi.fn()),
    );

    fireEvent.keyDown(document, { key: "ArrowLeft" });
    expect(handleSeek).toHaveBeenLastCalledWith(0);
  });
});
