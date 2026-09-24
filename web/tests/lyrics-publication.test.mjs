import assert from "node:assert/strict";
import { createRequire } from "node:module";
import test from "node:test";

import { createHookRuntime, loadSourceModule } from "./source-module-harness.mjs";

const require = createRequire(import.meta.url);
const { createElement } = require("react");
const { renderToStaticMarkup } = require("react-dom/server");

const esm = {
  "@/lib/lyrics-editions.mjs": await import("../src/lib/lyrics-editions.mjs"),
  "@/lib/lyrics-recovery.mjs": await import("../src/lib/lyrics-recovery.mjs"),
  "@/lib/lyrics-segmentation.mjs": await import("../src/lib/lyrics-segmentation.mjs"),
  "@/lib/lyrics-versioning.mjs": await import("../src/lib/lyrics-versioning.mjs"),
};
const yjs = loadSourceModule("lib/yjs-lyrics.ts", { "y-websocket": { WebsocketProvider: class {} } });
const model = loadSourceModule("components/lyrics/lyricsDocumentModel.ts", esm);

class APIError extends Error {
  constructor(status, body = {}) {
    super(body.error || "api_error");
    this.status = status;
    this.code = body.error;
    this.details = body.details || [];
    this.current = body.current;
  }
}

function legacyLyrics(revision, translation = "阿伊乌") {
  return {
    musicId: 7,
    status: "draft",
    revision,
    updatedAt: `2026-09-0${revision}T00:00:00Z`,
    attribution: "",
    translationCredit: "译者",
    proofreadingCredit: "",
    sourceUrl: "",
    lines: [{
      id: "line-1", order: 0, japanese: "あいう", "zh-CN": translation, "en-US": "",
      segments: [{ text: "あいう", performerIds: [], ruby: [{ text: "あいう" }] }],
    }],
  };
}

function renditionLyrics(revision) {
  return {
    musicId: 7,
    status: "draft",
    revision,
    updatedAt: `2026-09-0${revision}T00:00:00Z`,
    translationEditionKey: "ed-default",
    defaultTranslationEditionKey: "ed-default",
    translationEditions: [{ key: "ed-default", label: "默认译本" }],
    renditions: [{
      key: "sekai", kind: "sekai", label: "SEKAI", availableVersions: ["full"], performers: [],
      full: {
        version: { kind: "sekai", label: "SEKAI" },
        lines: [{
          id: "sekai-line-1", order: 0, japanese: "かきく", "zh-CN": "卡基库", "en-US": "",
          segments: [{ text: "かきく", performerIds: [], ruby: [{ text: "かきく" }] }],
          trailingPerformerIds: [],
        }],
      },
      relation: { kind: "none" },
      sourceTabPaths: [["SEKAI"]],
      provenance: [],
      translationCredits: { translation: "译者" },
    }],
  };
}

// Drives useLyricsPersistence against a stand-in collaboration whose shared document the
// test controls, recording every server call in order.
function openPersistence(t, { stored, shared, pendingTransition = null, sourceV3PublicState = null, api = {} }) {
  const originalWindow = globalThis.window;
  globalThis.window = { setTimeout: (callback) => setTimeout(callback, 0) };
  t.after(() => { globalThis.window = originalWindow; });
  const calls = [];
  const toasts = [];
  let sharedDocument = structuredClone(shared);
  let generation = 1;
  const runtime = createHookRuntime();
  const modules = {
    ...esm,
    react: runtime.react,
    "@/app/providers": { useToast: () => ({ show: (message, kind) => toasts.push({ message, kind }) }) },
    "@/lib/api": {
      APIError,
      checkpointLyrics: async (musicId) => {
        calls.push(["checkpoint", musicId]);
        return api.checkpoint ? api.checkpoint() : { ...structuredClone(sharedDocument), revision: stored.revision + 1 };
      },
      getLyrics: async () => structuredClone(stored),
      getProjectionStatus: async () => {
        calls.push(["projection-status"]);
        api.beforePublication?.();
        return { generation: generation++, pending: false };
      },
      mutateLyricsTranslationEdition: async () => { throw new Error("unused"); },
      publishLyrics: async (musicId, revision) => {
        calls.push(["publish", musicId, revision]);
        return { ...structuredClone(sharedDocument), status: "published", publishedRevision: revision };
      },
      saveLyrics: async () => { throw new Error("unused"); },
      unpublishLyrics: async (musicId, revision) => {
        calls.push(["unpublish", musicId, revision]);
        return structuredClone(sharedDocument);
      },
    },
    "@/lib/yjs-lyrics": yjs,
  };
  const cache = new Map();
  const { useLyricsEditorState } = loadSourceModule("components/lyrics/lyricsEditorState.ts", modules, cache);
  const { useLyricsPersistence } = loadSourceModule("components/lyrics/useLyricsPersistence.ts", modules, cache);
  const initial = useLyricsEditorState(false);
  const recorded = { errors: [], transitions: [] };
  const baseline = yjs.canonicalLyricsJSON(stored);
  const state = {
    ...initial,
    lyrics: structuredClone(shared),
    baseline,
    saveable: yjs.lyricsDocumentSaveable(shared, baseline),
    sourceV3PublicState,
    pendingTransition,
    selectedMusic: { musicId: stored.musicId, title: { "ja-JP": "テスト曲" } },
    setError: (value) => recorded.errors.push(value),
    setPendingTransition: (value) => recorded.transitions.push(value),
  };
  state.lyricsRef.current = state.lyrics;
  state.baselineRef.current = baseline;
  state.collaborationRef.current = {
    beginCheckpoint: () => null,
    checkpointCommitted() {},
    updateAuthoritativeEnvelope(document) {
      sharedDocument = { ...sharedDocument, revision: document.revision, updatedAt: document.updatedAt, status: document.status };
    },
    getSnapshot: () => ({ document: structuredClone(sharedDocument) }),
    undoManager: { canUndo: () => false },
    discardLocalChanges() {},
  };
  const loader = {
    loadCatalog: async () => {}, loadPerformers: async () => {}, performChooseMusic: async () => true,
    acceptAuthoritativeDocument() {}, startCollaboration() {}, requestIsCurrent: () => true, replaceEditionURL() {},
  };
  return {
    persistence: useLyricsPersistence(state, loader),
    calls, toasts, recorded,
    collaboratorEdits: (text) => {
      sharedDocument = structuredClone(sharedDocument);
      sharedDocument.lines[0]["zh-CN"] = text;
    },
  };
}

const publishTransition = { kind: "publish", nextPublished: true };

test("publishing a legacy song with savable shared edits saves first and publishes the saved revision", async (t) => {
  const stored = legacyLyrics(2);
  const editor = openPersistence(t, { stored, shared: legacyLyrics(2, "协作者留下的修改"), pendingTransition: publishTransition });

  await editor.persistence.continuePendingTransition(false);

  assert.deepEqual(editor.calls, [["checkpoint", 7], ["projection-status"], ["publish", 7, 3]]);
  assert.equal(editor.recorded.transitions.at(-1), null, "the dialog closes once the publication is submitted");
});

test("publishing without savable changes submits the current revision without saving", async (t) => {
  const stored = legacyLyrics(2);
  const editor = openPersistence(t, { stored, shared: stored, pendingTransition: publishTransition });

  await editor.persistence.continuePendingTransition(false);

  assert.deepEqual(editor.calls, [["projection-status"], ["publish", 7, 2]]);
});

test("a save that fails or conflicts before publishing publishes nothing and keeps the reason in the dialog", async (t) => {
  const stored = legacyLyrics(2);
  const conflict = new APIError(409, { error: "revision_conflict", current: legacyLyrics(3, "他人的新修订") });
  // saveFirst=false is the confirm button a dialog shows when only a collaborator's edits are unsaved.
  for (const failure of [conflict, new APIError(500, { error: "internal_error" })]) {
    for (const saveFirst of [true, false]) {
      const label = `${failure.code}, saveFirst=${saveFirst}`;
      const editor = openPersistence(t, {
        stored, shared: legacyLyrics(2, "未保存的修改"), pendingTransition: publishTransition,
        api: { checkpoint: () => { throw failure; } },
      });

      await editor.persistence.continuePendingTransition(saveFirst);

      assert.deepEqual(editor.calls, [["checkpoint", 7]], `${label}: nothing after the failed save`);
      assert.equal(editor.recorded.errors.at(-1), failure, `${label}: the dialog shows why`);
      assert.equal(editor.recorded.transitions.length, 0, `${label}: the dialog stays open`);
    }
  }
});

test("shared edits that appear before the publication request stop it", async (t) => {
  const stored = legacyLyrics(2);
  const editor = openPersistence(t, {
    stored, shared: stored, pendingTransition: { kind: "publish", nextPublished: false },
    api: { beforePublication: () => editor.collaboratorEdits("发布前一刻的修改") },
  });

  await editor.persistence.continuePendingTransition(false);

  assert.deepEqual(editor.calls, [["projection-status"]], "the room reset would have discarded the edit");
  assert.equal(editor.toasts.at(-1).kind, "err");
  assert.equal(editor.recorded.transitions.length, 0, "the dialog stays open to save first");
});

const servedItem = { musicId: 7, title: {}, runtimeLyrics: { source: "bundle", state: "complete" } };
const withdrawnItem = { musicId: 7, title: {}, lyricsWithdrawn: true };
// Neither in the embedded bundle nor ever published: the catalog omits both fields.
const notServedItem = { musicId: 7, title: {} };

test("the source-v3 save message follows the catalog's public state for the song", async (t) => {
  for (const [item, message] of [
    [withdrawnItem, "已保存（当前已撤下，未公开）"],
    [servedItem, "歌词已保存并公开"],
    [notServedItem, "歌词已保存（此前未公开）"],
  ]) {
    const stored = renditionLyrics(4);
    const shared = renditionLyrics(4);
    shared.renditions[0].full.lines[0]["zh-CN"] = "新的译文";
    const editor = openPersistence(t, { stored, shared, sourceV3PublicState: model.sourceV3PublicStateFor(stored, item) });

    assert.equal(await editor.persistence.save(), true);

    assert.deepEqual(editor.toasts.at(-1), { message, kind: "ok" });
  }
});

test("a source-v3 song is withdrawn only when the catalog says so, not when nothing is served", () => {
  assert.equal(model.sourceV3PublicStateFor(renditionLyrics(4), withdrawnItem), "withdrawn");
  assert.equal(model.sourceV3PublicStateFor(renditionLyrics(4), servedItem), "served");
  assert.equal(model.sourceV3PublicStateFor(renditionLyrics(4), notServedItem), "not_served");
  assert.equal(model.sourceV3PublicStateFor(legacyLyrics(2), withdrawnItem), null, "legacy songs publish explicitly");
  assert.equal(model.sourceV3PublicStateFor(renditionLyrics(4), null), "served");
  assert.equal(model.sourceV3PublicStateFor(null, notServedItem), null);
  assert.equal(model.sourceV3SaveWording(null), null);
  for (const state of ["withdrawn", "not_served"]) {
    const wording = model.sourceV3SaveWording(state);
    for (const text of [wording.button, wording.continueLabel, wording.saved, wording.title]) {
      assert.doesNotMatch(text, /并公开|保存即公开/, `${state}: ${text}`);
    }
  }
});

// Renders LyricsEditor's dialogs with prepared editor state; the document view is captured, not rendered.
function renderEditor(overrides) {
  const runtime = createHookRuntime();
  const stateModule = loadSourceModule("components/lyrics/lyricsEditorState.ts", {
    ...esm, react: runtime.react, "@/lib/yjs-lyrics": yjs,
  });
  const state = { ...stateModule.useLyricsEditorState(false), ...overrides };
  const activeTarget = stateModule.useLyricsActiveTarget(state);
  let viewProps = null;
  const { LyricsEditor } = loadSourceModule("components/LyricsEditor.tsx", {
    ...esm,
    "@/app/providers": { useToast: () => ({ show() {} }) },
    "@/components/Modal": { Modal: ({ open, title, children }) => (open ? createElement("section", { "data-title": title }, children) : null) },
    "@/components/lyrics/LyricsDocumentView": { LyricsDocumentView: (props) => { viewProps = props; return null; } },
    "@/components/lyrics/lyricsEditorState": { useLyricsEditorState: () => state, useLyricsActiveTarget: () => activeTarget },
    "@/components/lyrics/useLyricsDocumentCommands": { useLyricsDocumentCommands: () => ({ confirmAnnotationOperation() {} }) },
    "@/components/lyrics/useLyricsDocumentLoader": { useLyricsDocumentLoader: () => ({ loadCatalog() {}, startCollaboration() {} }) },
    "@/components/lyrics/useLyricsPersistence": { useLyricsPersistence: () => ({ continuePendingTransition() {} }) },
    "@/components/lyrics/useLyricsRecoveryTakeover": { useLyricsRecoveryTakeover: () => ({ dialog: null, open() {}, confirm() {}, close() {}, reload() {} }) },
    "@/components/lyrics/useLyricsSourceWorkflow": { useLyricsSourceWorkflow: () => ({ acceptPreview() {} }) },
    "@/lib/yjs-lyrics": yjs,
  });
  const markup = renderToStaticMarkup(createElement(LyricsEditor, { role: "admin" }));
  return { markup, viewProps };
}

test("the publish dialog and readiness follow savable shared edits, not only this client's own", () => {
  const { markup, viewProps } = renderEditor({
    lyrics: legacyLyrics(2, "协作者留下的修改"), dirty: false, saveable: true, pendingTransition: publishTransition,
  });
  assert.match(markup, /共享文档里还有未保存修改/);
  assert.match(markup, />保存并发布<\/button>/);
  assert.doesNotMatch(markup, />确认发布<\/button>/);
  assert.deepEqual(viewProps.publicationChecks[0], { label: "已保存草稿", complete: false });

  const clean = renderEditor({ lyrics: legacyLyrics(2), dirty: false, saveable: false, pendingTransition: publishTransition });
  assert.match(clean.markup, />确认发布<\/button>/);
  assert.doesNotMatch(clean.markup, /共享文档里还有未保存修改/);
  assert.deepEqual(clean.viewProps.publicationChecks[0], { label: "已保存草稿", complete: true });
});

test("a failed save before publishing shows why inside the publish dialog", () => {
  const error = new APIError(409, { error: "revision_conflict", current: legacyLyrics(3) });
  const { markup } = renderEditor({
    lyrics: legacyLyrics(2, "未保存的修改"), dirty: true, saveable: true, pendingTransition: publishTransition, error,
  });
  const dialog = markup.slice(markup.indexOf('data-title="确认发布歌词"'));
  assert.match(dialog, /其他编辑者已保存新版本/);
  assert.match(dialog, /未发布任何内容/);
  assert.match(dialog, />载入服务器版本<\/button>/);
});

test("the save-and-continue label follows the song's public state", () => {
  const pendingTransition = { kind: "choose", item: { musicId: 8, title: { "ja-JP": "別の曲" } } };
  for (const [name, lyrics, item, label] of [
    ["served", renditionLyrics(4), servedItem, "保存并公开后继续"],
    ["withdrawn", renditionLyrics(4), withdrawnItem, "保存后继续"],
    ["not served", renditionLyrics(4), notServedItem, "保存后继续"],
    ["legacy", legacyLyrics(2), notServedItem, "保存并继续"],
  ]) {
    const { markup } = renderEditor({
      lyrics, dirty: true, saveable: true, pendingTransition,
      sourceV3PublicState: model.sourceV3PublicStateFor(lyrics, item),
    });
    assert.match(markup, new RegExp(`>${label}</button>`), name);
  }
});
