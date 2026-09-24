import assert from "node:assert/strict";
import test from "node:test";
import { webcrypto } from "node:crypto";

import {
  eventEpisodeTxtImportPreview,
  parseEventTxtContent,
  validateEventEpisodeSnapshot,
} from "../src/lib/event-txt-import.mjs";

if (!globalThis.crypto) globalThis.crypto = webcrypto;

async function snapshotFixture([first, second] = ["一行目", "二行目"]) {
  const rawJson = JSON.stringify({
    ScenarioId: "event-import",
    Snippets: [
      { Action: 1, ReferenceIndex: 0 },
      { Action: 6, ReferenceIndex: 0 },
      { Action: 1, ReferenceIndex: 1 },
    ],
    TalkData: [
      { WindowDisplayName: "初音ミク_01", Body: first, Voices: [], WhenFinishCloseWindow: 0 },
      { WindowDisplayName: "鏡音リン", Body: second, Voices: [], WhenFinishCloseWindow: 0 },
    ],
    SpecialEffectData: [{ EffectType: 8, StringVal: "教室" }],
    AppearCharacters: [],
  });
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(rawJson));
  const sha256 = Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, "0")).join("");
  return {
    eventId: 44,
    episodeNo: "1",
    locale: "zh-CN",
    revision: "episode-import-v1",
    segments: [
      { id: "body-0", kind: "talk", position: 0, japanese: first, sourceHash: "body-0-hash", text: "", source: "unknown", revision: 0 },
      { id: "speaker-0", kind: "talk", position: 1, japanese: "初音ミク_01", sourceHash: "speaker-0-hash", text: "", source: "unknown", revision: 0 },
      { id: "body-1", kind: "talk", position: 2, japanese: second, sourceHash: "body-1-hash", text: "旧译", source: "human", revision: 2 },
      { id: "speaker-1", kind: "talk", position: 3, japanese: "鏡音リン", sourceHash: "speaker-1-hash", text: "镜音铃", source: "human", revision: 1 },
    ],
    scenario: {
      scenarioId: "event-import",
      fileName: "event-import.json",
      sha256,
      parserVersion: 1,
      rawJson,
      sourceTalks: [
        { speaker: "初音ミク", text: first, charIndex: 0, talkDataIndex: 0 },
        { speaker: "场景", text: "教室", charIndex: 0 },
        { speaker: "", text: "", charIndex: 0 },
        { speaker: "鏡音リン", text: second, charIndex: 0, talkDataIndex: 1 },
      ],
    },
  };
}

test("parses SekaiText TXT grammar", () => {
  assert.deepEqual(parseEventTxtContent("#SekaiText v1\r\n初音ミク：你好!\r\n\r\n教室\r\n"), [
    { idx: 1, speaker: "初音ミク", text: "你好!", start: true, end: true, checked: true, save: true, dstidx: 0 },
    { idx: 2, speaker: "", text: "", start: true, end: true, checked: true, save: true, dstidx: 1 },
    { idx: 3, speaker: "场景", text: "教室", start: true, end: true, checked: true, save: true, dstidx: 2 },
  ]);
});

test("only a zh-CN preview converts dialogue punctuation to Chinese", async () => {
  const snapshot = await snapshotFixture();
  const talks = parseEventTxtContent("初音ミク：Test, line?! (a~b)…\n教室\n\n鏡音リン：欸, 测试!");
  const imported = (locale) => {
    const rows = eventEpisodeTxtImportPreview({ ...snapshot, locale }, talks).rows;
    return ["body-0:body", "body-1:body"].map((id) => rows.find((row) => row.id === id)?.imported);
  };
  assert.deepEqual(imported("zh-CN"), ["Test， line？！ （a～b）…", "诶， 测试！"]);
  assert.deepEqual(imported("en-US"), ["Test, line?! (a~b)…", "欸, 测试!"]);
});

test("validates snapshot SHA and previews safe defaults versus explicit conflicts", async () => {
  const snapshot = await snapshotFixture();
  await validateEventEpisodeSnapshot(snapshot);
  const preview = eventEpisodeTxtImportPreview(snapshot, parseEventTxtContent([
    "初音ミク：你好！",
    "教室",
    "",
    "鏡音リン：新译",
  ].join("\n")));
  const byID = new Map(preview.rows.map((row) => [row.id, row]));
  assert.equal(byID.get("body-0:body")?.status, "matched");
  assert.equal(byID.get("body-0:body")?.selectedByDefault, true);
  assert.equal(byID.get("body-1:body")?.status, "conflict");
  assert.equal(byID.get("body-1:body")?.selectedByDefault, false);
  assert.equal(byID.get("speaker-1:speaker")?.selectable, false);

  await assert.rejects(validateEventEpisodeSnapshot({
    ...snapshot,
    scenario: { ...snapshot.scenario, sha256: "0".repeat(64) },
  }), /scenario SHA-256 mismatch/);
});

test("does not guess through repeated ambiguous structures or inserted rows", async () => {
  const snapshot = await snapshotFixture();
  const preview = eventEpisodeTxtImportPreview(snapshot, parseEventTxtContent([
    "初音ミク：你好！",
    "额外场景",
    "教室",
    "",
    "鏡音リン：新译",
  ].join("\n")));
  assert.ok(preview.rows.some((row) => row.status === "unmatched" || row.status === "conflict"));
  assert.ok(preview.rows.filter((row) => row.target === "structure").every((row) => !row.selectable));
});

test("a TXT line still in Japanese is refused before punctuation conversion or trimming", async () => {
  const snapshot = await snapshotFixture(["……うん！　そうだよね！", "テスト台詞です!\u3000"]);
  const talks = parseEventTxtContent("初音ミク：……うん！　そうだよね！\n教室\n\n鏡音リン：テスト台詞です!\u3000");
  for (const locale of ["zh-CN", "en-US"]) {
    const rows = new Map(eventEpisodeTxtImportPreview({ ...snapshot, locale }, talks).rows.map((row) => [row.id, row]));
    assert.deepEqual([rows.get("body-0:body")?.status, rows.get("body-0:body")?.selectable], ["missing", false], locale);
    assert.deepEqual([rows.get("body-1:body")?.status, rows.get("body-1:body")?.selectable], ["missing", false], locale);
  }
});

test("stored text equal to the trimmed Japanese still counts as Japanese, so the TXT translation is selected", async () => {
  const snapshot = await snapshotFixture(["テスト台詞です。\u3000", "二行目"]);
  snapshot.segments[0] = { ...snapshot.segments[0], text: "テスト台詞です。", source: "human", revision: 1 };
  const row = eventEpisodeTxtImportPreview(snapshot, parseEventTxtContent("初音ミク：测试台词。\n教室\n\n鏡音リン：新译"))
    .rows.find((candidate) => candidate.id === "body-0:body");
  assert.deepEqual([row?.status, row?.selectable, row?.selectedByDefault], ["matched", true, true]);
});

// A zh-CN event save of text equal to the Japanese often finds no legacy line row and fails with 404.
test("a zh-CN line that is only 「……」 like the Japanese stays untranslated", async () => {
  const snapshot = await snapshotFixture(["……", "二行目"]);
  const row = eventEpisodeTxtImportPreview(snapshot, parseEventTxtContent("初音ミク：……\n教室\n\n鏡音リン：新译"))
    .rows.find((candidate) => candidate.id === "body-0:body");
  assert.deepEqual([row?.status, row?.selectable, row?.imported], ["missing", false, "……"]);
});

test("a body whose line count differs from the Japanese is not selected by default", async () => {
  const snapshot = await snapshotFixture(["一行目\n続き", "二行目"]);
  const row = eventEpisodeTxtImportPreview(snapshot, parseEventTxtContent("初音ミク：第一行\\N第\\N二行\n教室\n\n鏡音リン：新译"))
    .rows.find((candidate) => candidate.id === "body-0:body");
  assert.deepEqual([row?.status, row?.selectable, row?.selectedByDefault], ["matched", true, false]);
  assert.match(row?.reason ?? "", /换行数与日文不一致（日文 1 处，TXT 2 处）/);
});
