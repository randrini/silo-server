import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { PlayerConfigProvider, type PlayerConfig } from "../context/PlayerConfigContext";
import { fixturePlanV3 } from "../protocol-v3.fixtures";
import type {
  PlaybackRealtimeCommandEnvelope,
  PlaybackRealtimeEventEnvelope,
} from "../realtime-protocol";
import type { PlayerSubtitleInfo } from "../types";
import { HLS_STARTUP_TIMEOUT_MS } from "../utils/hlsStartupGuard";
import { VideoPlayer } from "./VideoPlayer";

const realtimeOptions = vi.hoisted(() => ({
  current: null as null | {
    onEvent?: (event: PlaybackRealtimeEventEnvelope) => void;
    onCommand: (command: PlaybackRealtimeCommandEnvelope) => Promise<void> | void;
  },
}));
const controls = vi.hoisted(() => ({
  current: null as null | {
    activeSubtitleIndex: number | null;
    subtitleTracks: PlayerSubtitleInfo[];
    visible: boolean;
    onSurfaceTap?: (event: React.MouseEvent<HTMLElement>) => void;
    isFullscreen?: boolean;
    onFullscreenToggle?: () => void;
    onSubtitleSelect?: (index: number) => void;
  },
}));
const playerSeek = vi.hoisted(() => vi.fn());
const subtitleTimeline = vi.hoisted(() => ({
  textOffsetSeconds: null as number | null,
  assOffsetSeconds: null as number | null,
}));
const toastError = vi.hoisted(() => vi.fn());
const hlsJS = vi.hoisted(() => ({ supported: false, constructed: vi.fn() }));
// Captures the onSourceChanged handlers the mocked subtitle hooks receive, so
// tests can drive a subtitle_source_changed (409) signal from the outside.
const subtitleHooks = vi.hoisted(() => ({
  vttSourceChanged: null as null | (() => void),
  assSourceChanged: null as null | (() => void),
}));

vi.mock("sonner", () => ({ toast: { error: toastError, success: vi.fn(), message: vi.fn() } }));

vi.mock("../hooks/usePlaybackRealtime", () => ({
  usePlaybackRealtime: vi.fn((options) => {
    realtimeOptions.current = options;
    return { connectionState: "connected" };
  }),
}));
vi.mock("../hooks/useWatchProgress", () => ({
  useWatchProgress: () => vi.fn().mockResolvedValue(undefined),
}));
vi.mock("../hooks/useKeyboardShortcuts", () => ({ useKeyboardShortcuts: vi.fn() }));
vi.mock("../hooks/useRemuxSeeking", () => ({
  useRemuxSeeking: () => ({ handleSeek: playerSeek }),
}));
vi.mock("../hooks/useSubtitleTracks", () => ({
  useSubtitleTracks: (...args: unknown[]) => {
    subtitleTimeline.textOffsetSeconds = args[3] as number;
    subtitleHooks.vttSourceChanged = (args[11] as (() => void) | undefined) ?? null;
    return [];
  },
}));
vi.mock("../hooks/useASSSubtitles", () => ({
  useASSSubtitles: (...args: unknown[]) => {
    subtitleTimeline.assOffsetSeconds = args[4] as number;
    subtitleHooks.assSourceChanged = (args[7] as (() => void) | undefined) ?? null;
    return { isActive: false };
  },
}));
vi.mock("../hooks/useSubtitleAppearance", () => ({
  useSubtitleAppearance: () => ({
    settings: { position: "bottom", fontSize: "large" },
    containerStyle: {},
    cueStyle: {},
  }),
}));
vi.mock("../hooks/useSubtitleLayout", () => ({
  useSubtitleLayout: () => ({ positionStyle: {}, fontScale: 1 }),
}));
vi.mock("hls.js", () => ({
  default: class MockHls {
    static Events = {
      ERROR: "error",
      MANIFEST_PARSED: "manifestParsed",
      BUFFER_APPENDED: "bufferAppended",
    };
    static ErrorTypes = { NETWORK_ERROR: "networkError", MEDIA_ERROR: "mediaError" };
    static isSupported = () => hlsJS.supported;

    constructor(config?: unknown) {
      hlsJS.constructed(config);
    }

    on() {}
    loadSource() {}
    attachMedia() {}
    destroy() {}
  },
}));
vi.mock("./PlayerControls", () => ({
  SKIP_BACK_SECONDS: 10,
  SKIP_FORWARD_SECONDS: 30,
  PlayerControls: vi.fn(
    (props: {
      activeSubtitleIndex: number | null;
      subtitleTracks: PlayerSubtitleInfo[];
      visible: boolean;
      onSurfaceTap?: (event: React.MouseEvent<HTMLElement>) => void;
      isFullscreen?: boolean;
      onFullscreenToggle?: () => void;
    }) => {
      controls.current = props;
      return null;
    },
  ),
}));

const playerConfig: PlayerConfig = {
  apiBaseUrl: "/api/v1",
  getAccessToken: () => "token",
  getProfileId: () => "profile-1",
  getDeviceId: () => "test-device",
  getProfileToken: () => null,
};

function wrapper({ children }: { children: ReactNode }) {
  return createElement(PlayerConfigProvider, { config: playerConfig, children });
}

const directPlan = fixturePlanV3({
  delivery: "original_http",
  stream: {
    url: "/stream/session-1",
    protocol: "http_progressive",
    headers: {},
    header_refresh: "none",
  },
});

function playerProps(overrides: Partial<Parameters<typeof VideoPlayer>[0]> = {}) {
  return {
    title: "Test movie",
    streamUrl: "/api/v1/stream/session-1?token=token",
    plan: directPlan,
    planRevision: 1,
    sessionId: "session-1",
    subtitleUrls: [] as PlayerSubtitleInfo[],
    initialPosition: 0,
    intro: null,
    credits: null,
    qualityPreference: "original",
    onExit: vi.fn(),
    ...overrides,
  };
}

function renderPlayer(overrides: Partial<Parameters<typeof VideoPlayer>[0]> = {}) {
  const props = playerProps(overrides);
  const rendered = render(createElement(VideoPlayer, props), { wrapper });
  return {
    ...rendered,
    rerenderPlayer(next: Partial<Parameters<typeof VideoPlayer>[0]>) {
      rendered.rerender(createElement(VideoPlayer, { ...props, ...next }));
    },
  };
}

function planInvalidatedCommand(
  payload: Record<string, unknown> = {
    reason: "video_copy_unsafe",
    plan_id: directPlan.plan_id,
  },
): PlaybackRealtimeCommandEnvelope {
  return {
    type: "command",
    command_id: "cmd-invalidate-1",
    session_id: "session-1",
    name: "plan_invalidated",
    deadline_ms: 8_000,
    payload,
  };
}

function setMediaError(video: HTMLVideoElement, message: string) {
  Object.defineProperty(video, "error", {
    configurable: true,
    value: { code: 3, message },
  });
}

/** Installs a fake `TimeRanges` list so tests can drive buffer/seek checks. */
function setTimeRanges(
  video: HTMLVideoElement,
  property: "buffered" | "seekable",
  ranges: Array<[number, number]>,
) {
  Object.defineProperty(video, property, {
    configurable: true,
    value: {
      length: ranges.length,
      start: (index: number) => ranges[index]?.[0] ?? 0,
      end: (index: number) => ranges[index]?.[1] ?? 0,
    } as TimeRanges,
  });
}

describe("VideoPlayer plan failure recovery", () => {
  beforeEach(() => {
    realtimeOptions.current = null;
    controls.current = null;
    subtitleTimeline.textOffsetSeconds = null;
    subtitleTimeline.assOffsetSeconds = null;
    subtitleHooks.vttSourceChanged = null;
    subtitleHooks.assSourceChanged = null;
    hlsJS.supported = false;
    hlsJS.constructed.mockClear();
    toastError.mockClear();
    playerSeek.mockClear();
    vi.spyOn(HTMLMediaElement.prototype, "play").mockResolvedValue(undefined);
    vi.spyOn(HTMLMediaElement.prototype, "pause").mockImplementation(() => {});
    vi.spyOn(HTMLMediaElement.prototype, "load").mockImplementation(() => {});
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  it("toggles controls on a coarse-pointer single tap and seeks on a left double tap", async () => {
    vi.useFakeTimers();
    vi.stubGlobal(
      "matchMedia",
      vi.fn(() => ({
        matches: true,
        media: "(pointer: coarse)",
        onchange: null,
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
        addListener: vi.fn(),
        removeListener: vi.fn(),
        dispatchEvent: vi.fn(),
      })),
    );
    try {
      const { container } = renderPlayer({ shouldAutoPlay: false });
      const video = container.querySelector("video");
      if (!video) throw new Error("expected video element");
      Object.defineProperty(video, "readyState", { configurable: true, value: 3 });
      Object.defineProperty(video, "currentTime", { configurable: true, value: 50 });
      fireEvent.canPlay(video);
      await vi.waitFor(() => expect(controls.current?.onSurfaceTap).toBeTypeOf("function"));

      act(() =>
        controls.current?.onSurfaceTap?.({
          clientX: 200,
          currentTarget: { getBoundingClientRect: () => ({ left: 0, width: 390 }) },
        } as unknown as React.MouseEvent<HTMLElement>),
      );
      act(() => vi.advanceTimersByTime(250));
      expect(controls.current?.visible).toBe(false);

      const leftTap = {
        clientX: 20,
        currentTarget: { getBoundingClientRect: () => ({ left: 0, width: 390 }) },
      } as unknown as React.MouseEvent<HTMLElement>;
      act(() => {
        controls.current?.onSurfaceTap?.(leftTap);
        controls.current?.onSurfaceTap?.(leftTap);
      });
      expect(playerSeek).toHaveBeenCalledWith(40);
    } finally {
      vi.useRealTimers();
      vi.unstubAllGlobals();
    }
  });

  it("loads a replacement transport without resuming paused playback", async () => {
    const play = vi.mocked(HTMLMediaElement.prototype.play);
    const { container } = renderPlayer({ shouldAutoPlay: false });
    const video = container.querySelector("video");
    if (!video) throw new Error("expected video element");

    await waitFor(() => expect(video.src).toContain("/api/v1/stream/session-1"));
    Object.defineProperty(video, "readyState", { configurable: true, value: 3 });
    fireEvent.canPlay(video);
    expect(play).not.toHaveBeenCalled();
  });

  it("does not report a startup timeout while HLS is intentionally paused", () => {
    vi.useFakeTimers();
    try {
      const onPlanFailure = vi.fn();
      const hlsPlan = fixturePlanV3({
        delivery: "server_remux_hls",
        stream: {
          url: "/stream/session-1/master.m3u8",
          protocol: "hls",
          headers: {},
          header_refresh: "none",
        },
      });
      const { container } = renderPlayer({
        plan: hlsPlan,
        shouldAutoPlay: false,
        onPlanFailure,
      });
      const video = container.querySelector("video");
      if (!video) throw new Error("expected video element");

      Object.defineProperty(video, "readyState", { configurable: true, value: 3 });
      fireEvent.canPlay(video);
      act(() => vi.advanceTimersByTime(HLS_STARTUP_TIMEOUT_MS));

      expect(HTMLMediaElement.prototype.play).not.toHaveBeenCalled();
      expect(onPlanFailure).not.toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });

  it("surfaces a refused replan only for the transport-dead plan revision", async () => {
    const onPlanFailure = vi.fn();
    const { container, rerenderPlayer } = renderPlayer({ onPlanFailure });
    const video = container.querySelector("video");
    if (!video) throw new Error("expected video element");

    setMediaError(video, "decoder failed");
    fireEvent.error(video);
    expect(onPlanFailure).toHaveBeenCalledOnce();

    rerenderPlayer({ replanError: "Recovery was refused." });
    expect(await screen.findByText("Recovery was refused.")).toBeInTheDocument();

    const nextPlan = fixturePlanV3({
      ...directPlan,
      plan_id: "plan:2222222222222222",
      plan_attempt_key: "v3:2222222222222222",
    });
    rerenderPlayer({ plan: nextPlan, planRevision: 2, replanError: null });
    await waitFor(() =>
      expect(screen.queryByText("Recovery was refused.")).not.toBeInTheDocument(),
    );

    rerenderPlayer({ plan: nextPlan, planRevision: 2, replanError: "Unrelated replan error." });
    await act(async () => Promise.resolve());
    expect(screen.queryByText("Unrelated replan error.")).not.toBeInTheDocument();
  });

  it("re-arms the plan failure guard after a transient recovery request failure", async () => {
    const onPlanFailure = vi.fn();
    const { container, rerenderPlayer } = renderPlayer({ onPlanFailure });
    const video = container.querySelector("video");
    if (!video) throw new Error("expected video element");

    setMediaError(video, "decoder failed");
    fireEvent.error(video);
    expect(onPlanFailure).toHaveBeenCalledOnce();

    rerenderPlayer({ replanError: "Temporary recovery failure." });
    await screen.findByText("Temporary recovery failure.");

    fireEvent.error(video);
    expect(onPlanFailure).toHaveBeenCalledTimes(2);
  });

  it("replans off a plan the server invalidated", async () => {
    const onPlanInvalidated = vi.fn().mockResolvedValue(true);
    renderPlayer({ onPlanInvalidated });
    const onCommand = realtimeOptions.current?.onCommand;
    if (!onCommand) throw new Error("expected the realtime command handler");

    await act(async () => {
      await onCommand(planInvalidatedCommand());
    });

    expect(onPlanInvalidated).toHaveBeenCalledWith(directPlan.plan_id, "video_copy_unsafe", 0);
  });

  // A rejected result is the server's cue to stop the session, which is what
  // lets the client's own recovery mint a fresh attempt against the persisted
  // verdict. Swallowing the failure here would leave the copy route playing.
  it("rejects the invalidation command when no replacement plan is adopted", async () => {
    const onPlanInvalidated = vi.fn().mockResolvedValue(false);
    renderPlayer({ onPlanInvalidated });
    const onCommand = realtimeOptions.current?.onCommand;
    if (!onCommand) throw new Error("expected the realtime command handler");

    await expect(onCommand(planInvalidatedCommand())).rejects.toThrow(
      "plan_invalidation_replan_failed",
    );
  });

  it("rejects an invalidation command that names no plan", async () => {
    const onPlanInvalidated = vi.fn().mockResolvedValue(true);
    renderPlayer({ onPlanInvalidated });
    const onCommand = realtimeOptions.current?.onCommand;
    if (!onCommand) throw new Error("expected the realtime command handler");

    await expect(
      onCommand(planInvalidatedCommand({ reason: "video_copy_unsafe" })),
    ).rejects.toThrow("invalid_plan_invalidated_payload");
    expect(onPlanInvalidated).not.toHaveBeenCalled();
  });

  it("does not retry an auto-selected subtitle after its replan is refused", async () => {
    const onSubtitleTrackChange = vi.fn();
    const sidecarTrack: PlayerSubtitleInfo = {
      index: 2,
      media_file_id: 7,
      track_id: "file:7:subtitle:2",
      language: "en",
      codec: "srt",
      label: "English",
      source: "external",
      url: "/stream/session-1/subtitles/2.vtt",
    };
    const { rerenderPlayer } = renderPlayer({
      subtitleUrls: [sidecarTrack],
      subtitleMode: "always",
      preferredSubtitleLanguage: "en",
      onSubtitleTrackChange,
    });

    await waitFor(() => expect(onSubtitleTrackChange).toHaveBeenCalledOnce());
    expect(onSubtitleTrackChange).toHaveBeenCalledWith(2, 0);

    rerenderPlayer({ replanError: "Silo could not apply the subtitle selection." });

    await waitFor(() => expect(controls.current?.activeSubtitleIndex).toBeNull());
    expect(onSubtitleTrackChange).toHaveBeenCalledOnce();

    const nextPlan = fixturePlanV3({
      ...directPlan,
      plan_id: "plan:next-session",
      plan_attempt_key: "v3:next-session",
      session_id: "session-2",
    });
    rerenderPlayer({ sessionId: "session-2", plan: nextPlan, replanError: null });

    await waitFor(() => expect(controls.current?.activeSubtitleIndex).toBe(2));
    expect(onSubtitleTrackChange).toHaveBeenCalledTimes(2);
    expect(onSubtitleTrackChange).toHaveBeenLastCalledWith(2, 0);
  });

  // The rollback is otherwise silent: the refusal only renders inside the
  // quality menu, which a user who just picked a subtitle never opens.
  it("toasts the server's refusal when a subtitle change is rolled back", async () => {
    const onSubtitleTrackChange = vi.fn();
    const sidecarTrack: PlayerSubtitleInfo = {
      index: 2,
      media_file_id: 7,
      track_id: "file:7:subtitle:2",
      language: "en",
      codec: "srt",
      label: "English",
      source: "external",
      url: "/stream/session-1/subtitles/2.vtt",
    };
    const { rerenderPlayer } = renderPlayer({
      subtitleUrls: [sidecarTrack],
      subtitleMode: "always",
      preferredSubtitleLanguage: "en",
      onSubtitleTrackChange,
    });

    await waitFor(() => expect(onSubtitleTrackChange).toHaveBeenCalledOnce());
    expect(toastError).not.toHaveBeenCalled();

    rerenderPlayer({
      replanError: "The selected subtitle must be burned into the video, but 4K is disabled.",
      replanErrorTitle: "That subtitle track can't be used",
    });

    await waitFor(() => expect(toastError).toHaveBeenCalledOnce());
    expect(toastError).toHaveBeenCalledWith("That subtitle track can't be used", {
      description: "The selected subtitle must be burned into the video, but 4K is disabled.",
    });
  });

  it("falls back to a generic subtitle refusal title and toasts once", async () => {
    const onSubtitleTrackChange = vi.fn();
    const sidecarTrack: PlayerSubtitleInfo = {
      index: 2,
      media_file_id: 7,
      track_id: "file:7:subtitle:2",
      language: "en",
      codec: "srt",
      label: "English",
      source: "external",
      url: "/stream/session-1/subtitles/2.vtt",
    };
    const { rerenderPlayer } = renderPlayer({
      subtitleUrls: [sidecarTrack],
      subtitleMode: "always",
      preferredSubtitleLanguage: "en",
      onSubtitleTrackChange,
    });

    await waitFor(() => expect(onSubtitleTrackChange).toHaveBeenCalledOnce());
    rerenderPlayer({ replanError: "Silo could not apply the subtitle selection." });
    await waitFor(() => expect(toastError).toHaveBeenCalledOnce());
    expect(toastError).toHaveBeenCalledWith("That subtitle track can't be used", {
      description: "Silo could not apply the subtitle selection.",
    });

    // The ref cleared on rollback, so a re-render with the same refusal must
    // not stack a second toast.
    rerenderPlayer({ replanError: "Silo could not apply the subtitle selection." });
    await waitFor(() => expect(controls.current?.activeSubtitleIndex).toBeNull());
    expect(toastError).toHaveBeenCalledOnce();
  });
});

describe("VideoPlayer intro skip prompt", () => {
  beforeEach(() => {
    vi.spyOn(HTMLMediaElement.prototype, "play").mockResolvedValue(undefined);
    vi.spyOn(HTMLMediaElement.prototype, "pause").mockImplementation(() => {});
    vi.spyOn(HTMLMediaElement.prototype, "load").mockImplementation(() => {});
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  async function enterIntro(mode: "never" | "ask" | "always") {
    const rendered = renderPlayer({
      intro: { start: 10, end: 20 },
      introSkipMode: mode,
    });
    const video = rendered.container.querySelector("video");
    if (!video) throw new Error("expected video element");

    video.currentTime = 12;
    fireEvent.timeUpdate(video);
    await act(async () => Promise.resolve());
    return rendered;
  }

  it("renders the ask pill and consumes Escape", async () => {
    await enterIntro("ask");
    expect(await screen.findByRole("button", { name: "Skip Intro" })).toBeInTheDocument();

    fireEvent.keyDown(document, { key: "Escape" });

    await waitFor(() =>
      expect(screen.queryByRole("button", { name: "Skip Intro" })).not.toBeInTheDocument(),
    );
  });

  it("renders the undo action after an automatic skip", async () => {
    await enterIntro("always");
    const undo = await screen.findByRole("button", {
      name: "Watch Intro",
    });

    fireEvent.click(undo);

    await waitFor(() => expect(undo).not.toBeInTheDocument());
  });

  it("renders no intro action in never mode", async () => {
    await enterIntro("never");

    expect(screen.queryByRole("button", { name: /Intro/ })).not.toBeInTheDocument();
  });

  it("prompts for nothing while the intro mode is still unknown", async () => {
    const rendered = renderPlayer({ intro: { start: 10, end: 20 }, introSkipMode: null });
    const video = rendered.container.querySelector("video");
    if (!video) throw new Error("expected video element");

    video.currentTime = 12;
    fireEvent.timeUpdate(video);
    await act(async () => Promise.resolve());

    expect(screen.queryByRole("button", { name: /Intro/ })).not.toBeInTheDocument();
    // Nothing was skipped either: an unknown mode must not act like "always".
    expect(video.currentTime).toBe(12);
  });

  // Space belongs to whatever control has focus. Consuming it at the document
  // both skipped the intro and swallowed the press meant for Play/Pause.
  it("leaves Select to the focused transport control", async () => {
    const rendered = await enterIntro("ask");
    const prompt = await screen.findByRole("button", { name: "Skip Intro" });

    const transport = document.createElement("button");
    transport.textContent = "Play";
    rendered.container.firstElementChild?.appendChild(transport);
    transport.focus();

    const notPrevented = fireEvent.keyDown(transport, { key: " " });

    expect(notPrevented).toBe(true);
    expect(prompt).toBeInTheDocument();
  });

  it("acts on Select while the pill itself is focused", async () => {
    await enterIntro("ask");
    const prompt = await screen.findByRole("button", { name: "Skip Intro" });
    prompt.focus();

    fireEvent.keyDown(prompt, { key: " " });

    await waitFor(() => expect(prompt).not.toBeInTheDocument());
  });
});

describe("VideoPlayer native HLS timeline", () => {
  beforeEach(() => {
    realtimeOptions.current = null;
    controls.current = null;
    subtitleTimeline.textOffsetSeconds = null;
    subtitleTimeline.assOffsetSeconds = null;
    hlsJS.supported = false;
    hlsJS.constructed.mockClear();
    vi.spyOn(HTMLMediaElement.prototype, "play").mockResolvedValue(undefined);
    vi.spyOn(HTMLMediaElement.prototype, "pause").mockImplementation(() => {});
    vi.spyOn(HTMLMediaElement.prototype, "load").mockImplementation(() => {});
    vi.spyOn(HTMLMediaElement.prototype, "canPlayType").mockImplementation((mime) =>
      mime === "application/vnd.apple.mpegurl" ? "probably" : "",
    );
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("applies player_start_seconds before native HLS playback", async () => {
    const plan = fixturePlanV3({
      delivery: "server_remux_hls",
      stream: {
        url: "/playback/transcode/session-1/master.m3u8",
        protocol: "hls",
        headers: {},
        header_refresh: "none",
      },
      timeline: {
        source_start_seconds: 42,
        player_start_seconds: 7,
        stream_origin_seconds: 35,
        timeline_offset_seconds: 0,
        can_seek_anywhere: true,
        seek_restoration: "player_position",
      },
    });
    const { container } = renderPlayer({ plan, initialPosition: 42 });
    const video = container.querySelector("video");
    if (!video) throw new Error("expected video element");

    await waitFor(() => expect(video.src).toContain("/api/v1/stream/session-1"));
    fireEvent.loadedMetadata(video);

    expect(video.currentTime).toBe(7);
    expect(subtitleTimeline.textOffsetSeconds).toBe(0);
    expect(subtitleTimeline.assOffsetSeconds).toBe(0);
  });

  it("uses native HLS for Dolby Vision when hls.js is also available", async () => {
    hlsJS.supported = true;
    vi.stubGlobal("navigator", {
      ...navigator,
      userAgent:
        "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 Version/26.0 Safari/605.1.15",
    });
    const plan = fixturePlanV3({
      delivery: "server_remux_hls",
      stream: {
        url: "/playback/transcode/session-1/master.m3u8",
        protocol: "hls",
        headers: {},
        header_refresh: "none",
      },
      effective_recipe: {
        video_codec: "hevc",
        audio_codec: "eac3",
        dynamic_range: "dolby_vision",
      },
      timeline: {
        source_start_seconds: 42,
        stream_origin_seconds: 35,
        player_start_seconds: 7,
        timeline_offset_seconds: 0,
        can_seek_anywhere: false,
        seek_restoration: "source_position",
      },
    });
    const { container } = renderPlayer({ plan, initialPosition: 42 });
    const video = container.querySelector("video");
    if (!video) throw new Error("expected video element");

    await waitFor(() => expect(video.src).toContain("/api/v1/stream/session-1"));
    fireEvent.loadedMetadata(video);

    expect(video.currentTime).toBe(7);
    expect(hlsJS.constructed).not.toHaveBeenCalled();
  });

  it("uses hls.js for Dolby Vision in Chromium even when native HLS is advertised", async () => {
    hlsJS.supported = true;
    vi.stubGlobal("navigator", {
      ...navigator,
      userAgent:
        "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/151.0.0.0 Safari/537.36",
    });
    const plan = fixturePlanV3({
      delivery: "server_remux_hls",
      stream: {
        url: "/playback/transcode/session-1/master.m3u8",
        protocol: "hls",
        headers: {},
        header_refresh: "none",
      },
      effective_recipe: {
        video_codec: "hevc",
        audio_codec: "aac",
        dynamic_range: "dolby_vision",
      },
    });

    renderPlayer({ plan });

    await waitFor(() => expect(hlsJS.constructed).toHaveBeenCalledOnce());
  });
});

// A server-invalidated plan swaps the transport without any user gesture, and
// it is the one swap that can cross transport kinds — an optimistic progressive
// remux replaced by a tone-mapping HLS transcode. The replacement has to resume
// on its own: nothing is going to press play, and once the engine has filled its
// buffer it stops fetching, so a player left paused here is a player that stays
// paused until the viewer seeks.
describe("VideoPlayer server-invalidated transport swap", () => {
  const invalidatedHlsPlan = fixturePlanV3({
    delivery: "server_transcode_hls",
    plan_id: "plan:3333333333333333",
    plan_attempt_key: "v3:3333333333333333",
    stream: {
      url: "/playback/transcode/session-1/master.m3u8",
      protocol: "hls",
      headers: {},
      header_refresh: "none",
    },
    timeline: {
      source_start_seconds: 24,
      player_start_seconds: 24,
      stream_origin_seconds: 0,
      timeline_offset_seconds: 0,
      can_seek_anywhere: true,
      seek_restoration: "player_position",
    },
  });

  beforeEach(() => {
    realtimeOptions.current = null;
    hlsJS.supported = false;
    hlsJS.constructed.mockClear();
    vi.spyOn(HTMLMediaElement.prototype, "play").mockResolvedValue(undefined);
    vi.spyOn(HTMLMediaElement.prototype, "pause").mockImplementation(() => {});
    vi.spyOn(HTMLMediaElement.prototype, "load").mockImplementation(() => {});
    vi.spyOn(HTMLMediaElement.prototype, "canPlayType").mockImplementation((mime) =>
      mime === "application/vnd.apple.mpegurl" ? "probably" : "",
    );
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  it("resumes playback and restores the position on the replacement transport", async () => {
    const play = vi.mocked(HTMLMediaElement.prototype.play);
    let rerender: ((next: Partial<Parameters<typeof VideoPlayer>[0]>) => void) | null = null;
    const onPlanInvalidated = vi.fn(async () => {
      rerender?.({
        plan: invalidatedHlsPlan,
        planRevision: 2,
        streamUrl: "/api/v1/playback/transcode/session-1/master.m3u8?token=token",
      });
      return true;
    });

    const rendered = renderPlayer({ onPlanInvalidated });
    rerender = rendered.rerenderPlayer;
    const video = rendered.container.querySelector("video");
    if (!video) throw new Error("expected video element");

    await waitFor(() => expect(video.src).toContain("/api/v1/stream/session-1"));
    play.mockClear();

    const onCommand = realtimeOptions.current?.onCommand;
    if (!onCommand) throw new Error("expected the realtime command handler");
    await act(async () => {
      await onCommand(planInvalidatedCommand());
    });

    await waitFor(() => expect(video.src).toContain("master.m3u8"));
    fireEvent.loadedMetadata(video);
    expect(video.currentTime).toBe(24);

    Object.defineProperty(video, "readyState", { configurable: true, value: 3 });
    fireEvent.canPlay(video);

    expect(play).toHaveBeenCalledOnce();
  });

  // The previous transport is torn down with `load()` in the same commit that
  // builds the replacement, and the load algorithm is required to reject a play
  // that is still pending. Latching the autoplay attempt on that first rejection
  // left the element paused on a healthy buffer with nothing to restart it.
  it("retries a rejected play instead of leaving the replacement paused", async () => {
    vi.useFakeTimers();
    try {
      const play = vi.mocked(HTMLMediaElement.prototype.play);
      play
        .mockRejectedValueOnce(
          Object.assign(new Error("The play() request was interrupted"), { name: "AbortError" }),
        )
        .mockResolvedValue(undefined);

      const { container, rerenderPlayer } = renderPlayer();
      const video = container.querySelector("video");
      if (!video) throw new Error("expected video element");

      rerenderPlayer({
        plan: invalidatedHlsPlan,
        planRevision: 2,
        streamUrl: "/api/v1/playback/transcode/session-1/master.m3u8?token=token",
      });
      play.mockClear();

      Object.defineProperty(video, "readyState", { configurable: true, value: 3 });
      fireEvent.canPlay(video);
      expect(play).toHaveBeenCalledOnce();

      await act(async () => {
        await Promise.resolve();
      });
      await act(async () => {
        vi.advanceTimersByTime(1_000);
      });

      expect(play).toHaveBeenCalledTimes(2);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("VideoPlayer translation handoff", () => {
  beforeEach(() => {
    realtimeOptions.current = null;
    controls.current = null;
    vi.spyOn(HTMLMediaElement.prototype, "play").mockResolvedValue(undefined);
    vi.spyOn(HTMLMediaElement.prototype, "pause").mockImplementation(() => {});
    vi.spyOn(HTMLMediaElement.prototype, "load").mockImplementation(() => {});
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  it("selects the refreshed downloaded track and clears the live overlay", async () => {
    const onRefreshSubtitles = vi.fn();
    const onSubtitleChanged = vi.fn();
    const onSubtitleTrackChange = vi.fn();
    const { rerenderPlayer } = renderPlayer({
      onRefreshSubtitles,
      onSubtitleChanged,
      onSubtitleTrackChange,
    });

    act(() => {
      realtimeOptions.current?.onEvent?.({
        type: "event",
        session_id: "session-1",
        name: "subtitle_translation_started",
        payload: {
          session_id: "session-1",
          file_id: 7,
          job_id: 1,
          track_key: "translation-1",
          language: "es",
          label: "Spanish (AI)",
          total_cues: 2,
        },
      });
    });
    expect(onSubtitleTrackChange).not.toHaveBeenCalledWith(1_000_000, expect.any(Number));
    expect(controls.current?.activeSubtitleIndex).toBe(1_000_000);
    expect(controls.current?.subtitleTracks.some((track) => track.live)).toBe(true);

    act(() => {
      realtimeOptions.current?.onEvent?.({
        type: "event",
        session_id: "session-1",
        name: "subtitle_translation_completed",
        payload: {
          session_id: "session-1",
          file_id: 7,
          job_id: 1,
          track_key: "translation-1",
          subtitle_id: 44,
          language: "es",
          label: "Spanish (AI)",
        },
      });
    });
    expect(onRefreshSubtitles).toHaveBeenCalledOnce();
    expect(onSubtitleTrackChange).not.toHaveBeenCalledWith(1_000_000, expect.any(Number));
    expect(controls.current?.activeSubtitleIndex).toBe(1_000_000);

    const downloadedTrack: PlayerSubtitleInfo = {
      index: 4,
      media_file_id: 7,
      track_id: "downloaded:44",
      language: "es",
      codec: "srt",
      label: "Spanish (AI)",
      source: "downloaded",
      url: "/subtitles/44",
    };
    rerenderPlayer({
      plan: fixturePlanV3({
        ...directPlan,
        plan_id: "plan:2222222222222222",
        plan_attempt_key: "v3:2222222222222222",
      }),
      planRevision: 2,
      subtitleUrls: [downloadedTrack],
    });

    await waitFor(() => expect(onSubtitleChanged).toHaveBeenCalledWith(4, undefined));
    expect(onSubtitleTrackChange).toHaveBeenCalledWith(4, expect.any(Number));
    expect(controls.current?.activeSubtitleIndex).toBe(4);
    expect(controls.current?.subtitleTracks).toEqual([downloadedTrack]);
  });

  it.each(["missing", "rejecting"])(
    "falls back to webkitEnterFullscreen when requestFullscreen is %s",
    async (mode) => {
      const webkitEnterFullscreen = vi.fn();
      const webkitExitFullscreen = vi.fn();

      const { container } = renderPlayer();

      const video = container.querySelector("video") as HTMLVideoElement & {
        webkitSupportsFullscreen?: boolean;
        webkitDisplayingFullscreen?: boolean;
        webkitEnterFullscreen?: () => void;
        webkitExitFullscreen?: () => void;
      };
      video.webkitSupportsFullscreen = true;
      video.webkitEnterFullscreen = webkitEnterFullscreen;
      video.webkitExitFullscreen = webkitExitFullscreen;

      // Simulate container requestFullscreen rejecting (as WebKit on iPhone does)
      const playerContainer = container.querySelector(".player-container") as HTMLElement;
      expect(playerContainer).not.toBeNull();
      const requestFullscreen = vi.fn().mockRejectedValue(new Error("Not supported"));
      Object.defineProperty(playerContainer, "requestFullscreen", {
        value: mode === "rejecting" ? requestFullscreen : undefined,
        configurable: true,
      });

      act(() => {
        controls.current?.onFullscreenToggle?.();
      });

      await waitFor(() => expect(webkitEnterFullscreen).toHaveBeenCalledOnce());
      expect(requestFullscreen).toHaveBeenCalledTimes(mode === "rejecting" ? 1 : 0);

      video.webkitDisplayingFullscreen = true;
      act(() => {
        controls.current?.onFullscreenToggle?.();
      });
      expect(webkitExitFullscreen).toHaveBeenCalledOnce();
    },
  );

  it("tracks WebKit fullscreen events on the video element", async () => {
    const { container } = renderPlayer();

    const video = container.querySelector("video") as HTMLVideoElement & {
      webkitDisplayingFullscreen?: boolean;
    };

    video.webkitDisplayingFullscreen = true;
    act(() => {
      video.dispatchEvent(new Event("webkitbeginfullscreen"));
    });

    expect(controls.current?.isFullscreen).toBe(true);

    video.webkitDisplayingFullscreen = false;
    act(() => {
      video.dispatchEvent(new Event("webkitendfullscreen"));
    });

    expect(controls.current?.isFullscreen).toBe(false);
  });

  it("preserves player when transportRevision is unchanged across plan revisions", async () => {
    const removeAttrSpy = vi.spyOn(HTMLMediaElement.prototype, "removeAttribute");
    try {
      const { rerenderPlayer } = renderPlayer({
        planRevision: 1,
        transportRevision: 1,
      });
      removeAttrSpy.mockClear();

      const nextPlan = fixturePlanV3({
        ...directPlan,
        plan_id: "plan:subtitles-only",
        plan_attempt_key: "v3:subtitles-only",
      });
      rerenderPlayer({
        plan: nextPlan,
        planRevision: 2,
        transportRevision: 1,
      });
      expect(removeAttrSpy).not.toHaveBeenCalled();

      rerenderPlayer({
        plan: nextPlan,
        planRevision: 3,
        transportRevision: 2,
      });
      expect(removeAttrSpy).toHaveBeenCalledWith("src");
    } finally {
      removeAttrSpy.mockRestore();
    }
  });

  it("warms the new transport once when transportRevision bumps and not on first load", async () => {
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValue(new Response(null, { status: 200 }));
    try {
      const { rerenderPlayer } = renderPlayer({
        planRevision: 1,
        transportRevision: 0,
      });
      // First load with transportRevision 0: the transport did not change, so
      // no warm fetch fires.
      expect(fetchMock).not.toHaveBeenCalled();

      // A transport-changing replan (same URL, bumped revision) warms the
      // stream exactly once, before the transport effect reloads the element.
      rerenderPlayer({ planRevision: 2, transportRevision: 2 });
      expect(fetchMock).toHaveBeenCalledTimes(1);
      expect(fetchMock).toHaveBeenCalledWith(
        "/api/v1/stream/session-1?token=token",
        expect.objectContaining({ method: "GET", signal: expect.any(AbortSignal) }),
      );

      // The same transportRevision across a further plan revision must not
      // refire the warm fetch.
      rerenderPlayer({ planRevision: 3, transportRevision: 2 });
      expect(fetchMock).toHaveBeenCalledTimes(1);
    } finally {
      fetchMock.mockRestore();
    }
  });

  it("refreshes the subtitle inventory once per plan_id on a source-changed signal", async () => {
    const onRefreshSubtitles = vi.fn();
    const planA = fixturePlanV3({ plan_id: "plan:aaa", plan_attempt_key: "v3:aaa" });
    const { rerenderPlayer } = renderPlayer({ plan: planA, onRefreshSubtitles });
    expect(subtitleHooks.vttSourceChanged).toBeTypeOf("function");
    expect(subtitleHooks.assSourceChanged).toBeTypeOf("function");

    // The VTT window fetch and the ASS fetch can both see the rotation; both
    // signals for the same plan must collapse into a single refresh.
    act(() => subtitleHooks.vttSourceChanged?.());
    act(() => subtitleHooks.assSourceChanged?.());
    expect(onRefreshSubtitles).toHaveBeenCalledTimes(1);

    // A new plan (the refresh's own replan re-mints the URLs) re-arms the
    // signal: the next rotation for it refreshes again.
    const planB = fixturePlanV3({ plan_id: "plan:bbb", plan_attempt_key: "v3:bbb" });
    rerenderPlayer({ plan: planB });
    act(() => subtitleHooks.vttSourceChanged?.());
    expect(onRefreshSubtitles).toHaveBeenCalledTimes(2);
  });

  it("does not reload when only player_start_seconds changes (reused-transport replan)", async () => {
    const removeAttrSpy = vi.spyOn(HTMLMediaElement.prototype, "removeAttribute");
    try {
      const startPlan = fixturePlanV3({
        ...directPlan,
        timeline: {
          ...directPlan.timeline,
          player_start_seconds: 100,
        },
      });
      const { rerenderPlayer } = renderPlayer({
        plan: startPlan,
        planRevision: 1,
        transportRevision: 1,
      });
      removeAttrSpy.mockClear();

      // A reused-transport subtitle replan rewrites player_start_seconds to the
      // live playhead; the stream URL and transport identity are unchanged.
      const driftedPlan = fixturePlanV3({
        ...directPlan,
        timeline: {
          ...directPlan.timeline,
          player_start_seconds: 148,
        },
      });
      rerenderPlayer({
        plan: driftedPlan,
        planRevision: 2,
        transportRevision: 1,
      });
      expect(removeAttrSpy).not.toHaveBeenCalled();

      // A genuine transport change must still tear the element down.
      rerenderPlayer({
        plan: driftedPlan,
        planRevision: 3,
        transportRevision: 2,
      });
      expect(removeAttrSpy).toHaveBeenCalledWith("src");
    } finally {
      removeAttrSpy.mockRestore();
    }
  });

  it("does not reload when only player_start_seconds changes on a reused HLS transport", async () => {
    vi.spyOn(HTMLMediaElement.prototype, "canPlayType").mockImplementation((mime) =>
      mime === "application/vnd.apple.mpegurl" ? "probably" : "",
    );
    const removeAttrSpy = vi.spyOn(HTMLMediaElement.prototype, "removeAttribute");
    try {
      const startPlan = fixturePlanV3({
        timeline: {
          ...fixturePlanV3().timeline,
          player_start_seconds: 100,
        },
      });
      const { rerenderPlayer } = renderPlayer({
        plan: startPlan,
        planRevision: 1,
        transportRevision: 1,
        streamUrl: "/api/v1/stream/session-1/master.m3u8?token=token",
      });
      removeAttrSpy.mockClear();

      const driftedPlan = fixturePlanV3({
        timeline: {
          ...fixturePlanV3().timeline,
          player_start_seconds: 148,
        },
      });
      rerenderPlayer({
        plan: driftedPlan,
        planRevision: 2,
        transportRevision: 1,
      });
      expect(removeAttrSpy).not.toHaveBeenCalled();

      rerenderPlayer({
        plan: driftedPlan,
        planRevision: 3,
        transportRevision: 2,
      });
      expect(removeAttrSpy).toHaveBeenCalledWith("src");
    } finally {
      removeAttrSpy.mockRestore();
    }
  });

  it("rolls back an outstanding reanchor seek when its replan is refused", async () => {
    const onReanchorSeek = vi.fn();
    const plan = fixturePlanV3({
      ...directPlan,
      timeline: {
        ...directPlan.timeline,
        can_seek_anywhere: false,
      },
    });
    const { container, rerenderPlayer } = renderPlayer({ plan, onReanchorSeek });
    const video = container.querySelector("video");
    if (!video) throw new Error("expected video element");
    Object.defineProperty(video, "currentTime", { configurable: true, writable: true, value: 12 });

    act(() => {
      (controls.current as unknown as { onSeek: (s: number) => void }).onSeek(300);
    });
    expect(onReanchorSeek).toHaveBeenCalledWith(300);
    await waitFor(() =>
      expect((controls.current as unknown as { currentTime: number }).currentTime).toBe(300),
    );

    rerenderPlayer({ replanError: "Seek reanchor refused" });
    await waitFor(() =>
      expect((controls.current as unknown as { currentTime: number }).currentTime).toBe(12),
    );
  });

  it("does not move the scrubber on an unrelated replan refusal with no pending seek", async () => {
    const { container, rerenderPlayer } = renderPlayer({});
    const video = container.querySelector("video");
    if (!video) throw new Error("expected video element");
    Object.defineProperty(video, "currentTime", { configurable: true, writable: true, value: 50 });
    fireEvent.timeUpdate(video);
    await waitFor(() =>
      expect((controls.current as unknown as { currentTime: number }).currentTime).toBe(50),
    );

    Object.defineProperty(video, "currentTime", { configurable: true, writable: true, value: 5 });
    rerenderPlayer({ replanError: "Quality change refused" });
    await waitFor(() =>
      expect((controls.current as unknown as { currentTime: number }).currentTime).toBe(50),
    );
  });
});

describe("VideoPlayer buffered-first seeking", () => {
  beforeEach(() => {
    realtimeOptions.current = null;
    controls.current = null;
    subtitleTimeline.textOffsetSeconds = null;
    subtitleTimeline.assOffsetSeconds = null;
    subtitleHooks.vttSourceChanged = null;
    subtitleHooks.assSourceChanged = null;
    hlsJS.supported = false;
    hlsJS.constructed.mockClear();
    toastError.mockClear();
    playerSeek.mockClear();
    vi.spyOn(HTMLMediaElement.prototype, "play").mockResolvedValue(undefined);
    vi.spyOn(HTMLMediaElement.prototype, "pause").mockImplementation(() => {});
    vi.spyOn(HTMLMediaElement.prototype, "load").mockImplementation(() => {});
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  // The growing-manifest/remux routes force can_seek_anywhere=false, so they
  // exercise the path that used to fall straight through to the reanchor.
  const nonSeekablePlan = () =>
    fixturePlanV3({
      ...directPlan,
      timeline: { ...directPlan.timeline, can_seek_anywhere: false },
    });

  function renderSeekFixture(position: number, buffered: Array<[number, number]>) {
    const onReanchorSeek = vi.fn();
    const { container } = renderPlayer({ plan: nonSeekablePlan(), onReanchorSeek });
    const video = container.querySelector("video");
    if (!video) throw new Error("expected video element");
    Object.defineProperty(video, "currentTime", {
      configurable: true,
      writable: true,
      value: position,
    });
    setTimeRanges(video, "buffered", buffered);
    setTimeRanges(video, "seekable", []);
    return { video, onReanchorSeek };
  }

  function seekControls() {
    return controls.current as unknown as {
      currentTime: number;
      onSeek: (seconds: number) => void;
    };
  }

  it("skips 30s forward into the buffer without a reanchor", async () => {
    const { video, onReanchorSeek } = renderSeekFixture(10, [[0, 80]]);
    fireEvent.timeUpdate(video);
    await waitFor(() => expect(seekControls().currentTime).toBe(10));

    playerSeek.mockClear();
    act(() => seekControls().onSeek(seekControls().currentTime + 30));

    expect(onReanchorSeek).not.toHaveBeenCalled();
    expect(video.currentTime).toBe(40);
  });

  it("skips 30s backward into the buffer without a reanchor", async () => {
    const { video, onReanchorSeek } = renderSeekFixture(60, [[0, 80]]);
    fireEvent.timeUpdate(video);
    await waitFor(() => expect(seekControls().currentTime).toBe(60));

    playerSeek.mockClear();
    act(() => seekControls().onSeek(seekControls().currentTime - 30));

    expect(onReanchorSeek).not.toHaveBeenCalled();
    expect(video.currentTime).toBe(30);
  });

  it("seeks locally to a target just inside the buffer edge", async () => {
    const { video, onReanchorSeek } = renderSeekFixture(10, [[0, 40]]);
    fireEvent.timeUpdate(video);
    await waitFor(() => expect(seekControls().currentTime).toBe(10));

    act(() => seekControls().onSeek(39.9));

    expect(onReanchorSeek).not.toHaveBeenCalled();
    expect(video.currentTime).toBe(39.9);
  });

  it("still reanchors when the target sits exactly on the buffered end", async () => {
    const { video, onReanchorSeek } = renderSeekFixture(10, [[0, 40]]);
    fireEvent.timeUpdate(video);
    await waitFor(() => expect(seekControls().currentTime).toBe(10));

    act(() => seekControls().onSeek(seekControls().currentTime + 30));

    expect(onReanchorSeek).toHaveBeenCalledWith(40);
    expect(video.currentTime).toBe(10);
  });

  it("still reanchors when the target is outside every buffered range", async () => {
    const { video, onReanchorSeek } = renderSeekFixture(10, [[0, 40]]);
    fireEvent.timeUpdate(video);
    await waitFor(() => expect(seekControls().currentTime).toBe(10));

    act(() => seekControls().onSeek(100));

    expect(onReanchorSeek).toHaveBeenCalledWith(100);
    expect(video.currentTime).toBe(10);
  });

  it("keeps a random seek inside the buffer on the local path", async () => {
    const { video, onReanchorSeek } = renderSeekFixture(10, [[0, 200]]);
    fireEvent.timeUpdate(video);
    await waitFor(() => expect(seekControls().currentTime).toBe(10));

    act(() => seekControls().onSeek(150));

    expect(onReanchorSeek).not.toHaveBeenCalled();
    expect(video.currentTime).toBe(150);
  });
});

describe("VideoPlayer version switch UX", () => {
  beforeEach(() => {
    realtimeOptions.current = null;
    controls.current = null;
    subtitleTimeline.textOffsetSeconds = null;
    subtitleTimeline.assOffsetSeconds = null;
    hlsJS.supported = false;
    hlsJS.constructed.mockClear();
    vi.spyOn(HTMLMediaElement.prototype, "play").mockResolvedValue(undefined);
    vi.spyOn(HTMLMediaElement.prototype, "pause").mockImplementation(() => {});
    vi.spyOn(HTMLMediaElement.prototype, "load").mockImplementation(() => {});
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  const versionA = {
    file_id: 7,
    resolution: "1080p",
    codec_video: "h264",
    codec_audio: "aac",
    hdr: false,
    container: "mp4",
    file_size: 1,
    duration: 3600,
    bitrate: 1,
  };
  const versionB = {
    file_id: 99,
    resolution: "2160p",
    codec_video: "hevc",
    codec_audio: "aac",
    hdr: true,
    container: "mkv",
    file_size: 1,
    duration: 3600,
    bitrate: 1,
  };

  it("lights the clicked version as Requested optimistically via pendingSwitchFileId", async () => {
    const { rerenderPlayer } = renderPlayer({
      versions: [versionA, versionB],
      activeFileId: 7,
    });

    // Before the switch, the old plan names file 7 as requested.
    let props = controls.current as unknown as {
      versions?: Array<{
        fileId: number;
        isCurrentSource: boolean;
        isRequestedSource: boolean;
      }>;
    };
    expect(props.versions?.find((v) => v.fileId === 99)?.isRequestedSource).toBe(false);

    // The switch is in flight: pendingSwitchFileId names the clicked version.
    rerenderPlayer({ pendingSwitchFileId: 99 });
    props = controls.current as unknown as {
      versions?: Array<{
        fileId: number;
        isCurrentSource: boolean;
        isRequestedSource: boolean;
      }>;
    };
    expect(props.versions?.find((v) => v.fileId === 99)?.isRequestedSource).toBe(true);
    expect(props.versions?.find((v) => v.fileId === 7)?.isCurrentSource).toBe(true);
  });

  it("marks the path-matched candidate as Current Source for a resolved virtual row", () => {
    const virtualRow = {
      ...versionA,
      file_id: 100,
      container: "virtual",
      file_path: "virtual://movie/tt1?result=all",
    };
    const candidateRow = {
      ...versionA,
      file_id: 7,
      file_path: "/media/Movies/Example (2024)/Example.1080p.mkv",
    };
    const plan = fixturePlanV3({
      requested_media_file_id: 100,
      effective_media_file_id: 100,
      effective_virtual_uri: candidateRow.file_path,
    });

    renderPlayer({ plan, versions: [virtualRow, candidateRow], activeFileId: 100 });

    const props = controls.current as unknown as {
      versions?: Array<{ fileId: number; isCurrentSource: boolean; isRequestedSource: boolean }>;
    };
    // The collapsed VIRTUAL id stays in the plan, so only the path match can
    // light the concrete candidate the server is actually playing.
    expect(props.versions?.find((v) => v.fileId === 7)?.isCurrentSource).toBe(true);
    expect(props.versions?.find((v) => v.fileId === 100)?.isCurrentSource).toBe(false);
  });

  it("leaves the file_id match current when no effective virtual URI is published", () => {
    renderPlayer({ versions: [versionA, versionB], activeFileId: 7 });

    const props = controls.current as unknown as {
      versions?: Array<{ fileId: number; isCurrentSource: boolean }>;
    };
    expect(props.versions?.find((v) => v.fileId === 7)?.isCurrentSource).toBe(true);
    expect(props.versions?.find((v) => v.fileId === 99)?.isCurrentSource).toBe(false);
  });

  it("flags a version the catalog reports unavailable so the menu can warn", () => {
    const unavailableVersion = { ...versionB, available: false };
    renderPlayer({ versions: [versionA, unavailableVersion], activeFileId: 7 });

    const props = controls.current as unknown as {
      versions?: Array<{ fileId: number; unavailable?: boolean }>;
    };
    expect(props.versions?.find((v) => v.fileId === 99)?.unavailable).toBe(true);
    expect(props.versions?.find((v) => v.fileId === 7)?.unavailable).toBe(false);
  });

  it("shows the quality ellipsis only for quality replans, not track changes", async () => {
    const { rerenderPlayer } = renderPlayer({});

    // A track-change replan must not flicker the quality label.
    rerenderPlayer({ replanning: true, replanningQuality: false });
    expect((controls.current as unknown as { isTranscoding: boolean }).isTranscoding).toBe(false);

    // A quality/output replan does.
    rerenderPlayer({ replanning: true, replanningQuality: true });
    expect((controls.current as unknown as { isTranscoding: boolean }).isTranscoding).toBe(true);
  });

  it("remaps a manual subtitle selection by identity across a version switch", async () => {
    const englishTrack: PlayerSubtitleInfo = {
      index: 0,
      media_file_id: 7,
      track_id: "file:7:subtitle:0",
      language: "en",
      codec: "srt",
      label: "English",
      source: "external",
      url: "/stream/session-1/subtitles/0.vtt",
    };
    const frenchTrack: PlayerSubtitleInfo = {
      index: 1,
      media_file_id: 7,
      track_id: "file:7:subtitle:1",
      language: "fr",
      codec: "srt",
      label: "French",
      source: "external",
      url: "/stream/session-1/subtitles/1.vtt",
    };
    const { rerenderPlayer } = renderPlayer({
      subtitleUrls: [englishTrack, frenchTrack],
      subtitleMode: "always",
      preferredSubtitleLanguage: "en",
    });

    // The viewer manually picks French (index 1).
    act(() => {
      controls.current?.onSubtitleSelect?.(1);
    });
    await waitFor(() => expect(controls.current?.activeSubtitleIndex).toBe(1));

    // The new version's inventory reorders tracks: French is now index 0 and
    // English index 1. The raw index would silently switch to English.
    const nextPlan = fixturePlanV3({
      ...directPlan,
      plan_id: "plan:switched-version",
      plan_attempt_key: "v3:switched-version",
      requested_media_file_id: 99,
      effective_media_file_id: 99,
    });
    const frenchInNewFile: PlayerSubtitleInfo = {
      index: 0,
      media_file_id: 99,
      track_id: "file:99:subtitle:0",
      language: "fr",
      codec: "srt",
      label: "French",
      source: "external",
      url: "/stream/session-99/subtitles/0.vtt",
    };
    const englishInNewFile: PlayerSubtitleInfo = {
      index: 1,
      media_file_id: 99,
      track_id: "file:99:subtitle:1",
      language: "en",
      codec: "srt",
      label: "English",
      source: "external",
      url: "/stream/session-99/subtitles/1.vtt",
    };
    rerenderPlayer({
      plan: nextPlan,
      planRevision: 2,
      subtitleUrls: [frenchInNewFile, englishInNewFile],
    });

    // The manual French selection is remapped to the French track in the new
    // inventory (index 0) rather than keeping the raw index 1 (English).
    await waitFor(() => expect(controls.current?.activeSubtitleIndex).toBe(0));
  });

  it("falls back to the raw index when no identity match exists in the new inventory", async () => {
    const englishTrack: PlayerSubtitleInfo = {
      index: 0,
      media_file_id: 7,
      track_id: "file:7:subtitle:0",
      language: "en",
      codec: "srt",
      label: "English",
      source: "external",
      url: "/stream/session-1/subtitles/0.vtt",
    };
    const { rerenderPlayer } = renderPlayer({
      subtitleUrls: [englishTrack],
      subtitleMode: "always",
      preferredSubtitleLanguage: "en",
    });

    act(() => {
      controls.current?.onSubtitleSelect?.(0);
    });
    await waitFor(() => expect(controls.current?.activeSubtitleIndex).toBe(0));

    const nextPlan = fixturePlanV3({
      ...directPlan,
      plan_id: "plan:switched-version-2",
      plan_attempt_key: "v3:switched-version-2",
      requested_media_file_id: 99,
      effective_media_file_id: 99,
    });
    // The new file has a German track at index 0 — no identity match for the
    // English selection, so the raw index 0 is kept.
    const germanInNewFile: PlayerSubtitleInfo = {
      index: 0,
      media_file_id: 99,
      track_id: "file:99:subtitle:0",
      language: "de",
      codec: "srt",
      label: "German",
      source: "external",
      url: "/stream/session-99/subtitles/0.vtt",
    };
    rerenderPlayer({
      plan: nextPlan,
      planRevision: 2,
      subtitleUrls: [germanInNewFile],
    });

    await waitFor(() => expect(controls.current?.activeSubtitleIndex).toBe(0));
  });

  it("resets to auto-select when neither identity nor raw index matches", async () => {
    const englishTrack: PlayerSubtitleInfo = {
      index: 0,
      media_file_id: 7,
      track_id: "file:7:subtitle:0",
      language: "en",
      codec: "srt",
      label: "English",
      source: "external",
      url: "/stream/session-1/subtitles/0.vtt",
    };
    const { rerenderPlayer } = renderPlayer({
      subtitleUrls: [englishTrack],
      subtitleMode: "always",
      preferredSubtitleLanguage: "en",
    });

    act(() => {
      controls.current?.onSubtitleSelect?.(0);
    });
    await waitFor(() => expect(controls.current?.activeSubtitleIndex).toBe(0));

    const nextPlan = fixturePlanV3({
      ...directPlan,
      plan_id: "plan:switched-version-3",
      plan_attempt_key: "v3:switched-version-3",
      requested_media_file_id: 99,
      effective_media_file_id: 99,
    });
    // The new file has only a German track at index 5 — neither the identity
    // nor the raw index (0) exists, so the selection resets to auto-select.
    const germanInNewFile: PlayerSubtitleInfo = {
      index: 5,
      media_file_id: 99,
      track_id: "file:99:subtitle:5",
      language: "de",
      codec: "srt",
      label: "German",
      source: "external",
      url: "/stream/session-99/subtitles/5.vtt",
    };
    rerenderPlayer({
      plan: nextPlan,
      planRevision: 2,
      subtitleUrls: [germanInNewFile],
    });

    // Auto-select with mode "always" and preferred language "en" finds no
    // English track, so subtitles reset to off.
    await waitFor(() => expect(controls.current?.activeSubtitleIndex).toBeNull());
  });
});
