const ALIGNMENT_GAP = -5;
const INCOMPATIBLE = Number.NEGATIVE_INFINITY;
const KANA = /[\u3040-\u30ff]/;

function splitSpeaker(value) {
  return String(value || "").split("_", 1)[0];
}

function sameArray(left, right) {
  return JSON.stringify(left || []) === JSON.stringify(right || []);
}

function parseScenarioSourceTalks(rawJSON) {
  let raw;
  try {
    raw = JSON.parse(rawJSON);
  } catch {
    throw new Error("scenario JSON is invalid");
  }
  if (!raw || typeof raw !== "object" || typeof raw.ScenarioId !== "string" ||
      !Array.isArray(raw.Snippets) || !Array.isArray(raw.TalkData) || !Array.isArray(raw.SpecialEffectData)) {
    throw new Error("scenario JSON structure is invalid");
  }

  const talks = [];
  for (const snippet of raw.Snippets) {
    if (!snippet || !Number.isSafeInteger(snippet.Action) || !Number.isSafeInteger(snippet.ReferenceIndex)) continue;
    const reference = Number(snippet.ReferenceIndex);
    if (snippet.Action === 1) {
      const talk = raw.TalkData[reference];
      if (!talk || reference < 0) continue;
      const voices = Array.isArray(talk.Voices) ? talk.Voices : [];
      talks.push({
        speaker: splitSpeaker(typeof talk.WindowDisplayName === "string" ? talk.WindowDisplayName : ""),
        text: typeof talk.Body === "string" ? talk.Body : "",
        voices: voices.flatMap((voice) => typeof voice?.VoiceId === "string" ? [voice.VoiceId] : []),
        volume: voices.map((voice) => typeof voice?.Volume === "number" ? Math.trunc(voice.Volume) : 0),
        chara2d: voices.length > 0 && typeof voices[0]?.Character2dId === "number" ? Math.trunc(voices[0].Character2dId) : 0,
        charIndex: 0,
        talkDataIndex: reference,
      });
      if (typeof talk.WhenFinishCloseWindow === "number" && talk.WhenFinishCloseWindow !== 0) {
        talks.push({ speaker: "", text: "", charIndex: 0 });
      }
    } else if (snippet.Action === 6) {
      const effect = raw.SpecialEffectData[reference];
      if (!effect || reference < 0 || typeof effect.EffectType !== "number" || ![8, 18, 23].includes(effect.EffectType)) continue;
      const speaker = effect.EffectType === 8 ? "场景" : effect.EffectType === 18 ? "左上场景" : "选项";
      talks.push({
        speaker,
        text: typeof effect.StringVal === "string" ? effect.StringVal : "",
        charIndex: 0,
        effectType: effect.EffectType,
      });
      talks.push({ speaker: "", text: "", charIndex: 0, separatorEffectType: effect.EffectType });
    }
  }
  if (talks.at(-1)?.speaker === "" && talks.at(-1)?.text === "") talks.pop();
  return { scenarioId: raw.ScenarioId, talks };
}

function snapshotScenarioState(snapshot) {
  if (!snapshot || typeof snapshot !== "object" || typeof snapshot.revision !== "string" || !snapshot.revision) {
    throw new Error("event episode snapshot revision is required");
  }
  if (!Number.isSafeInteger(snapshot.eventId) || snapshot.eventId <= 0 || typeof snapshot.episodeNo !== "string" || !snapshot.episodeNo) {
    throw new Error("event episode identity is invalid");
  }
  return scenarioState(snapshot, "event");
}

function sideStorySnapshotState(snapshot) {
  if (!snapshot || typeof snapshot !== "object" || typeof snapshot.revision !== "string" || !snapshot.revision) {
    throw new Error("side story episode snapshot revision is required");
  }
  if ((snapshot.kind !== "card" && snapshot.kind !== "area") || typeof snapshot.id !== "string" || !snapshot.id ||
      typeof snapshot.episode !== "string" || !snapshot.episode) {
    throw new Error("side story episode identity is invalid");
  }
  return scenarioState(snapshot, "side story");
}

function scenarioState(snapshot, label) {
  if (!snapshot.scenario || typeof snapshot.scenario.fileName !== "string" ||
      !snapshot.scenario.fileName.toLocaleLowerCase().endsWith(".json") ||
      /[/\\\u0000-\u001f\u007f]/.test(snapshot.scenario.fileName) || snapshot.scenario.fileName.includes("..")) {
    throw new Error("scenario file name is unsafe");
  }
  if (snapshot.scenario.parserVersion !== 1) throw new Error("unsupported scenario parser version");
  if (!Array.isArray(snapshot.scenario.sourceTalks) || !Array.isArray(snapshot.segments)) {
    throw new Error(`${label} episode snapshot structure is invalid`);
  }

  const parsed = parseScenarioSourceTalks(snapshot.scenario.rawJson);
  if (parsed.scenarioId !== snapshot.scenario.scenarioId) throw new Error("scenario JSON identity mismatch");
  if (parsed.talks.length !== snapshot.scenario.sourceTalks.length) throw new Error("scenario SourceTalk count mismatch");
  parsed.talks.forEach((talk, index) => {
    const provided = snapshot.scenario.sourceTalks[index];
    if (!provided || talk.speaker !== provided.speaker || talk.text !== provided.text ||
        talk.talkDataIndex !== provided.talkDataIndex || (talk.chara2d || 0) !== (provided.chara2d || 0) ||
        !sameArray(talk.voices, provided.voices) || !sameArray(talk.volume, provided.volume)) {
      throw new Error(`scenario SourceTalk mismatch at position ${index}`);
    }
  });

  const segments = snapshot.segments.filter((segment) => segment.kind !== "title");
  const byPosition = new Map();
  for (const segment of segments) {
    if (!segment || !Number.isSafeInteger(segment.position) || segment.position < 0 || byPosition.has(segment.position)) {
      throw new Error(`invalid or duplicate ${label} segment position ${segment?.position}`);
    }
    byPosition.set(segment.position, segment);
  }

  for (const source of parsed.talks) {
    if (source.talkDataIndex === undefined) continue;
    if (!Number.isSafeInteger(source.talkDataIndex) || source.talkDataIndex < 0) {
      throw new Error(`invalid TalkData index ${source.talkDataIndex}`);
    }
    const body = byPosition.get(source.talkDataIndex * 2);
    const speaker = byPosition.get(source.talkDataIndex * 2 + 1);
    if (body && body.japanese !== source.text) throw new Error(`scenario body mismatch at TalkData ${source.talkDataIndex}`);
    if (speaker && splitSpeaker(speaker.japanese) !== source.speaker) {
      throw new Error(`scenario speaker mismatch at TalkData ${source.talkDataIndex}`);
    }
  }
  return { talks: parsed.talks, byPosition };
}

async function sha256(value) {
  if (!globalThis.crypto?.subtle) throw new Error("SHA-256 is unavailable");
  const digest = await globalThis.crypto.subtle.digest("SHA-256", new TextEncoder().encode(value));
  return Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, "0")).join("");
}

export async function validateEventEpisodeSnapshot(snapshot) {
  snapshotScenarioState(snapshot);
  await verifyScenarioSHA256(snapshot);
}

// A side-story snapshot must describe exactly the episode and locale the editor asked for.
export async function validateSideStoryEpisodeSnapshot(snapshot, expected) {
  sideStorySnapshotState(snapshot);
  if (snapshot.kind !== expected.kind || snapshot.id !== expected.id || snapshot.episode !== expected.episode ||
      snapshot.locale !== expected.locale) {
    throw new Error("side story snapshot identity mismatch");
  }
  await verifyScenarioSHA256(snapshot);
}

async function verifyScenarioSHA256(snapshot) {
  const expected = String(snapshot.scenario.sha256 || "").toLocaleLowerCase().replace(/^sha256:/, "");
  if (!/^[a-f0-9]{64}$/.test(expected)) throw new Error("scenario SHA-256 is invalid");
  if (await sha256(snapshot.scenario.rawJson) !== expected) throw new Error("scenario SHA-256 mismatch");
}

function normalizeImportedEventText(speaker, text) {
  if (speaker === "场景" || speaker === "左上场景" || speaker === "") {
    return text.split("\n").map((part) => part.trim()).join("\n");
  }
  const normalized = text.split("\n", 1)[0].trim();
  if (speaker === "选项") return normalized.includes("/") ? normalized : `${normalized}/`;
  return normalized;
}

// SekaiText's Simplified Chinese dialogue punctuation; other locales keep the TXT as written.
function chineseDialoguePunctuation(text) {
  return text
    .replaceAll("…", "...")
    .replaceAll("(", "（")
    .replaceAll(")", "）")
    .replaceAll(",", "，")
    .replaceAll("?", "？")
    .replaceAll("!", "！")
    .replaceAll("~", "～")
    .replaceAll("欸", "诶");
}

export function parseEventTxtContent(rawContent) {
  let content = String(rawContent || "").replace(/^\uFEFF/, "");
  if (content.startsWith("#SekaiText ")) {
    const headerEnd = content.indexOf("\n");
    if (headerEnd > 0) content = content.slice(headerEnd + 1);
  }

  const talks = [];
  let previousBlank = false;
  for (const [lineIndex, originalLine] of content.split("\n").entries()) {
    const line = originalLine.replace(":", "：");
    let speaker = "";
    let fullText = "";
    const separator = line.indexOf("：");
    if (separator >= 0) {
      speaker = line.slice(0, separator).trim();
      fullText = line.slice(separator + 1);
    } else if (line.includes("/")) {
      speaker = "选项";
      fullText = line;
    } else if (line.trim() === "") {
      fullText = line;
    } else {
      speaker = "场景";
      fullText = line;
    }
    if (speaker === "") {
      if (previousBlank) continue;
      previousBlank = true;
    } else {
      previousBlank = false;
    }
    const parts = fullText.split("\\N");
    for (const [partIndex, part] of parts.entries()) {
      talks.push({
        idx: lineIndex + 1,
        speaker,
        text: normalizeImportedEventText(speaker, part),
        start: partIndex === 0,
        end: partIndex === parts.length - 1,
        checked: true,
        save: true,
        dstidx: talks.length,
      });
    }
  }
  if (previousBlank && talks.length > 0) talks.pop();
  talks.forEach((talk, index) => { talk.dstidx = index; });
  return talks;
}

function importedRows(talks, locale) {
  const rows = [];
  for (const talk of talks) {
    if (!Number.isSafeInteger(talk?.idx) || talk.idx <= 0 || typeof talk.speaker !== "string" || typeof talk.text !== "string") {
      throw new Error("TXT parser returned an invalid row");
    }
    const current = rows.at(-1);
    if (current && current.line === talk.idx) {
      if (current.speaker !== talk.speaker) throw new Error(`TXT parser returned conflicting speakers on line ${talk.idx}`);
      current.text += `${current.text ? "\n" : ""}${talk.text}`;
      continue;
    }
    const kind = talk.speaker === "选项"
      ? "choice"
      : talk.speaker === "" && talk.text === ""
        ? "separator"
        : ["场景", "左上场景", ""].includes(talk.speaker)
          ? "scene"
          : "dialogue";
    rows.push({ line: talk.idx, speaker: talk.speaker, text: talk.text, kind });
  }
  if (locale === "zh-CN") {
    for (const row of rows) if (row.kind === "dialogue") row.text = chineseDialoguePunctuation(row.text);
  }
  return rows;
}

function sourceRows(state) {
  return state.talks.map((talk, order) => {
    const speaker = talk.talkDataIndex === undefined ? undefined : state.byPosition.get(talk.talkDataIndex * 2 + 1);
    const kind = talk.separatorEffectType !== undefined || talk.speaker === "" && talk.text === ""
      ? "separator"
      : talk.effectType === 23 || talk.speaker === "选项"
        ? "choice"
        : talk.effectType !== undefined || ["场景", "左上场景", ""].includes(talk.speaker)
          ? "scene"
          : "dialogue";
    return {
      order,
      talk,
      kind,
      acceptedSpeakers: [...new Set([talk.speaker, speaker?.text ? splitSpeaker(speaker.text) : ""].filter(Boolean))],
    };
  });
}

function alignmentScore(source, imported) {
  if (source.kind !== imported.kind) return INCOMPATIBLE;
  if (source.kind === "separator" || source.kind === "choice") return 12;
  if (source.kind === "scene") return 7;
  return source.acceptedSpeakers.includes(splitSpeaker(imported.speaker)) ? 15 : 6;
}

function alignmentCandidates(source, imported) {
  const forward = Array.from({ length: source.length + 1 }, () => Array(imported.length + 1).fill(INCOMPATIBLE));
  const backward = Array.from({ length: source.length + 1 }, () => Array(imported.length + 1).fill(INCOMPATIBLE));
  forward[0][0] = 0;
  for (let i = 1; i <= source.length; i++) forward[i][0] = forward[i - 1][0] + ALIGNMENT_GAP;
  for (let j = 1; j <= imported.length; j++) forward[0][j] = forward[0][j - 1] + ALIGNMENT_GAP;
  for (let i = 1; i <= source.length; i++) {
    for (let j = 1; j <= imported.length; j++) {
      const score = alignmentScore(source[i - 1], imported[j - 1]);
      forward[i][j] = Math.max(
        forward[i - 1][j] + ALIGNMENT_GAP,
        forward[i][j - 1] + ALIGNMENT_GAP,
        score === INCOMPATIBLE ? INCOMPATIBLE : forward[i - 1][j - 1] + score,
      );
    }
  }
  backward[source.length][imported.length] = 0;
  for (let i = source.length - 1; i >= 0; i--) backward[i][imported.length] = backward[i + 1][imported.length] + ALIGNMENT_GAP;
  for (let j = imported.length - 1; j >= 0; j--) backward[source.length][j] = backward[source.length][j + 1] + ALIGNMENT_GAP;
  for (let i = source.length - 1; i >= 0; i--) {
    for (let j = imported.length - 1; j >= 0; j--) {
      const score = alignmentScore(source[i], imported[j]);
      backward[i][j] = Math.max(
        backward[i + 1][j] + ALIGNMENT_GAP,
        backward[i][j + 1] + ALIGNMENT_GAP,
        score === INCOMPATIBLE ? INCOMPATIBLE : score + backward[i + 1][j + 1],
      );
    }
  }
  const optimum = forward[source.length][imported.length];
  const sourceCandidates = source.map(() => []);
  const importedCandidates = imported.map(() => []);
  for (let i = 0; i < source.length; i++) {
    for (let j = 0; j < imported.length; j++) {
      const score = alignmentScore(source[i], imported[j]);
      if (score === INCOMPATIBLE || forward[i][j] + score + backward[i + 1][j + 1] !== optimum) continue;
      sourceCandidates[i].push(j);
      importedCandidates[j].push(i);
    }
  }
  return { sourceCandidates, importedCandidates };
}

// repeatsJapanese(value, japanese) tells whether a value is still the untranslated Japanese.
function translationPreviewRow(source, imported, segment, target, importedValue, rowID, repeatsJapanese) {
  const japanese = target === "speaker" ? splitSpeaker(segment.japanese) : segment.japanese;
  const current = segment.text || "";
  const base = {
    id: rowID(segment, target),
    target,
    sourceOrder: source.order,
    importedLine: imported.line,
    segmentId: segment.id,
    segmentPosition: segment.position,
    sourceHash: segment.sourceHash,
    revision: segment.revision ?? 0,
    speaker: source.talk.speaker,
    japanese,
    current,
    imported: importedValue,
  };
  if (!importedValue || repeatsJapanese(importedValue, japanese)) {
    return { ...base, status: "missing", reason: importedValue ? "TXT 仍是当前日文原文，不会把原文写入译文字段" : "TXT 对应译文为空", selectable: false, selectedByDefault: false };
  }
  if (importedValue === current) {
    return { ...base, status: "matched", reason: "TXT 译文与当前权威译文一致，无需写入草稿", selectable: false, selectedByDefault: false };
  }
  if (current && !repeatsJapanese(current, japanese)) {
    return { ...base, status: "conflict", reason: "TXT 译文与当前权威译文不同；检查后可显式选择覆盖到本地草稿", selectable: true, selectedByDefault: false };
  }
  return { ...base, status: "matched", reason: "已按权威场景结构与 segment 身份对齐", selectable: true, selectedByDefault: true };
}

export function eventEpisodeTxtImportPreview(snapshot, talks) {
  return txtImportPreview(snapshot, snapshotScenarioState(snapshot), talks, (segment, target) => `${segment.id}:${target}`,
    (value, japanese) => value === japanese);
}

// Side-story segments are keyed by their Japanese text, so one key can occur at several
// TalkData positions; rows are identified by position and only the first occurrence is saved.
// Like the server's official import, text equal to kana-free Japanese (a shared name) is a translation.
export function sideStoryEpisodeTxtImportPreview(snapshot, talks) {
  const preview = txtImportPreview(snapshot, sideStorySnapshotState(snapshot), talks, (segment, target) => `${segment.position}:${target}`,
    (value, japanese) => value === japanese && KANA.test(japanese));
  const firstByKey = new Map();
  const rows = preview.rows.map((row) => {
    if (!row.segmentId || row.status === "missing") return row;
    const first = firstByKey.get(row.segmentId);
    if (!first) {
      firstByKey.set(row.segmentId, row);
      return row;
    }
    return row.imported === first.imported
      ? { ...row, status: "matched", reason: "同一日文在本话重复出现，TXT 译文与首次出现一致，随首次出现一并保存", selectable: false, selectedByDefault: false }
      : { ...row, status: "conflict", reason: "同一日文在本话重复出现但 TXT 译文不同；每个日文只保存一个译文，以首次出现为准", selectable: false, selectedByDefault: false };
  });
  const counts = { matched: 0, conflict: 0, missing: 0, unmatched: 0 };
  rows.forEach((row) => { counts[row.status]++; });
  return { revision: preview.revision, rows, counts };
}

/** One PUT batch for the selected rows, each carrying the revision the preview was built from. */
export function sideStoryTxtImportEdits(preview, selectedRowIDs) {
  const edits = new Map();
  for (const row of preview.rows) {
    if (!row.selectable || !selectedRowIDs.has(row.id) || !row.segmentId) continue;
    if (edits.has(row.segmentId)) throw new Error(`side story TXT import selected ${row.segmentId} twice`);
    edits.set(row.segmentId, { jp: row.segmentId, text: row.imported, source: "human", expectedRevision: row.revision ?? 0 });
  }
  return [...edits.values()];
}

function txtImportPreview(snapshot, state, talks, rowID, repeatsJapanese) {
  const source = sourceRows(state);
  const imported = importedRows(talks, snapshot.locale);
  if (source.length > 2000 || imported.length > 2000 || source.length * imported.length > 1000000) {
    throw new Error("event TXT alignment is too large for a local preview");
  }
  const { sourceCandidates, importedCandidates } = alignmentCandidates(source, imported);
  const rows = [];
  const pairedImports = new Set();

  source.forEach((sourceRow, sourceIndex) => {
    const candidates = sourceCandidates[sourceIndex];
    const importedIndex = candidates.length === 1 && importedCandidates[candidates[0]].length === 1 ? candidates[0] : -1;
    if (importedIndex < 0) {
      const status = candidates.length ? "conflict" : "missing";
      rows.push({
        id: `source:${sourceIndex}`,
        status,
        target: "structure",
        sourceOrder: sourceRow.order,
        speaker: sourceRow.talk.speaker,
        japanese: sourceRow.talk.text,
        current: "",
        imported: "",
        reason: candidates.length
          ? `该权威场景行存在多个同分结构候选（TXT 行 ${candidates.map((index) => imported[index].line).join("、")}），未按行号猜测`
          : "权威场景行在 TXT 中没有可对齐行",
        selectable: false,
        selectedByDefault: false,
      });
      return;
    }

    pairedImports.add(importedIndex);
    const importedRow = imported[importedIndex];
    if (sourceRow.talk.talkDataIndex === undefined) return;
    const body = state.byPosition.get(sourceRow.talk.talkDataIndex * 2);
    if (!body) {
      rows.push({
        id: `source:${sourceIndex}:body`, status: "conflict", target: "structure",
        sourceOrder: sourceRow.order, importedLine: importedRow.line, speaker: sourceRow.talk.speaker,
        japanese: sourceRow.talk.text, current: "", imported: importedRow.text,
        reason: "权威场景行缺少可写入的 body segment 身份", selectable: false, selectedByDefault: false,
      });
      return;
    }
    rows.push(translationPreviewRow(sourceRow, importedRow, body, "body", importedRow.text, rowID, repeatsJapanese));
    if (sourceRow.kind === "dialogue") {
      const speaker = state.byPosition.get(sourceRow.talk.talkDataIndex * 2 + 1);
      if (speaker) rows.push(translationPreviewRow(sourceRow, importedRow, speaker, "speaker", splitSpeaker(importedRow.speaker), rowID, repeatsJapanese));
    }
  });

  imported.forEach((importedRow, importedIndex) => {
    if (pairedImports.has(importedIndex)) return;
    const candidates = importedCandidates[importedIndex];
    rows.push({
      id: `import:${importedRow.line}:${importedIndex}`,
      status: candidates.length ? "conflict" : "unmatched",
      target: "structure",
      importedLine: importedRow.line,
      speaker: importedRow.speaker,
      japanese: "",
      current: "",
      imported: importedRow.text,
      reason: candidates.length
        ? `TXT 行可对应多个权威场景位置（${candidates.map((index) => source[index].order + 1).join("、")}），未按行号猜测`
        : "TXT 行在当前权威场景中没有兼容结构",
      selectable: false,
      selectedByDefault: false,
    });
  });

  const counts = { matched: 0, conflict: 0, missing: 0, unmatched: 0 };
  rows.forEach((row) => { counts[row.status]++; });
  return { revision: snapshot.revision, rows, counts };
}
