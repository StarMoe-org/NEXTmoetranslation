import assert from "node:assert/strict";
import test from "node:test";

import { createHookRuntime, loadSourceModule } from "./source-module-harness.mjs";

const segmentation = await import("../src/lib/lyrics-segmentation.mjs");
const versioning = await import("../src/lib/lyrics-versioning.mjs");
const colors = await import("../src/lib/performer-colors.mjs");

const runtime = createHookRuntime();
const yjsStub = { canonicalLyricsJSON: (value) => JSON.stringify(value) };
const commandsModule = await loadSourceModule("components/lyrics/lyricsDocumentCommands.ts", {
  "@/lib/performer-colors.mjs": colors,
});
const model = await loadSourceModule("components/lyrics/lyricsDocumentModel.ts", {
  "@/lib/lyrics-segmentation.mjs": segmentation,
  "@/lib/lyrics-versioning.mjs": versioning,
});
const editorState = await loadSourceModule("components/lyrics/lyricsEditorState.ts", {
  react: runtime.react,
  "@/components/lyrics/lyricsDocumentModel": model,
  "@/lib/lyrics-versioning.mjs": versioning,
  "@/lib/yjs-lyrics": yjsStub,
});
const commandHooks = await loadSourceModule("components/lyrics/useLyricsDocumentCommands.ts", {
  "@/app/providers": { useToast: () => ({ show: () => {} }) },
  "@/components/lyrics/lyricsDocumentModel": model,
  "@/components/lyrics/lyricsDocumentCommands": commandsModule,
  "@/lib/lyrics-segmentation.mjs": segmentation,
  "@/lib/lyrics-versioning.mjs": versioning,
  "@/lib/yjs-lyrics": yjsStub,
});

function manualLine(id, japanese) {
  return {
    id, order: 0, japanese, "zh-CN": "", "en-US": "",
    segments: [{ text: japanese, performerIds: [], ruby: japanese ? [{ text: japanese }] : [] }],
  };
}

function legacyLyrics(revision, lines = [manualLine("line-1", "あいう")]) {
  return {
    musicId: 7,
    status: revision === 0 ? "draft" : "published",
    revision,
    ...(revision > 0 ? { publishedRevision: revision } : {}),
    updatedAt: "2026-09-01T00:00:00Z",
    attribution: "",
    translationCredit: "",
    proofreadingCredit: "",
    sourceUrl: "",
    lines: lines.map((line, order) => ({ ...line, order })),
  };
}

function renditionLyrics() {
  const line = {
    id: "sekai-line-1", order: 0, japanese: "かきく", "zh-CN": "", "en-US": "",
    segments: [{ text: "かきく", performerIds: ["ichika"], ruby: [{ text: "かきく" }] }],
    trailingPerformerIds: [],
  };
  return {
    musicId: 9,
    status: "draft",
    revision: 4,
    updatedAt: "2026-09-01T00:00:00Z",
    translationEditionKey: "ed-default",
    defaultTranslationEditionKey: "ed-default",
    translationEditions: [{ key: "ed-default", label: "默认译本" }],
    renditions: [{
      key: "sekai",
      kind: "sekai",
      label: "SEKAI",
      availableVersions: ["full"],
      performers: [{ performerId: "ichika", name: "星乃一歌" }],
      full: { version: { kind: "sekai", label: "Leo/need" }, lines: [line] },
      relation: { kind: "none" },
      sourceTabPaths: [["SEKAI"]],
      provenance: [],
    }],
  };
}

function editor(lyrics, activeRenditionKey = "") {
  const writes = [];
  const state = {
    lyrics,
    setLyrics: (next) => writes.push(next),
    setError: () => {},
    performers: [],
    busyRef: { current: false },
    writeLocked: false,
    localSourceImportDraft: false,
    collaborationRef: { current: null },
    collaborationDocumentJSONRef: { current: "" },
    documentGenerationRef: { current: 0 },
    pendingAnnotationOperation: null,
    setPendingAnnotationOperation: () => {},
    activeTranslationEditionKey: "",
    activeRenditionKey,
    activeVersion: "full",
    segmentInputRefs: { current: {} },
    linesContainerRef: { current: null },
  };
  const target = editorState.useLyricsActiveTarget(state);
  return { target, commands: commandHooks.useLyricsDocumentCommands(state, target), writes };
}

test("lineWithEditablePatch carries Japanese source text through", () => {
  const line = manualLine("line-1", "あいう");
  const updated = commandsModule.lineWithEditablePatch(line, { japanese: "あいうえ" });
  assert.equal(updated.japanese, "あいうえ");
  assert.equal(line.japanese, "あいう", "the input line is not mutated");
});

test("Japanese typed into a revision-0 manual draft reaches line.japanese", () => {
  const { commands, writes } = editor(legacyLyrics(0));
  commands.updateSegment(0, 0, "あいうえ");
  assert.equal(writes.length, 1);
  assert.equal(writes[0].lines[0].segments[0].text, "あいうえ");
  assert.equal(writes[0].lines[0].japanese, "あいうえ");
});

test("saved legacy lyrics keep line structure and Japanese editable", () => {
  const saved = legacyLyrics(3, [manualLine("line-1", "あいう"), manualLine("line-2", "かきく")]);
  assert.equal(editor(saved).target.activeSideSourceMutable, true);

  const typed = editor(saved);
  typed.commands.updateSegment(1, 0, "かきくけ");
  assert.equal(typed.writes.at(-1).lines[1].japanese, "かきくけ");

  const added = editor(saved);
  added.commands.addLine();
  assert.deepEqual(added.writes.at(-1).lines.map((line) => line.order), [0, 1, 2]);

  const moved = editor(saved);
  moved.commands.moveLine(0, 1);
  assert.deepEqual(moved.writes.at(-1).lines.map((line) => line.id), ["line-2", "line-1"]);

  const originalWindow = globalThis.window;
  globalThis.window = { requestAnimationFrame: () => 0 };
  try {
    const removed = editor(saved);
    removed.commands.removeLine(0);
    assert.deepEqual(removed.writes.at(-1).lines.map((line) => line.id), ["line-2"]);

    const split = legacyLyrics(2, [{
      ...manualLine("line-1", "あいかき"),
      segments: [
        { text: "あい", performerIds: [], ruby: [{ text: "あい" }] },
        { text: "かき", performerIds: [], ruby: [{ text: "かき" }] },
      ],
    }]);
    const segmentMoved = editor(split);
    segmentMoved.commands.moveSegment(0, 0, 1);
    assert.equal(segmentMoved.writes.at(-1).lines[0].japanese, "かきあい");
  } finally {
    globalThis.window = originalWindow;
  }
});

test("source-v3 renditions and exact-projection Game sides stay source-read-only", () => {
  const rendition = editor(renditionLyrics(), "sekai");
  assert.equal(rendition.target.activeSideSourceMutable, false);
  rendition.commands.addLine();
  rendition.commands.moveLine(0, 1);
  rendition.commands.moveSegment(0, 0, 1);
  assert.equal(rendition.writes.length, 0);

  const projected = legacyLyrics(3);
  projected.availableVersions = ["full", "game"];
  projected.gameProjection = { reasonCode: "tagged_full_and_game", lineIds: ["line-1"] };
  const state = editor(projected);
  assert.equal(state.target.activeSideSourceMutable, true);
  const game = editorState.useLyricsActiveTarget({ lyrics: projected, performers: [], activeTranslationEditionKey: "", activeRenditionKey: "", activeVersion: "game" });
  assert.equal(game.activeSideSourceMutable, false);
});

test("exact-projection Game rows follow a Full translation edit through relation.lineIds", () => {
  const fullLine = (id, order, japanese) => ({
    id, order, japanese, "zh-CN": `${id} 译文`,
    segments: [{ text: japanese, performerIds: ["ichika"], ruby: [{ text: japanese }] }],
    trailingPerformerIds: [],
  });
  const document = renditionLyrics();
  const rendition = document.renditions[0];
  rendition.availableVersions = ["full", "game"];
  rendition.full.lines = [fullLine("full-000001", 0, "かきく"), fullLine("full-000002", 1, "さしす"), fullLine("full-000003", 2, "たちつ")];
  // The Game cut keeps rows 1 and 3 under its own row IDs, as served sources do.
  rendition.game = {
    version: rendition.full.version,
    lines: [
      { ...fullLine("game-000001", 0, "かきく"), "zh-CN": "full-000001 译文" },
      { ...fullLine("game-000002", 1, "たちつ"), "zh-CN": "full-000003 译文" },
    ],
  };
  rendition.relation = { kind: "exact_projection", fullRenditionKey: "sekai", lineIds: ["full-000001", "full-000003"] };

  const { commands, writes } = editor(document, "sekai");
  commands.updateLine(2, { "zh-CN": "第三行新译文" });
  const game = writes.at(-1).renditions[0].game.lines;
  assert.deepEqual(game.map((line) => [line.id, line["zh-CN"]]), [
    ["game-000001", "full-000001 译文"],
    ["game-000002", "第三行新译文"],
  ]);
  const projection = versioning.projectGameLyricsLines(writes.at(-1), "sekai");
  assert.deepEqual(projection.lines.map((line) => line["zh-CN"]), game.map((line) => line["zh-CN"]),
    "the stored Game rows agree with the projection the server checks");
});

test("picking a performer new to a rendition colours it from the side's version label", () => {
  const document = renditionLyrics();
  const rendition = document.renditions[0];
  const lines = [{ ...rendition.full.lines[0], segments: [{ ...rendition.full.lines[0].segments[0], performerIds: ["chorus"] }] }];
  const patch = commandsModule.renditionSideLinesPatch(rendition, rendition.full, "full", lines, []);
  assert.deepEqual(patch.performers.at(-1), {
    performerId: "chorus",
    name: "chorus",
    color: colors.unitRepresentativeColor("Leo/need"),
  });
  assert.equal(patch.performers.at(-1).color, "#4455DD");

  const { commands, writes } = editor(document, "sekai");
  commands.updateSegment(0, 0, "かきく", ["ichika", "saki"]);
  assert.deepEqual(writes.at(-1).renditions[0].performers.map((performer) => [performer.performerId, performer.color]), [
    ["ichika", undefined],
    ["saki", colors.OFFICIAL_PERFORMER_COLORS.saki],
  ]);
});
