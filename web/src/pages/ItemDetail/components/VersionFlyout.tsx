import { ListFilter, Play } from "lucide-react";
import type { FileVersion } from "@/api/types";
import { Badge } from "@/components/ui/badge";
import {
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
} from "@/components/ui/dropdown-menu";
import { formatFileSize, mapAudioLabel } from "@/lib/mediaFormat";
import { videoRangeLabel } from "@/lib/videoRange";
import {
  collectLanguageLabels,
  extractSourceHint,
  prettifyReleaseName,
} from "./versionFormatUtils";
import { audioScore, resolutionScore } from "./versionRankingUtils";
import { isVersionUnavailable, useVersionVisibility } from "./versionAvailability";

// ---------------------------------------------------------------------------
// Exported helper functions (also used by tests)
// ---------------------------------------------------------------------------

export function buildQualitySummary(version: FileVersion): string {
  if (version.file_path) {
    try {
      const parsed = new URL(version.file_path, "http://silo.local");
      if (parsed.searchParams.get("results") === "all" && !parsed.searchParams.has("result")) {
        return "More results…";
      }
    } catch {
      // Fall through to the regular media quality summary for malformed paths.
    }
  }
  const parts: string[] = [];

  if (version.resolution) parts.push(version.resolution);

  const textToScan = [version.file_name, version.edition_raw].filter(Boolean).join(" ");
  const sourceHint = textToScan ? extractSourceHint(textToScan) : null;
  if (sourceHint) parts.push(sourceHint);

  if (version.codec_video) parts.push(version.codec_video.toUpperCase());
  const rangeLabel = videoRangeLabel(version);
  if (rangeLabel) parts.push(rangeLabel);
  if (version.codec_audio) parts.push(mapAudioLabel(version.codec_audio));
  // Audio languages are now rendered as badges in the UI.
  // const audioLangs = audioLanguageSummary(version.audio_tracks);
  // if (audioLangs) parts.push(audioLangs);
  if (parts.length === 0 && version.container) {
    parts.push(version.container.toUpperCase());
  }

  return parts.join(" · ");
}

export function buildDetailLine(version: FileVersion): string {
  const parts: string[] = [];

  // The release name (provider label for virtual candidates, file stem for
  // local files) leads the line; edition_raw is the fallback for rows scanned
  // before release_name existed.
  const releaseName = prettifyReleaseName(version.release_name || version.edition_raw);
  if (releaseName) parts.push(releaseName);

  const size = formatFileSize(version.file_size);
  if (size) parts.push(size);

  const textToScan = [version.file_name, version.edition_raw, version.release_name]
    .filter(Boolean)
    .join(" ");
  const hint = textToScan ? extractSourceHint(textToScan) : null;
  if (hint) parts.push(hint);

  return parts.join(" · ");
}

export function sortByResolution(versions: FileVersion[]): FileVersion[] {
  return [...versions].sort(
    (a, b) =>
      resolutionScore(b.resolution) - resolutionScore(a.resolution) ||
      Number(b.hdr) - Number(a.hdr) ||
      audioScore(b.codec_audio) - audioScore(a.codec_audio) ||
      (b.file_size ?? 0) - (a.file_size ?? 0),
  );
}

// ---------------------------------------------------------------------------
// VersionFlyoutItems (default export)
// ---------------------------------------------------------------------------

interface VersionFlyoutItemsProps {
  versions: FileVersion[];
  onPlayVersion: (fileId: number) => void;
}

export default function VersionFlyoutItems({ versions, onPlayVersion }: VersionFlyoutItemsProps) {
  const sorted = sortByResolution(versions);
  const { visibleVersions, hiddenUnavailableCount, setShowUnavailable } = useVersionVisibility(
    sorted,
    null,
  );

  return (
    <>
      <DropdownMenuLabel>Play Version</DropdownMenuLabel>
      <DropdownMenuSeparator />

      {visibleVersions.map((version) => {
        const qualitySummary = buildQualitySummary(version);
        const detailLine = buildDetailLine(version);
        const isMoreAction = qualitySummary === "More results…";
        const isVirtual =
          version.container === "virtual" ||
          Boolean(version.file_path?.toLowerCase().startsWith("virtual://"));
        const unavailable = isVersionUnavailable(version);

        return (
          <DropdownMenuItem
            key={version.file_id}
            className={`flex items-center gap-3 rounded-lg py-2.5 ${unavailable ? "opacity-80" : ""}`}
            onSelect={() => onPlayVersion(version.file_id)}
          >
            <span className="bg-accent/70 flex size-7 shrink-0 items-center justify-center rounded-full">
              {isMoreAction ? (
                <ListFilter className="text-foreground size-3.5" />
              ) : (
                <Play className="text-foreground size-3.5 fill-current" />
              )}
            </span>

            <span className="min-w-0 flex-1">
              <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                <span className="text-foreground flex items-center gap-1.5 text-sm font-semibold">
                  <span>{qualitySummary}</span>
                  {isVirtual && !isMoreAction && (
                    <Badge
                      variant="secondary"
                      className="shrink-0 px-1.5 py-0 text-[10px] font-medium"
                    >
                      Virtual
                    </Badge>
                  )}
                  {unavailable && (
                    <Badge
                      variant="outline"
                      className="shrink-0 border-amber-500/30 bg-amber-500/15 px-1.5 py-0 text-[10px] font-medium text-amber-600 dark:text-amber-300"
                    >
                      Will retry on play
                    </Badge>
                  )}
                </span>
                <div className="flex flex-wrap gap-1">
                  {collectLanguageLabels(version.audio_tracks?.map((t) => t.language) ?? []).map(
                    (lang) => (
                      <Badge
                        key={lang}
                        variant="outline"
                        className="border-blue-500/20 bg-blue-500/10 px-1 py-0 text-[10px] font-medium text-blue-400"
                      >
                        <span className="mr-0.5 opacity-70">🔊</span>
                        {lang}
                      </Badge>
                    ),
                  )}
                </div>
              </div>
              {detailLine && (
                <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-1">
                  <span className="text-muted-foreground text-xs">{detailLine}</span>
                  <div className="flex flex-wrap gap-1">
                    {collectLanguageLabels(
                      version.subtitle_tracks?.map((t) => t.language) ?? [],
                    ).map((lang) => (
                      <Badge
                        key={lang}
                        variant="outline"
                        className="border-amber-500/20 bg-amber-500/10 px-1 py-0 text-[10px] font-medium text-amber-400"
                      >
                        <span className="mr-0.5 opacity-70">CC</span>
                        {lang}
                      </Badge>
                    ))}
                  </div>
                </div>
              )}
            </span>
          </DropdownMenuItem>
        );
      })}
      {hiddenUnavailableCount > 0 && (
        <DropdownMenuItem
          className="text-muted-foreground justify-center text-xs font-medium"
          onSelect={() => setShowUnavailable(true)}
        >
          Show {hiddenUnavailableCount} unavailable{" "}
          {hiddenUnavailableCount === 1 ? "version" : "versions"}
        </DropdownMenuItem>
      )}
    </>
  );
}
