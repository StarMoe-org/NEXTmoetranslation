import assert from "node:assert/strict";
import { webcrypto } from "node:crypto";
import { readFile } from "node:fs/promises";
import test from "node:test";

import {
  eventEpisodeTxtImportPreview,
  parseEventTxtContent,
  sideStoryEpisodeTxtImportPreview,
  sideStoryTxtImportEdits,
  validateEventEpisodeSnapshot,
  validateSideStoryEpisodeSnapshot,
} from "../src/lib/event-txt-import.mjs";
import { loadSourceModule } from "./source-module-harness.mjs";

if (!globalThis.crypto) globalThis.crypto = webcrypto;

const model = loadSourceModule("lib/side-story-console.ts");

// Synthetic test script: the third talk repeats the first line's Japanese text.
async function sideSnapshot(overrides = {}) {
  const rawJson = JSON.stringify({
    ScenarioId: "test_card_101_1",
    Snippets: [
      { Action: 1, ReferenceIndex: 0 },
      { Action: 1, ReferenceIndex: 1 },
      { Action: 1, ReferenceIndex: 2 },
    ],
    TalkData: [
      { WindowDisplayName: "テスト話者_01", Body: "テスト台詞一", Voices: [], WhenFinishCloseWindow: 0 },
      { WindowDisplayName: "テスト話者", Body: "テスト台詞二", Voices: [], WhenFinishCloseWindow: 0 },
      { WindowDisplayName: "テスト話者", Body: "テスト台詞一", Voices: [], WhenFinishCloseWindow: 0 },
    ],
    SpecialEffectData: [],
    AppearCharacters: [],
  });
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(rawJson));
  const sha256 = Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, "0")).join("");
  const segment = (id, position, japanese, text = "", revision = 0, source = "") =>
    ({ id, kind: "talk", position, japanese, sourceHash: "", text, source, revision });
  return {
    kind: "card",
    id: "101",
    episode: "1",
    locale: "zh-CN",
    revision: "test-snapshot-revision",
    segments: [
      segment("テスト台詞一", 0, "テスト台詞一"),
      segment("テスト話者_01", 1, "テスト話者"),
      segment("テスト台詞二", 2, "テスト台詞二", "测试旧译", 2, "human"),
      segment("テスト話者", 3, "テスト話者", "测试说话人", 1, "human"),
      segment("テスト台詞一", 4, "テスト台詞一"),
      segment("テスト話者", 5, "テスト話者", "测试说话人", 1, "human"),
    ],
    scenario: {
      scenarioId: "test_card_101_1",
      fileName: "test_card_101_1.json",
      sha256,
      parserVersion: 1,
      rawJson,
      sourceTalks: [
        { speaker: "テスト話者", text: "テスト台詞一", charIndex: 0, talkDataIndex: 0 },
        { speaker: "テスト話者", text: "テスト台詞二", charIndex: 0, talkDataIndex: 1 },
        { speaker: "テスト話者", text: "テスト台詞一", charIndex: 0, talkDataIndex: 2 },
      ],
    },
    ...overrides,
  };
}

const expected = { kind: "card", id: "101", episode: "1", locale: "zh-CN" };
const txt = "测试话者：测试台词一\n测试话者：测试台词二\n测试话者：测试台词一改\n";

test("a side-story snapshot must match the requested kind, id, episode and locale", async () => {
  const snapshot = await sideSnapshot();
  await validateSideStoryEpisodeSnapshot(snapshot, expected);
  for (const [field, value] of [["kind", "area"], ["id", "102"], ["episode", "2"], ["locale", "en-US"]]) {
    await assert.rejects(validateSideStoryEpisodeSnapshot(snapshot, { ...expected, [field]: value }), /side story snapshot identity mismatch/, field);
  }
  await assert.rejects(validateSideStoryEpisodeSnapshot({ ...snapshot, kind: "event" }, { ...expected, kind: "event" }), /side story episode identity is invalid/);
  await assert.rejects(validateSideStoryEpisodeSnapshot({ ...snapshot, episode: "" }, { ...expected, episode: "" }), /side story episode identity is invalid/);
  await assert.rejects(validateSideStoryEpisodeSnapshot({ ...snapshot, revision: "" }, expected), /revision is required/);
  await assert.rejects(
    validateSideStoryEpisodeSnapshot({ ...snapshot, scenario: { ...snapshot.scenario, sha256: "0".repeat(64) } }, expected),
    /scenario SHA-256 mismatch/,
  );
});

test("event and side-story snapshots are not interchangeable", async () => {
  const snapshot = await sideSnapshot();
  await assert.rejects(validateEventEpisodeSnapshot(snapshot), /event episode identity is invalid/);
  assert.throws(() => eventEpisodeTxtImportPreview(snapshot, parseEventTxtContent(txt)), /event episode identity is invalid/);
  const eventShaped = { ...snapshot, kind: undefined, id: undefined, episode: undefined, eventId: 44, episodeNo: "1" };
  assert.throws(() => sideStoryEpisodeTxtImportPreview(eventShaped, parseEventTxtContent(txt)), /side story episode identity is invalid/);
});

test("the side-story preview keys rows by position and saves a repeated Japanese line once", async () => {
  const preview = sideStoryEpisodeTxtImportPreview(await sideSnapshot(), parseEventTxtContent(txt));
  const rows = new Map(preview.rows.map((row) => [row.id, row]));
  assert.deepEqual([...rows.keys()], ["0:body", "1:speaker", "2:body", "3:speaker", "4:body", "5:speaker"]);
  assert.equal(rows.get("0:body").status, "matched");
  assert.equal(rows.get("0:body").selectedByDefault, true);
  assert.equal(rows.get("1:speaker").segmentId, "テスト話者_01");
  assert.equal(rows.get("2:body").status, "conflict");
  assert.equal(rows.get("2:body").selectable, true);
  assert.equal(rows.get("2:body").selectedByDefault, false);
  assert.equal(rows.get("4:body").status, "conflict", "a repeat with different TXT text");
  assert.equal(rows.get("4:body").selectable, false);
  assert.equal(rows.get("5:speaker").status, "matched", "a repeat with the same TXT text");
  assert.equal(rows.get("5:speaker").selectable, false);
  assert.deepEqual(preview.counts, { matched: 3, conflict: 3, missing: 0, unmatched: 0 });
  assert.equal(preview.revision, "test-snapshot-revision");
});

// Synthetic script: the first and third speakers are kana-free like names Chinese shares with Japanese.
function kanaFreeSpeakerSnapshot() {
  const talkData = [["試験", "テスト台詞一"], ["テスト話者", "テスト台詞二"], ["検査", "テスト台詞三"]];
  const rawJson = JSON.stringify({
    ScenarioId: "test_card_102_1",
    Snippets: talkData.map((_, index) => ({ Action: 1, ReferenceIndex: index })),
    TalkData: talkData.map(([speaker, body]) => ({ WindowDisplayName: speaker, Body: body, Voices: [], WhenFinishCloseWindow: 0 })),
    SpecialEffectData: [],
    AppearCharacters: [],
  });
  const segment = (id, position, text = "", revision = 0, source = "") =>
    ({ id, kind: "talk", position, japanese: id, sourceHash: "", text, source, revision });
  return {
    kind: "card", id: "102", episode: "1", locale: "zh-CN", revision: "test-snapshot-revision",
    segments: [
      segment("テスト台詞一", 0), segment("試験", 1), segment("テスト台詞二", 2), segment("テスト話者", 3),
      segment("テスト台詞三", 4), segment("検査", 5, "検査", 1, "official"),
    ],
    scenario: {
      scenarioId: "test_card_102_1", fileName: "test_card_102_1.json", sha256: "", parserVersion: 1, rawJson,
      sourceTalks: talkData.map(([speaker, text], index) => ({ speaker, text, charIndex: 0, talkDataIndex: index })),
    },
  };
}

test("text equal to kana-free Japanese is a side-story translation, but not in the event preview", () => {
  const talks = parseEventTxtContent("試験：测试台词一\nテスト話者：测试台词二\n测试检查：测试台词三\n");
  const preview = sideStoryEpisodeTxtImportPreview(kanaFreeSpeakerSnapshot(), talks);
  const rows = new Map(preview.rows.map((row) => [row.id, row]));
  assert.deepEqual([rows.get("1:speaker").status, rows.get("1:speaker").selectedByDefault], ["matched", true], "a shared name is imported");
  assert.equal(rows.get("3:speaker").status, "missing", "the kana Japanese is still refused");
  assert.deepEqual([rows.get("5:speaker").status, rows.get("5:speaker").selectedByDefault], ["conflict", false],
    "a stored shared name is a translation the TXT would overwrite");
  const defaults = new Set(preview.rows.filter((row) => row.selectedByDefault).map((row) => row.id));
  assert.deepEqual(sideStoryTxtImportEdits(preview, defaults).find((edit) => edit.jp === "試験"),
    { jp: "試験", text: "試験", source: "human", expectedRevision: 0 });

  const eventShaped = { ...kanaFreeSpeakerSnapshot(), kind: undefined, id: undefined, episode: undefined, eventId: 44, episodeNo: "1" };
  const event = new Map(eventEpisodeTxtImportPreview(eventShaped, talks).rows.map((row) => [row.id, row]));
  assert.equal(event.get("試験:speaker").status, "missing");
  assert.deepEqual([event.get("検査:speaker").status, event.get("検査:speaker").selectedByDefault], ["matched", true]);
});

test("selected preview rows become one batch of human edits with their snapshot revisions", async () => {
  const preview = sideStoryEpisodeTxtImportPreview(await sideSnapshot(), parseEventTxtContent(txt));
  const edits = sideStoryTxtImportEdits(preview, new Set(["0:body", "1:speaker", "2:body", "4:body"]));
  assert.deepEqual(edits, [
    { jp: "テスト台詞一", text: "测试台词一", source: "human", expectedRevision: 0 },
    { jp: "テスト話者_01", text: "测试话者", source: "human", expectedRevision: 0 },
    { jp: "テスト台詞二", text: "测试台词二", source: "human", expectedRevision: 2 },
  ]);
  const forged = { ...preview, rows: preview.rows.map((row) => row.id === "4:body" ? { ...row, selectable: true } : row) };
  assert.throws(() => sideStoryTxtImportEdits(forged, new Set(["0:body", "4:body"])), /selected テスト台詞一 twice/);
});

test("an import is refused when the loaded episode no longer equals the snapshot", async () => {
  const snapshot = await sideSnapshot();
  const entries = model.buildSideStoryEntries({
    kind: "card", id: "101", locale: "zh-CN", title: "测试卡面",
    episodes: [{ key: "1", title: "", fetched: true, cnState: "absent", enState: "absent", lines: [
      { jp: "テスト台詞一", role: "talk", speaker: "テスト話者", position: 0, text: "", source: "", revision: 0 },
      { jp: "テスト話者_01", role: "speaker", position: 1, text: "", source: "", revision: 0 },
      { jp: "テスト台詞二", role: "talk", speaker: "テスト話者", position: 2, text: "测试旧译", source: "human", revision: 2 },
      { jp: "テスト話者", role: "speaker", position: 3, text: "测试说话人", source: "human", revision: 1 },
    ] }],
  });
  assert.equal(model.sideStorySnapshotMismatch(entries, snapshot), "");
  const stale = entries.map((entry) => entry.japanese === "テスト台詞二" ? { ...entry, revision: 3 } : entry);
  assert.match(model.sideStorySnapshotMismatch(stale, snapshot), /「テスト台詞二」已不等于服务器快照/);
  assert.match(model.sideStorySnapshotMismatch(entries.filter((entry) => entry.japanese !== "テスト話者"), snapshot), /「テスト話者」/);
});

test("the side-story importer validates the snapshot identity before previewing and saves one batch", async () => {
  const source = await readFile(new URL("../src/components/SideStoryTxtImport.tsx", import.meta.url), "utf8");
  const order = [
    "getSideStoryEpisodeSnapshot(kind, storyId, episode, locale)",
    "await validateSideStoryEpisodeSnapshot(snapshot, { kind, id: storyId, episode, locale });",
    "const mismatch = sideStorySnapshotMismatch(entries, snapshot);",
    "sideStoryEpisodeTxtImportPreview(snapshot, parseEventTxtContent(content))",
  ].map((needle) => source.indexOf(needle));
  assert.ok(order.every((index, position) => index > 0 && (position === 0 || index > order[position - 1])), String(order));
  assert.equal(source.match(/await saveBatch\(/g)?.length, 1);
});
