import { readFile, readdir } from "node:fs/promises";

// Source-text contract tests read component source by path. Console.tsx and
// LyricsEditor.tsx are composition only; their behaviour lives in the
// console/ and lyrics/ modules, so tests read each surface as one string.
export const read = (path) => readFile(new URL(`../${path}`, import.meta.url), "utf8");

export const readConsoleSurface = async () => {
  const modules = (await readdir(new URL("../src/components/console/", import.meta.url))).sort();
  const sources = await Promise.all([
    read("src/components/Console.tsx"),
    ...modules.map((name) => read(`src/components/console/${name}`)),
  ]);
  return sources.join("\n");
};

// Explicit order: slice-based assertions rely on LyricsEditor.tsx coming first
// and on useLyricsPersistence.ts keeping its internal order. The six display
// components under lyrics/ are deliberately excluded so negative assertions
// keep their original scope.
const LYRICS_EDITOR_MODULES = [
  "LyricsEditor.tsx", "lyrics/lyricsDocumentModel.ts", "lyrics/lyricsDocumentCommands.ts",
  "lyrics/lyricsEditorState.ts", "lyrics/useLyricsDocumentLoader.ts", "lyrics/useLyricsDocumentCommands.ts",
  "lyrics/useLyricsSourceWorkflow.ts", "lyrics/useLyricsPersistence.ts", "lyrics/LyricsDocumentView.tsx",
];

export const readLyricsEditor = async () =>
  (await Promise.all(LYRICS_EDITOR_MODULES.map((name) => read(`src/components/${name}`)))).join("\n");
