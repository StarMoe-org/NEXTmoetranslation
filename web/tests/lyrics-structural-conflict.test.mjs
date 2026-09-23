import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import { readLyricsEditor } from "./source-surfaces.mjs";


test("LyricsEditor turns malformed synced Yjs state into a terminal read-only conflict", async () => {
  const [editor, banner, css] = await Promise.all([
    readLyricsEditor(),
    readFile(new URL("../src/components/lyrics/LyricsCollaborationBanner.tsx", import.meta.url), "utf8"),
    readFile(new URL("../src/app/globals.css", import.meta.url), "utf8"),
  ]);
  const combined = `${banner}\n${editor}`;

  assert.match(editor, /snapshot\.error\?\.message === "invalid_lyrics_collaboration_document"/);
  assert.match(editor, /snapshot\.synced && snapshot\.document === null/);
  assert.match(editor, /collaborationStructuralConflictRef\.current = true;[\s\S]*collaborationGenerationRef\.current\+\+;[\s\S]*collaborationRef\.current = null;[\s\S]*conflicted\?\.destroy\(\)/);
  assert.match(editor, /collaborationAuthoritativeRef\.current = authoritative/);
  assert.match(editor, /const writeLocked = producerWriteLocked \|\| collaborationStructuralConflict/);
  assert.match(combined, /\u534f\u4f5c\u6587\u6863\u53d1\u751f\u7ed3\u6784\u51b2\u7a81\uff0c\u5df2\u5207\u6362\u4e3a\u53ea\u8bfb/);
  assert.match(combined, /\u7f16\u8f91\u4e0e\u4fdd\u5b58\u5df2\u505c\u7528/);
  assert.match(combined, /\u8bf7\u91cd\u65b0\u52a0\u8f7d\u5f53\u524d\u6b4c\u8bcd\uff1b\u82e5\u91cd\u65b0\u52a0\u8f7d\u540e\u4ecd\u51fa\u73b0\u6b64\u72b6\u6001\uff0c\u8bf7\u8054\u7cfb\u5176\u4ed6\u534f\u4f5c\u8005\u505c\u6b62\u7f16\u8f91/);
  assert.match(combined, /collaborationStructuralConflict && <button[\s\S]*?\u91cd\u65b0\u52a0\u8f7d\u6b4c\u8bcd<\/button>/);
  assert.match(combined, /!collaborationStructuralConflict && !localSourceImportDraft && collaborationStatus === "error"[\s\S]*reconnectNow\(\)/);
  assert.match(css, /\.lyrics-collaboration-state\.structural-conflict/);
  assert.match(css, /\.lyrics-structural-conflict-empty/);
});
