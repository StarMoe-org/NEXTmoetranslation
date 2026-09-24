import assert from "node:assert/strict";
import { createRequire } from "node:module";
import test from "node:test";

import { createHookRuntime, loadSourceModule } from "./source-module-harness.mjs";

const esm = {
  "@/lib/lyrics-editions.mjs": await import("../src/lib/lyrics-editions.mjs"),
  "@/lib/lyrics-segmentation.mjs": await import("../src/lib/lyrics-segmentation.mjs"),
  "@/lib/lyrics-versioning.mjs": await import("../src/lib/lyrics-versioning.mjs"),
};

const yjs = loadSourceModule("lib/yjs-lyrics.ts", { "y-websocket": { WebsocketProvider: class {} } });

class APIError extends Error {
  constructor(status, body = {}) {
    super(body.error || "api_error");
    this.status = status;
    this.code = body.error;
  }
}

function loadLoader() {
  const runtime = createHookRuntime();
  const collaborations = [];
  const pendingLyrics = [];
  class FakeCollaboration {
    constructor(options) { collaborations.push(options.musicId); }
    destroy() {}
    reconnectNow() {}
  }
  const modules = {
    ...esm,
    react: runtime.react,
    "@/app/providers": { useToast: () => ({ show() {} }) },
    "@/lib/api": {
      APIError,
      getCatalogMusic: async () => ({ items: [] }),
      getCatalogPerformers: async () => ({ items: [] }),
      getClientID: () => "client-1",
      getUsername: () => "editor",
      getLyrics: () => new Promise((resolve) => { pendingLyrics.push(resolve); }),
      issueLyricsCollabTicket: async () => { throw new Error("unused"); },
      subscribeSessionChanged: () => () => {},
    },
    "@/lib/yjs-lyrics": { ...yjs, LyricsCollaboration: FakeCollaboration },
  };
  const cache = new Map();
  const { useLyricsEditorState } = loadSourceModule("components/lyrics/lyricsEditorState.ts", modules, cache);
  const { useLyricsDocumentLoader } = loadSourceModule("components/lyrics/useLyricsDocumentLoader.ts", modules, cache);
  const state = useLyricsEditorState(false);
  const loader = useLyricsDocumentLoader(state);
  return { runtime, loader, state, collaborations, pendingLyrics };
}

const lyrics = {
  musicId: 7,
  status: "draft",
  revision: 2,
  updatedAt: "2026-09-01T00:00:00Z",
  attribution: "",
  translationCredit: "",
  proofreadingCredit: "",
  sourceUrl: "",
  lines: [{
    id: "line-1", order: 0, japanese: "あいう", "zh-CN": "阿伊乌", "en-US": "",
    segments: [{ text: "あいう", performerIds: [], ruby: [{ text: "あいう" }] }],
  }],
};

test("a lyrics load that resolves after the editor unmounts never starts a collaboration", async (t) => {
  const originalWindow = globalThis.window;
  globalThis.window = {
    setTimeout: () => 0,
    clearTimeout() {},
    location: { search: "", href: "http://localhost/lyrics" },
    history: { state: null, replaceState() {} },
  };
  t.after(() => { globalThis.window = originalWindow; });
  const { runtime, loader, state, collaborations, pendingLyrics } = loadLoader();
  runtime.mount();

  const loading = loader.performChooseMusic({ musicId: 7, title: { "ja-JP": "テスト曲" } });
  assert.equal(pendingLyrics.length, 1);
  runtime.unmount();
  pendingLyrics[0](lyrics);

  assert.equal(await loading, false);
  assert.deepEqual(collaborations, []);
  assert.equal(state.collaborationRef.current, null);
});

test("a lyrics load that resolves while the editor is mounted starts its collaboration", async (t) => {
  const originalWindow = globalThis.window;
  globalThis.window = {
    setTimeout: () => 0,
    clearTimeout() {},
    location: { search: "", href: "http://localhost/lyrics" },
    history: { state: null, replaceState() {} },
  };
  t.after(() => { globalThis.window = originalWindow; });
  const { runtime, loader, collaborations, pendingLyrics } = loadLoader();
  runtime.mount();

  const loading = loader.performChooseMusic({ musicId: 7, title: { "ja-JP": "テスト曲" } });
  pendingLyrics[0](lyrics);

  assert.equal(await loading, true);
  assert.deepEqual(collaborations, [7]);
  runtime.unmount();
});

function testWindow(t) {
  const originalWindow = globalThis.window;
  globalThis.window = {
    setTimeout: () => 0,
    clearTimeout() {},
    location: { search: "", href: "http://localhost/lyrics" },
    history: { state: null, replaceState() {} },
  };
  t.after(() => { globalThis.window = originalWindow; });
}

// Drives the loader's Yjs snapshot handling with the real baseline helpers and records
// every baseline it sets, so the test reads what the editor would compare against.
async function openCollaborativeLyrics(stored) {
  const runtime = createHookRuntime();
  const sessions = [];
  class FakeCollaboration {
    constructor(options) { this.options = options; sessions.push(this); }
    destroy() {}
    reconnectNow() {}
    retireLocalHistory() {}
    settleRetainedLocalChanges() {}
  }
  const cache = new Map();
  const modules = {
    ...esm,
    react: runtime.react,
    "@/app/providers": { useToast: () => ({ show() {} }) },
    "@/lib/api": {
      APIError,
      getCatalogMusic: async () => ({ items: [] }),
      getCatalogPerformers: async () => ({ items: [] }),
      getClientID: () => "client-2",
      getUsername: () => "second-editor",
      getLyrics: async () => structuredClone(stored),
      issueLyricsCollabTicket: async () => { throw new Error("unused"); },
      subscribeSessionChanged: () => () => {},
    },
    "@/lib/yjs-lyrics": { ...yjs, LyricsCollaboration: FakeCollaboration },
  };
  const { useLyricsEditorState } = loadSourceModule("components/lyrics/lyricsEditorState.ts", modules, cache);
  const { useLyricsDocumentLoader } = loadSourceModule("components/lyrics/useLyricsDocumentLoader.ts", modules, cache);
  const state = useLyricsEditorState(false);
  const baselines = [];
  state.setBaseline = (value) => { baselines.push(value); };
  const editions = [];
  state.setActiveTranslationEditionKey = (value) => { editions.push(value); };
  const loader = useLyricsDocumentLoader(state);
  runtime.mount();
  assert.equal(await loader.performChooseMusic({ musicId: stored.musicId, title: { "ja-JP": "テスト曲" } }), true);
  assert.equal(sessions.length, 1);
  return {
    activeEdition: () => editions.at(-1),
    baseline: () => baselines.at(-1),
    baselineWrites: () => baselines.length,
    receive: (document, localChanges = false) => sessions[0].options.onSnapshot({
      document: structuredClone(document), status: "synced", peers: [], synced: true, localChanges,
    }),
    close: () => runtime.unmount(),
  };
}

function withTranslation(document, text) {
  const next = structuredClone(document);
  next.lines[0]["zh-CN"] = text;
  return next;
}

test("a remote checkpoint leaves a collaborator who changed nothing clean and with nothing to save", async (t) => {
  testWindow(t);
  const editor = await openCollaborativeLyrics(lyrics);
  editor.receive(lyrics);
  assert.equal(yjs.lyricsDocumentSaveable(lyrics, editor.baseline()), false);

  const edited = withTranslation(lyrics, "协作者的修改");
  editor.receive(edited);
  const checkpointed = { ...edited, revision: 3, updatedAt: "2026-09-02T00:00:00Z" };
  editor.receive(checkpointed);

  assert.equal(yjs.lyricsDocumentDirty(checkpointed, editor.baseline(), false), false, "not dirty, so never frozen");
  assert.equal(yjs.lyricsDocumentSaveable(checkpointed, editor.baseline()), false);
  const writes = editor.baselineWrites();
  editor.receive(checkpointed);
  assert.equal(editor.baselineWrites(), writes, "an unchanged envelope keeps the baseline");
  editor.close();
});

test("edits a collaborator left unsaved in the shared document stay savable by another collaborator", async (t) => {
  testWindow(t);
  const present = await openCollaborativeLyrics(lyrics);
  present.receive(lyrics);
  const leftover = withTranslation(lyrics, "离开前未保存的修改");
  present.receive(leftover);
  assert.equal(yjs.lyricsDocumentDirty(leftover, present.baseline(), false), false);
  assert.equal(yjs.lyricsDocumentSaveable(leftover, present.baseline()), true, "a collaborator who saw the edit can save it");
  present.close();

  const newcomer = await openCollaborativeLyrics(lyrics);
  newcomer.receive(leftover);
  assert.equal(newcomer.baseline(), yjs.canonicalLyricsJSON(lyrics), "the stored document stays the baseline");
  assert.equal(yjs.lyricsDocumentDirty(leftover, newcomer.baseline(), false), false);
  assert.equal(yjs.lyricsDocumentSaveable(leftover, newcomer.baseline()), true, "a collaborator who joins later can save it");
  newcomer.close();
});

test("a room that no longer carries the loaded document's envelope or edition becomes the baseline", async (t) => {
  testWindow(t);
  const moved = await openCollaborativeLyrics(lyrics);
  const saved = { ...withTranslation(lyrics, "已保存的新修订"), revision: 3, updatedAt: "2026-09-02T00:00:00Z" };
  moved.receive(saved);
  assert.equal(yjs.lyricsDocumentSaveable(saved, moved.baseline()), false);
  moved.close();

  const rendition = (editionKey, translation) => ({
    musicId: 7,
    status: "draft",
    revision: 4,
    updatedAt: "2026-09-01T00:00:00Z",
    translationEditionKey: editionKey,
    defaultTranslationEditionKey: "ed-default",
    translationEditions: [{ key: "ed-default", label: "默认译本" }, { key: "ed-second", label: "第二译本" }],
    renditions: [{
      key: "sekai", kind: "sekai", label: "SEKAI", availableVersions: ["full"], performers: [],
      full: {
        version: { kind: "sekai", label: "SEKAI" },
        lines: [{
          id: "sekai-line-1", order: 0, japanese: "かきく", "zh-CN": translation, "en-US": "",
          segments: [{ text: "かきく", performerIds: [], ruby: [{ text: "かきく" }] }],
          trailingPerformerIds: [],
        }],
      },
      relation: { kind: "none" },
      sourceTabPaths: [["SEKAI"]],
      provenance: [],
    }],
  });
  const otherEdition = await openCollaborativeLyrics(rendition("ed-second", "第二译本的译文"));
  assert.equal(otherEdition.activeEdition(), "ed-second");
  const room = rendition("ed-default", "默认译本的译文");
  otherEdition.receive(room);
  assert.equal(yjs.lyricsDocumentSaveable(room, otherEdition.baseline()), false, "the room holds the default edition");
  assert.equal(otherEdition.activeEdition(), "ed-default", "the edition picker names the edition a save writes");
  otherEdition.close();
});

// The loader driven by the real LyricsCollaboration: the test plays the collaboration
// server, holding its room document and the stored document getLyrics returns.
const Y = createRequire(import.meta.url)("yjs");
const providers = [];
const liveYjs = loadSourceModule("lib/yjs-lyrics.ts", {
  "y-websocket": {
    WebsocketProvider: class {
      constructor(url, room, doc) {
        this.doc = doc;
        this.handlers = new Map();
        this.awareness = { setLocalState() {}, on() {}, getStates: () => new Map(), destroy() {} };
        providers.push(this);
      }
      on(name, handler) { this.handlers.set(name, handler); }
      connect() {}
      destroy() {}
    },
  },
});

const flush = async () => {
  for (let tick = 0; tick < 10; tick++) await new Promise((resolve) => setTimeout(resolve, 0));
};

function twoLineLyrics() {
  const document = structuredClone(lyrics);
  document.lines.push({
    id: "line-2", order: 1, japanese: "かきく", "zh-CN": "卡基库", "en-US": "",
    segments: [{ text: "かきく", performerIds: [], ruby: [{ text: "かきく" }] }],
  });
  return document;
}

async function openLiveCollaboration(t, stored) {
  const originalWindow = globalThis.window;
  globalThis.window = {
    setTimeout: () => 0,
    clearTimeout() {},
    location: { search: "", href: "http://localhost/lyrics", origin: "http://localhost" },
    history: { state: null, replaceState() {} },
  };
  providers.length = 0;
  const server = { doc: new Y.Doc(), stored: structuredClone(stored) };
  const room = server.doc.getMap(liveYjs.LYRICS_YJS_ROOT);
  liveYjs.syncLyricsDocument(room, stored);
  const runtime = createHookRuntime();
  t.after(() => {
    runtime.unmount();
    globalThis.window = originalWindow;
  });
  const cache = new Map();
  const modules = {
    ...esm,
    react: runtime.react,
    "@/app/providers": { useToast: () => ({ show() {} }) },
    "@/lib/api": {
      APIError,
      getCatalogMusic: async () => ({ items: [] }),
      getCatalogPerformers: async () => ({ items: [] }),
      getClientID: () => "client-a",
      getUsername: () => "editor-a",
      getLyrics: async () => structuredClone(server.stored),
      issueLyricsCollabTicket: async () => ({ ticket: "ticket", room: "lyrics-7-e1", expiresAt: new Date(Date.now() + 60_000).toISOString() }),
      subscribeSessionChanged: () => () => {},
    },
    "@/lib/yjs-lyrics": liveYjs,
  };
  const { useLyricsEditorState } = loadSourceModule("components/lyrics/lyricsEditorState.ts", modules, cache);
  const { useLyricsDocumentLoader } = loadSourceModule("components/lyrics/useLyricsDocumentLoader.ts", modules, cache);
  const state = useLyricsEditorState(false);
  const baselines = [];
  state.setBaseline = (value) => { baselines.push(value); };
  const loader = useLyricsDocumentLoader(state);
  runtime.mount();
  assert.equal(await loader.performChooseMusic({ musicId: stored.musicId, title: { "ja-JP": "テスト曲" } }), true);
  await flush();
  const collaboration = state.collaborationRef.current;
  const provider = providers[0];
  assert.ok(collaboration && provider, "the collaboration connected");
  const deliver = () => Y.applyUpdate(collaboration.doc, Y.encodeStateAsUpdate(server.doc, Y.encodeStateVector(collaboration.doc)), provider);
  deliver();
  provider.handlers.get("sync")(true);

  const shared = () => liveYjs.materializeLyricsDocument(collaboration.root);
  return {
    collaboration,
    shared,
    baseline: () => baselines.at(-1),
    dirty: () => liveYjs.lyricsDocumentDirty(shared(), baselines.at(-1), collaboration.hasLocalChanges()),
    saveable: () => liveYjs.lyricsDocumentSaveable(shared(), baselines.at(-1)),
    edit(lineIndex, text) {
      const next = shared();
      next.lines[lineIndex]["zh-CN"] = text;
      assert.equal(collaboration.updateDocument(next), true);
    },
    // Integrates this client's pending updates into the server room.
    send: () => Y.applyUpdate(server.doc, Y.encodeStateAsUpdate(collaboration.doc, Y.encodeStateVector(server.doc))),
    collaboratorEdits(lineIndex, text) {
      const next = liveYjs.materializeLyricsDocument(room);
      next.lines[lineIndex]["zh-CN"] = text;
      liveYjs.syncLyricsDocument(room, next);
      deliver();
    },
    // Mirrors collab.Checkpoint: store the room as snapshotted, then broadcast the new envelope.
    snapshotCheckpoint(revision, updatedAt) {
      server.stored = { ...liveYjs.materializeLyricsDocument(room), revision, updatedAt };
      return () => {
        server.doc.transact(() => {
          room.set("revision", revision);
          room.set("updatedAt", updatedAt);
        });
        deliver();
      };
    },
  };
}

test("after another client's checkpoint the saved edit neither keeps this client dirty nor reverts on discard", async (t) => {
  const editor = await openLiveCollaboration(t, twoLineLyrics());
  assert.equal(editor.dirty(), false);
  editor.edit(0, "本客户端的修改");
  editor.send();
  assert.equal(editor.dirty(), true);

  editor.snapshotCheckpoint(3, "2026-09-02T00:00:00Z")();
  await flush();
  assert.equal(editor.collaboration.hasLocalChanges(), false, "the checkpoint stored every local edit");
  assert.equal(editor.dirty(), false);

  editor.collaboratorEdits(1, "协作者之后的修改");
  assert.equal(editor.dirty(), false, "a collaborator's edit does not make this client dirty");
  assert.equal(editor.saveable(), true);

  editor.collaboration.discardLocalChanges();
  assert.equal(editor.shared().lines[0]["zh-CN"], "本客户端的修改", "discarding never reverts saved content");
  assert.equal(editor.shared().lines[1]["zh-CN"], "协作者之后的修改");
});

test("an edit made after the server's checkpoint snapshot but before its envelope stays dirty and savable", async (t) => {
  const editor = await openLiveCollaboration(t, twoLineLyrics());
  editor.edit(0, "快照前的修改");
  editor.send();
  const broadcastEnvelope = editor.snapshotCheckpoint(3, "2026-09-02T00:00:00Z");
  editor.edit(1, "快照后的修改");
  broadcastEnvelope();
  await flush();

  assert.equal(editor.collaboration.undoManager.canUndo(), false, "history reaching into stored content is retired");
  assert.equal(editor.saveable(), true, "the stored document, not the shared one, is the baseline");
  assert.equal(editor.dirty(), true, "the unsaved edit keeps this client dirty");

  editor.send();
  editor.snapshotCheckpoint(4, "2026-09-03T00:00:00Z")();
  await flush();
  assert.equal(editor.collaboration.hasLocalChanges(), false, "a checkpoint that stored the edit settles it");
  assert.equal(editor.dirty(), false);
  assert.equal(editor.saveable(), false);
});
