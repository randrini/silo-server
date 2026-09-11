import { fireEvent, render, screen, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { describe, expect, it, vi } from "vitest";

import type { FileVersion } from "@/api/types";
import VersionDropdown from "./VersionDropdown";
import VersionFlyoutItems from "./VersionFlyout";

vi.mock("@/components/ui/dropdown-menu", () => ({
  DropdownMenuLabel: ({ children }: { children: ReactNode }) => <div>{children}</div>,
  DropdownMenuSeparator: () => <div />,
  DropdownMenuItem: ({
    children,
    onSelect,
    className,
  }: {
    children: ReactNode;
    onSelect?: () => void;
    className?: string;
  }) => (
    <div role="menuitem" className={className} onClick={onSelect}>
      {children}
    </div>
  ),
}));

function makeVersion(overrides: Partial<FileVersion> = {}): FileVersion {
  return {
    file_id: overrides.file_id ?? 1,
    resolution: overrides.resolution ?? "1080p",
    codec_video: overrides.codec_video ?? "h264",
    codec_audio: overrides.codec_audio ?? "aac",
    hdr: overrides.hdr ?? false,
    container: overrides.container ?? "mkv",
    file_size: overrides.file_size ?? 0,
    duration: overrides.duration ?? 0,
    bitrate: overrides.bitrate ?? 0,
    available: overrides.available,
  };
}

function openVersionDropdown() {
  fireEvent.click(screen.getByRole("button", { name: /Version/ }));
  return within(screen.getByRole("dialog"));
}

describe("VersionDropdown unavailable versions", () => {
  it("hides unavailable versions by default and shows them via the toggle", () => {
    const versions = [
      makeVersion({ file_id: 1, resolution: "2160p" }),
      makeVersion({ file_id: 2, resolution: "1080p", available: false }),
    ];
    const onSelectVersion = vi.fn();
    render(
      <VersionDropdown
        versions={versions}
        selectedVersion={versions[0]!}
        onSelectVersion={onSelectVersion}
      />,
    );

    const dialog = openVersionDropdown();

    // The unavailable version is hidden; the toggle reveals it.
    expect(dialog.queryByText("1080p")).not.toBeInTheDocument();
    fireEvent.click(dialog.getByRole("button", { name: /Show 1 unavailable version/ }));

    const unavailableRow = dialog.getByRole("button", { name: /1080p/ });
    expect(unavailableRow).toBeInTheDocument();
    expect(dialog.getByText("Will retry on play")).toBeInTheDocument();
    expect(unavailableRow).toHaveClass("opacity-80");
  });

  it("keeps the active selection visible even when it becomes unavailable", () => {
    const versions = [
      makeVersion({ file_id: 1, resolution: "2160p" }),
      makeVersion({ file_id: 2, resolution: "1080p", available: false }),
    ];
    render(
      <VersionDropdown
        versions={versions}
        selectedVersion={versions[1]!}
        onSelectVersion={vi.fn()}
      />,
    );

    const dialog = openVersionDropdown();

    // The selected (unavailable) version stays in the list, and the toggle is
    // not offered because nothing else is hidden.
    expect(dialog.getByRole("button", { name: /1080p/ })).toBeInTheDocument();
    expect(dialog.getByText("Will retry on play")).toBeInTheDocument();
    expect(dialog.queryByRole("button", { name: /Show .* unavailable/ })).not.toBeInTheDocument();
  });

  it("shows a recovered version without an unavailable toggle after a successful check", () => {
    // The item metadata marked the version unavailable, but the liveness check
    // reported it available again; the merged version (available: true) must
    // render normally with no "Show N unavailable" toggle.
    const versions = [
      makeVersion({ file_id: 1, resolution: "2160p" }),
      makeVersion({ file_id: 2, resolution: "1080p", available: true }),
    ];
    render(
      <VersionDropdown
        versions={versions}
        selectedVersion={versions[0]!}
        onSelectVersion={vi.fn()}
      />,
    );

    const dialog = openVersionDropdown();

    expect(dialog.getByRole("button", { name: /1080p/ })).toBeInTheDocument();
    expect(dialog.queryByText("Unavailable")).not.toBeInTheDocument();
    expect(dialog.queryByRole("button", { name: /Show .* unavailable/ })).not.toBeInTheDocument();
  });

  it("does not filter when every version is unavailable", () => {
    const versions = [
      makeVersion({ file_id: 1, resolution: "2160p", available: false }),
      makeVersion({ file_id: 2, resolution: "1080p", available: false }),
    ];
    render(
      <VersionDropdown
        versions={versions}
        selectedVersion={versions[0]!}
        onSelectVersion={vi.fn()}
      />,
    );

    const dialog = openVersionDropdown();

    expect(dialog.getByRole("button", { name: /2160p/ })).toBeInTheDocument();
    expect(dialog.getByRole("button", { name: /1080p/ })).toBeInTheDocument();
    expect(dialog.queryByRole("button", { name: /Show .* unavailable/ })).not.toBeInTheDocument();
  });
});

describe("VersionFlyoutItems unavailable versions", () => {
  it("hides unavailable versions by default and shows them via the toggle", () => {
    const versions = [
      makeVersion({ file_id: 1, resolution: "2160p" }),
      makeVersion({ file_id: 2, resolution: "1080p", available: false }),
    ];
    render(<VersionFlyoutItems versions={versions} onPlayVersion={vi.fn()} />);

    expect(screen.queryByText("1080p")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("menuitem", { name: /Show 1 unavailable version/ }));

    const unavailableRow = screen.getByRole("menuitem", { name: /1080p/ });
    expect(unavailableRow).toBeInTheDocument();
    expect(screen.getByText("Will retry on play")).toBeInTheDocument();
    expect(unavailableRow).toHaveClass("opacity-80");
  });
});
