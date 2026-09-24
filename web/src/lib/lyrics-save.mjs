import { isRenditionLyricsDocument, lyricsVersionSaveProblems } from "./lyrics-versioning.mjs";
import {
  isTranslationEditionKey,
  validateTranslationEditionSummaries,
} from "./lyrics-editions.mjs";

const SONG_LYRICS_KEYS = new Set([
  "musicId", "status", "revision", "publishedRevision", "updatedAt", "attribution",
  "translationCredit", "proofreadingCredit", "sourceNote", "sourceUrl", "licenseNote", "sourcePageId", "sourceRevisionId",
  "sourceSha1", "sourceFetchedAt", "lines",
]);
const RENDITION_DOCUMENT_KEYS = new Set([
  "musicId", "status", "revision", "publishedRevision", "updatedAt", "translationEditionKey",
  "defaultTranslationEditionKey", "translationEditions", "renditions", "recoveryLedgerOwned",
]);
const RENDITION_KEYS = new Set([
  "key", "kind", "label", "availableVersions", "performers", "full", "game", "relation",
  "sourceTabPaths", "provenance", "translationCredits",
]);
const RENDITION_SIDE_KEYS = new Set(["version", "lines"]);
const RENDITION_VERSION_KEYS = new Set(["kind", "label"]);
const RENDITION_PERFORMER_KEYS = new Set(["performerId", "name", "color"]);
const RENDITION_RELATION_KEYS = new Set(["kind", "fullRenditionKey", "lineIds"]);
const RENDITION_PROVENANCE_KEYS = new Set([
  "component", "provider", "title", "revisionId", "revisionUrl", "licenseName", "licenseUrl",
]);
const RENDITION_CREDIT_KEYS = new Set(["translation", "proofreading"]);
const LYRIC_LINE_KEYS = new Set(["id", "order", "japanese", "zh-CN", "en-US", "stanzaBreakBefore", "segments", "trailingPerformerIds"]);
const LYRIC_SEGMENT_KEYS = new Set(["text", "performerIds", "ruby"]);
const LYRIC_RUBY_KEYS = new Set(["text", "reading"]);
const OPTIONAL_SONG_LYRICS_KEYS = [
  "publishedRevision", "sourceNote", "sourceUrl", "licenseNote",
  "sourcePageId", "sourceRevisionId", "sourceSha1", "sourceFetchedAt",
];
const MAX_LYRICS_METADATA_BYTES = 16 * 1024;
const MAX_RENDITION_CREDIT_BYTES = 2048;
const MAX_LEGACY_RUBY_SPANS = 256;
const MAX_RENDITION_RUBY_SPANS = 8 * 1024;

function record(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function rejectUnknownKeys(value, allowed, path, errors) {
  for (const key of Object.keys(value)) {
    if (!allowed.has(key)) errors.push(`${path}.${key} is not allowed`);
  }
}

function modeledRubySpan(span) {
  return {
    text: span?.text,
    ...(Object.hasOwn(span || {}, "reading") ? { reading: span.reading } : {}),
  };
}

function modeledSegment(segment) {
  return {
    text: segment?.text,
    performerIds: Array.isArray(segment?.performerIds) ? [...segment.performerIds] : segment?.performerIds,
    ruby: Array.isArray(segment?.ruby) ? segment.ruby.map(modeledRubySpan) : segment?.ruby,
  };
}

function modeledLine(line, v3 = false) {
  return {
    id: line?.id,
    order: line?.order,
    japanese: line?.japanese,
    ...(!v3 || Object.hasOwn(line || {}, "zh-CN") ? { "zh-CN": line?.["zh-CN"] } : {}),
    ...(!v3 || Object.hasOwn(line || {}, "en-US") ? { "en-US": line?.["en-US"] } : {}),
    ...(Object.hasOwn(line || {}, "stanzaBreakBefore") ? { stanzaBreakBefore: line.stanzaBreakBefore } : {}),
    segments: Array.isArray(line?.segments) ? line.segments.map(modeledSegment) : line?.segments,
    ...(v3 ? { trailingPerformerIds: Array.isArray(line?.trailingPerformerIds) ? [...line.trailingPerformerIds] : line?.trailingPerformerIds } : {}),
  };
}

function modeledRenditionSide(side) {
  return {
    version: record(side?.version) ? { kind: side.version.kind, label: side.version.label } : side?.version,
    lines: Array.isArray(side?.lines) ? side.lines.map((line) => modeledLine(line, true)) : side?.lines,
  };
}

function modeledRendition(rendition) {
  const result = {
    key: rendition?.key,
    kind: rendition?.kind,
    label: rendition?.label,
    availableVersions: Array.isArray(rendition?.availableVersions) ? [...rendition.availableVersions] : rendition?.availableVersions,
    performers: Array.isArray(rendition?.performers) ? rendition.performers.map((performer) => ({
      performerId: performer?.performerId,
      name: performer?.name,
      ...(Object.hasOwn(performer || {}, "color") ? { color: performer.color } : {}),
    })) : rendition?.performers,
    ...(Object.hasOwn(rendition || {}, "full") ? { full: modeledRenditionSide(rendition.full) } : {}),
    ...(Object.hasOwn(rendition || {}, "game") ? { game: modeledRenditionSide(rendition.game) } : {}),
    relation: record(rendition?.relation) ? {
      kind: rendition.relation.kind,
      ...(Object.hasOwn(rendition.relation, "fullRenditionKey") ? { fullRenditionKey: rendition.relation.fullRenditionKey } : {}),
      ...(Object.hasOwn(rendition.relation, "lineIds") ? { lineIds: Array.isArray(rendition.relation.lineIds) ? [...rendition.relation.lineIds] : rendition.relation.lineIds } : {}),
    } : rendition?.relation,
    sourceTabPaths: Array.isArray(rendition?.sourceTabPaths)
      ? rendition.sourceTabPaths.map((path) => Array.isArray(path) ? [...path] : path)
      : rendition?.sourceTabPaths,
    provenance: Array.isArray(rendition?.provenance) ? rendition.provenance.map((attribution) => ({
      component: attribution?.component,
      provider: attribution?.provider,
      title: attribution?.title,
      revisionId: attribution?.revisionId,
      revisionUrl: attribution?.revisionUrl,
      licenseName: attribution?.licenseName,
      licenseUrl: attribution?.licenseUrl,
    })) : rendition?.provenance,
  };
  if (Object.hasOwn(rendition || {}, "translationCredits")) {
    result.translationCredits = record(rendition.translationCredits) ? {
      ...(Object.hasOwn(rendition.translationCredits, "translation") ? { translation: rendition.translationCredits.translation } : {}),
      ...(Object.hasOwn(rendition.translationCredits, "proofreading") ? { proofreading: rendition.translationCredits.proofreading } : {}),
    } : rendition.translationCredits;
  }
  if (result.relation?.kind === "exact_projection" && record(result.full) && record(result.game) &&
      Array.isArray(result.full.lines) && Array.isArray(result.game.lines) && Array.isArray(result.relation.lineIds)) {
    const fullByID = new Map(result.full.lines.map((line) => [line.id, line]));
    result.game = {
      ...result.game,
      lines: result.game.lines.map((line, index) => {
        const fullLine = fullByID.get(result.relation.lineIds[index]);
        if (!fullLine) return line;
        const projected = { ...line };
        for (const locale of ["zh-CN", "en-US"]) {
          if (Object.hasOwn(fullLine, locale)) projected[locale] = fullLine[locale];
          else delete projected[locale];
        }
        return projected;
      }),
    };
  }
  return result;
}

function modeledSongLyrics(lyrics, options = {}) {
  if (isRenditionLyricsDocument(lyrics)) {
    return {
      musicId: lyrics?.musicId,
      status: lyrics?.status,
      revision: lyrics?.revision,
      ...(Object.hasOwn(lyrics || {}, "publishedRevision") ? { publishedRevision: lyrics.publishedRevision } : {}),
      updatedAt: lyrics?.updatedAt,
      translationEditionKey: lyrics?.translationEditionKey,
      ...(options.includeEditionSummaries === false ? {} : {
        defaultTranslationEditionKey: lyrics?.defaultTranslationEditionKey,
        translationEditions: Array.isArray(lyrics?.translationEditions)
          ? lyrics.translationEditions.map((summary) => ({ key: summary?.key, label: summary?.label }))
          : lyrics?.translationEditions,
      }),
      renditions: Array.isArray(lyrics?.renditions) ? lyrics.renditions.map(modeledRendition) : lyrics?.renditions,
      // Server-derived; the collaboration room carries it too, so baselines keep it, saves do not send it.
      ...(options.forSave !== true && lyrics?.recoveryLedgerOwned === true ? { recoveryLedgerOwned: true } : {}),
    };
  }
  const document = {
    musicId: lyrics?.musicId,
    status: lyrics?.status,
    revision: lyrics?.revision,
    updatedAt: lyrics?.updatedAt,
    attribution: lyrics?.attribution ?? "",
    translationCredit: lyrics?.translationCredit ?? "",
    proofreadingCredit: lyrics?.proofreadingCredit ?? "",
    lines: Array.isArray(lyrics?.lines) ? lyrics.lines.map((line) => modeledLine(line, false)) : lyrics?.lines,
  };
  for (const key of OPTIONAL_SONG_LYRICS_KEYS) {
    if (Object.hasOwn(lyrics || {}, key)) document[key] = lyrics[key];
  }
  return document;
}

export function buildLyricsSavePayload(lyrics, sourceImportToken, clientId) {
  const document = modeledSongLyrics(lyrics, { includeEditionSummaries: false, forSave: true });
  return {
    ...document,
    ...(document.revision === 0 && sourceImportToken ? { sourceImportToken } : {}),
    clientId,
  };
}

function optionalString(value, key, errors) {
  if (value[key] !== undefined && typeof value[key] !== "string") errors.push(`${key} must be a string when present`);
}

function requiredLyricsMetadataString(value, key, errors) {
  if (typeof value[key] !== "string") {
    errors.push(`${key} must be a string`);
    return;
  }
  if (new TextEncoder().encode(value[key]).length > MAX_LYRICS_METADATA_BYTES) {
    errors.push(`${key} exceeds the 16 KiB metadata limit`);
  }
}

function optionalPositiveInteger(value, key, errors) {
  if (value[key] !== undefined && (!Number.isSafeInteger(value[key]) || value[key] <= 0)) {
    errors.push(`${key} must be a positive integer when present`);
  }
}

const RUBY_HAN_RE = /\p{Script=Han}/u;
const RUBY_NUMBER_RE = /\p{Number}/u;
const RUBY_KANA_RE = /\p{Script=Hiragana}|\p{Script=Katakana}/u;

function rubyBaseRuneMayReceiveReading(value) {
  return RUBY_HAN_RE.test(value) && value !== "〇" && !RUBY_NUMBER_RE.test(value);
}

function rubyReadingBaseIsHan(value) {
  const runes = Array.from(value);
  return runes.length > 0 && runes.every(rubyBaseRuneMayReceiveReading);
}

function rubyReadingIsKana(value) {
  let hasKana = false;
  for (const rune of Array.from(value)) {
    if (RUBY_KANA_RE.test(rune)) {
      hasKana = true;
      continue;
    }
    if (rune === "ー" || rune === "・" || /\p{Mark}/u.test(rune)) continue;
    return false;
  }
  return hasKana;
}

function validateRuby(ruby, segmentText, path, errors, maximumSpans) {
  if (!Array.isArray(ruby) || ruby.length === 0 || ruby.length > maximumSpans) {
    errors.push(`${path}.ruby must contain 1-${maximumSpans} spans`);
    return;
  }
  let text = "";
  for (let index = 0; index < ruby.length; index++) {
    const span = ruby[index];
    if (!record(span)) {
      errors.push(`${path}.ruby[${index}] must be an object`);
      continue;
    }
    rejectUnknownKeys(span, LYRIC_RUBY_KEYS, `${path}.ruby[${index}]`, errors);
    if (typeof span.text !== "string" || span.text === "") {
      errors.push(`${path}.ruby[${index}].text must be a non-empty string`);
      continue;
    }
    if (span.reading !== undefined && typeof span.reading !== "string") {
      errors.push(`${path}.ruby[${index}].reading must be a string when present`);
    } else if (typeof span.reading === "string" && span.reading !== "") {
      if (!rubyReadingBaseIsHan(span.text)) {
        errors.push(`${path}.ruby[${index}].reading base must contain only non-numeric Han text`);
      }
      if (!rubyReadingIsKana(span.reading)) {
        errors.push(`${path}.ruby[${index}].reading must contain kana only`);
      }
    }
    text += span.text;
  }
  if (text !== segmentText) errors.push(`${path}.ruby text must equal segment text`);
}

function validateLines(lines, errors, options = {}) {
  const { pathPrefix = "lines", v3 = false } = options;
  if (!Array.isArray(lines) || lines.length === 0 || lines.length > 5000) {
    errors.push(`${pathPrefix} must contain 1-5000 items`);
    return;
  }
  const ids = new Set();
  const orders = new Set();
  for (let lineIndex = 0; lineIndex < lines.length; lineIndex++) {
    const line = lines[lineIndex];
    const path = `${pathPrefix}[${lineIndex}]`;
    if (!record(line)) {
      errors.push(`${path} must be an object`);
      continue;
    }
    rejectUnknownKeys(line, LYRIC_LINE_KEYS, path, errors);
    if (typeof line.id !== "string" || line.id.trim() === "" || ids.has(line.id)) errors.push(`${path}.id must be unique and non-empty`);
    else ids.add(line.id);
    if (!Number.isSafeInteger(line.order) || line.order < 0 || orders.has(line.order)) errors.push(`${path}.order must be unique and non-negative`);
    else orders.add(line.order);
    if (typeof line.japanese !== "string" || line.japanese.trim() === "") errors.push(`${path}.japanese must be non-empty`);
    if (v3) {
      if (line["zh-CN"] !== undefined && typeof line["zh-CN"] !== "string") errors.push(`${path}.zh-CN must be a string when present`);
      if (line["en-US"] !== undefined && typeof line["en-US"] !== "string") errors.push(`${path}.en-US must be a string when present`);
      if (!Array.isArray(line.trailingPerformerIds) || line.trailingPerformerIds.some((id) => typeof id !== "string" || !id) ||
          new Set(line.trailingPerformerIds).size !== line.trailingPerformerIds.length) {
        errors.push(`${path}.trailingPerformerIds must contain unique non-empty strings`);
      }
    } else {
      if (typeof line["zh-CN"] !== "string") errors.push(`${path}.zh-CN must be a string`);
      if (typeof line["en-US"] !== "string") errors.push(`${path}.en-US must be a string`);
    }
    if (line.stanzaBreakBefore !== undefined && typeof line.stanzaBreakBefore !== "boolean") errors.push(`${path}.stanzaBreakBefore must be boolean when present`);
    if (!Array.isArray(line.segments) || line.segments.length === 0 || line.segments.length > 100) {
      errors.push(`${path}.segments must contain 1-100 items`);
      continue;
    }
    let japanese = "";
    for (let segmentIndex = 0; segmentIndex < line.segments.length; segmentIndex++) {
      const segment = line.segments[segmentIndex];
      const segmentPath = `${path}.segments[${segmentIndex}]`;
      if (!record(segment)) {
        errors.push(`${segmentPath} must be an object`);
        continue;
      }
      rejectUnknownKeys(segment, LYRIC_SEGMENT_KEYS, segmentPath, errors);
      if (typeof segment.text !== "string") {
        errors.push(`${segmentPath}.text must be a string`);
        continue;
      }
      japanese += segment.text;
      if (!Array.isArray(segment.performerIds) || segment.performerIds.length > 64) {
        errors.push(`${segmentPath}.performerIds must be an array with at most 64 items`);
      } else {
        const performerIds = new Set();
        for (const performerId of segment.performerIds) {
          const valid = v3 ? typeof performerId === "string" && performerId.length > 0 : Number.isSafeInteger(performerId) && performerId > 0;
          if (!valid || performerIds.has(performerId)) {
            errors.push(v3
              ? `${segmentPath}.performerIds must contain unique non-empty strings`
              : `${segmentPath}.performerIds must contain unique positive integers`);
            break;
          }
          performerIds.add(performerId);
        }
      }
      validateRuby(segment.ruby, segment.text, segmentPath, errors,
        v3 ? MAX_RENDITION_RUBY_SPANS : MAX_LEGACY_RUBY_SPANS);
    }
    if (typeof line.japanese === "string" && japanese !== line.japanese) errors.push(`${path}.segments must concatenate to japanese`);
  }
}

function validateRendition(rendition, index, errors) {
  const path = `renditions[${index}]`;
  if (!record(rendition)) {
    errors.push(`${path} must be an object`);
    return;
  }
  rejectUnknownKeys(rendition, RENDITION_KEYS, path, errors);
  if (typeof rendition.key !== "string" || !/^[a-z0-9][a-z0-9._-]{0,127}$/.test(rendition.key)) errors.push(`${path}.key is invalid`);
  if (!["original", "sekai", "vocaloid", "alternate"].includes(rendition.kind)) errors.push(`${path}.kind is invalid`);
  if (typeof rendition.label !== "string" || !rendition.label) errors.push(`${path}.label must be non-empty`);
  if (!Array.isArray(rendition.availableVersions)) errors.push(`${path}.availableVersions must be an array`);
  if (!Array.isArray(rendition.performers)) errors.push(`${path}.performers must be an array`);
  else for (let performerIndex = 0; performerIndex < rendition.performers.length; performerIndex++) {
    const performer = rendition.performers[performerIndex];
    const performerPath = `${path}.performers[${performerIndex}]`;
    if (!record(performer)) {
      errors.push(`${performerPath} must be an object`);
      continue;
    }
    rejectUnknownKeys(performer, RENDITION_PERFORMER_KEYS, performerPath, errors);
    if (typeof performer.performerId !== "string" || !performer.performerId) errors.push(`${performerPath}.performerId must be non-empty`);
    if (typeof performer.name !== "string" || !performer.name) errors.push(`${performerPath}.name must be non-empty`);
    if (performer.color !== undefined && !/^#[0-9A-F]{6}$/.test(performer.color)) errors.push(`${performerPath}.color must be canonical uppercase RGB`);
  }
  for (const sideName of ["full", "game"]) {
    if (!Object.hasOwn(rendition, sideName)) continue;
    const side = rendition[sideName];
    const sidePath = `${path}.${sideName}`;
    if (!record(side)) {
      errors.push(`${sidePath} must be an object`);
      continue;
    }
    rejectUnknownKeys(side, RENDITION_SIDE_KEYS, sidePath, errors);
    if (!record(side.version)) errors.push(`${sidePath}.version must be an object`);
    else {
      rejectUnknownKeys(side.version, RENDITION_VERSION_KEYS, `${sidePath}.version`, errors);
      if (side.version.kind !== rendition.kind || typeof side.version.label !== "string" || !side.version.label) {
        errors.push(`${sidePath}.version must match the rendition family`);
      }
    }
    validateLines(side.lines, errors, { pathPrefix: `${sidePath}.lines`, v3: true });
  }
  if (!record(rendition.relation)) errors.push(`${path}.relation must be an object`);
  else {
    rejectUnknownKeys(rendition.relation, RENDITION_RELATION_KEYS, `${path}.relation`, errors);
    if (!["none", "exact_projection"].includes(rendition.relation.kind)) errors.push(`${path}.relation.kind is invalid`);
  }
  if (!Array.isArray(rendition.sourceTabPaths) || rendition.sourceTabPaths.length === 0 || rendition.sourceTabPaths.some((tabPath) =>
    !Array.isArray(tabPath) || tabPath.length === 0 || tabPath.some((label) => typeof label !== "string" || !label))) {
    errors.push(`${path}.sourceTabPaths must contain non-empty label paths`);
  }
  if (!Array.isArray(rendition.provenance) || rendition.provenance.length === 0) errors.push(`${path}.provenance must be non-empty`);
  else for (let provenanceIndex = 0; provenanceIndex < rendition.provenance.length; provenanceIndex++) {
    const attribution = rendition.provenance[provenanceIndex];
    const attributionPath = `${path}.provenance[${provenanceIndex}]`;
    if (!record(attribution)) {
      errors.push(`${attributionPath} must be an object`);
      continue;
    }
    rejectUnknownKeys(attribution, RENDITION_PROVENANCE_KEYS, attributionPath, errors);
    for (const key of ["component", "provider", "title", "revisionUrl", "licenseName", "licenseUrl"]) {
      if (typeof attribution[key] !== "string" || !attribution[key]) errors.push(`${attributionPath}.${key} must be non-empty`);
    }
    if (!Number.isSafeInteger(attribution.revisionId) || attribution.revisionId <= 0) errors.push(`${attributionPath}.revisionId must be positive`);
  }
  if (rendition.translationCredits !== undefined) {
    if (!record(rendition.translationCredits)) errors.push(`${path}.translationCredits must be an object`);
    else {
      rejectUnknownKeys(rendition.translationCredits, RENDITION_CREDIT_KEYS, `${path}.translationCredits`, errors);
      if (Object.keys(rendition.translationCredits).length === 0 ||
          !["translation", "proofreading"].some((key) => typeof rendition.translationCredits[key] === "string" && rendition.translationCredits[key] !== "")) {
        errors.push(`${path}.translationCredits must contain at least one non-empty credit`);
      }
      for (const key of RENDITION_CREDIT_KEYS) {
        if (!Object.hasOwn(rendition.translationCredits, key)) continue;
        const credit = rendition.translationCredits[key];
        if (typeof credit !== "string") {
          errors.push(`${path}.translationCredits.${key} must be a string`);
        } else if (credit !== credit.trim()) {
          errors.push(`${path}.translationCredits.${key} must not contain surrounding whitespace`);
        } else if (new TextEncoder().encode(credit).length > MAX_RENDITION_CREDIT_BYTES) {
          errors.push(`${path}.translationCredits.${key} exceeds the 2048-byte public limit`);
        }
      }
    }
  }
}

function validateTranslationEditionEnvelope(value, errors) {
  const summaries = validateTranslationEditionSummaries(value.translationEditions);
  if (!summaries.ok) {
    errors.push(...summaries.details);
    return;
  }
  const keys = new Set(summaries.value.map((summary) => summary.key));
  if (!isTranslationEditionKey(value.translationEditionKey) || !keys.has(value.translationEditionKey)) {
    errors.push("translationEditionKey must identify a listed edition");
  }
  if (!isTranslationEditionKey(value.defaultTranslationEditionKey) || !keys.has(value.defaultTranslationEditionKey)) {
    errors.push("defaultTranslationEditionKey must identify a listed edition");
  }
}

function normalizeTimestamp(value) {
  if (typeof value !== "string" || value === "") return "";
  const timestamp = Date.parse(value);
  return Number.isFinite(timestamp) ? new Date(timestamp).toISOString() : value;
}

function editableContent(lyrics) {
  if (isRenditionLyricsDocument(lyrics)) {
    return {
      renditions: [...lyrics.renditions].sort((left, right) => String(left.key).localeCompare(String(right.key))).map(modeledRendition),
    };
  }
  return {
    attribution: lyrics.attribution || "",
    translationCredit: lyrics.translationCredit || "",
    proofreadingCredit: lyrics.proofreadingCredit || "",
    sourceNote: lyrics.sourceNote || "",
    sourceUrl: lyrics.sourceUrl || "",
    licenseNote: lyrics.licenseNote || "",
    sourcePageId: lyrics.sourcePageId || 0,
    sourceRevisionId: lyrics.sourceRevisionId || 0,
    sourceSha1: lyrics.sourceSha1 || "",
    sourceFetchedAt: normalizeTimestamp(lyrics.sourceFetchedAt),
    lines: [...lyrics.lines].sort((left, right) => left.order - right.order).map((line) => ({
      id: line.id,
      order: line.order,
      japanese: line.japanese,
      "zh-CN": line["zh-CN"],
      "en-US": line["en-US"],
      stanzaBreakBefore: Boolean(line.stanzaBreakBefore),
      segments: line.segments.map((segment) => ({
        text: segment.text,
        performerIds: [...segment.performerIds],
        ruby: segment.ruby.map((span) => ({ text: span.text, reading: span.reading || "" })),
      })),
    })),
  };
}

function validateEnvelope(value, errors) {
  if (!Number.isSafeInteger(value.musicId) || value.musicId <= 0) errors.push("musicId must be a positive integer");
  if (!["draft", "published", "draft-published"].includes(value.status)) errors.push("status is invalid");
  if (!Number.isSafeInteger(value.revision) || value.revision <= 0) errors.push("revision must be a positive integer");
  if (typeof value.updatedAt !== "string" || value.updatedAt === "" || !Number.isFinite(Date.parse(value.updatedAt))) {
    errors.push("updatedAt must be a valid timestamp");
  }
  optionalPositiveInteger(value, "publishedRevision", errors);
  if (value.status === "published" && value.publishedRevision !== value.revision) {
    errors.push("published response must identify the current revision as published");
  }
  if (value.status === "draft-published" && (!Number.isSafeInteger(value.publishedRevision) || value.publishedRevision <= 0 || value.publishedRevision >= value.revision)) {
    errors.push("draft-published response must identify an older published revision");
  }
  if (value.status === "draft" && value.publishedRevision !== undefined) {
    errors.push("draft response must not include publishedRevision");
  }
}

export function validateSongLyricsMutationResponse(value, expectation) {
  const errors = [];
  if (!record(value)) return { ok: false, details: ["response must be a SongLyrics object"] };
  const isRendition = isRenditionLyricsDocument(value);
  rejectUnknownKeys(value, isRendition ? RENDITION_DOCUMENT_KEYS : SONG_LYRICS_KEYS, "response", errors);
  validateEnvelope(value, errors);

  if (isRendition) {
    validateTranslationEditionEnvelope(value, errors);
    if (value.recoveryLedgerOwned !== undefined && typeof value.recoveryLedgerOwned !== "boolean") {
      errors.push("recoveryLedgerOwned must be a boolean when present");
    }
    if (value.renditions.length === 0 || value.renditions.length > 16) errors.push("renditions must contain 1-16 items");
    const keys = new Set();
    for (let index = 0; index < value.renditions.length; index++) {
      validateRendition(value.renditions[index], index, errors);
      const key = value.renditions[index]?.key;
      if (keys.has(key)) errors.push(`renditions repeat stable key ${key}`);
      keys.add(key);
    }
    errors.push(...lyricsVersionSaveProblems(value));
  } else {
    for (const key of ["attribution", "translationCredit", "proofreadingCredit"]) {
      requiredLyricsMetadataString(value, key, errors);
    }
    for (const key of ["sourceNote", "sourceUrl", "licenseNote", "sourceSha1", "sourceFetchedAt"]) optionalString(value, key, errors);
    for (const key of ["sourcePageId", "sourceRevisionId"]) optionalPositiveInteger(value, key, errors);
    if (value.sourceSha1 !== undefined && !/^[0-9a-f]{40}$/.test(value.sourceSha1)) errors.push("sourceSha1 must be canonical lowercase SHA1 when present");
    if (value.sourceFetchedAt !== undefined && !Number.isFinite(Date.parse(value.sourceFetchedAt))) errors.push("sourceFetchedAt must be a valid timestamp when present");
    validateLines(value.lines, errors);
  }

  if (!record(expectation) || !["save", "publish", "unpublish", "edition", "conflict"].includes(expectation.operation)) {
    errors.push("response correlation expectation is invalid");
  } else if (!Number.isSafeInteger(expectation.musicId) || expectation.musicId <= 0 || value.musicId !== expectation.musicId) {
    errors.push("response musicId does not match the request");
  } else if (!Number.isSafeInteger(expectation.revision) || expectation.revision < 0) {
    errors.push("request revision correlation is invalid");
  } else {
    if (expectation.editionKey !== undefined) {
      if (!isRendition || !isTranslationEditionKey(expectation.editionKey) || value.translationEditionKey !== expectation.editionKey) {
        errors.push("response translationEditionKey does not match the request");
      }
    } else if (expectation.operation === "edition") {
      errors.push("edition response correlation requires an editionKey");
    }

    if (expectation.operation === "save") {
      const revisionMatches = expectation.revision === 0
        ? value.revision === 1
        : value.revision === expectation.revision || value.revision === expectation.revision + 1;
      if (!revisionMatches) errors.push("save response revision does not advance from the requested revision");
      const documentsMatch = isRendition === isRenditionLyricsDocument(expectation.document);
      if (!record(expectation.document) || !documentsMatch ||
          !isRendition && !Array.isArray(expectation.document.lines) ||
          isRendition && !Array.isArray(expectation.document.renditions)) {
        errors.push("save response correlation document is invalid");
      } else if (errors.length === 0) {
        try {
          if (JSON.stringify(editableContent(value)) !== JSON.stringify(editableContent(expectation.document))) {
            errors.push("save response content does not match the submitted document");
          }
        } catch {
          errors.push("save response correlation document is invalid");
        }
      }
    } else if (expectation.operation === "edition") {
      if (value.revision !== expectation.revision + 1) {
        errors.push("edition response revision must advance exactly once from the requested revision");
      }
    } else if (expectation.operation === "conflict") {
      if (value.revision <= expectation.revision) errors.push("conflict current revision must be newer than the request");
    } else if (value.revision !== expectation.revision) {
      errors.push(`${expectation.operation} response revision does not match the request`);
    } else if (expectation.operation === "publish" && (value.status !== "published" || value.publishedRevision !== expectation.revision)) {
      errors.push("publish response does not confirm the requested publication");
    } else if (expectation.operation === "unpublish" && (value.status !== "draft" || value.publishedRevision !== undefined)) {
      errors.push("unpublish response does not confirm removal of the publication");
    }
  }

  return errors.length > 0 ? { ok: false, details: errors } : { ok: true, value: modeledSongLyrics(value) };
}

/**
 * A checkpoint persists the already-shared Y.Doc, so concurrent collaborators
 * may legitimately make the authoritative response differ from the caller's
 * latest React snapshot. Validate the complete DTO and correlate musicId, but
 * deliberately do not compare it with a client-submitted document.
 */
export function validateSongLyricsCheckpointResponse(value, musicId) {
  const revision = record(value) && Number.isSafeInteger(value.revision) && value.revision > 0
    ? value.revision
    : 0;
  return validateSongLyricsMutationResponse(value, {
    operation: "save",
    musicId,
    revision,
    document: value,
  });
}
