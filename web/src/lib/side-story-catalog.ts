import type { EventStorySummary, SideStorySummary } from "./api";

export const SIDE_STORY_PAGE_SIZE = 50;

export interface SideStoryUnit {
  id: string;
  label: string;
  characterIds: readonly number[];
}

export const SIDE_STORY_UNITS: readonly SideStoryUnit[] = [
  { id: "leoneed", label: "Leo/need", characterIds: [1, 2, 3, 4] },
  { id: "mmj", label: "MORE MORE JUMP!", characterIds: [5, 6, 7, 8] },
  { id: "vbs", label: "Vivid BAD SQUAD", characterIds: [9, 10, 11, 12] },
  { id: "wxs", label: "ワンダーランズ×ショウタイム", characterIds: [13, 14, 15, 16] },
  { id: "n25", label: "25時、ナイトコードで。", characterIds: [17, 18, 19, 20] },
  { id: "vs", label: "VIRTUAL SINGER", characterIds: [21, 22, 23, 24, 25, 26] },
];

export const SIDE_STORY_CHARACTER_NAMES: Readonly<Record<number, string>> = {
  1: "星乃一歌", 2: "天馬咲希", 3: "望月穂波", 4: "日野森志歩",
  5: "花里みのり", 6: "桐谷遥", 7: "桃井愛莉", 8: "日野森雫",
  9: "小豆沢こはね", 10: "白石杏", 11: "東雲彰人", 12: "青柳冬弥",
  13: "天馬司", 14: "鳳えむ", 15: "草薙寧々", 16: "神代類",
  17: "宵崎奏", 18: "朝比奈まふゆ", 19: "東雲絵名", 20: "暁山瑞希",
  21: "初音ミク", 22: "鏡音リン", 23: "鏡音レン", 24: "巡音ルカ", 25: "MEIKO", 26: "KAITO",
};

export function sideStoryCharacterName(characterId: number): string {
  return SIDE_STORY_CHARACTER_NAMES[characterId] ?? (characterId > 0 ? `角色 #${characterId}` : "");
}

export interface CardStoryFilter {
  unit: string;
  characterId: number;
  query: string;
}

/** Filters card stories by unit, character and name/id search (`#1473` as shown), newest first. */
export function filterCardStories(stories: readonly SideStorySummary[], filter: CardStoryFilter): SideStorySummary[] {
  const unit = SIDE_STORY_UNITS.find((candidate) => candidate.id === filter.unit);
  const q = filter.query.trim().replace(/^#/, "").toLowerCase();
  return stories
    .filter((story) => {
      if (unit && !unit.characterIds.includes(story.characterId)) return false;
      if (filter.characterId > 0 && story.characterId !== filter.characterId) return false;
      if (!q) return true;
      return `${story.title}\n${story.id}\n${sideStoryCharacterName(story.characterId)}`.toLowerCase().includes(q);
    })
    .sort((a, b) => (b.releasedAt - a.releasedAt) || (Number(b.id) - Number(a.id)));
}

// ---- Area talk groups (mirrors the main site's story/area sections) ----

export interface AreaTalkGroup {
  key: string;
  label: string;
  stories: SideStorySummary[];
  untranslated: number;
}

function areaCategoryRank(category: string): [number, number] {
  if (category === "grade1") return [0, 0];
  if (category === "grade2") return [0, 1];
  if (category === "theater") return [0, 2];
  let match = /^event_(\d+)$/.exec(category);
  if (match) return [1, -Number(match[1])];
  match = /^limited_(\d+)$/.exec(category);
  if (match) return [2, Number(match[1])];
  match = /^aprilfool(\d+)$/.exec(category);
  if (match) return [3, -Number(match[1])];
  return [4, 0];
}

export function areaCategoryLabel(category: string, eventNames: ReadonlyMap<number, string>, areaName = ""): string {
  let match = /^event_(\d+)$/.exec(category);
  if (match) {
    const name = eventNames.get(Number(match[1]));
    return name ? `活动 ${match[1]} · ${name}` : `活动 ${match[1]}`;
  }
  if (category === "grade1") return "升学前";
  if (category === "grade2") return "升学后";
  if (category === "theater") return "剧场";
  match = /^limited_(\d+)$/.exec(category);
  if (match) return areaName ? `限定区域 ${match[1]} · ${areaName}` : `限定区域 ${match[1]}`;
  match = /^aprilfool(\d+)$/.exec(category);
  if (match) return `愚人节 ${match[1]}`;
  return category || "未分类";
}

export function eventNameIndex(eventStories: readonly EventStorySummary[]): Map<number, string> {
  const names = new Map<number, string>();
  for (const story of eventStories) {
    const name = story.eventName || story.eventNameJapanese;
    if (name) names.set(story.eventId, name);
  }
  return names;
}

/** Groups area talks by category; a search keeps whole groups whose label matches, else talks matching scenarioId or area name. */
export function groupAreaTalks(
  stories: readonly SideStorySummary[], query: string, eventNames: ReadonlyMap<number, string>,
): AreaTalkGroup[] {
  const q = query.trim().toLowerCase();
  const groups = new Map<string, SideStorySummary[]>();
  for (const story of stories) {
    const list = groups.get(story.areaCategory);
    if (list) list.push(story);
    else groups.set(story.areaCategory, [story]);
  }
  return [...groups.entries()]
    .flatMap(([key, list]) => {
      list.sort((a, b) => (a.actionSetId - b.actionSetId) || a.id.localeCompare(b.id));
      const label = areaCategoryLabel(key, eventNames, list[0]?.title);
      const matched = !q || label.toLowerCase().includes(q)
        ? list
        : list.filter((story) => `${story.id}\n${story.title}`.toLowerCase().includes(q));
      if (matched.length === 0) return [];
      return [{
        key,
        label,
        stories: matched,
        untranslated: matched.reduce((sum, story) => sum + story.untranslatedCount, 0),
      }];
    })
    .sort((a, b) => {
      const [ga, sa] = areaCategoryRank(a.key);
      const [gb, sb] = areaCategoryRank(b.key);
      return (ga - gb) || (sa - sb) || a.key.localeCompare(b.key);
    });
}
