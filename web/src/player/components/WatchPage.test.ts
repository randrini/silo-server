import { act, fireEvent, render, screen } from "@testing-library/react";
import { createElement } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { fixturePlanV3 } from "../protocol-v3.fixtures";
import { derivePersistedSubtitleMode } from "../utils/subtitleMode";
import type { UsePlaybackSessionResult } from "../hooks/usePlaybackSession";
import type { PlayerAudioTrack, PlayerFileVersion, WatchPageProps } from "../types";
import { WatchPage } from "./WatchPage";

const playbackSessionMock = vi.hoisted(() => vi.fn());
const videoPlayerMock = vi.hoisted(() => vi.fn());
const toastErrorMock = vi.hoisted(() => vi.fn());
const fetchWatchDetailMock = vi.hoisted(() => vi.fn());

vi.mock("../hooks/usePlaybackSession", () => ({
  usePlaybackSession: playbackSessionMock,
}));
vi.mock("@/hooks/queries/items", () => ({
  fetchWatchDetail: fetchWatchDetailMock,
}));
vi.mock("./VideoPlayer", () => ({
  VideoPlayer: (props: unknown) => {
    videoPlayerMock(props);
    return "Mounted video player";
  },
}));
vi.mock("../context/PlayerConfigContext", () => ({
  usePlayerConfig: () => ({
    apiBaseUrl: "/api/v1",
    getAccessToken: () => "token",
    getProfileId: () => "profile-1",
    getDeviceId: () => "test-device",
  }),
}));
vi.mock("@tanstack/react-query", () => ({
  useQueryClient: () => ({ fetchQuery: vi.fn() }),
}));
vi.mock("@/playback/watchPlaybackContext", () => ({
  useWatchPlaybackController: () => ({ startPlayback: vi.fn() }),
}));
vi.mock("../hooks/useWatchTogetherRoomConnection", () => ({
  useWatchTogetherRoomConnection: () => ({ room: null }),
}));
vi.mock("sonner", () => ({
  toast: { error: toastErrorMock },
}));

const version: PlayerFileVersion = {
  file_id: 7,
  resolution: "1080p",
  codec_video: "h264",
  codec_audio: "aac",
  hdr: false,
  container: "mp4",
  file_size: 1,
  duration: 3600,
  bitrate: 1,
  chapters: [{ index: 0, title: "Chapter", start_seconds: 0, end_seconds: 3600, source: "test" }],
};

const watchPageProps: WatchPageProps = {
  contentId: "content-1",
  title: "Test movie",
  versions: [version],
  subtitles: [],
  intro: null,
  credits: null,
  onExit: vi.fn(),
};

function playbackSession(
  overrides: Partial<UsePlaybackSessionResult> = {},
): UsePlaybackSessionResult {
  return {
    plan: fixturePlanV3(),
    planRevision: 1,
    transportRevision: 1,
    streamUrl: "/stream/session-1",
    sessionId: "session-1",
    playbackAttemptId: "attempt-1",
    mediaFileId: 7,
    initialPosition: 0,
    audioTrackIndex: 0,
    durationSeconds: 3600,
    subtitleUrls: [],
    planAudioTracks: [],
    qualityPreference: "original",
    shouldAutoPlay: true,
    loading: false,
    replacing: false,
    replanning: false,
    replanningQuality: false,
    pendingSwitchFileId: null,
    errorTitle: null,
    error: null,
    initialSubtitleErrorTitle: null,
    initialSubtitleError: null,
    switchVersion: vi.fn(),
    switchAudioTrack: vi.fn(),
    changeSubtitleTrack: vi.fn(),
    changeQuality: vi.fn(),
    recoverFromFailure: vi.fn(),
    invalidatePlan: vi.fn().mockResolvedValue(true),
    reanchorSeek: vi.fn(),
    refreshSubtitles: vi.fn(),
    applySubtitleTrack: vi.fn(),
    applyAudioInventory: vi.fn(),
    updatePlaybackState: vi.fn(),
    reportEvent: vi.fn(),
    ...overrides,
  };
}

beforeEach(() => {
  playbackSessionMock.mockReset();
  videoPlayerMock.mockReset();
  toastErrorMock.mockReset();
});

describe("derivePersistedSubtitleMode", () => {
  it("persists an enabled mode when a subtitle track is selected", () => {
    expect(derivePersistedSubtitleMode(3)).toBe("always");
  });

  it("persists off when subtitles are disabled", () => {
    expect(derivePersistedSubtitleMode(null)).toBe("off");
  });
});

describe("WatchPage playback errors", () => {
  it("keeps the player mounted when a replan fails with an active plan", () => {
    playbackSessionMock.mockReturnValue(
      playbackSession({ errorTitle: "Quality change failed", error: "Temporary server error" }),
    );

    render(createElement(WatchPage, watchPageProps));

    expect(screen.getByText("Mounted video player")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Go Back" })).not.toBeInTheDocument();
  });

  it("shows the fatal error screen when startup fails without a plan", () => {
    playbackSessionMock.mockReturnValue(
      playbackSession({
        plan: null,
        streamUrl: null,
        sessionId: null,
        mediaFileId: null,
        errorTitle: "Playback unavailable",
        error: "Failed to start playback",
      }),
    );

    render(createElement(WatchPage, watchPageProps));

    expect(screen.getByText("Failed to start playback")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Go Back" })).toBeInTheDocument();
    expect(screen.queryByText("Mounted video player")).not.toBeInTheDocument();
  });

  it("keeps a refused initial bitmap subtitle off without treating it as a playback error", () => {
    playbackSessionMock.mockReturnValue(
      playbackSession({
        initialSubtitleErrorTitle: "That subtitle track can't be used",
        initialSubtitleError: "Enable HDR transcoding to use this subtitle.",
      }),
    );

    render(
      createElement(WatchPage, {
        ...watchPageProps,
        subtitleMode: "always",
        showForcedSubtitles: true,
      }),
    );

    const props = videoPlayerMock.mock.calls[0]?.[0] as {
      subtitleMode?: string;
      showForcedSubtitles?: boolean;
      replanError?: string | null;
    };
    expect(props.subtitleMode).toBe("off");
    expect(props.showForcedSubtitles).toBe(false);
    expect(props.replanError).toBeNull();
    expect(toastErrorMock).toHaveBeenCalledWith("That subtitle track can't be used", {
      description: "Enable HDR transcoding to use this subtitle.",
    });
  });
});

describe("WatchPage playback state", () => {
  it("keeps the session resume anchor current while forwarding state", () => {
    const updatePlaybackState = vi.fn();
    const onPlaybackStateChange = vi.fn();
    playbackSessionMock.mockReturnValue(playbackSession({ updatePlaybackState }));

    render(createElement(WatchPage, { ...watchPageProps, onPlaybackStateChange }));

    const props = videoPlayerMock.mock.calls[0]?.[0] as {
      onPlaybackStateChange?: (state: {
        currentTime: number;
        duration: number;
        playing: boolean;
      }) => void;
    };
    const state = { currentTime: 321, duration: 3600, playing: true };
    props.onPlaybackStateChange?.(state);

    expect(updatePlaybackState).toHaveBeenCalledWith(321, true);
    expect(onPlaybackStateChange).toHaveBeenCalledWith(state);
  });
});

describe("WatchPage audio menu", () => {
  it("prefers the plan's audio inventory over item metadata", () => {
    const planAudioTracks = [
      { language: "eng", codec: "aac", channels: 2, default: true },
      { language: "spa", codec: "ac3", channels: 6, default: false },
    ];
    playbackSessionMock.mockReturnValue(playbackSession({ planAudioTracks }));

    render(createElement(WatchPage, watchPageProps));

    const props = videoPlayerMock.mock.calls[0]?.[0] as { audioTracks?: unknown[] };
    expect(props.audioTracks).toEqual(planAudioTracks);
  });

  it("falls back to the version's item metadata when the plan publishes no inventory", () => {
    const versionWithTracks: PlayerFileVersion = {
      ...version,
      audio_tracks: [{ language: "eng", codec: "aac", channels: 2, default: true }],
    };
    playbackSessionMock.mockReturnValue(playbackSession({ planAudioTracks: [] }));

    render(
      createElement(WatchPage, {
        ...watchPageProps,
        versions: [versionWithTracks],
      }),
    );

    const props = videoPlayerMock.mock.calls[0]?.[0] as { audioTracks?: unknown[] };
    expect(props.audioTracks).toEqual(versionWithTracks.audio_tracks);
  });
});

describe("WatchPage version switch feedback", () => {
  it("shows a non-blocking switching indicator while replacing with an active plan", () => {
    playbackSessionMock.mockReturnValue(playbackSession({ replacing: true }));

    render(createElement(WatchPage, watchPageProps));

    expect(screen.getByRole("status", { name: "Switching version" })).toBeInTheDocument();
    expect(screen.getByText("Switching version…")).toBeInTheDocument();
    // The old stream keeps playing: the player stays mounted.
    expect(screen.getByText("Mounted video player")).toBeInTheDocument();
  });

  it("keeps the full-screen loading overlay for the no-plan case", () => {
    playbackSessionMock.mockReturnValue(
      playbackSession({
        plan: null,
        streamUrl: null,
        sessionId: null,
        mediaFileId: null,
        loading: true,
        replacing: true,
      }),
    );

    render(createElement(WatchPage, watchPageProps));

    expect(screen.getByText("Loading player...")).toBeInTheDocument();
    expect(screen.queryByRole("status", { name: "Switching version" })).not.toBeInTheDocument();
    expect(screen.queryByText("Mounted video player")).not.toBeInTheDocument();
  });

  it("forwards the quality-replan and pending-switch flags to the player", () => {
    playbackSessionMock.mockReturnValue(
      playbackSession({ replanningQuality: true, pendingSwitchFileId: 99 }),
    );

    render(createElement(WatchPage, watchPageProps));

    const props = videoPlayerMock.mock.calls[0]?.[0] as {
      replanningQuality?: boolean;
      pendingSwitchFileId?: number | null;
    };
    expect(props.replanningQuality).toBe(true);
    expect(props.pendingSwitchFileId).toBe(99);
  });

  it("shows a dismissible notice when the server played a different version than auto-selected", () => {
    playbackSessionMock.mockReturnValue(
      playbackSession({
        plan: fixturePlanV3({ requested_media_file_id: 7, effective_media_file_id: 8 }),
      }),
    );

    render(createElement(WatchPage, watchPageProps));

    expect(
      screen.getByText(
        "Playing a different version than selected — the requested version isn't playable on this device.",
      ),
    ).toBeInTheDocument();

    // Dismissing hides the notice.
    fireEvent.click(screen.getByRole("button", { name: "Dismiss version notice" }));
    expect(
      screen.queryByText(
        "Playing a different version than selected — the requested version isn't playable on this device.",
      ),
    ).not.toBeInTheDocument();
  });

  it("does not show the version-swap notice when the selection was explicit", () => {
    playbackSessionMock.mockReturnValue(
      playbackSession({
        plan: fixturePlanV3({ requested_media_file_id: 7, effective_media_file_id: 8 }),
      }),
    );

    render(createElement(WatchPage, { ...watchPageProps, explicitFileSelection: true }));

    expect(
      screen.queryByText(
        "Playing a different version than selected — the requested version isn't playable on this device.",
      ),
    ).not.toBeInTheDocument();
  });

  it("does not show the version-swap notice when the plan kept the requested file", () => {
    playbackSessionMock.mockReturnValue(
      playbackSession({
        plan: fixturePlanV3({ requested_media_file_id: 7, effective_media_file_id: 7 }),
      }),
    );

    render(createElement(WatchPage, watchPageProps));

    expect(
      screen.queryByText(
        "Playing a different version than selected — the requested version isn't playable on this device.",
      ),
    ).not.toBeInTheDocument();
  });
});

const planSubtitle = {
  index: 0,
  language: "en",
  codec: "srt",
  label: "English",
  source: "embedded" as const,
  url: "/api/v1/stream/session-1/subtitles/0.vtt",
};

const richerAudioTracks: PlayerAudioTrack[] = [
  { codec: "eac3", channels: 6, layout: "5.1", language: "eng", default: true },
  { codec: "ac3", channels: 6, layout: "5.1", language: "spa", index: 9 },
];

const virtualVersion: PlayerFileVersion = { ...version, container: "virtual" };

describe("WatchPage live inventory refresh", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    fetchWatchDetailMock.mockReset();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("polls only an incomplete virtual audio inventory and fills it in", async () => {
    const applyAudioInventory = vi.fn();
    const switchVersion = vi.fn();
    playbackSessionMock.mockReturnValue(
      playbackSession({
        planAudioTracks: [{ codec: "eac3", channels: 6, layout: "5.1", language: "eng" }],
        subtitleUrls: [planSubtitle],
        applyAudioInventory,
        switchVersion,
      }),
    );
    fetchWatchDetailMock.mockResolvedValue({
      versions: [{ ...virtualVersion, audio_tracks: richerAudioTracks }],
    });

    render(createElement(WatchPage, { ...watchPageProps, versions: [virtualVersion] }));

    // Nothing is fetched before the first interval.
    expect(fetchWatchDetailMock).not.toHaveBeenCalled();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(20_000);
    });

    expect(fetchWatchDetailMock).toHaveBeenCalledTimes(1);
    expect(applyAudioInventory).toHaveBeenCalledWith(richerAudioTracks);
    // Menu data only: no restart or stream swap.
    expect(switchVersion).not.toHaveBeenCalled();
    const playerCalls = videoPlayerMock.mock.calls;
    const playerProps = playerCalls[playerCalls.length - 1]?.[0] as { streamUrl?: string };
    expect(playerProps.streamUrl).toBe("/stream/session-1");
  });

  it("does not poll a local file or a complete inventory", async () => {
    playbackSessionMock.mockReturnValue(
      playbackSession({
        planAudioTracks: [{ codec: "eac3", channels: 6, layout: "5.1", language: "eng" }],
        subtitleUrls: [planSubtitle],
      }),
    );

    render(createElement(WatchPage, watchPageProps));

    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });

    expect(fetchWatchDetailMock).not.toHaveBeenCalled();
  });

  it("requests a subtitle replan when the catalog gains tracks the plan lacks", async () => {
    const refreshSubtitles = vi.fn();
    playbackSessionMock.mockReturnValue(
      playbackSession({
        planAudioTracks: richerAudioTracks,
        subtitleUrls: [],
        refreshSubtitles,
      }),
    );
    fetchWatchDetailMock.mockResolvedValue({
      versions: [
        {
          ...virtualVersion,
          subtitle_tracks: [{ index: 13, language: "en", codec: "pgs", title: "English" }],
        },
      ],
    });

    render(createElement(WatchPage, { ...watchPageProps, versions: [virtualVersion] }));

    await act(async () => {
      await vi.advanceTimersByTimeAsync(20_000);
    });

    expect(refreshSubtitles).toHaveBeenCalledTimes(1);
  });

  it("stops after the attempt cap when the inventory never fills in", async () => {
    playbackSessionMock.mockReturnValue(
      playbackSession({
        planAudioTracks: [{ codec: "eac3", channels: 6, layout: "5.1", language: "eng" }],
        subtitleUrls: [],
      }),
    );
    // The catalog never grows past the plan's single track.
    fetchWatchDetailMock.mockResolvedValue({
      versions: [
        {
          ...virtualVersion,
          audio_tracks: [{ codec: "eac3", channels: 6, layout: "5.1", language: "eng" }],
        },
      ],
    });

    render(createElement(WatchPage, { ...watchPageProps, versions: [virtualVersion] }));

    await act(async () => {
      await vi.advanceTimersByTimeAsync(20_000 * 10);
    });

    // One attempt per interval, capped at five.
    expect(fetchWatchDetailMock).toHaveBeenCalledTimes(5);
  });
});
