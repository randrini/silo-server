import { memo, useMemo, useState } from "react";
import { Check, ChevronDown, Disc3, Layers3 } from "lucide-react";

import type { FileVersion, PlaybackVariant } from "@/api/types";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { videoRangeLabel } from "@/lib/videoRange";
import DetailPopover from "./DetailPopover";
import { sortPlaybackVariantsByEditionPreference } from "./versionRankingUtils";
import { collectLanguageLabels } from "./versionFormatUtils";
import { buildDetailLine, buildQualitySummary, sortByResolution } from "./VersionFlyout";
import { isVersionUnavailable, useVersionVisibility } from "./versionAvailability";

interface VersionDropdownProps {
  versions: FileVersion[];
  playbackVariants?: PlaybackVariant[];
  selectedVersion: FileVersion | null;
  onSelectVersion: (version: FileVersion) => void;
  /** Fired whenever a picker popover opens or closes (open=true on open). */
  onOpenChange?: (open: boolean) => void;
}

interface EditionOption {
  id: string;
  label: string;
  variant: PlaybackVariant;
  defaultVersion: FileVersion;
  versions: FileVersion[];
}

function VersionDropdown({
  versions,
  playbackVariants,
  selectedVersion,
  onSelectVersion,
  onOpenChange,
}: VersionDropdownProps) {
  const [editionOpen, setEditionOpen] = useState(false);
  const [versionOpen, setVersionOpen] = useState(false);

  const handleEditionOpenChange = (open: boolean) => {
    setEditionOpen(open);
    onOpenChange?.(open);
  };
  const handleVersionOpenChange = (open: boolean) => {
    setVersionOpen(open);
    onOpenChange?.(open);
  };

  const sorted = useMemo(() => sortByResolution(versions), [versions]);
  const editionOptions = useMemo(
    () => buildEditionOptions(playbackVariants, versions),
    [playbackVariants, versions],
  );

  const hasNamedEditions = editionOptions.some((option) => option.variant.edition_key);
  const showEditionDropdown =
    editionOptions.length > 1 &&
    new Set(editionOptions.map((option) => option.label.toLowerCase())).size > 1 &&
    hasNamedEditions;

  const selectedEdition = showEditionDropdown
    ? resolveSelectedEditionOption(editionOptions, selectedVersion)
    : null;
  const activeVersions = selectedEdition?.versions ?? sorted;
  const activeVersion =
    selectedVersion ?? selectedEdition?.defaultVersion ?? activeVersions[0] ?? null;
  const showVersionDropdown = activeVersions.length > 1;

  const { visibleVersions, hiddenUnavailableCount, setShowUnavailable } = useVersionVisibility(
    activeVersions,
    activeVersion?.file_id,
  );

  if (!showEditionDropdown && !showVersionDropdown) {
    return null;
  }

  return (
    <>
      {showEditionDropdown && selectedEdition ? (
        <DetailPopover
          open={editionOpen}
          onOpenChange={handleEditionOpenChange}
          contentClassName="w-[30rem] p-1.5"
          trigger={
            <Button
              variant="glass"
              className="h-8 max-w-full min-w-0 shrink gap-1.5 rounded-full px-3 text-xs font-medium"
            >
              <Layers3 className="size-3.5" />
              Edition
              <span className="text-muted-foreground max-w-44 truncate text-[11px] font-normal sm:max-w-64">
                {selectedEdition.label}
              </span>
              <ChevronDown className="text-muted-foreground size-3" />
            </Button>
          }
        >
          <div className="space-y-0.5">
            {editionOptions.map((option) => {
              const isSelected = option.id === selectedEdition.id;
              const detail = buildEditionDetail(option);

              return (
                <button
                  key={option.id}
                  type="button"
                  onClick={() => {
                    onSelectVersion(option.defaultVersion);
                    setEditionOpen(false);
                    setVersionOpen(false);
                  }}
                  className={`flex w-full items-center gap-3 rounded-lg px-3 py-2.5 text-left transition-colors ${
                    isSelected ? "bg-accent text-accent-foreground" : "hover:bg-accent/50"
                  }`}
                >
                  <div className="min-w-0 flex-1">
                    <div className="truncate text-sm font-medium">{option.label}</div>
                    {detail && <div className="text-muted-foreground text-xs">{detail}</div>}
                  </div>
                  {isSelected && <Check className="text-primary size-4 shrink-0" />}
                </button>
              );
            })}
          </div>
        </DetailPopover>
      ) : null}

      {showVersionDropdown ? (
        <DetailPopover
          open={versionOpen}
          onOpenChange={handleVersionOpenChange}
          contentClassName="w-[30rem] p-1.5"
          trigger={
            <Button
              variant="glass"
              className="h-8 max-w-full min-w-0 shrink gap-1.5 rounded-full px-3 text-xs font-medium"
            >
              <Disc3 className="size-3.5" />
              Version
              <span className="text-muted-foreground max-w-44 truncate text-[11px] font-normal sm:max-w-64">
                {activeVersion ? buildVersionTriggerSummary(activeVersion) : ""}
              </span>
              <ChevronDown className="text-muted-foreground size-3" />
            </Button>
          }
        >
          <div className="space-y-0.5">
            {visibleVersions.map((version) => {
              const isSelected = version.file_id === activeVersion?.file_id;
              const summary = buildQualitySummary(version);
              const detail = buildDetailLine(version);
              const rangeLabel = videoRangeLabel(version);
              const unavailable = isVersionUnavailable(version);

              return (
                <button
                  key={version.file_id}
                  type="button"
                  onClick={() => {
                    onSelectVersion(version);
                    setVersionOpen(false);
                  }}
                  className={`flex w-full items-center gap-3 rounded-lg px-3 py-2.5 text-left transition-colors ${
                    isSelected ? "bg-accent text-accent-foreground" : "hover:bg-accent/50"
                  } ${unavailable && !isSelected ? "opacity-80" : ""}`}
                >
                  <div className="min-w-0 flex-1">
                    <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                      <div className="flex items-center gap-2">
                        <span className="text-sm font-medium">
                          {summary || `Version ${version.file_id}`}
                        </span>
                        {rangeLabel ? (
                          <Badge variant="secondary" className="px-1.5 py-0 text-[10px] uppercase">
                            {rangeLabel}
                          </Badge>
                        ) : null}
                        {unavailable ? (
                          <Badge
                            variant="outline"
                            className="border-amber-500/30 bg-amber-500/15 px-1.5 py-0 text-[10px] font-medium text-amber-600 dark:text-amber-300"
                          >
                            Will retry on play
                          </Badge>
                        ) : null}
                      </div>
                      <div className="flex flex-wrap gap-1">
                        {collectLanguageLabels(
                          version.audio_tracks?.map((t) => t.language) ?? [],
                        ).map((lang) => (
                          <Badge
                            key={lang}
                            variant="outline"
                            className="border-blue-500/20 bg-blue-500/10 px-1 py-0 text-[10px] font-medium text-blue-400"
                          >
                            <span className="mr-0.5 opacity-70">🔊</span>
                            {lang}
                          </Badge>
                        ))}
                      </div>
                    </div>
                    {detail && (
                      <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-1">
                        <span className="text-muted-foreground text-xs">{detail}</span>
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
                  </div>
                  {isSelected && <Check className="text-primary size-4 shrink-0" />}
                </button>
              );
            })}
            {hiddenUnavailableCount > 0 && (
              <button
                type="button"
                onClick={() => setShowUnavailable(true)}
                className="text-muted-foreground hover:bg-accent/50 hover:text-foreground w-full rounded-lg px-3 py-2 text-left text-xs font-medium transition-colors"
              >
                Show {hiddenUnavailableCount} unavailable{" "}
                {hiddenUnavailableCount === 1 ? "version" : "versions"}
              </button>
            )}
          </div>
        </DetailPopover>
      ) : null}
    </>
  );
}

export default memo(VersionDropdown);

function buildVersionTriggerSummary(version: FileVersion): string {
  return buildQualitySummary(version) || buildDetailLine(version) || `Version ${version.file_id}`;
}

function buildEditionOptions(
  playbackVariants: PlaybackVariant[] | undefined,
  versions: FileVersion[],
): EditionOption[] {
  if (!playbackVariants || playbackVariants.length === 0) {
    return [];
  }

  const orderedVariants = sortPlaybackVariantsByEditionPreference(playbackVariants);
  const hasNamedEditions = orderedVariants.some((variant) => variant.edition_key);

  return orderedVariants
    .map((variant) => {
      const firstPart = [...(variant.parts ?? [])].sort((a, b) => a.part_index - b.part_index)[0];
      if (!firstPart) {
        return null;
      }

      const partVersions = sortByResolution([...(firstPart.versions ?? [])]);
      const defaultVersion =
        (firstPart.default_file_id != null
          ? versions.find((version) => version.file_id === firstPart.default_file_id)
          : undefined) ?? partVersions[0];
      if (!defaultVersion) {
        return null;
      }

      return {
        id: variant.variant_id,
        label: buildEditionLabel(variant, hasNamedEditions),
        variant,
        defaultVersion,
        versions: partVersions,
      };
    })
    .filter((entry): entry is EditionOption => !!entry);
}

function resolveSelectedEditionOption(
  editionOptions: EditionOption[],
  selectedVersion: FileVersion | null,
): EditionOption | null {
  if (editionOptions.length === 0) {
    return null;
  }

  if (selectedVersion) {
    const matching = editionOptions.find((option) =>
      option.variant.parts.some((part) =>
        part.versions.some((candidate) => candidate.file_id === selectedVersion.file_id),
      ),
    );
    if (matching) {
      return matching;
    }
  }

  return editionOptions[0] ?? null;
}

function buildEditionLabel(variant: PlaybackVariant, hasNamedEditions: boolean): string {
  if (variant.edition_raw?.trim()) {
    return variant.edition_raw.trim();
  }
  if (variant.edition_key?.trim()) {
    return humanizeEditionKey(variant.edition_key);
  }
  return hasNamedEditions ? "Standard" : "Edition";
}

function humanizeEditionKey(value: string): string {
  return value
    .split(/[_-]+/)
    .filter(Boolean)
    .map((part) => {
      if (part.toLowerCase() === "imax") {
        return "IMAX";
      }
      return `${part.charAt(0).toUpperCase()}${part.slice(1)}`;
    })
    .join(" ");
}

function buildEditionDetail(option: EditionOption): string {
  const parts: string[] = [];
  if (option.versions.length > 1) {
    parts.push(`${option.versions.length} versions`);
  }

  const defaultSummary = buildQualitySummary(option.defaultVersion);
  if (defaultSummary) {
    parts.push(defaultSummary);
  }

  return parts.join(" · ");
}
