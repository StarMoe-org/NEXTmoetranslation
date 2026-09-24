import assert from "node:assert/strict";
import { createRequire } from "node:module";
import test from "node:test";

import { createHookRuntime, loadSourceModule } from "./source-module-harness.mjs";

const require = createRequire(import.meta.url);
const { createElement } = require("react");
const { renderToStaticMarkup } = require("react-dom/server");

const esm = {
  "@/lib/lyrics-segmentation.mjs": await import("../src/lib/lyrics-segmentation.mjs"),
  "@/lib/lyrics-versioning.mjs": await import("../src/lib/lyrics-versioning.mjs"),
  "@/lib/performer-colors.mjs": await import("../src/lib/performer-colors.mjs"),
  "@/lib/music-vocals.mjs": await import("../src/lib/music-vocals.mjs"),
};
const lyricsSave = await import("../src/lib/lyrics-save.mjs");
const runtime = createHookRuntime();
const yjsStub = {
  canonicalLyricsJSON: (value) => JSON.stringify(value),
  lyricsDocumentDirty: (document, baseline, localChanges) =>
    document != null && localChanges !== false && JSON.stringify(document) !== baseline,
  lyricsDocumentSaveable: (document, baseline) => document != null && JSON.stringify(document) !== baseline,
};
const model = loadSourceModule("components/lyrics/lyricsDocumentModel.ts", esm);
const editorState = loadSourceModule("components/lyrics/lyricsEditorState.ts", {
  ...esm, react: runtime.react, "@/components/lyrics/lyricsDocumentModel": model, "@/lib/yjs-lyrics": yjsStub,
});
const { LyricsDocumentView } = loadSourceModule("components/lyrics/LyricsDocumentView.tsx", {
  ...esm, "@/components/lyrics/lyricsDocumentModel": model,
});
const commandHooks = loadSourceModule("components/lyrics/useLyricsDocumentCommands.ts", {
  ...esm,
  "@/app/providers": { useToast: () => ({ show() {} }) },
  "@/components/lyrics/lyricsDocumentModel": model,
  "@/lib/yjs-lyrics": yjsStub,
});

// The real API client, with a signed-in admin session and the network replaced per test.
const sessionEnvelope = { epoch: "epoch-1", session: { token: "token", username: "admin", role: "admin" } };
const api = loadSourceModule("lib/api.ts", {
  "./session": {
    REFRESH_LOCK: "refresh",
    clearSession: async () => false,
    commitIdentitySession: async () => {},
    commitRefreshedSession: async () => {},
    ensureSessionMigrated: async () => {},
    getSessionEnvelope: () => sessionEnvelope,
    sameSessionIdentity: (left, right) => left === right,
    sameSessionVersion: (left, right) => left === right,
    validSession: () => true,
    validSessionRole: () => true,
    withSessionIdentityLock: async (_mode, action) => action(),
  },
  "./catalog-pagination.mjs": await import("../src/lib/catalog-pagination.mjs"),
  "./lyrics-save.mjs": lyricsSave,
});
const { APIError } = api;

const noop = new Proxy({}, { get: () => () => {} });
const music = { musicId: 7, title: { "ja-JP": "テスト曲", "zh-CN": "测试曲" } };
const servedMusic = { ...music, runtimeLyrics: { source: "bundle", state: "complete", availableVersions: ["full"] } };

function renditionLyrics({ recoveryLedgerOwned = true, credits = { translation: "译者" } } = {}) {
  return {
    musicId: 7,
    status: "draft",
    revision: 4,
    updatedAt: "2026-09-01T00:00:00Z",
    translationEditionKey: "ed-default",
    defaultTranslationEditionKey: "ed-default",
    translationEditions: [{ key: "ed-default", label: "默认译本" }],
    ...(recoveryLedgerOwned ? { recoveryLedgerOwned: true } : {}),
    renditions: [{
      key: "sekai",
      kind: "sekai",
      label: "SEKAI",
      availableVersions: ["full"],
      performers: [{ performerId: "ichika", name: "星乃一歌" }, { performerId: "saki", name: "天马咲希" }],
      full: {
        version: { kind: "sekai", label: "Leo/need" },
        lines: [{
          id: "sekai-line-1", order: 0, japanese: "かきく", "zh-CN": "卡基库", "en-US": "",
          segments: [{ text: "かきく", performerIds: ["ichika"], ruby: [{ text: "かきく" }] }],
          trailingPerformerIds: [],
        }],
      },
      relation: { kind: "none" },
      sourceTabPaths: [["SEKAI"]],
      provenance: [],
      translationCredits: credits,
    }],
  };
}

function renderView(lyrics, { role = "admin", ...overrides } = {}) {
  const state = {
    ...editorState.useLyricsEditorState(false),
    selectedMusic: servedMusic,
    lyrics,
    sourceV3PublicState: model.sourceV3PublicStateFor(lyrics, servedMusic),
    dirty: false,
    saveable: false,
    collaborationStatus: "synced",
    activeRenditionKey: "sekai",
    activeTranslationEditionKey: "ed-default",
    ...overrides,
  };
  const activeTarget = editorState.useLyricsActiveTarget(state);
  return renderToStaticMarkup(createElement(LyricsDocumentView, {
    state, activeTarget, loader: noop, commands: noop, source: noop, persistence: noop, role,
    publicationChecks: [], publicationComplete: false, onRequestRecoveryTakeover() {},
  }));
}

function tag(markup, pattern) {
  const match = markup.match(pattern);
  assert.ok(match, `missing ${pattern}`);
  return match[0];
}

test("a recovery-ledger song keeps Japanese, ruby, segments and performers read-only while translations stay editable", () => {
  const markup = renderView(renditionLyrics());
  assert.match(markup, /class="lyrics-recovery-notice" role="note"/);
  assert.match(markup, /日文、注音、分段与演唱者暂时只读/);
  assert.match(tag(markup, /<input type="checkbox"[^>]*\/> 段落前空行/), /disabled=""/);
  assert.match(tag(markup, /<select[^>]*aria-label="第 1 行分段 1 的演唱者"[^>]*>/), /disabled=""/);
  assert.match(tag(markup, /<select aria-label="第 1 行尾演唱者"[^>]*>/), /disabled=""/);
  assert.doesNotMatch(markup, /lyric-structure-actions/);
  assert.match(markup, /Ruby 注音（只读）/);
  assert.match(tag(markup, /<input aria-label="第 1 行分段 1"[^>]*>/), /readOnly=""/);
  assert.doesNotMatch(tag(markup, /<textarea aria-label="第 1 行简体中文译文"[^>]*>/), /readOnly/);
  assert.doesNotMatch(tag(markup, /<textarea aria-label="第 1 行英文译文"[^>]*>/), /readOnly/);

  const editable = renderView(renditionLyrics({ recoveryLedgerOwned: false }));
  assert.doesNotMatch(editable, /lyrics-recovery-notice/);
  assert.doesNotMatch(tag(editable, /<input type="checkbox"[^>]*\/> 段落前空行/), /disabled/);
  assert.doesNotMatch(tag(editable, /<select[^>]*aria-label="第 1 行分段 1 的演唱者"[^>]*>/), /disabled/);
  assert.match(editable, /lyric-structure-actions/);
});

test("admins get the 转为可编辑 action; editors only see the read-only note", () => {
  const admin = renderView(renditionLyrics(), { role: "admin" });
  assert.match(admin, /<button type="button" class="btn btn-secondary btn-sm">转为可编辑<\/button>/);
  const editor = renderView(renditionLyrics(), { role: "editor" });
  assert.match(editor, /日文、注音、分段与演唱者暂时只读/);
  assert.match(editor, /请联系管理员把这首歌转为可编辑/);
  assert.doesNotMatch(editor, />转为可编辑<\/button>/);
});

test("source-layer commands on a recovery-ledger song write nothing; translation edits still land", () => {
  const writes = [];
  const state = {
    lyrics: renditionLyrics(),
    setLyrics: (next) => writes.push(next),
    setError() {},
    performers: [],
    busyRef: { current: false },
    writeLocked: false,
    localSourceImportDraft: false,
    collaborationRef: { current: null },
    collaborationDocumentJSONRef: { current: "" },
    documentGenerationRef: { current: 0 },
    pendingAnnotationOperation: null,
    setPendingAnnotationOperation() {},
    activeTranslationEditionKey: "ed-default",
    activeRenditionKey: "sekai",
    activeVersion: "full",
    segmentInputRefs: { current: {} },
    linesContainerRef: { current: null },
  };
  const target = editorState.useLyricsActiveTarget(state);
  assert.equal(target.recoveryLedgerOwned, true);
  const commands = commandHooks.useLyricsDocumentCommands(state, target);
  commands.updateLine(0, { stanzaBreakBefore: true });
  commands.updateLine(0, { trailingPerformerIds: ["saki"] });
  commands.updateSegment(0, 0, "かきく", ["saki"]);
  commands.updateRubySpan(0, 0, 0, { reading: "かきく" });
  assert.equal(writes.length, 0);
  commands.updateLine(0, { "zh-CN": "新的译文" });
  assert.equal(writes.length, 1);
  assert.equal(writes[0].renditions[0].full.lines[0]["zh-CN"], "新的译文");
});

test("the recovery-ledger flag is modelled from the server but never sent back in a save", () => {
  const document = renditionLyrics();
  document.renditions[0].provenance = [{
    component: "renditions/sekai/full_text", provider: "sekaipedia", title: "sekai-full_text", revisionId: 1,
    revisionUrl: "https://example.invalid/revision/1", licenseName: "CC BY-SA 4.0", licenseUrl: "https://creativecommons.org/licenses/by-sa/4.0/",
  }];
  const accepted = lyricsSave.validateSongLyricsCheckpointResponse(document, 7);
  assert.deepEqual(accepted.details ?? [], []);
  assert.equal(accepted.value.recoveryLedgerOwned, true);
  const invalid = lyricsSave.validateSongLyricsCheckpointResponse({ ...document, recoveryLedgerOwned: "yes" }, 7);
  assert.equal(invalid.ok, false);
  assert.ok(invalid.details.some((detail) => detail.includes("recoveryLedgerOwned")));
  const payload = lyricsSave.buildLyricsSavePayload(accepted.value, "", "client-1");
  assert.equal(payload.clientId, "client-1");
  assert.equal(Object.hasOwn(payload, "recoveryLedgerOwned"), false);
});

// Hooks with state that survives re-renders, so the dialog phases can be followed.
function hookHost() {
  const slots = [];
  let index = 0;
  const react = {
    useState(initial) {
      const slot = index++;
      if (!(slot in slots)) slots[slot] = typeof initial === "function" ? initial() : initial;
      return [slots[slot], (next) => { slots[slot] = typeof next === "function" ? next(slots[slot]) : next; }];
    },
    useRef(current) {
      const slot = index++;
      if (!(slot in slots)) slots[slot] = { current };
      return slots[slot];
    },
  };
  return { react, render: (hook) => { index = 0; return hook(); } };
}

const exported = {
  musicId: 7, from: "served", servedVersion: 3, servedRevision: 5,
  document: { musicId: 7, expectedRevision: 12, source: { url: "https://example.invalid/page?oldid=1" }, renditions: [] },
  warnings: [{ code: "english_dropped", rendition: "sekai", message: "english translations are not kept" }],
};

function openTakeover({ role = "admin", saveable = false, takeover, exportResult = exported } = {}) {
  const host = hookHost();
  const calls = [];
  const toasts = [];
  const busy = [];
  const { useLyricsRecoveryTakeover } = loadSourceModule("components/lyrics/useLyricsRecoveryTakeover.ts", {
    ...esm,
    react: host.react,
    "@/app/providers": { useToast: () => ({ show: (message, kind) => toasts.push({ message, kind }) }) },
    "@/components/lyrics/lyricsDocumentModel": model,
    "@/lib/api": {
      APIError,
      getLyricsDocumentExport: async (musicId) => { calls.push(["export", musicId]); return structuredClone(exportResult); },
      takeOverLyricsDocument: async (musicId, expectedRevision) => {
        calls.push(["takeover", musicId, expectedRevision]);
        return takeover();
      },
    },
  });
  const state = {
    lyricsRef: { current: renditionLyrics() },
    saveable,
    selectedMusic: servedMusic,
    selectedMusicIDRef: { current: 7 },
    busyRef: { current: false },
    setBusy: (value) => busy.push(value),
    writeLockedRef: { current: false },
    query: "テスト",
  };
  const loader = {
    performChooseMusic: async (item) => { calls.push(["reload", item.musicId]); },
    loadCatalog: async (query) => { calls.push(["catalog", query]); },
  };
  const render = () => host.render(() => useLyricsRecoveryTakeover(state, loader, role));
  return { render, calls, toasts, busy, state };
}

const { LyricsRecoveryTakeoverDialog } = loadSourceModule("components/lyrics/LyricsRecoveryTakeover.tsx", {
  "@/components/Modal": { Modal: ({ open, title, children }) => (open ? createElement("section", { "data-title": title }, children) : null) },
  "@/components/lyrics/lyricsDocumentModel": model,
});

function dialogMarkup(takeover, { saveable = false, writeLocked = false } = {}) {
  return renderToStaticMarkup(createElement(LyricsRecoveryTakeoverDialog, { takeover, saveable, writeLocked }));
}

const takeoverResult = {
  dryRun: false, musicId: 7, revision: 13, publicPath: "/files/lyrics/7.json", document: {},
  changes: { against: "served", changed: false },
  warnings: [{ code: "ruby_not_served", rendition: "sekai", side: "full", line: 2, message: "served ruby is missing" }],
};

test("confirming the conversion publishes against the exported revision, then reloads the song", async () => {
  const harness = openTakeover({ takeover: async () => structuredClone(takeoverResult) });
  await harness.render().open();
  let takeover = harness.render();
  assert.equal(takeover.dialog.phase, "confirm");
  assert.equal(takeover.dialog.expectedRevision, 12);
  const confirmMarkup = dialogMarkup(takeover);
  assert.match(confirmMarkup, /data-title="转为可编辑文档"/);
  assert.match(confirmMarkup, /恢复记录会保留，不会删除/);
  assert.doesNotMatch(confirmMarkup, /公开页面的内容保持不变/);
  assert.match(confirmMarkup, /<li>公开页面上除下面列出的变化外，其余内容保持不变；<\/li>/);
  assert.match(confirmMarkup, /<strong>转换后公开页面上会有以下变化：<\/strong><ul><li>sekai：英文译文不会保留<\/li><\/ul>/);
  assert.match(confirmMarkup, /转换后日文和注音通过整曲文档接口修改（例如交给 agent 提交），本页仍只读；分段、演唱者和段落空行可以在本页调整。/);
  assert.match(confirmMarkup, />确认转换<\/button>/);

  await takeover.confirm();
  takeover = harness.render();
  assert.deepEqual(harness.calls, [["export", 7], ["takeover", 7, 12], ["catalog", "テスト"], ["reload", 7]]);
  assert.deepEqual(harness.busy, [true, false]);
  assert.deepEqual(harness.toasts, [{ message: "已转为可编辑文档", kind: "ok" }]);
  assert.equal(takeover.dialog.phase, "done");
  const doneMarkup = dialogMarkup(takeover);
  assert.match(doneMarkup, /已转为可编辑文档，公开页面保持不变/);
  assert.match(doneMarkup, /sekai Full 第 3 行：公开页面缺少注音/);
  assert.doesNotMatch(doneMarkup, /ruby_not_served|served ruby is missing/);
});

test("a 409 revision conflict converts nothing and offers a reload", async () => {
  const harness = openTakeover({
    takeover: async () => { throw new APIError(409, { error: "revision_conflict", current: { revision: 14 } }); },
  });
  await harness.render().open();
  await harness.render().confirm();
  const takeover = harness.render();
  assert.equal(takeover.dialog.phase, "failed");
  const markup = dialogMarkup(takeover);
  assert.match(markup, /role="alert"><strong>这首歌在打开对话框后有了新的修改（当前 revision 14），没有转换。请重新载入后再试。<\/strong>/);
  assert.match(markup, />重新载入<\/button>/);
  assert.doesNotMatch(markup, />(确认|重试)转换<\/button>/);
  assert.deepEqual(harness.toasts, []);

  await takeover.reload();
  assert.equal(harness.render().dialog, null);
  assert.deepEqual(harness.calls, [["export", 7], ["takeover", 7, 12], ["reload", 7]]);
});

test("a 422 lists the server's issues in place and changes nothing", async () => {
  const harness = openTakeover({
    takeover: async () => {
      throw new APIError(422, {
        error: "invalid_document",
        issues: [{ rendition: "sekai", side: "full", line: 0, field: "ja", message: "synthetic mismatch" }],
        details: ["synthetic detail"],
      });
    },
  });
  await harness.render().open();
  await harness.render().confirm();
  const takeover = harness.render();
  const markup = dialogMarkup(takeover);
  assert.match(markup, /服务器拒绝了这次转换，公开页面和数据库都没有改变/);
  assert.match(markup, /<span>sekai Full 第 1 行 · 日文：synthetic mismatch<\/span>/);
  assert.match(markup, /<span>synthetic detail<\/span>/);
  assert.doesNotMatch(markup, />(确认|重试)转换<\/button>|>重新载入<\/button>/);
  assert.deepEqual(harness.calls, [["export", 7], ["takeover", 7, 12]]);
  assert.deepEqual(harness.toasts, []);
});

test("editors cannot open the conversion, and unsaved edits block it", async () => {
  const editor = openTakeover({ role: "editor", takeover: async () => structuredClone(takeoverResult) });
  await editor.render().open();
  assert.equal(editor.render().dialog, null);
  assert.deepEqual(editor.calls, []);

  const dirty = openTakeover({ saveable: true, takeover: async () => structuredClone(takeoverResult) });
  await dirty.render().open();
  let takeover = dirty.render();
  assert.match(tag(dialogMarkup(takeover, { saveable: true }), /<button[^>]*>确认转换<\/button>/), /disabled=""/);
  assert.match(dialogMarkup(takeover, { saveable: true }), /还有未保存的修改/);
  await takeover.confirm();
  takeover = dirty.render();
  assert.equal(takeover.dialog.phase, "confirm");
  assert.deepEqual(dirty.calls, [["export", 7]]);
});

function stubFetch(t, respond) {
  const original = globalThis.fetch;
  const requests = [];
  globalThis.fetch = async (url, init) => {
    requests.push({ url, init });
    const { status, body } = respond();
    return { status, ok: status >= 200 && status < 300, statusText: String(status), json: async () => structuredClone(body) };
  };
  t.after(() => { globalThis.fetch = original; api.clearLoadedProducerState(); });
  assert.equal(api.acceptLoadedProducerState({
    version: 1, running: false, instanceId: "instance", revision: 3, generation: 2, completedGeneration: 2,
  }), true);
  return requests;
}

test("takeOverLyricsDocument posts the revision with producer proof and returns the warnings", async (t) => {
  const requests = stubFetch(t, () => ({ status: 200, body: takeoverResult }));
  const result = await api.takeOverLyricsDocument(7, 12);
  assert.equal(requests[0].url, "/api/editor/v1/lyrics/document/takeover");
  assert.equal(requests[0].init.method, "POST");
  assert.deepEqual(JSON.parse(requests[0].init.body), { musicId: 7, expectedRevision: 12 });
  assert.equal(requests[0].init.headers["X-Moe-Loaded-Producer-State"], "instance:3:2");
  assert.equal(result.revision, 13);
  assert.deepEqual(result.changes, { against: "served", changed: false });
  assert.deepEqual(result.warnings, takeoverResult.warnings);
});

test("takeOverLyricsDocument rejects a response that does not confirm the publication", async (t) => {
  let body = { ...takeoverResult, dryRun: true };
  stubFetch(t, () => ({ status: 200, body }));
  await assert.rejects(api.takeOverLyricsDocument(7, 12), (error) => error.status === 502 && error.code === "invalid_lyrics_response");
  body = { ...takeoverResult, warnings: [{ code: "english_dropped" }] };
  await assert.rejects(api.takeOverLyricsDocument(7, 12), (error) => error.status === 502);
});

test("takeover errors keep their 409 revision and 422 issues", async (t) => {
  let response = { status: 409, body: { error: "revision_conflict", current: { revision: 14 } } };
  stubFetch(t, () => response);
  await assert.rejects(api.takeOverLyricsDocument(7, 12), (error) =>
    error.status === 409 && error.code === "revision_conflict" && error.current.revision === 14);
  const issue = { rendition: "sekai", side: "full", line: 0, field: "ja", message: "synthetic mismatch" };
  response = { status: 422, body: { error: "invalid_document", issues: [issue] } };
  await assert.rejects(api.takeOverLyricsDocument(7, 12), (error) =>
    error.status === 422 && error.issues.length === 1 && error.issues[0].message === "synthetic mismatch");
});

test("a served song without any rendition credit is not labelled as published on save", () => {
  const uncredited = renditionLyrics({ recoveryLedgerOwned: false, credits: {} });
  assert.equal(model.sourceV3PublicStateFor(uncredited, servedMusic), "served_uncredited");
  assert.equal(model.sourceV3PublicStateFor(renditionLyrics({ credits: { translation: "  " } }), servedMusic), "served_uncredited");
  assert.equal(model.sourceV3PublicStateFor(renditionLyrics({ credits: { proofreading: "校对者" } }), servedMusic), "served");
  const wording = model.sourceV3SaveWording("served_uncredited");
  for (const text of [wording.button, wording.continueLabel, wording.saved, wording.title]) {
    assert.doesNotMatch(text, /并公开|保存即公开/, text);
  }
  assert.match(wording.saved, /还没有署名，暂不公开/);

  const markup = renderView(uncredited, { dirty: true, saveable: true });
  assert.match(markup, />保存（未署名，暂不公开）</);
  assert.doesNotMatch(markup, />保存并公开</);
  assert.match(markup, /class="lyrics-credit-notice" role="note"><strong>还没有署名，保存后不会公开<\/strong>/);
  assert.match(markup, /请在下面的“翻译”或“校对”栏填写署名/);
  assert.ok(markup.indexOf("lyrics-credit-notice") < markup.indexOf("校对者名称"), "the note sits above the credit fields");

  const credited = renderView(renditionLyrics({ recoveryLedgerOwned: false }), { dirty: true, saveable: true });
  assert.match(credited, />保存并公开</);
  assert.doesNotMatch(credited, /lyrics-credit-notice/);
});

test("every export warning the server emits reads in plain Chinese", () => {
  const codes = [
    "served_revision_differs", "unpublished_draft_replaced", "withdrawn_republished", "credit_missing", "source_missing",
    "source_unsupported", "source_url_reencoded", "source_differs", "rendition_inferred", "game_projection_not_ordered",
    "game_projection_differs", "side_label_differs", "trailing_performers_dropped", "unknown_performer",
    "ruby_not_served", "ruby_not_expressible", "english_dropped",
  ];
  for (const code of codes) {
    const text = model.lyricsDocumentWarningText({ code, message: "synthetic server message" });
    assert.notEqual(text, "synthetic server message", code);
    assert.match(text, /[\u4e00-\u9fff]/, code);
  }
  assert.equal(model.lyricsDocumentWarningText({ code: "withdrawn_republished", message: "m" }), "这首歌已撤下，按这份文档发布会重新公开它");
  assert.equal(model.lyricsDocumentWarningText({ code: "credit_missing", message: "m" }),
    "没有任何演唱版本带翻译或校对署名，服务器会拒绝发布；需要先补上署名");
  assert.equal(model.lyricsDocumentWarningText({ code: "translation_editions_dropped", message: "synthetic server message" }),
    "synthetic server message");
});

const convertButton = /<button type="button" class="btn btn-secondary btn-sm"[^>]*>转为可编辑<\/button>/;

test("the conversion is disabled with the way forward while the site does not serve the song", () => {
  const notServed = renderView(renditionLyrics({ credits: {} }), { sourceV3PublicState: "not_served" });
  assert.match(tag(notServed, convertButton), /disabled=""/);
  assert.match(notServed, /<span>这首歌还没有公开，公开站点上没有可转换的内容。请先在本页填好简中译文和署名并保存；公开页面收录这首歌后就可以转换。<\/span>/);

  const withdrawn = renderView(renditionLyrics(), { sourceV3PublicState: "withdrawn" });
  assert.match(tag(withdrawn, convertButton), /disabled=""/);
  assert.match(withdrawn, /<span>这首歌已撤下，公开站点上没有可转换的内容。请先由管理员恢复公开/);

  const editor = renderView(renditionLyrics({ credits: {} }), { role: "editor", sourceV3PublicState: "not_served" });
  assert.match(editor, /这首歌还没有公开，公开站点上没有可转换的内容/);
  assert.doesNotMatch(editor, convertButton);

  for (const sourceV3PublicState of ["served", "served_uncredited"]) {
    const markup = renderView(renditionLyrics(), { sourceV3PublicState });
    assert.match(markup, /<button type="button" class="btn btn-secondary btn-sm">转为可编辑<\/button>/, sourceV3PublicState);
    assert.doesNotMatch(markup, /没有可转换的内容/, sourceV3PublicState);
  }
});

test("the notice says Japanese and ruby are edited through the whole-song document route after conversion", () => {
  const markup = renderView(renditionLyrics());
  assert.match(markup, /转为可编辑后，日文和注音要通过整曲文档接口修改（例如交给 agent 提交），本页仍只读；分段、演唱者和段落空行可以在本页调整。/);
  assert.doesNotMatch(markup, /日文和注音可以修改/);
});

test("when the site serves nothing the dialog gives the way forward and lists no differences", async () => {
  const cases = [
    { warnings: [{ code: "credit_missing", message: "no rendition carries a credit" }], text: /<strong>这首歌还没有公开，公开站点上没有可转换的内容。<\/strong><span>请先在本页填好简中译文和署名并保存/ },
    {
      warnings: [{ code: "withdrawn_republished", message: "the song is withdrawn" }, { code: "credit_missing", message: "no rendition carries a credit" }],
      text: /<strong>这首歌已撤下，公开站点上没有可转换的内容。<\/strong><span>请先由管理员恢复公开/,
    },
  ];
  for (const { warnings, text } of cases) {
    const harness = openTakeover({
      exportResult: { ...exported, from: "database", servedVersion: 0, servedRevision: 0, warnings },
      takeover: async () => structuredClone(takeoverResult),
    });
    await harness.render().open();
    const takeover = harness.render();
    assert.equal(takeover.dialog.notServed, true);
    const markup = dialogMarkup(takeover);
    assert.match(markup, text);
    assert.doesNotMatch(markup, /差异|会有以下变化|保持不变/);
    for (const warning of warnings) assert.ok(!markup.includes(model.lyricsDocumentWarningText(warning)), warning.code);
    assert.match(tag(markup, /<button[^>]*>确认转换<\/button>/), /disabled=""/);
    await takeover.confirm();
    assert.deepEqual(harness.calls, [["export", 7]]);
  }
});

test("the dialog promises an unchanged public page only when no warning changes it", async () => {
  const unchanged = [
    { code: "source_url_reencoded", message: "m" },
    { code: "game_projection_differs", rendition: "sekai", message: "m" },
  ];
  const quiet = openTakeover({ exportResult: { ...exported, warnings: unchanged }, takeover: async () => structuredClone(takeoverResult) });
  await quiet.render().open();
  const quietMarkup = dialogMarkup(quiet.render());
  assert.match(quietMarkup, /<li>公开页面的内容保持不变；<\/li>/);
  assert.match(quietMarkup, /<strong>转换时会有以下差异（不改变公开页面的显示）：<\/strong><ul><li>来源链接会按规范格式重新记录/);
  assert.doesNotMatch(quietMarkup, /会有以下变化/);

  const none = openTakeover({ exportResult: { ...exported, warnings: [] }, takeover: async () => structuredClone(takeoverResult) });
  await none.render().open();
  const noneMarkup = dialogMarkup(none.render());
  assert.match(noneMarkup, /<li>公开页面的内容保持不变；<\/li>/);
  assert.doesNotMatch(noneMarkup, /差异|会有以下变化/);

  const mixedWarnings = [
    { code: "trailing_performers_dropped", rendition: "sekai", side: "full", line: 0, message: "m" },
    { code: "source_url_reencoded", message: "m" },
  ];
  const mixed = openTakeover({
    exportResult: { ...exported, warnings: mixedWarnings },
    takeover: async () => ({ ...structuredClone(takeoverResult), warnings: structuredClone(mixedWarnings) }),
  });
  await mixed.render().open();
  const mixedMarkup = dialogMarkup(mixed.render());
  assert.doesNotMatch(mixedMarkup, /公开页面的内容保持不变/);
  assert.match(mixedMarkup, /<li>公开页面上除下面列出的变化外，其余内容保持不变；<\/li>/);
  assert.match(mixedMarkup, /<strong>转换后公开页面上会有以下变化：<\/strong><ul><li>sekai Full 第 1 行：行尾演唱者无法保留<\/li><\/ul>/);
  assert.match(mixedMarkup, /<strong>转换时会有以下差异（不改变公开页面的显示）：<\/strong><ul><li>来源链接会按规范格式重新记录/);

  await mixed.render().confirm();
  const doneMarkup = dialogMarkup(mixed.render());
  assert.doesNotMatch(doneMarkup, /公开页面保持不变/);
  assert.match(doneMarkup, /已转为可编辑文档。公开页面上有以下变化：<\/p><ul class="lyrics-takeover-warnings"><li>sekai Full 第 1 行：行尾演唱者无法保留<\/li><\/ul>/);
  assert.match(doneMarkup, /另有以下差异，不改变公开页面的显示：<\/p><ul class="lyrics-takeover-warnings"><li>来源链接会按规范格式重新记录/);
});

test("a takeover refused for a missing producer proof can be retried once the proof is loaded again", async (t) => {
  const requests = stubFetch(t, () => ({ status: 200, body: takeoverResult }));
  api.clearLoadedProducerState();
  const missingProof = await api.takeOverLyricsDocument(7, 12).then(() => null, (error) => error);
  assert.ok(missingProof instanceof APIError);
  assert.equal(requests.length, 0);
  const failure = model.lyricsRecoveryTakeoverFailure(missingProof);
  assert.deepEqual({ retry: failure.retry, reload: failure.reload }, { retry: true, reload: false });

  let attempts = 0;
  const harness = openTakeover({
    takeover: async () => {
      if (attempts++ === 0) throw missingProof;
      return structuredClone(takeoverResult);
    },
  });
  await harness.render().open();
  await harness.render().confirm();
  let takeover = harness.render();
  assert.equal(takeover.dialog.phase, "failed");
  harness.state.writeLockedRef.current = true;
  let markup = dialogMarkup(takeover, { writeLocked: true });
  assert.match(markup, /<strong>内容版本还没有完成校对，这次没有转换，也没有改动任何数据。校对完成后可以重试。<\/strong>/);
  assert.match(tag(markup, /<button[^>]*>重试转换<\/button>/), /disabled=""/);
  assert.match(markup, /role="status" aria-live="polite">编辑器正在同步内容版本或协作文档，暂时不能转换；同步完成后按钮会恢复可用。<\/p>/);
  await takeover.confirm();
  assert.equal(attempts, 1);

  harness.state.writeLockedRef.current = false;
  takeover = harness.render();
  markup = dialogMarkup(takeover);
  assert.doesNotMatch(tag(markup, /<button[^>]*>重试转换<\/button>/), /disabled/);
  assert.doesNotMatch(markup, /编辑器正在同步/);
  await takeover.confirm();
  assert.equal(attempts, 2);
  assert.deepEqual(harness.calls, [["export", 7], ["takeover", 7, 12], ["takeover", 7, 12], ["catalog", "テスト"], ["reload", 7]]);
  assert.equal(harness.render().dialog.phase, "done");
});

test("an issue about a translation edition names the edition and the field in Chinese", () => {
  assert.equal(
    model.lyricsDocumentIssueText({ rendition: "sekai", side: "full", line: 2, field: "zhEditions", edition: "alt", message: "zh must be one line" }),
    `${model.lyricsDocumentIssueText({ rendition: "sekai", side: "full", line: 2, message: "x" }).replace(/：x$/, "")} · 译本 alt · 其他译本的简中：zh must be one line`,
  );
  assert.equal(
    model.lyricsDocumentIssueText({ rendition: "", field: "translationEditions", edition: "Alt!", message: "bad key" }),
    "译本 Alt! · 译本列表：bad key",
  );
});
