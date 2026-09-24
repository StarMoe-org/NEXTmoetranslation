import { useMemo, useState } from "react";
import type { EventStorySummary, SideStorySummary } from "@/lib/api";
import {
  SIDE_STORY_CHARACTER_NAMES, SIDE_STORY_PAGE_SIZE, SIDE_STORY_UNITS,
  eventNameIndex, filterCardStories, groupAreaTalks, sideStoryCharacterName,
} from "@/lib/side-story-catalog";
import { SIDE_STORY_CATEGORY, sideStoryStatusLabel } from "@/lib/side-story-console";
import type { SideStoryListState } from "@/components/console/useSideStoryCatalog";

const AREA_SEARCH_OPEN_LIMIT = 200;

function SideStoryItem({ story, category, active, detail, label, hideBadge, onSelect }: {
  story: SideStorySummary;
  category: string;
  active: boolean;
  label: string;
  detail: string;
  hideBadge: boolean;
  onSelect: (category: string, field: string) => void;
}) {
  const status = sideStoryStatusLabel(story);
  return (
    <button type="button" className={`field-item ${active ? "active" : ""}`} aria-current={active ? "page" : undefined} onClick={() => onSelect(category, story.id)}>
      <span className="field-item-copy">
        <span>{label}</span>
        <small>{detail}</small>
      </span>
      <span className="side-story-badges">
        <span className={`side-story-status ${status.tone}`}>{status.label}</span>
        {!hideBadge && story.status !== "pending" && story.untranslatedCount > 0 && (
          <span className="badge work" title="未翻译行数">{story.untranslatedCount}</span>
        )}
      </span>
    </button>
  );
}

function ListState({ list, onRetry }: { list: SideStoryListState; onRetry: () => void }) {
  if (list.failed && !list.loaded) {
    return <button type="button" className="sidebar-more" onClick={onRetry}>载入失败，点击重试</button>;
  }
  if (!list.loaded) return <p className="sidebar-note" role="status">正在载入…</p>;
  return null;
}

function MoreButton({ shown, total, onMore }: { shown: number; total: number; onMore: () => void }) {
  if (total <= shown) return null;
  return <button type="button" className="sidebar-more" onClick={onMore}>显示更多（剩余 {total - shown}）</button>;
}

export interface SideStorySidebarProps {
  category: string;
  field: string;
  hiddenBadges: Set<string>;
  cardStories: SideStoryListState;
  areaTalks: SideStoryListState;
  eventStories: EventStorySummary[];
  cardExpanded: boolean;
  setCardExpanded: (expanded: boolean) => void;
  areaExpanded: boolean;
  setAreaExpanded: (expanded: boolean) => void;
  onRetry: (kind: "card" | "area") => void;
  selectField: (category: string, field: string) => void;
}

export function SideStorySidebar({
  category, field, hiddenBadges, cardStories, areaTalks, eventStories,
  cardExpanded, setCardExpanded, areaExpanded, setAreaExpanded, onRetry, selectField,
}: SideStorySidebarProps) {
  const [unit, setUnit] = useState("");
  const [characterId, setCharacterId] = useState(0);
  const [cardQuery, setCardQuery] = useState("");
  const [cardShown, setCardShown] = useState(SIDE_STORY_PAGE_SIZE);
  const [areaQuery, setAreaQuery] = useState("");
  const [openGroups, setOpenGroups] = useState<Set<string>>(new Set());
  const [groupShown, setGroupShown] = useState<Record<string, number>>({});

  const filteredCards = useMemo(
    () => filterCardStories(cardStories.stories, { unit, characterId, query: cardQuery }),
    [cardStories.stories, unit, characterId, cardQuery],
  );
  const eventNames = useMemo(() => eventNameIndex(eventStories), [eventStories]);
  const areaGroups = useMemo(() => groupAreaTalks(areaTalks.stories, areaQuery, eventNames), [areaTalks.stories, areaQuery, eventNames]);
  const areaMatches = useMemo(() => areaGroups.reduce((sum, group) => sum + group.stories.length, 0), [areaGroups]);
  const unitCharacters = SIDE_STORY_UNITS.find((candidate) => candidate.id === unit)?.characterIds
    ?? Object.keys(SIDE_STORY_CHARACTER_NAMES).map(Number);
  // A search opens the matching groups only while the result stays small enough to render at once.
  const searchOpensGroups = areaQuery.trim() !== "" && areaMatches <= AREA_SEARCH_OPEN_LIMIT;

  const toggleGroup = (key: string) => setOpenGroups((current) => {
    const next = new Set(current);
    if (next.has(key)) next.delete(key);
    else next.add(key);
    return next;
  });

  return (
    <>
      <div className="field-group side-story-group">
        <button type="button" className="field-group-toggle" aria-expanded={cardExpanded} aria-controls="card-story-sidebar-list" onClick={() => setCardExpanded(!cardExpanded)}>
          <span>卡牌剧情{cardStories.loaded ? ` (${filteredCards.length}/${cardStories.stories.length})` : ""}</span>
          <span className="field-group-chevron" aria-hidden="true">{cardExpanded ? "▾" : "▸"}</span>
        </button>
        {cardExpanded && (
          <div id="card-story-sidebar-list">
            <div className="sidebar-filter-row">
              <select aria-label="按团体筛选卡牌剧情" value={unit} onChange={(event) => { setUnit(event.target.value); setCharacterId(0); setCardShown(SIDE_STORY_PAGE_SIZE); }}>
                <option value="">全部团体</option>
                {SIDE_STORY_UNITS.map((candidate) => <option key={candidate.id} value={candidate.id}>{candidate.label}</option>)}
              </select>
              <select aria-label="按角色筛选卡牌剧情" value={characterId} onChange={(event) => { setCharacterId(Number(event.target.value)); setCardShown(SIDE_STORY_PAGE_SIZE); }}>
                <option value={0}>全部角色</option>
                {unitCharacters.map((id) => <option key={id} value={id}>{sideStoryCharacterName(id)}</option>)}
              </select>
            </div>
            <input className="sidebar-filter" aria-label="按卡面名称或编号搜索卡牌剧情" placeholder="按卡面名称或编号搜索…" value={cardQuery} onChange={(event) => { setCardQuery(event.target.value); setCardShown(SIDE_STORY_PAGE_SIZE); }} />
            <ListState list={cardStories} onRetry={() => onRetry("card")} />
            {cardStories.loaded && filteredCards.length === 0 && <p className="sidebar-note">没有符合条件的卡牌剧情</p>}
            {filteredCards.slice(0, cardShown).map((story) => (
              <SideStoryItem
                key={story.id}
                story={story}
                category={SIDE_STORY_CATEGORY.card}
                active={category === SIDE_STORY_CATEGORY.card && field === story.id}
                label={story.title || `Card #${story.id}`}
                detail={`#${story.id}${story.characterId ? ` · ${sideStoryCharacterName(story.characterId)}` : ""}`}
                hideBadge={hiddenBadges.has(`${SIDE_STORY_CATEGORY.card}:${story.id}`)}
                onSelect={selectField}
              />
            ))}
            <MoreButton shown={cardShown} total={filteredCards.length} onMore={() => setCardShown((shown) => shown + SIDE_STORY_PAGE_SIZE)} />
          </div>
        )}
      </div>

      <div className="field-group side-story-group">
        <button type="button" className="field-group-toggle" aria-expanded={areaExpanded} aria-controls="area-talk-sidebar-list" onClick={() => setAreaExpanded(!areaExpanded)}>
          <span>区域对话{areaTalks.loaded ? ` (${areaMatches}/${areaTalks.stories.length})` : ""}</span>
          <span className="field-group-chevron" aria-hidden="true">{areaExpanded ? "▾" : "▸"}</span>
        </button>
        {areaExpanded && (
          <div id="area-talk-sidebar-list">
            <input className="sidebar-filter" aria-label="按 scenarioId 或区域名搜索区域对话" placeholder="按 scenarioId 或区域名搜索…" value={areaQuery} onChange={(event) => setAreaQuery(event.target.value)} />
            <ListState list={areaTalks} onRetry={() => onRetry("area")} />
            {areaTalks.loaded && areaGroups.length === 0 && <p className="sidebar-note">没有符合条件的区域对话</p>}
            {areaGroups.map((group) => {
              const open = searchOpensGroups || openGroups.has(group.key);
              const shown = groupShown[group.key] ?? SIDE_STORY_PAGE_SIZE;
              return (
                <div className="side-story-subgroup" key={group.key}>
                  <button type="button" className="side-story-subgroup-toggle" aria-expanded={open} onClick={() => toggleGroup(group.key)} disabled={searchOpensGroups}>
                    <span className="side-story-subgroup-label">{group.label}</span>
                    <span className="side-story-subgroup-meta">
                      {group.stories.length}{group.untranslated > 0 ? ` · 未译 ${group.untranslated}` : ""}
                      <span className="field-group-chevron" aria-hidden="true">{open ? "▾" : "▸"}</span>
                    </span>
                  </button>
                  {open && <>
                    {group.stories.slice(0, shown).map((story) => (
                      <SideStoryItem
                        key={story.id}
                        story={story}
                        category={SIDE_STORY_CATEGORY.area}
                        active={category === SIDE_STORY_CATEGORY.area && field === story.id}
                        label={story.id}
                        detail={story.title}
                        hideBadge={hiddenBadges.has(`${SIDE_STORY_CATEGORY.area}:${story.id}`)}
                        onSelect={selectField}
                      />
                    ))}
                    <MoreButton shown={shown} total={group.stories.length} onMore={() => setGroupShown((current) => ({ ...current, [group.key]: shown + SIDE_STORY_PAGE_SIZE }))} />
                  </>}
                </div>
              );
            })}
          </div>
        )}
      </div>
    </>
  );
}
