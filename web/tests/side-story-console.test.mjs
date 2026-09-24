import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { createRequire } from "node:module";
import test from "node:test";

import { createHookRuntime, loadSourceModule } from "./source-module-harness.mjs";

const require = createRequire(import.meta.url);
const { createElement } = require("react");
const { renderToStaticMarkup } = require("react-dom/server");

const catalog = loadSourceModule("lib/side-story-catalog.ts");
const model = loadSourceModule("lib/side-story-console.ts");
const labels = loadSourceModule("lib/labels.ts");
const { SideStorySidebar } = loadSourceModule("components/console/SideStorySidebar.tsx");
const { SideStoryBackfillPanel } = loadSourceModule("components/console/SideStoryBackfillPanel.tsx");
const { TranslationEntryWorkspace } = loadSourceModule("components/console/TranslationEntryWorkspace.tsx");
const read = (path) => readFile(new URL(`../${path}`, import.meta.url), "utf8");

// All titles and lines below are synthetic test data.
function summary(overrides) {
  return {
    kind: "card", id: "1", title: "测试卡面", characterId: 1, areaId: 0, areaCategory: "", actionSetId: 0,
    releasedAt: 0, episodeCount: 2, fetchedEpisodeCount: 2, lineCount: 10, translatedCount: 0, untranslatedCount: 10,
    sourceCounts: { official: 0, llm: 0, human: 0 }, primarySource: "", status: "untranslated", updatedAt: 0,
    ...overrides,
  };
}

function area(id, areaCategory, actionSetId, untranslatedCount = 1, title = "测试区域") {
  return summary({ kind: "area", id, title, characterId: 0, areaCategory, actionSetId, untranslatedCount });
}

test("area talks group in the main site's section order with category labels and untranslated sums", () => {
  const stories = [
    area("areatalk_ev_3", "event_12", 1203, 2),
    area("areatalk_lt_1", "limited_7", 5001, 0, "测试限定区域"),
    area("areatalk_af_1", "aprilfool2023", 7001),
    area("areatalk_g2_1", "grade2", 300),
    area("areatalk_ev_1", "event_12", 1201, 3),
    area("areatalk_ev_9", "event_40", 4001),
    area("areatalk_th_1", "theater", 500),
    area("areatalk_af_2", "aprilfool2024", 7101),
    area("areatalk_g1_1", "grade1", 100),
    area("areatalk_lt_2", "limited_3", 5101, 0, "测试限定区域二"),
  ];
  const groups = catalog.groupAreaTalks(stories, "", new Map([[12, "测试活动"]]));
  assert.deepEqual(groups.map((group) => group.key), [
    "grade1", "grade2", "theater", "event_40", "event_12", "limited_3", "limited_7", "aprilfool2024", "aprilfool2023",
  ]);
  assert.deepEqual(groups.map((group) => group.label), [
    "升学前", "升学后", "剧场", "活动 40", "活动 12 · 测试活动", "限定区域 3 · 测试限定区域二", "限定区域 7 · 测试限定区域",
    "愚人节 2024", "愚人节 2023",
  ]);
  const event12 = groups.find((group) => group.key === "event_12");
  assert.deepEqual(event12.stories.map((story) => story.id), ["areatalk_ev_1", "areatalk_ev_3"]);
  assert.equal(event12.untranslated, 5);

  const searched = catalog.groupAreaTalks(stories, "EV_3", new Map());
  assert.deepEqual(searched.map((group) => [group.key, group.stories.map((story) => story.id)]), [["event_12", ["areatalk_ev_3"]]]);
  assert.equal(catalog.areaCategoryLabel("", new Map()), "未分类");
});

test("an area search also matches the group label the sidebar shows", () => {
  const stories = [
    area("areatalk_ev_1", "event_12", 1201, 3),
    area("areatalk_ev_2", "event_12", 1202, 1),
    area("areatalk_ev_9", "event_40", 4001),
    area("areatalk_g1_1", "grade1", 100),
    area("areatalk_g2_1", "grade2", 300),
    area("areatalk_th_1", "theater", 500),
    area("areatalk_lt_1", "limited_7", 5001, 1, "测试限定区域"),
    area("areatalk_af_1", "aprilfool2024", 7101),
  ];
  const names = new Map([[12, "测试活动"]]);
  const search = (query) => catalog.groupAreaTalks(stories, query, names).map((group) => [group.key, group.stories.map((story) => story.id)]);
  assert.deepEqual(search("测试活动"), [["event_12", ["areatalk_ev_1", "areatalk_ev_2"]]]);
  assert.deepEqual(search("活动 40"), [["event_40", ["areatalk_ev_9"]]]);
  assert.deepEqual(search("升学前"), [["grade1", ["areatalk_g1_1"]]]);
  assert.deepEqual(search("升学后"), [["grade2", ["areatalk_g2_1"]]]);
  assert.deepEqual(search("剧场"), [["theater", ["areatalk_th_1"]]]);
  assert.deepEqual(search("限定区域 7"), [["limited_7", ["areatalk_lt_1"]]]);
  assert.deepEqual(search("愚人节 2024"), [["aprilfool2024", ["areatalk_af_1"]]]);
  assert.deepEqual(search("ev_2"), [["event_12", ["areatalk_ev_2"]]], "a talk match keeps only that talk");
  const [event12] = catalog.groupAreaTalks(stories, "ev_2", names);
  assert.equal(event12.label, "活动 12 · 测试活动");
  assert.equal(event12.untranslated, 1);
});

test("the event name index prefers the translated name and falls back to the Japanese one", () => {
  const names = catalog.eventNameIndex([
    { eventId: 5, eventName: "测试活动名", eventNameJapanese: "テストイベント" },
    { eventId: 6, eventName: "", eventNameJapanese: "テストイベント二" },
    { eventId: 7, eventName: "", eventNameJapanese: "" },
  ]);
  assert.deepEqual([...names], [[5, "测试活动名"], [6, "テストイベント二"]]);
});

test("card stories filter by unit, character and search, newest first", () => {
  const stories = [
    summary({ id: "10", characterId: 1, releasedAt: 100, title: "测试卡面甲" }),
    summary({ id: "11", characterId: 6, releasedAt: 300, title: "测试卡面乙" }),
    summary({ id: "12", characterId: 2, releasedAt: 200, title: "测试卡面丙" }),
    summary({ id: "13", characterId: 21, releasedAt: 200, title: "测试卡面丁" }),
  ];
  const ids = (filter) => catalog.filterCardStories(stories, { unit: "", characterId: 0, query: "", ...filter }).map((story) => story.id);
  assert.deepEqual(ids({}), ["11", "13", "12", "10"]);
  assert.deepEqual(ids({ unit: "leoneed" }), ["12", "10"]);
  assert.deepEqual(ids({ unit: "leoneed", characterId: 2 }), ["12"]);
  assert.deepEqual(ids({ unit: "vs" }), ["13"]);
  assert.deepEqual(ids({ query: "乙" }), ["11"]);
  assert.deepEqual(ids({ query: "13" }), ["13"]);
  assert.deepEqual(ids({ query: "桐谷" }), ["11"], "search matches the character name");
  assert.equal(catalog.sideStoryCharacterName(99), "角色 #99");
});

test("status labels map list status and primary source", () => {
  const label = (status, primarySource) => model.sideStoryStatusLabel({ status, primarySource });
  assert.deepEqual(label("pending", ""), { label: "未获取", tone: "pending" });
  assert.deepEqual(label("pending", "human"), { label: "未获取", tone: "pending" });
  assert.deepEqual(label("untranslated", ""), { label: "未翻译", tone: "untranslated" });
  assert.deepEqual(label("partial", "official"), { label: "官方", tone: "official" });
  assert.deepEqual(label("translated", "llm"), { label: "AI", tone: "llm" });
  assert.deepEqual(label("partial", "human"), { label: "人工", tone: "human" });
  assert.equal(model.sideStoryEntrySource("official"), "cn");
  assert.equal(model.sideStoryEntrySource("llm"), "llm");
  assert.equal(model.sideStoryEntrySource(""), "unknown");
  assert.equal(model.sideStoryEditSource("cn"), "human", "editors never store official rows");
  assert.equal(model.sideStoryKindForCategory("cardStory"), "card");
  assert.equal(model.sideStoryKindForCategory("areaTalk"), "area");
  assert.equal(model.sideStoryKindForCategory("eventStory"), null);
  assert.equal(model.sideStoryLocale("ja-JP"), "zh-CN");
  assert.equal(model.sideStoryLocale("en-US"), "en-US");
});

const detail = {
  kind: "card", id: "101", locale: "zh-CN", title: "测试卡面",
  episodes: [
    { key: "1", title: "テスト一話", fetched: true, cnState: "imported", enState: "pending", lines: [
      { jp: "テスト台詞二", role: "talk", speaker: "テスト話者", position: 2, text: "", source: "", revision: 0 },
      { jp: "テスト一話", role: "title", position: 0, text: "测试标题一", source: "official", revision: 2 },
      { jp: "テスト話者", role: "speaker", position: 1, text: "测试说话人", source: "human", revision: 1 },
    ] },
    { key: "2", title: "テスト二話", fetched: true, cnState: "absent", enState: "absent", lines: [
      { jp: "テスト台詞三", role: "talk", speaker: "テスト話者", position: 1, text: "测试台词三", source: "llm", revision: 4 },
    ] },
  ],
};

test("story entries group by episode, order by position and carry the speaker of each talk line", () => {
  const entries = model.buildSideStoryEntries(detail);
  assert.deepEqual(entries.map((entry) => [entry.key, entry.entryType, entry.lineRole, entry.source, entry.revision]), [
    ["1|テスト一話", "title", "title", "cn", 2],
    ["1|テスト話者", "talk", "speaker", "human", 1],
    ["1|テスト台詞二", "talk", "talk", "unknown", 0],
    ["2|テスト台詞三", "talk", "talk", "llm", 4],
  ]);
  assert.equal(entries[2].speakerName, "テスト話者");
  assert.equal(entries[1].speakerName, undefined);
  assert.equal(model.sideStoryEntryUntranslated(entries[2]), true);
  assert.equal(labels.storyEntrySourceText(entries[0]), "[章节标题] テスト一話");
  assert.equal(labels.storyEntrySourceText(entries[1]), "[说话人] テスト話者");
  const japanese = model.buildSideStoryEntries(detail, true);
  assert.equal(japanese[3].text, "テスト台詞三");
  assert.equal(japanese[3].source, "unknown");
});

test("a line edit carries the loaded revision as expectedRevision", () => {
  const [, speaker, talk] = model.buildSideStoryEntries(detail);
  assert.deepEqual(model.sideStoryLineEdit(speaker, "测试说话人二", "human"),
    { jp: "テスト話者", text: "测试说话人二", source: "human", expectedRevision: 1 });
  assert.deepEqual(model.sideStoryLineEdit(talk, "测试台词二", "llm"),
    { jp: "テスト台詞二", text: "测试台词二", source: "llm", expectedRevision: 0 });
});

test("authoritative line states apply per episode and never roll a newer revision back", () => {
  const entries = model.buildSideStoryEntries(detail);
  const next = model.applySideStoryLineStates(entries, "1", [
    { jp: "テスト台詞二", role: "talk", position: 2, text: "测试台词二", source: "human", revision: 1 },
    { jp: "テスト話者", role: "speaker", position: 1, text: "测试旧说话人", source: "human", revision: 0 },
    { jp: "テスト台詞三", role: "talk", position: 1, text: "测试错话", source: "human", revision: 9 },
  ]);
  assert.equal(next[2].text, "测试台词二");
  assert.equal(next[2].revision, 1);
  assert.equal(next[2].source, "human");
  assert.equal(next[1].text, "测试说话人", "an older revision is ignored");
  assert.equal(next[3].text, "测试台词三", "a line of another episode is untouched");
});

class TestAPIError extends Error {
  constructor(status, body) {
    super(body.error);
    this.status = status;
    this.code = body.error;
    this.details = body.details || [];
    this.current = body.current;
  }
}

const conflictError = () => new TestAPIError(409, {
  error: "revision_conflict",
  current: { conflicts: [{ jp: "テスト台詞二", expectedRevision: 0, currentRevision: 5, currentText: "测试服务器译文", currentSource: "human" }] },
});

test("conflict errors parse only from a 409 revision_conflict and unknown lines only from a 422", () => {
  assert.deepEqual(model.sideStoryConflictsFromError(conflictError()), [
    { jp: "テスト台詞二", expectedRevision: 0, currentRevision: 5, currentText: "测试服务器译文", currentSource: "human" },
  ]);
  assert.equal(model.sideStoryConflictsFromError(new TestAPIError(409, { error: "backfill_disabled" })), null);
  assert.equal(model.sideStoryConflictsFromError(new TestAPIError(500, { error: "revision_conflict", current: { conflicts: [] } })), null);
  assert.deepEqual(model.sideStoryUnknownLinesFromError(new TestAPIError(422, { error: "unknown_lines", current: { lines: ["テスト台詞九", 3] } })), ["テスト台詞九"]);
  assert.equal(model.sideStoryMutationResultIsAmbiguous(new TestAPIError(409, { error: "revision_conflict" })), false);
  assert.equal(model.sideStoryMutationResultIsAmbiguous(new TestAPIError(502, { error: "upstream_unavailable" })), true);
  assert.equal(model.sideStoryMutationResultIsAmbiguous(new TypeError("network")), true);
  assert.match(model.sideStoryErrorMessage(new TestAPIError(409, { error: "backfill_disabled" }), "x"), /回填未启用/);
  assert.equal(model.sideStoryErrorMessage(new TestAPIError(400, { error: "something_else" }), "兜底"), "something_else");
});

test("error messages append the contract details and map the runner codes", () => {
  const message = (status, body) => model.sideStoryErrorMessage(new TestAPIError(status, body), "兜底");
  assert.equal(message(500, { error: "internal_error", details: ["测试批次 2/5 失败", "测试 HTTP 429"] }), "服务器内部错误：测试批次 2/5 失败；测试 HTTP 429");
  assert.equal(message(409, { error: "already_running", details: ["测试任务运行中"] }), "另一个任务正在运行（同步、AI 翻译或备份恢复），请稍后再试：测试任务运行中");
  assert.equal(message(503, { error: "draining", details: ["测试停机"] }), "服务正在关闭或重启，请稍后再试：测试停机");
  assert.equal(message(409, { error: "producer_state_changed", details: ["测试状态变化"] }), "保存被拒绝：测试状态变化");
  assert.equal(message(400, { error: "something_else", details: ["测试原因"] }), "something_else：测试原因");
  assert.equal(message(500, { error: "internal_error" }), "服务器内部错误");
  assert.equal(message(409, { error: "script_changed", details: ["the Japanese script changed upstream; refresh the story before importing"] }),
    "上游剧本已变化，请先由管理员重新获取剧本后再导入", "an English restatement of the mapped message is dropped");
  assert.match(message(409, { error: "backfill_disabled", details: ["the side-story backfill is disabled"] }), /卡牌剧情\/区域对话后台回填.*SIDE_STORY_BACKFILL_ENABLED=false$/);
  assert.equal(model.sideStoryErrorMessage({ code: "internal_error", details: [3, ""] }, "兜底"), "服务器内部错误");
  assert.equal(model.sideStoryErrorMessage(new Error("测试网络错误"), "兜底"), "测试网络错误");
  assert.equal(model.sideStoryErrorMessage(null, "兜底"), "兜底");
});

test("admin action results describe AI fill and refresh outcomes", () => {
  assert.equal(model.describeSideStoryAIResult({ translated: 3, remaining: 1 }, "2"), "AI 补充翻译完成（第 2 话）：已翻译 3 行，仍有 1 行未翻译");
  assert.match(model.describeSideStoryAIResult({ translated: 0, remaining: 0 }, ""), /（整篇）/);
  const refresh = model.describeSideStoryRefresh([
    { kind: "card", id: "101", key: "1", fetched: true, scriptChanged: true, cnState: "imported", enState: "absent", officialWritten: 4, droppedHumanLines: 1, error: "" },
    { kind: "card", id: "101", key: "2", fetched: false, scriptChanged: false, cnState: "pending", enState: "pending", officialWritten: 0, droppedHumanLines: 0, error: "ja-JP: 测试错误" },
  ]);
  assert.equal(refresh.ok, false);
  assert.equal(refresh.message, "已重新获取 1/2 话剧本，1 话剧本有变化，写入官方译文 4 行，1 行人工译文因原文已删除而移除，失败：第 2 话 ja-JP: 测试错误");
  assert.equal(model.SIDE_STORY_FETCH_STATE_LABELS.mismatch, "剧本结构不一致");
});

const applied = (overrides) => ({
  kind: "card", id: "101", key: "1", fetched: true, scriptChanged: false, cnState: "imported", enState: "imported",
  officialWritten: 0, droppedHumanLines: 0, error: "", ...overrides,
});

test("a refresh lists a pending locale's note as a retry and fails only on the JP script or an error/mismatch state", () => {
  const retried = model.describeSideStoryRefresh([
    applied({ cnState: "pending", enState: "pending", error: "zh-CN: not found; en-US: 测试源甲: HTTP 503; 测试源乙: HTTP 503" }),
    applied({ key: "2", enState: "pending", error: "en-US: official script repeats the Japanese text" }),
  ]);
  assert.equal(retried.ok, true);
  assert.equal(retried.message, "已重新获取 2/2 话剧本，待重试：第 1 话 zh-CN: not found；第 1 话 en-US: 测试源甲: HTTP 503; 测试源乙: HTTP 503；" +
    "第 2 话 en-US: official script repeats the Japanese text");
  const mixed = model.describeSideStoryRefresh([
    applied({ cnState: "pending", enState: "mismatch", error: "zh-CN: not found; en-US: TalkData length mismatch (3 != 2)" }),
  ]);
  assert.equal(mixed.ok, false);
  assert.equal(mixed.message, "已重新获取 1/1 话剧本，失败：第 1 话 en-US: TalkData length mismatch (3 != 2)，待重试：第 1 话 zh-CN: not found");
  const failed = model.describeSideStoryRefresh([applied({ cnState: "error", error: "zh-CN: 测试解析错误" })]);
  assert.equal(failed.ok, false);
  assert.match(failed.message, /，失败：第 1 话 zh-CN: 测试解析错误$/);
});

test("the episode notice reads only the viewed locale and shows a pending locale's note as a retry", () => {
  const notice = (overrides, locale) => model.sideStoryEpisodeNotice({ fetched: true, cnState: "imported", enState: "imported", lastError: "", ...overrides }, locale);
  const cnFailure = { cnState: "error", lastError: "zh-CN: 测试错误" };
  assert.equal(notice(cnFailure, "zh-CN"), "error");
  assert.equal(notice(cnFailure, "en-US"), null, "a CN failure is not an en-US notice");
  const enRetry = { enState: "pending", lastError: "en-US: not found" };
  assert.equal(notice(enRetry, "en-US"), "retry");
  assert.equal(notice(enRetry, "zh-CN"), null);
  assert.equal(notice({ cnState: "pending", lastError: "zh-CN: 测试源甲: HTTP 503; 测试源乙: HTTP 503" }, "zh-CN"), "retry", "a message containing '; ' stays one part");
  assert.equal(notice({ enState: "mismatch" }, "en-US"), "error", "a failed state is an error without a note");
  assert.equal(notice({ cnState: "pending" }, "zh-CN"), null, "pending without a note is only not imported yet");
  assert.equal(notice({ fetched: false, cnState: "pending", lastError: "ja-JP: 测试错误" }, "en-US"), "error");
  assert.equal(notice({ fetched: false, cnState: "pending" }, "zh-CN"), null);
  assert.equal(notice(cnFailure, "ja-JP"), "error", "the Japanese view follows the CN state label");
});

test("the toolbar shows the viewed locale's notice and keeps the whole error in the tooltip", () => {
  const { SideStoryToolbar } = loadSourceModule("components/console/SideStoryToolbar.tsx", {
    "@/components/SideStoryTxtImport": { SideStoryTxtImport: () => null },
  });
  const lastError = "zh-CN: 测试错误; en-US: not found";
  const toolbar = (locale) => renderToStaticMarkup(createElement(SideStoryToolbar, {
    role: "editor", locale, kind: "card", storyId: "101", entries: [], chapters: [], selectedEpisode: "1", selectChapter() {},
    busy: false, saving: false, writesLocked: false, entryDirty: false, saveBatch: async () => ({ status: "saved", updated: 0, unchanged: 0 }),
    onAI() {}, onRefresh() {}, onReload() {},
    detail: { ...detail, episodes: [{ ...detail.episodes[0], cnState: "error", enState: "pending", lastError }] },
  }));
  const english = toolbar("en-US");
  assert.match(english, /第 1 话：英文官方待导入 · 待重试<\/span>/);
  assert.doesNotMatch(english, /有错误/);
  assert.match(english, new RegExp(`title="${lastError}"`));
  assert.match(toolbar("zh-CN"), /第 1 话：简中官方导入失败 · 有错误<\/span>/);
});

test("Moesekai links point at the main site's card, area and event story pages", () => {
  assert.match(labels.sideStoryMoesekaiUrl("card", "101"), /^https?:\/\/[^/]+(\/.*)?\/story\/card\/101\/$/);
  assert.match(labels.sideStoryMoesekaiUrl("area", "areatalk_ev_1", "event_12"), /\/story\/area\/event_12\/areatalk_ev_1\/$/);
  assert.match(labels.sideStoryMoesekaiUrl("area", "a b", "limited_7"), /\/story\/area\/limited_7\/a%20b\/$/);
  assert.equal(labels.sideStoryMoesekaiUrl("area", "areatalk_ev_1", ""), null);
  assert.equal(labels.sideStoryMoesekaiUrl("card", ""), null);
  assert.equal(labels.buildMoesekaiUrl("cardStory", "101"), labels.sideStoryMoesekaiUrl("card", "101"));
  assert.match(labels.buildMoesekaiUrl("eventStory", "163"), /\/story\/event\/163\/$/);
  assert.equal(labels.CATEGORY_LABELS.cardStory, "卡牌剧情");
  assert.equal(labels.CATEGORY_LABELS.areaTalk, "区域对话");
});

test("sidestory.updated events normalise and resolve to ignore, apply-lines or reload", () => {
  assert.equal(model.normalizeSideStoryUpdateEvent({ kind: "event", id: "1", action: "update" }), null);
  assert.equal(model.normalizeSideStoryUpdateEvent({ kind: "card", id: "", action: "update" }), null);
  assert.equal(model.normalizeSideStoryUpdateEvent({ kind: "card", id: "1", action: "delete" }), null);
  const update = model.normalizeSideStoryUpdateEvent({
    kind: "card", id: "101", episode: "1", locale: "zh-CN", action: "update", clientId: "tab-b",
    lines: [{ jp: "テスト台詞二", role: "talk", position: 2, text: "测试台词二", source: "human", revision: 1 }, { jp: 3 }],
  });
  assert.equal(update.user, "协作者");
  assert.equal(update.lines.length, 1);

  const view = { kind: "card", id: "101", locale: "zh-CN" };
  const effect = (event, overrides = {}, clientID = "tab-a") => model.sideStoryUpdateEffect({ ...event, ...overrides }, { ...view }, clientID);
  assert.equal(effect(update), "apply-lines");
  assert.equal(effect(update, {}, "tab-b"), "ignore", "own echo");
  assert.equal(effect(update, { id: "102" }), "ignore");
  assert.equal(effect(update, { kind: "area" }), "ignore");
  assert.equal(effect(update, { locale: "en-US" }), "ignore");
  assert.equal(effect(update, { action: "ai", lines: [] }), "reload");
  assert.equal(effect(update, { lines: [] }), "reload");
  assert.equal(effect(update, { action: "refresh", locale: "", lines: [] }), "reload");
  assert.equal(model.sideStoryUpdateEffect({ ...update, action: "refresh" }, { ...view, locale: "ja-JP" }, "tab-a"), "reload");
  assert.equal(model.sideStoryUpdateEffect(update, { ...view, locale: "ja-JP" }, "tab-a"), "ignore");

  assert.equal(model.sideStoryUpdateRefreshesList(update, "zh-CN"), true);
  assert.equal(model.sideStoryUpdateRefreshesList(update, "en-US"), false);
  assert.equal(model.sideStoryUpdateRefreshesList({ ...update, action: "refresh", locale: "" }, "en-US"), true);
});

test("the SSE client subscribes to side-story events and the realtime hook routes them", async () => {
  const [sse, realtime] = await Promise.all([read("src/lib/sse.ts"), read("src/components/console/useConsoleRealtime.ts")]);
  assert.match(sse, /const SSE_EVENTS = new Set<SSEEvent>\(\[[^\]]*"sidestory\.updated", "sidestory\.sync"/);
  assert.match(realtime, /event === "sidestory\.updated"\) \{\s*const update = normalizeSideStoryUpdateEvent\(d\);/);
  assert.match(realtime, /sideStoryUpdateRefreshesList\(update, sideStoryLocale\(locale\)\)\) refreshSideStoryLists\(update\.kind\)/);
  assert.match(realtime, /event === "sidestory\.sync"\) \{\s*refreshSideStoryLists\(\);/);
});

function editorHarness({ respond, entry, locale = "zh-CN", kind = "card" }) {
  const runtime = createHookRuntime();
  const calls = [];
  const toasts = [];
  const reconciles = [];
  const saved = [];
  let remoteConflict = null;
  let editValue = "测试草稿";
  const api = {
    APIError: TestAPIError,
    updateSideStoryLines: async (...args) => { calls.push(args); return respond(...args); },
    updateEntry: async () => { throw new Error("generic save must not run for a side story"); },
    updateEventStoryLine: async () => { throw new Error("event save must not run for a side story"); },
  };
  const { useEntryEditor } = loadSourceModule("components/console/useEntryEditor.ts", { react: runtime.react, "@/lib/api": api });
  const entriesRef = { current: [entry] };
  const setEntries = (next) => { entriesRef.current = typeof next === "function" ? next(entriesRef.current) : next; };
  const editor = useEntryEditor({
    username: "tester", show: (message, tone) => toasts.push([tone, message]), category: kind === "card" ? "cardStory" : "areaTalk",
    field: "101", locale, isEventStory: false, sideStoryKind: kind, isReadOnly: false,
    entries: entriesRef.current, entriesRef, setEntries, filtered: entriesRef.current,
    selectedKey: entry.key, setSelectedKey() {}, selectedEntry: entry, selectedEpisode: "all",
    editValue, setEditValue: (value) => { editValue = value; }, entryDirty: true,
    eventTxtDraft: null, setEventTxtDraft() {}, eventTxtDraftDirty: false, keepTranslationEntryVisible() {},
    reloadSidebar: async () => true,
    reconcileContentRef: { current: async (...args) => { reconciles.push(args); return true; } },
    writeFenceRef: { current: false }, savingRef: { current: false }, setSaving() {},
    remoteConflictRef: { current: null }, setRemoteConflict: (next) => { remoteConflict = next; },
    contextGenerationRef: { current: 1 }, onSideStorySaved: (savedKind) => saved.push(savedKind),
  });
  return {
    editor, calls, toasts, reconciles, saved,
    entries: () => entriesRef.current, remoteConflict: () => remoteConflict, editValue: () => editValue,
  };
}

const talkEntry = model.buildSideStoryEntries(detail)[2];
const okResult = (lines) => ({ status: "ok", kind: "card", id: "101", episode: "1", locale: "zh-CN", updated: lines.length, unchanged: 0, lines });

test("saving a line sends one PUT with expectedRevision and applies the returned revision", async () => {
  const harness = editorHarness({
    entry: { ...talkEntry, revision: 3 },
    locale: "en-US",
    respond: () => okResult([{ jp: "テスト台詞二", role: "talk", position: 2, text: "测试草稿", source: "human", revision: 4 }]),
  });
  assert.equal(await harness.editor.save("human", false), true);
  assert.deepEqual(harness.calls, [["card", "101", "1", "en-US", [{ jp: "テスト台詞二", text: "测试草稿", source: "human", expectedRevision: 3 }]]]);
  assert.equal(harness.entries()[0].revision, 4);
  assert.equal(harness.entries()[0].text, "测试草稿");
  assert.deepEqual(harness.saved, ["card"]);
  assert.deepEqual(harness.reconciles, []);
});

test("a revision conflict keeps the draft, shows the server line and does not reconcile", async () => {
  const harness = editorHarness({ entry: talkEntry, respond: () => { throw conflictError(); } });
  assert.equal(await harness.editor.save("human", false), false);
  assert.equal(harness.editValue(), "测试草稿", "the local draft stays in the textarea");
  assert.equal(harness.entries()[0].text, "测试服务器译文");
  assert.equal(harness.entries()[0].revision, 5);
  assert.deepEqual(harness.remoteConflict(), { key: talkEntry.key, user: "服务器", current: { text: "测试服务器译文", revision: 5 } });
  assert.deepEqual(harness.reconciles, []);
  assert.match(harness.toasts.at(-1)[1], /保存被拒绝/);
});

test("an ambiguous save failure freezes the draft and reconciles", async () => {
  const harness = editorHarness({ entry: talkEntry, respond: () => { throw new TestAPIError(503, { error: "side_story_unavailable" }); } });
  assert.equal(await harness.editor.save("human", false), false);
  assert.equal(harness.reconciles.length, 1);
  assert.equal(harness.reconciles[0][0], "remote");
  assert.equal(JSON.parse(harness.reconciles[0][1]).staleText, "测试草稿");
  assert.deepEqual(harness.toasts.at(-1), ["err", "剧情服务当前不可用"]);
});

test("a TXT batch saves the whole episode in one PUT and reports conflicts without writing", async () => {
  const edits = [
    { jp: "テスト台詞二", text: "测试台词二", source: "human", expectedRevision: 0 },
    { jp: "テスト話者", text: "测试说话人", source: "human", expectedRevision: 1 },
  ];
  const saved = editorHarness({ entry: talkEntry, respond: (...args) => okResult(args[4].map((edit, index) => ({ ...edit, role: "talk", position: index, revision: edit.expectedRevision + 1 }))) });
  assert.deepEqual(await saved.editor.saveSideStoryBatch("1", edits), { status: "saved", updated: 2, unchanged: 0 });
  assert.equal(saved.calls.length, 1);
  assert.deepEqual(saved.calls[0][4], edits);
  assert.equal(saved.editValue(), "测试台词二", "the selected line follows the saved text");

  const conflicted = editorHarness({ entry: talkEntry, respond: () => { throw conflictError(); } });
  const outcome = await conflicted.editor.saveSideStoryBatch("1", edits);
  assert.equal(outcome.status, "conflict");
  assert.equal(outcome.conflicts[0].currentText, "测试服务器译文");
  assert.equal(conflicted.entries()[0].text, "测试服务器译文");
  assert.deepEqual(conflicted.saved, []);
});

test("the workspace shows the server text of a revision conflict with reload", () => {
  const entry = { ...talkEntry, text: "测试服务器译文", revision: 5 };
  const html = renderToStaticMarkup(createElement(TranslationEntryWorkspace, {
    category: "cardStory", field: "101", isEventStory: false, isSideStory: true, isReadOnly: false,
    loading: false, saving: false, writesLocked: false, filtered: [entry], selectedKey: entry.key,
    selectedEntry: entry, selectedIndex: 0, selectedEventStoryIdentityMissing: false, remoteHighlights: {},
    remoteConflict: { key: entry.key, user: "服务器", current: { text: "测试服务器译文", revision: 5 } },
    setRemoteConflict() {}, eventTxtDraftDirty: false, editValue: "测试草稿", setEditValue() {},
    editRef: { current: null }, translationEntryListRef: { current: null }, enterSaves: false, setEnterSaves() {},
    onTextareaKey() {}, navigate() {}, save: async () => true, selectEntry() {}, handleSourceChange() {},
    detailUrl: labels.sideStoryMoesekaiUrl("card", "101"), onReloadStory() {},
  }));
  assert.match(html, /remote-conflict-banner revision-conflict/);
  assert.match(html, /服务器当前译文：测试服务器译文/);
  assert.match(html, /revision 5/);
  assert.match(html, />重新载入本篇</);
  assert.match(html, /第 1 话/);
  assert.match(html, /テスト話者/, "the talk line shows its speaker");
  assert.doesNotMatch(html, /<option value="cn"/, "side-story rows only offer human and AI sources");
});

const loaded = (stories) => ({ stories, loaded: true, loading: false, failed: false });

function sidebar(props) {
  return renderToStaticMarkup(createElement(SideStorySidebar, {
    category: "", field: "", hiddenBadges: new Set(), cardStories: loaded([]), areaTalks: loaded([]), eventStories: [],
    cardExpanded: true, setCardExpanded() {}, areaExpanded: true, setAreaExpanded() {}, onRetry() {}, selectField() {},
    ...props,
  }));
}

test("the card story group pages by 50 and shows status pills with untranslated counts", () => {
  const cards = Array.from({ length: 53 }, (_, index) => summary({ id: String(index + 1), releasedAt: index, title: `测试卡面${index + 1}` }));
  cards[52] = { ...cards[52], status: "partial", primarySource: "human", untranslatedCount: 4 };
  cards[51] = { ...cards[51], status: "pending", untranslatedCount: 9 };
  const html = sidebar({ cardStories: loaded(cards), hiddenBadges: new Set(["cardStory:51"]) });
  assert.match(html, /卡牌剧情 \(53\/53\)/);
  assert.equal((html.match(/class="field-item /g) ?? []).length, 50);
  assert.match(html, /显示更多（剩余 3）/);
  assert.match(html, /测试卡面53<\/span><small>#53 · 星乃一歌<\/small><\/span><span class="side-story-badges"><span class="side-story-status human">人工<\/span><span class="badge work" title="未翻译行数">4<\/span>/);
  assert.match(html, /测试卡面52<\/span>[^]*?<span class="side-story-status pending">未获取<\/span><\/span>/);
  assert.match(html, /测试卡面51<\/span>[^]*?<span class="side-story-status untranslated">未翻译<\/span><\/span>/, "a hidden badge drops the count");
});

test("the area talk group renders collapsed category subgroups with untranslated sums", () => {
  const html = sidebar({
    cardExpanded: false,
    areaTalks: loaded([area("areatalk_ev_1", "event_12", 1201, 3), area("areatalk_g1_1", "grade1", 100, 0)]),
    eventStories: [{ eventId: 12, eventName: "测试活动", eventNameJapanese: "" }],
  });
  assert.match(html, /区域对话 \(2\/2\)/);
  assert.match(html, /升学前<\/span><span class="side-story-subgroup-meta">1<span[^]*活动 12 · 测试活动<\/span><span class="side-story-subgroup-meta">1 · 未译 3/);
  assert.doesNotMatch(html, /areatalk_ev_1/, "groups start collapsed");
  assert.doesNotMatch(html, /id="card-story-sidebar-list"/, "a collapsed group renders no list");
});

test("the side-story groups sit between event stories and music content", async () => {
  const source = await read("src/components/console/ConsoleSidebar.tsx");
  const eventGroup = source.indexOf("活动剧情 (");
  const sideStories = source.indexOf("{sideStories}");
  const music = source.indexOf("音乐内容");
  assert.ok(eventGroup > 0 && eventGroup < sideStories && sideStories < music);
});

test("the backfill panel shows progress and offers resume and catalog refresh to admins only", () => {
  const status = {
    state: {
      enabled: true, running: false, lastRoundAt: "", nextRoundAt: "", lastRoundError: "测试错误信息",
      lastRound: { episodes: 20, requests: 30, fetched: 18, officialWritten: 100, errors: 2 },
    },
    totals: { card: { stories: 5, episodes: 10, fetched: 8, pendingFetch: 2, errors: 1, cnImported: 6, cnPending: 1, cnAbsent: 1, cnMismatch: 0, cnError: 0, enImported: 2, enPending: 3, enAbsent: 3, enMismatch: 0, enError: 0 } },
  };
  const panel = (role) => renderToStaticMarkup(createElement(SideStoryBackfillPanel, {
    role, expanded: true, setExpanded() {}, status, busy: false, watch() {}, reload() {}, runSync() {},
  }));
  const admin = panel("admin");
  assert.match(admin, /5 篇 · 已获取 8\/10 话 · 待获取 2 · 获取失败 1/);
  assert.match(admin, /（20 话，请求 30 次，获取 18，官方写入 100 行，错误 2）/);
  assert.match(admin, /最近错误<\/dt><dd class="side-story-backfill-error">测试错误信息/);
  assert.match(admin, />立即续跑</);
  assert.match(admin, />刷新目录</);
  const editor = panel("editor");
  assert.doesNotMatch(editor, /立即续跑|刷新目录/);
  assert.match(editor, />刷新进度</);
});
