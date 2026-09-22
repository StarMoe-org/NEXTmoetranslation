import assert from "node:assert/strict";
import test from "node:test";

import {
  lyricsHasPerformerSegmentation,
  lyricsRenditionByKey,
  lyricsRenditionKeys,
  lyricsVersionSaveProblems,
  normalizedLyricsVersions,
  projectGameLyricsLines,
  retainedLyricsTranslationTarget,
  referencedGameFullLineIds,
  removedReferencedFullLineIds,
  renditionProjectionStatus,
  resolvedLyricsComponentProvenance,
} from "../src/lib/lyrics-versioning.mjs";

function document() {
  return {
    availableVersions: ["full", "game"],
    lines: [
      { id: "full-1", japanese: "一" },
      { id: "full-2", japanese: "二" },
      { id: "full-3", japanese: "三" },
    ],
    gameProjection: { reasonCode: "tagged_full_and_game", lineIds: ["full-1", "full-3"] },
  };
}

function renditionLine(id, japanese, translation, performerId, reading) {
  return {
    id,
    order: 0,
    japanese,
    "zh-CN": translation,
    "en-US": `${translation}-en`,
    segments: [{ text: japanese, performerIds: performerId ? [performerId] : [], ruby: [{ text: japanese, ...(reading ? { reading } : {}) }] }],
    trailingPerformerIds: [],
  };
}

function attribution(key, component) {
  return {
    component: `renditions/${key}/${component}`,
    provider: "sekaipedia",
    title: `${key}-${component}`,
    revisionId: 1,
    revisionUrl: "https://example.invalid/revision/1",
    licenseName: "CC BY-SA 4.0",
    licenseUrl: "https://creativecommons.org/licenses/by-sa/4.0/",
  };
}

function remDocument() {
  const full = renditionLine("sekai-line-1", "歌", "sekai-full", "sekai-a", "うた");
  const game = renditionLine("game-sekai-line-1", "歌", "sekai-game", "sekai-b", "ウタ");
  const gameOnly = renditionLine("virtual-line-1", "歌", "virtual-game", "virtual-a", "うた");
  return {
    musicId: 765,
    status: "draft",
    revision: 1,
    updatedAt: "2026-08-07T00:00:00Z",
    renditions: [
      {
        key: "sekai",
        kind: "sekai",
        label: "SEKAI",
        availableVersions: ["full", "game"],
        performers: [
          { performerId: "sekai-a", name: "SEKAI A" },
          { performerId: "sekai-b", name: "SEKAI B" },
        ],
        full: { version: { kind: "sekai", label: "SEKAI" }, lines: [full] },
        game: { version: { kind: "sekai", label: "SEKAI" }, lines: [game] },
        relation: { kind: "exact_projection", fullRenditionKey: "sekai", lineIds: ["sekai-line-1"] },
        sourceTabPaths: [["SEKAI"]],
        provenance: [attribution("sekai", "full_text"), attribution("sekai", "game_text"), attribution("sekai", "relation")],
        translationCredits: { translation: "sekai-translator", proofreading: "sekai-proofreader" },
      },
      {
        key: "virtual-singer",
        kind: "vocaloid",
        label: "VIRTUAL SINGER",
        availableVersions: ["game"],
        performers: [{ performerId: "virtual-a", name: "VIRTUAL A" }],
        game: { version: { kind: "vocaloid", label: "VIRTUAL SINGER" }, lines: [gameOnly] },
        relation: { kind: "none" },
        sourceTabPaths: [["VIRTUAL SINGER"]],
        provenance: [attribution("virtual-singer", "game_text"), attribution("virtual-singer", "relation")],
        translationCredits: { translation: "virtual-translator", proofreading: "virtual-proofreader" },
      },
    ],
  };
}

test("v1 and absent version metadata stay Full-only", () => {
  assert.deepEqual(normalizedLyricsVersions({ lines: [] }), ["full"]);
  assert.deepEqual(projectGameLyricsLines({ lines: [] }), { ok: true, lines: [], lineIds: [], errors: [] });
  assert.deepEqual(lyricsVersionSaveProblems({ availableVersions: ["full"], lines: [] }), []);
});

test("Game is a read-only ordered line-ID projection over the same Full line objects", () => {
  const source = document();
  const projected = projectGameLyricsLines(source);

  assert.equal(projected.ok, true);
  assert.deepEqual(projected.lineIds, ["full-1", "full-3"]);
  assert.deepEqual(projected.lines.map((line) => line.japanese), ["一", "三"]);
  assert.equal(projected.lines[0], source.lines[0]);
  assert.equal(projected.lines[1], source.lines[2]);
});

test("Game projection fails closed when authoritative Full IDs are duplicated", () => {
  const source = document();
  source.lines[1].id = "full-1";
  const projected = projectGameLyricsLines(source);

  assert.equal(projected.ok, false);
  assert.deepEqual(projected.lines, []);
  assert.ok(projected.errors.some((problem) => problem.includes("重复行 ID full-1")));
  assert.ok(lyricsVersionSaveProblems(source).some((problem) => problem.includes("重复行 ID full-1")));
});

test("save validation fails closed for missing, duplicate, out-of-order, or unpublishable Game references", () => {
  const mutations = [
    (value) => { value.gameProjection.lineIds = ["full-1", "missing"]; },
    (value) => { value.gameProjection.lineIds = ["full-1", "full-1"]; },
    (value) => { value.gameProjection.lineIds = ["full-3", "full-1"]; },
    (value) => { value.gameProjection.reasonCode = "untagged_game_subset"; },
    (value) => { value.availableVersions = ["full"]; },
  ];
  for (const mutate of mutations) {
    const candidate = document();
    mutate(candidate);
    assert.notEqual(lyricsVersionSaveProblems(candidate).length, 0);
  }
});

test("deleting a referenced Full line is reported before save", () => {
  const candidate = document();
  candidate.lines = candidate.lines.filter((line) => line.id !== "full-3");

  assert.deepEqual(removedReferencedFullLineIds(candidate), ["full-3"]);
  assert.ok(lyricsVersionSaveProblems(candidate).some((problem) => problem.includes("full-3")));
});

test("untagged uncut identity must project every Full line exactly", () => {
  const candidate = document();
  candidate.gameProjection = {
    reasonCode: "untagged_uncut_identity",
    lineIds: candidate.lines.map((line) => line.id),
  };
  assert.deepEqual(lyricsVersionSaveProblems(candidate), []);

  candidate.gameProjection.lineIds = ["full-1", "full-3"];
  assert.ok(lyricsVersionSaveProblems(candidate).some((problem) => problem.includes("全部 Full 行")));
});

test("provenance-free VOCALOID-only lines infer no performer segmentation from their actual shape", () => {
  const vocaloidOnly = {
    reasonCode: "untagged_full_only",
    lines: [
      { japanese: "歌う", segments: [{ text: "歌う", performerIds: [] }], trailingPerformerIds: [] },
      { japanese: "未来へ", segments: [{ text: "未来へ", performerIds: [] }], trailingPerformerIds: [] },
    ],
  };

  assert.equal(lyricsHasPerformerSegmentation(vocaloidOnly), false);
  assert.equal(resolvedLyricsComponentProvenance(vocaloidOnly).some((row) => row.component === "performerSegmentation"), false);
});

test("provenance-free actual performer IDs or meaningful segmentation enable performer editing", () => {
  const line = { japanese: "歌う", segments: [{ text: "歌う", performerIds: [] }], trailingPerformerIds: [] };
  assert.equal(lyricsHasPerformerSegmentation({ lines: [{ ...line, segments: [{ text: "歌う", performerIds: [21] }] }] }), true);
  assert.equal(lyricsHasPerformerSegmentation({ lines: [{ ...line, segments: [
    { text: "歌", performerIds: [] }, { text: "う", performerIds: [] },
  ] }] }), true);
  assert.equal(lyricsHasPerformerSegmentation({ lines: [{ ...line, trailingPerformerIds: [21] }] }), true);
  assert.equal(lyricsHasPerformerSegmentation({ lines: [] }), true, "an empty draft cannot prove the VOCALOID-only shape");
});

test("performer provenance is positive-only and concrete segmentation always fails closed", () => {
  const exactVocaloidLine = { japanese: "歌う", segments: [{ text: "歌う", performerIds: [] }], trailingPerformerIds: [] };
  assert.equal(lyricsHasPerformerSegmentation({
    provenance: { performerSegmentation: { renditionKey: "sekai" } }, lines: [exactVocaloidLine],
  }), true);
  assert.equal(lyricsHasPerformerSegmentation({
    provenance: { fullText: { renditionKey: "vocaloid" } }, lines: [exactVocaloidLine],
  }), false);

  const contradictory = [
    { provenance: { fullText: { renditionKey: "vocaloid" } }, performers: [{ performerId: "miku" }], lines: [exactVocaloidLine] },
    { provenance: { fullText: { renditionKey: "vocaloid" } }, lines: [{ ...exactVocaloidLine, segments: [{ text: "歌う", performerIds: [21] }] }] },
    { provenance: { fullText: { renditionKey: "vocaloid" } }, lines: [{ ...exactVocaloidLine, trailingPerformerIds: [21] }] },
    { provenance: { fullText: { renditionKey: "vocaloid" } }, lines: [{ ...exactVocaloidLine, segments: [
      { text: "歌", performerIds: [] }, { text: "う", performerIds: [] },
    ] }] },
    { provenance: { fullText: { renditionKey: "vocaloid" } }, lines: [{ ...exactVocaloidLine, segments: [{ text: "別の歌詞", performerIds: [] }] }] },
  ];
  for (const candidate of contradictory) assert.equal(lyricsHasPerformerSegmentation(candidate), true);
});

test("source-review analysis uses performer metadata and extracted concrete shape", () => {
  const exactVocaloidLine = { japanese: "歌う", segments: [{ text: "歌う", performerIds: [] }], trailingPerformerIds: [] };
  assert.equal(lyricsHasPerformerSegmentation({ performers: [], extractedLines: [exactVocaloidLine] }), false);
  assert.equal(lyricsHasPerformerSegmentation({ performers: [{ performerId: "miku" }], extractedLines: [exactVocaloidLine] }), true);
  assert.equal(lyricsHasPerformerSegmentation({
    performers: [], extractedLines: [{ ...exactVocaloidLine, segments: [{ text: "歌", performerIds: [] }, { text: "う", performerIds: [] }] }],
  }), true);
});

test("REM keeps two equal-text families independent by stable key", () => {
  const candidate = remDocument();

  assert.deepEqual(lyricsRenditionKeys(candidate), ["sekai", "virtual-singer"]);
  assert.equal(lyricsRenditionByKey(candidate, "sekai").translationCredits.translation, "sekai-translator");
  assert.equal(lyricsRenditionByKey(candidate, "virtual-singer").translationCredits.translation, "virtual-translator");
  assert.notEqual(lyricsRenditionByKey(candidate, "sekai").game.lines[0], lyricsRenditionByKey(candidate, "virtual-singer").game.lines[0]);
  assert.equal(projectGameLyricsLines(candidate).ok, false, "multi-family REM requires an explicit stable key");
  assert.deepEqual(lyricsVersionSaveProblems(candidate), []);
});

test("manual REM ruby spans may leave Han unannotated while present readings stay kana over Han", () => {
  const candidate = remDocument();
  for (const side of ["full", "game"]) candidate.renditions[0][side].lines[0].segments[0].ruby = [{ text: "歌" }];
  candidate.renditions[1].game.lines[0].segments[0].ruby = [{ text: "歌" }];

  assert.deepEqual(lyricsVersionSaveProblems(candidate), []);

  candidate.renditions[0].full.lines[0].segments[0].ruby = [{ text: "歌", reading: "uta" }];
  assert.ok(lyricsVersionSaveProblems(candidate).some((problem) => problem.includes("假名注音")));
});

test("REM authoritative reload retains a still-valid stable rendition side and falls back deterministically", () => {
  const candidate = remDocument();

  assert.deepEqual(retainedLyricsTranslationTarget(candidate, "sekai", "game"), {
    renditionKey: "sekai", version: "game",
  });
  assert.deepEqual(retainedLyricsTranslationTarget(candidate, "virtual-singer", "full"), {
    renditionKey: "virtual-singer", version: "game",
  });
  assert.deepEqual(retainedLyricsTranslationTarget(candidate, "removed-family", "game"), {
    renditionKey: "sekai", version: "game",
  });
  assert.deepEqual(retainedLyricsTranslationTarget({ lines: [] }, "sekai", "game"), {
    renditionKey: "", version: "full",
  });
});

test("Full | Game exact projection preserves Game source facts while previewing current Full translations", () => {
  const candidate = remDocument();
  const projected = projectGameLyricsLines(candidate, "sekai");
  const rendition = lyricsRenditionByKey(candidate, "sekai");

  assert.equal(renditionProjectionStatus(candidate, "sekai"), "exact_projection");
  assert.deepEqual(referencedGameFullLineIds(candidate, "sekai"), ["sekai-line-1"]);
  assert.equal(projected.ok, true);
  assert.notEqual(projected.lines[0], rendition.game.lines[0]);
  assert.notEqual(projected.lines[0], rendition.full.lines[0]);
  assert.equal(projected.lines[0]["zh-CN"], "sekai-full");
  assert.equal(projected.lines[0]["en-US"], "sekai-full-en");
  assert.equal(rendition.game.lines[0]["zh-CN"], "sekai-game", "authoritative Game source facts are not mutated");
  assert.deepEqual(projected.lines[0].segments[0].performerIds, ["sekai-b"]);
  assert.equal(projected.lines[0].id, "game-sekai-line-1", "Game keeps its own stable line ID");
  assert.equal(projected.lines[0].segments[0].ruby[0].reading, "ウタ");

  candidate.renditions[0].full.lines[0]["zh-CN"] = "未保存的 Full 简中";
  candidate.renditions[0].full.lines[0]["en-US"] = "Unsaved Full English";
  const liveProjected = projectGameLyricsLines(candidate, "sekai");
  assert.equal(liveProjected.lines[0]["zh-CN"], "未保存的 Full 简中");
  assert.equal(liveProjected.lines[0]["en-US"], "Unsaved Full English");
  assert.deepEqual(liveProjected.lines[0].segments, rendition.game.lines[0].segments);

  const crossFamily = structuredClone(candidate);
  crossFamily.renditions[0].relation.fullRenditionKey = "virtual-singer";
  assert.equal(renditionProjectionStatus(crossFamily, "sekai"), "invalid");
  assert.ok(lyricsVersionSaveProblems(crossFamily).some((problem) => problem.includes("同一 stable key")));

  const changedGameText = structuredClone(candidate);
  changedGameText.renditions[0].game.lines[0].japanese = "別";
  changedGameText.renditions[0].game.lines[0].segments[0].text = "別";
  changedGameText.renditions[0].game.lines[0].segments[0].ruby = [{ text: "別", reading: "べつ" }];
  assert.ok(lyricsVersionSaveProblems(changedGameText).some((problem) => problem.includes("对应 Full 原文")));
});

test("Game-only rendition stays editable and never fabricates a Full side", () => {
  const candidate = remDocument();
  const rendition = lyricsRenditionByKey(candidate, "virtual-singer");
  const projected = projectGameLyricsLines(candidate, "virtual-singer");

  assert.deepEqual(normalizedLyricsVersions(candidate, "virtual-singer"), ["game"]);
  assert.equal(renditionProjectionStatus(candidate, "virtual-singer"), "game_only");
  assert.equal(Object.hasOwn(rendition, "full"), false);
  assert.deepEqual(referencedGameFullLineIds(candidate, "virtual-singer"), []);
  assert.equal(projected.ok, true);
  assert.equal(projected.lines[0]["zh-CN"], "virtual-game");
  assert.deepEqual(projected.lines[0].segments[0].performerIds, ["virtual-a"]);
  assert.equal(lyricsHasPerformerSegmentation(candidate, "virtual-singer", "game"), true);
});

test("component provenance resolves rendition refs without projecting private facts into public data", () => {
  const candidate = {
    provenance: {
      fullText: { renditionKey: "full-source" },
      ruby: { renditionKey: "ruby-source" },
      versionEvidence: { renditionKey: "full-source" },
    },
    fixedIdentities: [
      { renditionKey: "full-source", provider: "vocaloid_fandom", revisionId: 34 },
      { renditionKey: "ruby-source", provider: "moegirl", revisionId: 78 },
    ],
  };
  const rows = resolvedLyricsComponentProvenance(candidate);

  assert.deepEqual(rows.map((row) => [row.component, row.renditionKey, row.identity?.provider]), [
    ["fullText", "full-source", "vocaloid_fandom"],
    ["ruby", "ruby-source", "moegirl"],
    ["versionEvidence", "full-source", "vocaloid_fandom"],
  ]);
});
