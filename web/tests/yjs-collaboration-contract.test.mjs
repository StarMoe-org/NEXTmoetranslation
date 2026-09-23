import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import { readLyricsEditor } from "./source-surfaces.mjs";

import ts from "typescript";
import * as Y from "yjs";

const source = await readFile(new URL("../src/lib/yjs-lyrics.ts", import.meta.url), "utf8");
const apiSource = await readFile(new URL("../src/lib/api.ts", import.meta.url), "utf8");
const nextConfigSource = await readFile(new URL("../next.config.ts", import.meta.url), "utf8");

const providerStubSource = `
export class WebsocketProvider {
  constructor(serverUrl, room, doc, options) {
    this.serverUrl = serverUrl;
    this.room = room;
    this.doc = doc;
    this.options = options;
    this.shouldConnect = true;
    this.handlers = new Map();
    this.destroyed = false;
    this.connected = false;
    this.awareness = {
      states: new Map(),
      handlers: new Map(),
      setLocalState: state => { this.awareness.localState = state; },
      getStates: () => this.awareness.states,
      on: (name, handler) => { this.awareness.handlers.set(name, handler); },
      destroy: () => { this.awareness.destroyed = true; },
    };
    globalThis.__lyricsYjsProviders.push(this);
  }
  on(name, handler) { this.handlers.set(name, handler); }
  connect() { this.connected = true; }
  destroy() { this.destroyed = true; }
}
`;

globalThis.__lyricsYjsProviders = [];
const providerStubURL = `data:text/javascript;base64,${Buffer.from(providerStubSource).toString("base64")}`;
const compiled = ts.transpileModule(source, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
}).outputText
  .replaceAll('"yjs"', JSON.stringify(import.meta.resolve("yjs")))
  .replaceAll('"y-websocket"', JSON.stringify(providerStubURL));
const collaboration = await import(`data:text/javascript;base64,${Buffer.from(compiled).toString("base64")}`);

async function waitFor(predicate) {
  for (let attempt = 0; attempt < 50; attempt++) {
    if (predicate()) return;
    await new Promise(resolve => setTimeout(resolve, 0));
  }
  assert.fail("timed out waiting for collaboration state");
}

function sampleLyrics() {
  return {
    musicId: 42,
    status: "draft",
    revision: 0,
    updatedAt: "2026-08-14T00:00:00Z",
    attribution: "MoeSeka team",
    translationCredit: "Translator",
    proofreadingCredit: "Proofreader",
    sourceUrl: "https://example.test/revision/7",
    lines: [{
      id: "line-1",
      order: 0,
      japanese: "歌",
      "zh-CN": "歌",
      "en-US": "Song",
      segments: [{
        text: "歌",
        performerIds: [1],
        ruby: [{ text: "歌", reading: "うた" }],
      }],
    }],
  };
}

function lyricLine(id, japanese, chinese, english) {
  return {
    id,
    order: 0,
    japanese,
    "zh-CN": chinese,
    "en-US": english,
    segments: [{ text: japanese, performerIds: [1], ruby: [{ text: japanese, reading: "" }] }],
  };
}

function pairedLyricsDocs(lyrics = sampleLyrics()) {
  const left = new Y.Doc();
  const leftRoot = left.getMap(collaboration.LYRICS_YJS_ROOT);
  collaboration.syncLyricsDocument(leftRoot, lyrics);
  const right = new Y.Doc();
  Y.applyUpdate(right, Y.encodeStateAsUpdate(left));
  return {
    left,
    leftRoot,
    leftBaseline: Y.encodeStateVector(left),
    right,
    rightRoot: right.getMap(collaboration.LYRICS_YJS_ROOT),
    rightBaseline: Y.encodeStateVector(right),
  };
}

function exchangeConcurrentUpdates(pair) {
  const leftUpdate = Y.encodeStateAsUpdate(pair.left, pair.leftBaseline);
  const rightUpdate = Y.encodeStateAsUpdate(pair.right, pair.rightBaseline);
  Y.applyUpdate(pair.left, rightUpdate);
  Y.applyUpdate(pair.right, leftUpdate);
}

function firstSegment(root) {
  return root.get("lines").get(0).get("segments").get(0);
}

function splitSegmentDocument(lyrics, offset) {
  const next = structuredClone(lyrics);
  const segment = next.lines[0].segments[0];
  const left = segment.text.slice(0, offset);
  const right = segment.text.slice(offset);
  next.lines[0].segments = [
    { ...segment, text: left, ruby: [{ text: left }] },
    { ...segment, text: right, ruby: [{ text: right }] },
  ];
  return next;
}

function splitRubyDocument(lyrics, offset) {
  const next = structuredClone(lyrics);
  const segment = next.lines[0].segments[0];
  const span = segment.ruby[0];
  const left = span.text.slice(0, offset);
  const right = span.text.slice(offset);
  segment.ruby = [{ text: left }, { text: right }];
  return next;
}

test("Yjs collaboration uses a one-time ticket instead of a long-lived JWT query", async () => {
  assert.match(apiSource, /apiFetch<unknown>\(`\/editor\/v1\/lyrics\/\$\{musicId\}\/collab-ticket`,\s*\{\s*method:\s*"POST"/s);
  assert.match(apiSource, /collab-ticket`[\s\S]*body:\s*JSON\.stringify\(\{\}\)/);
  assert.match(apiSource, /collab-ticket`[\s\S]*signal,\s*\},\s*true\)/);
  assert.doesNotMatch(source, /\b(?:getSessionEnvelope|getStoredSessionEnvelope|session\.token|Authorization|Bearer)\b/);

  const originalWindow = globalThis.window;
  const originalBase = process.env.NEXT_PUBLIC_API_BASE;
  Object.defineProperty(globalThis, "window", {
    configurable: true,
    value: { location: { origin: "https://console.example" } },
  });
  delete process.env.NEXT_PUBLIC_API_BASE;
  globalThis.__lyricsYjsProviders.length = 0;
  let ticketCalls = 0;
  const instance = new collaboration.LyricsCollaboration({
    musicId: 42,
    clientId: "tab-1",
    username: "editor",
    color: "#1677FF",
    issueTicket: async () => ({
      ticket: `short-ticket-${++ticketCalls}`,
      room: "lyrics-42-e7",
      expiresAt: new Date(Date.now() + 30_000).toISOString(),
    }),
    onSnapshot: () => {},
  });

  try {
    await waitFor(() => globalThis.__lyricsYjsProviders.length === 1);
    const first = globalThis.__lyricsYjsProviders[0];
    assert.equal(first.serverUrl, "wss://console.example/yjs/lyrics");
    assert.equal(first.room, "42");
    assert.deepEqual(first.options.params, { ticket: "short-ticket-1" });
    assert.equal(first.serverUrl.includes("token="), false);
    assert.deepEqual(Object.keys(first.options.params), ["ticket"]);
    assert.deepEqual(first.awareness.localState, {
      clientId: "tab-1",
      username: "editor",
      color: "#1677FF",
    });
    assert.equal(instance.getSnapshot().document, null, "the client must not seed REST data before first sync");

    const firstDoc = instance.doc;
    instance.reconnectNow();
    await waitFor(() => globalThis.__lyricsYjsProviders.length === 2);
    assert.equal(first.destroyed, true);
    assert.equal(first.awareness.destroyed, true);
    assert.equal(ticketCalls, 2);
    assert.deepEqual(globalThis.__lyricsYjsProviders[1].options.params, { ticket: "short-ticket-2" });
    assert.equal(instance.doc, firstDoc, "same-room transient reconnects must retain the Y.Doc");
  } finally {
    instance.destroy();
    if (originalBase === undefined) delete process.env.NEXT_PUBLIC_API_BASE;
    else process.env.NEXT_PUBLIC_API_BASE = originalBase;
    Object.defineProperty(globalThis, "window", { configurable: true, value: originalWindow });
  }
});

test("single-editor rooms force a 15 second Yjs resync", () => {
  assert.equal(collaboration.LYRICS_YJS_RESYNC_INTERVAL_MS, 15_000);
  assert.match(source, /resyncInterval:\s*LYRICS_YJS_RESYNC_INTERVAL_MS/);
  assert.match(source, /new WebsocketProvider\(websocketServerURL\(\),\s*String\(this\.options\.musicId\)/s);
});

test("the Next development proxy forwards the event stream and the Yjs websocket", () => {
  assert.match(nextConfigSource, /source:\s*"\/sse"[\s\S]*destination:\s*`\$\{backend\}\/sse`/);
  assert.match(nextConfigSource, /source:\s*"\/yjs\/:path\*"[\s\S]*destination:\s*`\$\{backend\}\/yjs\/:path\*`/);
  assert.doesNotMatch(nextConfigSource, /source:\s*"\/ws"/);
});

test("canonical room changes discard the stale epoch document and clean up providers", async () => {
  const originalWindow = globalThis.window;
  Object.defineProperty(globalThis, "window", {
    configurable: true,
    value: { location: { origin: "https://console.example" } },
  });
  globalThis.__lyricsYjsProviders.length = 0;
  let room = "lyrics-42-e7";
  const snapshots = [];
  const instance = new collaboration.LyricsCollaboration({
    musicId: 42,
    clientId: "tab-1",
    username: "editor",
    color: "#1677FF",
    issueTicket: async () => ({
      ticket: `ticket-${room}`,
      room,
      expiresAt: new Date(Date.now() + 30_000).toISOString(),
    }),
    onSnapshot: snapshot => snapshots.push(snapshot),
  });

  try {
    await waitFor(() => globalThis.__lyricsYjsProviders.length === 1);
    const firstProvider = globalThis.__lyricsYjsProviders[0];
    const firstDoc = instance.doc;
    collaboration.syncLyricsDocument(instance.root, sampleLyrics());
    firstProvider.handlers.get("sync")(true);
    assert.deepEqual(instance.getSnapshot().document, sampleLyrics());

    firstProvider.awareness.states.set(99, {
      clientId: "tab-2",
      username: "reviewer",
      color: "#16A085",
    });
    firstProvider.awareness.handlers.get("change")();
    assert.deepEqual(instance.getSnapshot().peers, [{
      clientId: "tab-2",
      username: "reviewer",
      color: "#16A085",
    }]);

    room = "lyrics-42-e8";
    instance.reconnectNow();
    await waitFor(() => globalThis.__lyricsYjsProviders.length === 2);
    const secondProvider = globalThis.__lyricsYjsProviders[1];
    assert.notEqual(instance.doc, firstDoc);
    assert.equal(secondProvider.doc, instance.doc);
    assert.equal(instance.getSnapshot().document, null);
    assert.equal(instance.getSnapshot().synced, false);
    assert.deepEqual(instance.getSnapshot().peers, []);
    assert.equal(firstProvider.destroyed, true);
    assert.equal(firstProvider.awareness.destroyed, true);

    instance.destroy();
    assert.equal(secondProvider.destroyed, true);
    assert.equal(secondProvider.awareness.destroyed, true);
    assert.ok(snapshots.length > 0);
  } finally {
    instance.destroy();
    Object.defineProperty(globalThis, "window", { configurable: true, value: originalWindow });
  }
});

test("checkpoint sends only the tab client identity and never a full document", () => {
  const checkpoint = apiSource.slice(
    apiSource.indexOf("export const checkpointLyrics"),
    apiSource.indexOf("export const publishLyrics"),
  );
  assert.match(checkpoint, /body:\s*JSON\.stringify\(\{ clientId: getClientID\(\) \}\)/);
  const requestBody = checkpoint.match(/body:\s*JSON\.stringify\(([^\n]+)\)/)?.[1];
  assert.equal(requestBody, "{ clientId: getClientID() }");
  assert.doesNotMatch(checkpoint, /buildLyricsSavePayload|body:[^\n]*(?:document|revision)/);
});

test("lyrics documents use nested shared Y types instead of an opaque JSON blob", () => {
  const doc = new Y.Doc();
  const root = doc.getMap(collaboration.LYRICS_YJS_ROOT);
  const lyrics = sampleLyrics();
  collaboration.syncLyricsDocument(root, lyrics);

  const lines = root.get("lines");
  assert.ok(lines instanceof Y.Array);
  const line = lines.get(0);
  assert.ok(line instanceof Y.Map);
  assert.equal(line.get("id"), "line-1");
  const japanese = line.get("japanese");
  assert.ok(japanese instanceof Y.Text);
  const segments = line.get("segments");
  assert.ok(segments instanceof Y.Array);
  const segment = segments.get(0);
  assert.ok(segment instanceof Y.Map);
  assert.match(segment.get("__yjsId"), /^segments:/);
  assert.match(segment.get("__yjsGeneration"), /^seed:segments:/);
  assert.ok(segment.get("text") instanceof Y.Text);
  assert.ok(segment.get("performerIds") instanceof Y.Array);
  const ruby = segment.get("ruby");
  assert.ok(ruby instanceof Y.Array);
  assert.ok(ruby.get(0) instanceof Y.Map);
  assert.match(ruby.get(0).get("__yjsId"), /^ruby:/);
  assert.match(ruby.get(0).get("__yjsGeneration"), /^seed:ruby:/);
  assert.ok(ruby.get(0).get("reading") instanceof Y.Text);
  assert.equal(root.get("status"), "draft");
  assert.equal(root.get("updatedAt"), "2026-08-14T00:00:00Z");
  assert.equal(root.get("sourceUrl"), "https://example.test/revision/7");
  assert.ok(root.get("attribution") instanceof Y.Text);
  assert.deepEqual(collaboration.materializeLyricsDocument(root), lyrics);

  const changed = structuredClone(lyrics);
  changed.lines[0].japanese = "歌詞";
  collaboration.syncLyricsDocument(root, changed);
  assert.equal(line.get("japanese"), japanese);
  assert.equal(japanese.toString(), "歌詞");
  assert.deepEqual(collaboration.materializeLyricsDocument(root), changed);
});

test("concurrent segment splits are detected instead of silently duplicating lyrics", () => {
  const seed = sampleLyrics();
  seed.lines[0].japanese = "ABCD";
  seed.lines[0].segments = [{ text: "ABCD", performerIds: [1], ruby: [{ text: "ABCD" }] }];
  const pair = pairedLyricsDocs(seed);

  collaboration.syncLyricsDocument(pair.leftRoot, splitSegmentDocument(seed, 1));
  collaboration.syncLyricsDocument(pair.rightRoot, splitSegmentDocument(seed, 3));
  exchangeConcurrentUpdates(pair);

  assert.deepEqual(pair.leftRoot.toJSON(), pair.rightRoot.toJSON(), "the CRDT state must still converge byte-for-byte");
  assert.equal(collaboration.materializeLyricsDocument(pair.leftRoot), null);
  assert.equal(collaboration.materializeLyricsDocument(pair.rightRoot), null);
  const mergedSegments = pair.leftRoot.get("lines").get(0).get("segments").toArray();
  assert.ok(mergedSegments.length >= 3, "the incompatible split branches remain available for explicit repair");
  assert.equal(mergedSegments.some((item) => item.get("__yjsOrigin")), true);
});

test("concurrent ruby splits are detected instead of silently duplicating spans", () => {
  const seed = sampleLyrics();
  seed.lines[0].japanese = "ABCD";
  seed.lines[0].segments = [{ text: "ABCD", performerIds: [1], ruby: [{ text: "ABCD" }] }];
  const pair = pairedLyricsDocs(seed);

  collaboration.syncLyricsDocument(pair.leftRoot, splitRubyDocument(seed, 1));
  collaboration.syncLyricsDocument(pair.rightRoot, splitRubyDocument(seed, 3));
  exchangeConcurrentUpdates(pair);

  assert.deepEqual(pair.leftRoot.toJSON(), pair.rightRoot.toJSON(), "the CRDT state must still converge byte-for-byte");
  assert.equal(collaboration.materializeLyricsDocument(pair.leftRoot), null);
  assert.equal(collaboration.materializeLyricsDocument(pair.rightRoot), null);
  const mergedRuby = firstSegment(pair.leftRoot).get("ruby").toArray();
  assert.ok(mergedRuby.length >= 3, "the incompatible ruby split branches remain available for explicit repair");
  assert.equal(mergedRuby.some((item) => item.get("__yjsOrigin")), true);
});

test("a structural conflict received after sync fails closed instead of remaining writable", async () => {
  const originalWindow = globalThis.window;
  Object.defineProperty(globalThis, "window", {
    configurable: true,
    value: { location: { origin: "https://console.example" } },
  });
  globalThis.__lyricsYjsProviders.length = 0;
  const snapshots = [];
  const instance = new collaboration.LyricsCollaboration({
    musicId: 42,
    clientId: "tab-1",
    username: "editor",
    color: "#1677FF",
    issueTicket: async () => ({
      ticket: "short-ticket",
      room: "lyrics-42-e7",
      expiresAt: new Date(Date.now() + 30_000).toISOString(),
    }),
    onSnapshot: snapshot => snapshots.push(snapshot),
  });

  try {
    await waitFor(() => globalThis.__lyricsYjsProviders.length === 1);
    const provider = globalThis.__lyricsYjsProviders[0];
    const seed = sampleLyrics();
    seed.lines[0].japanese = "ABCD";
    seed.lines[0].segments = [{ text: "ABCD", performerIds: [1], ruby: [{ text: "ABCD" }] }];
    collaboration.syncLyricsDocument(instance.root, seed);
    provider.handlers.get("sync")(true);
    assert.equal(instance.getSnapshot().status, "synced");

    const remote = new Y.Doc();
    Y.applyUpdate(remote, Y.encodeStateAsUpdate(instance.doc));
    const remoteBaseline = Y.encodeStateVector(remote);
    const remoteRoot = remote.getMap(collaboration.LYRICS_YJS_ROOT);
    collaboration.syncLyricsDocument(instance.root, splitSegmentDocument(seed, 1));
    collaboration.syncLyricsDocument(remoteRoot, splitSegmentDocument(seed, 3));
    Y.applyUpdate(instance.doc, Y.encodeStateAsUpdate(remote, remoteBaseline));

    const snapshot = instance.getSnapshot();
    assert.equal(snapshot.document, null);
    assert.equal(snapshot.status, "error");
    assert.equal(snapshot.synced, false);
    assert.equal(snapshot.error?.message, "invalid_lyrics_collaboration_document");
    assert.equal(provider.destroyed, true);
    assert.equal(provider.awareness.destroyed, true);
    assert.equal(instance.updateDocument(seed), false, "invalid collaboration state must remain read-only");
    assert.equal(snapshots.at(-1)?.status, "error");
  } finally {
    instance.destroy();
    Object.defineProperty(globalThis, "window", { configurable: true, value: originalWindow });
  }
});

test("independent Y.Docs converge after concurrent edits to different nested Y.Text fields", () => {
  const pair = pairedLyricsDocs();
  const leftLyrics = sampleLyrics();
  leftLyrics.lines[0].japanese = "左侧日文";
  leftLyrics.lines[0].segments[0].text = "左侧日文";
  leftLyrics.lines[0].segments[0].ruby = [{ text: "左侧日文", reading: "" }];
  const rightLyrics = sampleLyrics();
  rightLyrics.lines[0]["zh-CN"] = "右侧中文";

  collaboration.syncLyricsDocument(pair.leftRoot, leftLyrics);
  collaboration.syncLyricsDocument(pair.rightRoot, rightLyrics);
  exchangeConcurrentUpdates(pair);

  const left = collaboration.materializeLyricsDocument(pair.leftRoot);
  const right = collaboration.materializeLyricsDocument(pair.rightRoot);
  assert.deepEqual(left, right);
  assert.equal(left.lines[0].japanese, "左侧日文");
  assert.equal(left.lines[0]["zh-CN"], "右侧中文");
});

test("concurrent lines Y.Array inserts preserve stable IDs and converge", () => {
  const seed = sampleLyrics();
  seed.lines = [lyricLine("line-base", "基准", "基准", "Base")];
  const pair = pairedLyricsDocs(seed);
  const leftLyrics = structuredClone(seed);
  const rightLyrics = structuredClone(seed);
  leftLyrics.lines.unshift(lyricLine("line-left", "左", "左", "Left"));
  rightLyrics.lines.unshift(lyricLine("line-right", "右", "右", "Right"));

  collaboration.syncLyricsDocument(pair.leftRoot, leftLyrics);
  collaboration.syncLyricsDocument(pair.rightRoot, rightLyrics);
  exchangeConcurrentUpdates(pair);

  const left = collaboration.materializeLyricsDocument(pair.leftRoot);
  const right = collaboration.materializeLyricsDocument(pair.rightRoot);
  assert.deepEqual(left, right);
  assert.deepEqual(left.lines.map(line => line.id).sort(), ["line-base", "line-left", "line-right"]);
  assert.deepEqual(Object.fromEntries(left.lines.map(line => [line.id, line.japanese])), {
    "line-base": "基准",
    "line-left": "左",
    "line-right": "右",
  });
});

test("a materialized shared document compares clean against the server response regardless of key insertion order", () => {
  const lyrics = sampleLyrics();
  const reverseKeys = value => Object.fromEntries(Object.entries(value).reverse());
  const seededByPeer = reverseKeys({ ...lyrics, lines: lyrics.lines.map(line => reverseKeys(line)) });
  const doc = new Y.Doc();
  const root = doc.getMap(collaboration.LYRICS_YJS_ROOT);
  collaboration.syncLyricsDocument(root, seededByPeer);

  const materialized = collaboration.materializeLyricsDocument(root);
  assert.deepEqual(materialized, lyrics);
  assert.notEqual(JSON.stringify(materialized), JSON.stringify(lyrics));
  assert.equal(collaboration.canonicalLyricsJSON(materialized), collaboration.canonicalLyricsJSON(lyrics));

  const changed = structuredClone(lyrics);
  changed.lines[0]["zh-CN"] = "改动";
  assert.notEqual(collaboration.canonicalLyricsJSON(materialized), collaboration.canonicalLyricsJSON(changed));
  const reordered = structuredClone(lyrics);
  reordered.lines[0].segments[0].performerIds = [2, 1];
  assert.notEqual(collaboration.canonicalLyricsJSON(lyrics), collaboration.canonicalLyricsJSON(reordered));
});

test("LyricsEditor dirty tracking never compares raw JSON.stringify output against the baseline", async () => {
  const editor = await readLyricsEditor();
  assert.match(editor, /const dirty = lyrics != null && canonicalLyricsJSON\(lyrics\) !== baseline;/);
  assert.match(editor, /setBaseline\(canonicalLyricsJSON\(persisted\)\)/);
  assert.match(editor, /canonicalLyricsJSON\(editableLyricsDocument\(currentSharedDocument\)\) !== canonicalLyricsJSON\(document\)/);
  assert.doesNotMatch(editor, /JSON\.stringify\([^\n]*\) !== baseline/);
  assert.doesNotMatch(editor, /setBaseline\(JSON\.stringify/);
  assert.doesNotMatch(editor, /const serialized = JSON\.stringify/);
  assert.doesNotMatch(editor, /collaborationDocumentJSONRef\.current = JSON\.stringify/);
});
