import assert from "node:assert/strict";
import test from "node:test";
import { webcrypto } from "node:crypto";

import {
  eventEpisodeTxtImportPreview,
  parseEventTxtContent,
  validateEventEpisodeSnapshot,
} from "../src/lib/event-txt-import.mjs";

if (!globalThis.crypto) globalThis.crypto = webcrypto;

async function snapshotFixture() {
  const rawJson = JSON.stringify({
    ScenarioId: "event-import",
    Snippets: [
      { Action: 1, ReferenceIndex: 0 },
      { Action: 6, ReferenceIndex: 0 },
      { Action: 1, ReferenceIndex: 1 },
    ],
    TalkData: [
      { WindowDisplayName: "初音ミク_01", Body: "一行目", Voices: [], WhenFinishCloseWindow: 0 },
      { WindowDisplayName: "鏡音リン", Body: "二行目", Voices: [], WhenFinishCloseWindow: 0 },
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
      { id: "body-0", kind: "talk", position: 0, japanese: "一行目", sourceHash: "body-0-hash", text: "", source: "unknown", revision: 0 },
      { id: "speaker-0", kind: "talk", position: 1, japanese: "初音ミク_01", sourceHash: "speaker-0-hash", text: "", source: "unknown", revision: 0 },
      { id: "body-1", kind: "talk", position: 2, japanese: "二行目", sourceHash: "body-1-hash", text: "旧译", source: "human", revision: 2 },
      { id: "speaker-1", kind: "talk", position: 3, japanese: "鏡音リン", sourceHash: "speaker-1-hash", text: "镜音铃", source: "human", revision: 1 },
    ],
    scenario: {
      scenarioId: "event-import",
      fileName: "event-import.json",
      sha256,
      parserVersion: 1,
      rawJson,
      sourceTalks: [
        { speaker: "初音ミク", text: "一行目", charIndex: 0, talkDataIndex: 0 },
        { speaker: "场景", text: "教室", charIndex: 0 },
        { speaker: "", text: "", charIndex: 0 },
        { speaker: "鏡音リン", text: "二行目", charIndex: 0, talkDataIndex: 1 },
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
  assert.deepEqual(imported("zh-CN"), ["Test， line？！ （a～b）...", "诶， 测试！"]);
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
