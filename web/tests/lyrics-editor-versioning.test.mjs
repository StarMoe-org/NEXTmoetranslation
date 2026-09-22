import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

test("LyricsEditor selects stable rendition families without merging equal text", async () => {
  const [editor, metadata] = await Promise.all([
    readFile(new URL("../src/components/LyricsEditor.tsx", import.meta.url), "utf8"),
    readFile(new URL("../src/components/lyrics/LyricsMetadataCard.tsx", import.meta.url), "utf8"),
  ]);
  const combined = `${editor}\n${metadata}`;

  assert.match(editor, /\[activeRenditionKey, setActiveRenditionKey\] = useState\(""\)/);
  assert.match(editor, /useState<"full" \| "game">\("full"\)/);
  assert.match(editor, /lyricsRenditionByKey\(lyrics, activeRenditionKey\)/);
  assert.match(editor, /renditionKeys\.map\(\(renditionKey: string\)/);
  assert.match(editor, /每个 stable key 都保留自己的 Full \/ Game、relation、演唱者分段、ruby、翻译与翻译\/校对署名/);
  assert.match(editor, /即使文本相同也不会与其他 family 合并/);
  assert.match(editor, /projectGameLyricsLines\(lyrics, activeRenditionKey\)/);
  assert.match(editor, /const gameSideReadOnlyReason = !activeRendition \|\| activeRendition\.relation\.kind === "exact_projection"/);
  assert.match(editor, /const activeSideReadOnly = activeVersion === "game" && gameSideReadOnlyReason !== null/);
  assert.match(editor, /Game <span>\{gameSideReadOnlyReason === "exact_projection" \? "只读 exact projection" : "独立简中可编辑"\}<\/span>/);
  assert.match(editor, /independent Game 简中按该 stable key\/side 独立保存/);
  assert.match(editor, /const saved = importToken\s*\?\s*await saveLyrics\(lyrics, importToken\)\s*:\s*await checkpointLyrics\(musicID\)/);
  assert.doesNotMatch(editor, /renditionKey: activeRendition\.key, side: activeVersion/);
  assert.match(editor, /activeTarget: \(\) => selectedMusicIDRef\.current == null \? null/);
  assert.match(editor, /renditionKey: activeRendition\?\.key \|\| ""/);
  assert.match(editor, /side: activeVersion/);
  assert.match(editor, /projectionKind,/);
  assert.match(editor, /Full <span>\{activeRendition \? "仅简中可编辑" : "可编辑"\}<\/span>/);
  assert.match(editor, /writeLocked \|\| activeSideReadOnly/);
  assert.match(combined, /value=\{activeTranslationCredit\}[\s\S]*updateActiveCredits\("translation"/);
  assert.match(combined, /value=\{activeProofreadingCredit\}[\s\S]*updateActiveCredits\("proofreading"/);
  assert.match(combined, /maxLength=\{activeRendition \? 2048 : undefined\}/);
  assert.match(editor, /snapshot: \(\) => \(\{/);
  assert.match(editor, /isDirty: \(\) => lyricsRef\.current != null/);
  assert.match(editor, /discard: \(\) => boolean/);
  assert.doesNotMatch(editor, /gameProjection\?\.lines/);
});

test("authoritative conflict reload retains the current stable rendition side", async () => {
  const editor = await readFile(new URL("../src/components/LyricsEditor.tsx", import.meta.url), "utf8");
  const conflictStart = editor.indexOf('<Modal open={confirmConflictReload');
  const conflictReload = editor.slice(conflictStart, editor.indexOf('open={editionWorkflow', conflictStart));

  assert.match(conflictReload, /loadConflictAuthoritative\(\)/);
  assert.match(editor, /loadConflictAuthoritative[\s\S]*getLyrics\(musicID, preferredEditionKey\)[\s\S]*acceptAuthoritativeDocument\(authoritative, activeRenditionKey, activeVersion\)/);
  assert.match(editor, /acceptAuthoritativeDocument[\s\S]*retainedLyricsTranslationTarget\(editable, preferredRenditionKey, preferredVersion\)/);
  assert.match(editor, /setActiveRenditionKey\(retainedTarget\.renditionKey\)/);
  assert.match(editor, /setActiveVersion\(retainedTarget\.version === "game" \? "game" : "full"\)/);
});

test("save and publish preflight block dangling Game references before network mutation", async () => {
  const editor = await readFile(new URL("../src/components/LyricsEditor.tsx", import.meta.url), "utf8");
  const savePreflight = editor.indexOf("const preflightProblems = lyricsVersionSaveProblems(lyrics);");
  const saveRequest = editor.indexOf("await checkpointLyrics(musicID)");

  assert.ok(savePreflight >= 0 && savePreflight < saveRequest);
  assert.match(editor, /Rendition \/ projection 或公开署名合同无效，未发送保存请求/);
  assert.match(editor, /referencedGameFullLineIds\(lyrics, activeRenditionKey\)\.includes\(removedLineID\)/);
  assert.match(editor, /Game 投影需要先修复，未打开发布操作/);
  assert.match(editor, /每个 stable key 的 Full 与 Game 都保持在本 family 内/);
});

test("ruby editing, performer squares, and private component provenance remain explicit", async () => {
  const [editor, lineEditor, css] = await Promise.all([
    readFile(new URL("../src/components/LyricsEditor.tsx", import.meta.url), "utf8"),
    readFile(new URL("../src/components/lyrics/LyricsLineEditor.tsx", import.meta.url), "utf8"),
    readFile(new URL("../src/app/globals.css", import.meta.url), "utf8"),
  ]);

  assert.match(lineEditor, /Ruby 注音（可编辑）/);
  assert.match(lineEditor, /className="lyric-performer-swatch"/);
  assert.match(lineEditor, /line\.trailingPerformerIds !== undefined/);
  assert.match(lineEditor, /行尾演唱者/);
  assert.match(css, /\.lyric-performer-swatch \{[^}]*border-radius: 0;[^}]*box-shadow: none;/s);
  assert.match(editor, /组件 provenance/);
  assert.match(editor, /仅认证编辑器显示固定证据；公开输出使用对应版本的严格 attribution contract/);
  assert.match(editor, /resolvedLyricsComponentProvenance\(lyrics, activeRenditionKey\)/);
});

test("VOCALOID-only lyrics omit performer controls, squares, and publication requirements", async () => {
  const [editor, lineEditor, review] = await Promise.all([
    readFile(new URL("../src/components/LyricsEditor.tsx", import.meta.url), "utf8"),
    readFile(new URL("../src/components/lyrics/LyricsLineEditor.tsx", import.meta.url), "utf8"),
    readFile(new URL("../src/components/LyricsSourceReview.tsx", import.meta.url), "utf8"),
  ]);

  assert.match(editor, /lyricsHasPerformerSegmentation\(lyrics, activeRenditionKey, activeVersion\)/);
  assert.match(editor, /lyricsHasPerformerSegmentation\(lyrics, target\.key === "legacy-v2" \? undefined : target\.key, target\.version\)/);
  assert.match(editor, /line\.segments\.some\(\(segment\) => segment\.performerIds\.length === 0\)/);
  assert.match(editor, /segment\.performerIds\.length > 0 && <span className="lyric-performer-squares"/);
  assert.match(lineEditor, /\{showPerformerSegmentation && <>[\s\S]*lyric-performer-summary/);
  assert.match(lineEditor, /\{showPerformerSegmentation && <span className="lyric-structure-actions">/);
  assert.match(review, /selectedVersion\.kind !== "vocaloid" \|\|[\s\S]*lyricsHasPerformerSegmentation\(detail\.analysis/);
  assert.match(review, /showPerformerSegmentation && segment\.performerIds\.length > 0/);
  assert.match(review, /showPerformerSegmentation && line\.trailingPerformerIds\.length > 0/);
});

test("catalog listing requests every song and delegates total size to cursor pagination", async () => {
  const [editor, api] = await Promise.all([
    readFile(new URL("../src/components/LyricsEditor.tsx", import.meta.url), "utf8"),
    readFile(new URL("../src/lib/api.ts", import.meta.url), "utf8"),
  ]);

  assert.match(editor, /getCatalogMusic\(search, false\)/);
  assert.match(api, /collectCatalogPages\(async \(cursor: string\)/);
  assert.match(api, /if \(cursor\) p\.set\("cursor", cursor\)/);
  assert.doesNotMatch(`${editor}\n${api}`, /\b701\b/);
});

test("embedded Public Lyrics metadata remains independent from editable SQLite state", async () => {
  const [editor, sidebar, api] = await Promise.all([
    readFile(new URL("../src/components/LyricsEditor.tsx", import.meta.url), "utf8"),
    readFile(new URL("../src/components/lyrics/LyricsCatalogSidebar.tsx", import.meta.url), "utf8"),
    readFile(new URL("../src/lib/api.ts", import.meta.url), "utf8"),
  ]);
  const combined = `${editor}\n${sidebar}`;

  assert.match(api, /interface RuntimeLyricsMetadata/);
  assert.match(api, /immutableOverlay: boolean/);
  assert.match(api, /runtimeLyrics\?: RuntimeLyricsMetadata/);
  assert.match(api, /lyricsAvailabilityState\?:/);
  assert.match(combined, /数据库：\{databaseLyricsStatusLabel\(item\)\}/);
  assert.match(editor, /数据库已记录歌词可用性，但当前没有可编辑正文/);
  assert.match(combined, /公开镜像：\{runtimeLyricsStateLabel\(item\.runtimeLyrics\.state\)\}/);
  // The public-mirror line reflects whatever is currently served (bundle or database
  // publication); only the read-only 404 fallback is tied to the embedded release.
  assert.match(sidebar, /\{item\.runtimeLyrics && \(/);
  assert.doesNotMatch(sidebar, /item\.runtimeLyrics\?\.immutableOverlay &&/);
  assert.match(api, /source\?: "bundle" \| "db_publication" \| "localization_projection"/);
  assert.match(editor, /reason instanceof APIError && reason\.status === 404 && item\.runtimeLyrics\?\.immutableOverlay/);
  assert.match(editor, /setRuntimeOnlyMissingDatabaseSource\(true\)/);
  assert.match(editor, /公开镜像仍在，后台数据库尚无可编辑源/);
  assert.match(editor, /系统不会把 detail 404 静默转换成可保存的空草稿/);

  const availabilityGuard = editor.indexOf("reason instanceof APIError && reason.status === 404 && item.lyricsAvailabilityState");
  const runtimeGuard = editor.indexOf("reason instanceof APIError && reason.status === 404 && item.runtimeLyrics?.immutableOverlay");
  assert.ok(availabilityGuard >= 0 && availabilityGuard < runtimeGuard, "database availability must be handled before runtime-only fallback");
  assert.ok(runtimeGuard >= 0, "runtime-only 404 must remain a distinct read-only state");
  assert.doesNotMatch(editor, /emptyLyrics|const blank =/);
  assert.match(editor, /reason instanceof APIError && reason\.status === 404\) \{[\s\S]*startCollaboration\(item\.musicId\)/);
});
