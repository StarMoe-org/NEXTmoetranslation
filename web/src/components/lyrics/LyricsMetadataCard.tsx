import type { LyricsRendition, RenditionLyricsDocument, SongLyrics } from "@/lib/api";

export interface LyricsMetadataCardProps {
  activeTranslationCredit: string;
  activeProofreadingCredit: string;
  activeRendition: LyricsRendition | null;
  activeVersion: "full" | "game";
  writeLocked: boolean;
  updateActiveCredits: (field: "translation" | "proofreading", value: string) => void;
}

export function LyricsMetadataCard({
  activeTranslationCredit,
  activeProofreadingCredit,
  activeRendition,
  activeVersion,
  writeLocked,
  updateActiveCredits,
}: LyricsMetadataCardProps) {
  return (
    <div className="lyrics-metadata">
      <label>
        翻译
        <input
          value={activeTranslationCredit}
          onChange={(event) => updateActiveCredits("translation", event.target.value)}
          placeholder="译者名称；将随公开歌词分发"
          maxLength={activeRendition ? 2048 : undefined}
          readOnly={writeLocked || (!activeRendition && activeVersion === "game")}
        />
      </label>
      <label>
        校对
        <input
          value={activeProofreadingCredit}
          onChange={(event) => updateActiveCredits("proofreading", event.target.value)}
          placeholder="校对者名称；没有可留空"
          maxLength={activeRendition ? 2048 : undefined}
          readOnly={writeLocked || (!activeRendition && activeVersion === "game")}
        />
      </label>
    </div>
  );
}

export interface LyricsSourceMetadataCardProps {
  activeRendition: LyricsRendition | null;
  legacyLyrics: SongLyrics | null;
  activeVersion: "full" | "game";
  projectionKind: "full_only" | "game_only" | "exact_projection" | "independent_game" | "invalid";
  writeLocked: boolean;
  onUpdateLyrics: (patch: Partial<SongLyrics> | Partial<RenditionLyricsDocument>) => void;
}

/** Internal notes and source identity; rendered inside the editor's 技术详情 disclosure. */
export function LyricsSourceMetadataCard({
  activeRendition,
  legacyLyrics,
  activeVersion,
  projectionKind,
  writeLocked,
  onUpdateLyrics,
}: LyricsSourceMetadataCardProps) {
  if (!legacyLyrics && !activeRendition) return null;
  return (
    <div className="lyrics-source-metadata">
      {legacyLyrics ? (
        <>
          <label>
            内部来源备注
            <input
              value={legacyLyrics.sourceNote || ""}
              onChange={(event) => onUpdateLyrics({ sourceNote: event.target.value })}
              readOnly={writeLocked || activeVersion === "game"}
            />
          </label>
          <label>
            内部授权备注
            <input
              value={legacyLyrics.licenseNote || ""}
              onChange={(event) => onUpdateLyrics({ licenseNote: event.target.value })}
              readOnly={writeLocked || activeVersion === "game"}
            />
          </label>
          {legacyLyrics.sourceUrl && (
            <a href={legacyLyrics.sourceUrl} target="_blank" rel="noopener noreferrer">
              固定来源修订 {legacyLyrics.sourceRevisionId}
            </a>
          )}
        </>
      ) : activeRendition ? (
        <>
          <label>
            Stable key
            <input value={activeRendition.key} readOnly />
          </label>
          <label>
            Projection relation
            <input value={projectionKind} readOnly />
          </label>
        </>
      ) : null}
    </div>
  );
}
