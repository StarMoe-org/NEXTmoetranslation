export interface FontCoverage { revision: string; fonts: { coverage: [number, number][] }[] }
export function missingFontCharacters(text: string, catalog: FontCoverage): string[] {
  const missing = new Set<number>();
  for (const char of text) {
    const code = char.codePointAt(0)!;
    if (code < 32 || (code >= 127 && code <= 159) || [0x61c, 0x200b, 0x200e, 0x200f, 0x2060].includes(code)) continue;
    const supported = catalog.fonts.some(font => {
      let left = 0, right = font.coverage.length;
      while (left < right) {
        const middle = (left + right) >>> 1, range = font.coverage[middle];
        if (code < range[0]) right = middle; else if (code > range[1]) left = middle + 1; else return true;
      }
      return false;
    });
    if (!supported) missing.add(code);
  }
  return [...missing].sort((a, b) => a - b).slice(0, 16).map(code => `U+${code.toString(16).toUpperCase().padStart(4, "0")}`);
}
