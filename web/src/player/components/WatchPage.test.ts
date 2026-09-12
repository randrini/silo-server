import { act, fireEvent, render, screen } from "@testing-library/react";
import { createElement } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { fixturePlanV3 } from "../protocol-v3.fixtures";
import { derivePersistedSubtitleMode } from "../utils/subtitleMode";
import type { UsePlaybackSessionResult } from "../hooks/usePlaybackSession";
import type { PlayerAudioTrack, PlayerFileVersion, WatchPageProps } from "../types";
import {
  INVENTORY_REFRESH_DEADLINE_MS,
  INVENTORY_REFRESH_INTERVAL_MS,
  WatchPage,
} from "./WatchPage";

const playbackSessionMock = vi.hoisted(() => vi.fn());
const videoPlayerMock = vi.hoisted(() => vi.fn());
const toastErrorMock = vi.hoisted(() => vi.fn());
const fetchWatchDetailMock = vi.hoisted(() => vi.fn());
const fetchQueryMock = vi.hoisted(() => vi.fn());

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
  useQueryClient: () => ({ fetchQuery: fetchQueryMock }),
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
    effectiveVirtualUri: null,
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
    errorReason: null,
    errorRetryable: false,
    retrying: false,
    initialSubtitleErrorTitle: null,
    initialSubtitleError: null,
    switchVersion: vi.fn(),
    retryStart: vi.fn(),
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
  fetchWatchDetailMock.mockReset();
  // The component reads watch detail through the shared react-query cache. The
  // fake client passes straight through to the queryFn so these tests keep
  // exercising the poll's attempt/deadline logic; the cache dedupe itself is
  // covered in items.test.ts.
  fetchQueryMock.mockReset();
  fetchQueryMock.mockImplementation((options: { queryFn: () => unknown }) => options.queryFn());
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

  it("offers Try again for a retryable virtual-source terminal and keeps Go Back", () => {
    const retryStart = vi.fn();
    playbackSessionMock.mockReturnValue(
      playbackSession({
        plan: null,
        streamUrl: null,
        sessionId: null,
        mediaFileId: null,
        errorTitle: "Playback unavailable",
        error: "The virtual source could not be resolved for playback.",
        errorReason: "virtual_source_unavailable",
        errorRetryable: true,
        retryStart,
      }),
    );

    render(createElement(WatchPage, watchPageProps));

    expect(
      screen.getByText("The virtual source could not be resolved for playback."),
    ).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(retryStart).toHaveBeenCalledTimes(1);
    expect(screen.getByRole("button", { name: "Go Back" })).toBeInTheDocument();
  });

  it("keeps a non-retryable terminal a Go Back-only dead-end", () => {
    playbackSessionMock.mockReturnValue(
      playbackSession({
        plan: null,
        streamUrl: null,
        sessionId: null,
        mediaFileId: null,
        errorTitle: "This video is no longer available",
        error: "The file needed to play it can't be found right now.",
        errorReason: "source_unavailable",
        errorRetryable: false,
      }),
    );

    render(createElement(WatchPage, watchPageProps));

    expect(screen.queryByRole("button", { name: "Try again" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Go Back" })).toBeInTheDocument();
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

describe("WatchPage virtual version substitution notice", () => {
  const notice =
    "Playing a different version than selected — the requested version isn't playable on this device.";
  const virtualRow: PlayerFileVersion = {
    ...version,
    file_id: 100,
    container: "virtual",
    file_path: "virtual://movie/tt1?result=all",
  };
  const candidateRow: PlayerFileVersion = {
    ...version,
    file_id: 7,
    file_path: "/media/Movies/Example (2024)/Example.1080p.mkv",
  };

  it("fires when the resolved virtual candidate differs from the requested row's path", () => {
    const effectiveVirtualUri = candidateRow.file_path;
    playbackSessionMock.mockReturnValue(
      playbackSession({
        mediaFileId: 100,
        effectiveVirtualUri: effectiveVirtualUri ?? null,
        plan: fixturePlanV3({
          requested_media_file_id: 100,
          effective_media_file_id: 100,
          effective_virtual_uri: effectiveVirtualUri,
        }),
      }),
    );

    render(createElement(WatchPage, { ...watchPageProps, versions: [virtualRow, candidateRow] }));

    expect(screen.getByText(notice)).toBeInTheDocument();
  });

  it("stays quiet when the requested row is the effective virtual candidate", () => {
    const effectiveVirtualUri = candidateRow.file_path;
    playbackSessionMock.mockReturnValue(
      playbackSession({
        mediaFileId: 7,
        effectiveVirtualUri: effectiveVirtualUri ?? null,
        plan: fixturePlanV3({
          requested_media_file_id: 7,
          effective_media_file_id: 7,
          effective_virtual_uri: effectiveVirtualUri,
        }),
      }),
    );

    render(createElement(WatchPage, { ...watchPageProps, versions: [virtualRow, candidateRow] }));

    expect(screen.queryByText(notice)).not.toBeInTheDocument();
  });

  it("does not fire for an explicit selection even when the virtual candidate differs", () => {
    const effectiveVirtualUri = candidateRow.file_path;
    playbackSessionMock.mockReturnValue(
      playbackSession({
        mediaFileId: 100,
        effectiveVirtualUri: effectiveVirtualUri ?? null,
        plan: fixturePlanV3({
          requested_media_file_id: 100,
          effective_media_file_id: 100,
          effective_virtual_uri: effectiveVirtualUri,
        }),
      }),
    );

    render(
      createElement(WatchPage, {
        ...watchPageProps,
        versions: [virtualRow, candidateRow],
        explicitFileSelection: true,
      }),
    );

    expect(screen.queryByText(notice)).not.toBeInTheDocument();
  });
});

describe("WatchPage effective virtual version", () => {
  it("selects the path-matched candidate when the session's id is the VIRTUAL row", () => {
    const virtualRow: PlayerFileVersion = {
      ...version,
      file_id: 100,
      container: "virtual",
      file_path: "virtual://movie/tt1?result=all",
    };
    const candidateRow: PlayerFileVersion = {
      ...version,
      file_id: 7,
      file_path: "/media/Movies/Example (2024)/Example.1080p.mkv",
    };
    playbackSessionMock.mockReturnValue(
      playbackSession({
        mediaFileId: 100,
        effectiveVirtualUri: candidateRow.file_path ?? null,
      }),
    );

    render(createElement(WatchPage, { ...watchPageProps, versions: [virtualRow, candidateRow] }));

    const props = videoPlayerMock.mock.calls[0]?.[0] as { selectedVersion?: PlayerFileVersion };
    expect(props.selectedVersion?.file_id).toBe(7);
  });

  it("keeps file_id matching when the plan publishes no effective virtual URI", () => {
    playbackSessionMock.mockReturnValue(
      playbackSession({ mediaFileId: 7, effectiveVirtualUri: null }),
    );

    render(createElement(WatchPage, watchPageProps));

    const props = videoPlayerMock.mock.calls[0]?.[0] as { selectedVersion?: PlayerFileVersion };
    expect(props.selectedVersion?.file_id).toBe(7);
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

  it("reads the effective virtual candidate's inventory when the id names the collapsed row", async () => {
    const applyAudioInventory = vi.fn();
    const refreshSubtitles = vi.fn();
    // The session targets the collapsed VIRTUAL row (id 7); probes persist to
    // the resolved candidate row (id 8), which the plan identifies by path.
    const collapsedVirtualVersion: PlayerFileVersion = {
      ...virtualVersion,
      file_id: 7,
      file_path: "virtual://movie/tt1?result=all",
    };
    const candidateVersion: PlayerFileVersion = {
      ...virtualVersion,
      file_id: 8,
      file_path: "/media/Movies/Example (2024)/Example.1080p.mkv",
    };
    playbackSessionMock.mockReturnValue(
      playbackSession({
        mediaFileId: 7,
        effectiveVirtualUri: candidateVersion.file_path ?? null,
        planAudioTracks: [{ codec: "eac3", channels: 6, layout: "5.1", language: "eng" }],
        subtitleUrls: [],
        applyAudioInventory,
        refreshSubtitles,
      }),
    );
    // The collapsed row carries no probe inventory; only the candidate does.
    fetchWatchDetailMock.mockResolvedValue({
      versions: [
        collapsedVirtualVersion,
        {
          ...candidateVersion,
          audio_tracks: richerAudioTracks,
          subtitle_tracks: [{ index: 13, language: "en", codec: "pgs", title: "English" }],
        },
      ],
    });

    render(
      createElement(WatchPage, {
        ...watchPageProps,
        versions: [collapsedVirtualVersion, candidateVersion],
      }),
    );

    await act(async () => {
      await vi.advanceTimersByTimeAsync(INVENTORY_REFRESH_INTERVAL_MS);
    });

    expect(applyAudioInventory).toHaveBeenCalledWith(richerAudioTracks);
    expect(refreshSubtitles).toHaveBeenCalledTimes(1);
  });

  it("falls back to the collapsed id when the plan publishes no effective virtual URI", async () => {
    const applyAudioInventory = vi.fn();
    const collapsedVersion: PlayerFileVersion = {
      ...virtualVersion,
      file_id: 7,
      audio_tracks: richerAudioTracks,
    };
    const otherVersion: PlayerFileVersion = {
      ...virtualVersion,
      file_id: 8,
      file_path: "/media/Movies/Example (2024)/Example.2160p.mkv",
      audio_tracks: [],
    };
    playbackSessionMock.mockReturnValue(
      playbackSession({
        mediaFileId: 7,
        effectiveVirtualUri: null,
        planAudioTracks: [{ codec: "eac3", channels: 6, layout: "5.1", language: "eng" }],
        subtitleUrls: [planSubtitle],
        applyAudioInventory,
      }),
    );
    fetchWatchDetailMock.mockResolvedValue({ versions: [collapsedVersion, otherVersion] });

    render(
      createElement(WatchPage, { ...watchPageProps, versions: [collapsedVersion, otherVersion] }),
    );

    await act(async () => {
      await vi.advanceTimersByTimeAsync(INVENTORY_REFRESH_INTERVAL_MS);
    });

    expect(applyAudioInventory).toHaveBeenCalledWith(richerAudioTracks);
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

  it("retries transient fetch errors without spending the attempt budget", async () => {
    const applyAudioInventory = vi.fn();
    playbackSessionMock.mockReturnValue(
      playbackSession({
        planAudioTracks: [{ codec: "eac3", channels: 6, layout: "5.1", language: "eng" }],
        subtitleUrls: [planSubtitle],
        applyAudioInventory,
      }),
    );
    fetchWatchDetailMock
      .mockRejectedValueOnce(new Error("network"))
      .mockRejectedValueOnce(new Error("network"))
      .mockRejectedValueOnce(new Error("network"))
      .mockResolvedValue({
        versions: [{ ...virtualVersion, audio_tracks: richerAudioTracks }],
      });

    render(createElement(WatchPage, { ...watchPageProps, versions: [virtualVersion] }));

    await act(async () => {
      await vi.advanceTimersByTimeAsync(INVENTORY_REFRESH_INTERVAL_MS * 4);
    });

    // The three failed fetches do not count against the cap, so the fourth
    // (successful) request still runs and fills the inventory in.
    expect(fetchWatchDetailMock).toHaveBeenCalledTimes(4);
    expect(applyAudioInventory).toHaveBeenCalledWith(richerAudioTracks);
  });

  it("stops polling at the elapsed deadline when every request fails", async () => {
    playbackSessionMock.mockReturnValue(
      playbackSession({
        planAudioTracks: [{ codec: "eac3", channels: 6, layout: "5.1", language: "eng" }],
        subtitleUrls: [],
      }),
    );
    // Every request fails, so the completed-attempt cap never trips.
    fetchWatchDetailMock.mockRejectedValue(new Error("network"));

    render(createElement(WatchPage, { ...watchPageProps, versions: [virtualVersion] }));

    await act(async () => {
      await vi.advanceTimersByTimeAsync(INVENTORY_REFRESH_DEADLINE_MS * 2);
    });

    // One request per interval until the five-minute deadline, then none.
    expect(fetchWatchDetailMock).toHaveBeenCalledTimes(
      INVENTORY_REFRESH_DEADLINE_MS / INVENTORY_REFRESH_INTERVAL_MS,
    );
  });

  it("discards a slow response that lands after the session switched files", async () => {
    const applyAudioInventory = vi.fn();
    let resolveFetch: (value: { versions: PlayerFileVersion[] }) => void = () => {};
    fetchWatchDetailMock.mockImplementation(
      () =>
        new Promise<{ versions: PlayerFileVersion[] }>((resolve) => {
          resolveFetch = resolve;
        }),
    );
    playbackSessionMock.mockReturnValue(
      playbackSession({
        mediaFileId: 7,
        sessionId: "session-1",
        planAudioTracks: [{ codec: "eac3", channels: 6, layout: "5.1", language: "eng" }],
        subtitleUrls: [planSubtitle],
        applyAudioInventory,
      }),
    );

    const { rerender } = render(
      createElement(WatchPage, { ...watchPageProps, versions: [virtualVersion] }),
    );

    await act(async () => {
      await vi.advanceTimersByTimeAsync(INVENTORY_REFRESH_INTERVAL_MS);
    });
    expect(fetchWatchDetailMock).toHaveBeenCalledTimes(1);

    // The session switches to file 8 while file 7's request is in flight.
    playbackSessionMock.mockReturnValue(
      playbackSession({
        mediaFileId: 8,
        sessionId: "session-2",
        planAudioTracks: [{ codec: "eac3", channels: 6, layout: "5.1", language: "eng" }],
        subtitleUrls: [planSubtitle],
        applyAudioInventory,
      }),
    );
    rerender(createElement(WatchPage, { ...watchPageProps, versions: [virtualVersion] }));

    // File 7's response resolves after the switch.
    await act(async () => {
      resolveFetch({ versions: [{ ...virtualVersion, audio_tracks: richerAudioTracks }] });
      await Promise.resolve();
    });

    expect(applyAudioInventory).not.toHaveBeenCalled();
  });
});
