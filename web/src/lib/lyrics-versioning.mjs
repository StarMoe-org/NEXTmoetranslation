const AVAILABLE_FULL = Object.freeze(["full"]);
const AVAILABLE_GAME = Object.freeze(["game"]);
const AVAILABLE_FULL_GAME = Object.freeze(["full", "game"]);
const LEGACY_RENDITION_KEY = "legacy-v2";
const STABLE_RENDITION_KEY = /^[a-z0-9][a-z0-9._-]{0,127}$/;
const MAX_RENDITION_CREDIT_BYTES = 2048;
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

function record(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

export function isRenditionLyricsDocument(document) {
  return record(document) && Array.isArray(document.renditions);
}

export function lyricsRenditionKeys(document) {
  if (!isRenditionLyricsDocument(document)) return [LEGACY_RENDITION_KEY];
  return document.renditions
    .filter((rendition) => record(rendition) && typeof rendition.key === "string")
    .map((rendition) => rendition.key);
}

export function lyricsRenditionByKey(document, renditionKey) {
  if (!isRenditionLyricsDocument(document) || typeof renditionKey !== "string" || !renditionKey) return null;
  return document.renditions.find((rendition) => record(rendition) && rendition.key === renditionKey) ?? null;
}

function selectedRendition(document, renditionKey) {
  if (!isRenditionLyricsDocument(document)) return null;
  if (typeof renditionKey === "string" && renditionKey) return lyricsRenditionByKey(document, renditionKey);
  return document.renditions.length === 1 && record(document.renditions[0]) ? document.renditions[0] : null;
}

function canonicalVersions(versions) {
  if (!Array.isArray(versions)) return [];
  if (versions.length === 1 && versions[0] === "full") return [...AVAILABLE_FULL];
  if (versions.length === 1 && versions[0] === "game") return [...AVAILABLE_GAME];
  if (versions.length === 2 && versions[0] === "full" && versions[1] === "game") return [...AVAILABLE_FULL_GAME];
  return [];
}

export function normalizedLyricsVersions(document, renditionKey) {
  const rendition = selectedRendition(document, renditionKey);
  if (rendition) return canonicalVersions(rendition.availableVersions);
  if (isRenditionLyricsDocument(document)) return [];
  const versions = document?.availableVersions;
  if (Array.isArray(versions) && versions.length === 2 && versions[0] === "full" && versions[1] === "game") {
    return [...AVAILABLE_FULL_GAME];
  }
  return [...AVAILABLE_FULL];
}

export function retainedLyricsTranslationTarget(document, preferredRenditionKey = "", preferredVersion = "full") {
  if (!isRenditionLyricsDocument(document)) return { renditionKey: "", version: "full" };
  const preferred = lyricsRenditionByKey(document, preferredRenditionKey);
  const rendition = preferred || document.renditions.find(record) || null;
  if (!rendition) return { renditionKey: "", version: "full" };
  const versions = normalizedLyricsVersions(document, rendition.key);
  const version = versions.includes(preferredVersion)
    ? preferredVersion
    : versions.includes("full") ? "full" : "game";
  return { renditionKey: rendition.key, version };
}

export function renditionProjectionStatus(document, renditionKey) {
  const rendition = selectedRendition(document, renditionKey);
  if (!rendition) {
    if (isRenditionLyricsDocument(document)) return "invalid";
    return normalizedLyricsVersions(document).includes("game") || record(document?.gameProjection)
      ? "exact_projection"
      : "full_only";
  }
  const hasFull = record(rendition.full);
  const hasGame = record(rendition.game);
  if (hasFull && !hasGame) return "full_only";
  if (!hasFull && hasGame) return "game_only";
  if (!hasFull || !hasGame || !record(rendition.relation)) return "invalid";
  if (rendition.relation.kind === "exact_projection") {
    return renditionSaveProblems(rendition).length === 0 ? "exact_projection" : "invalid";
  }
  if (rendition.relation.kind === "none") return "independent_game";
  return "invalid";
}

function legacyGameProjection(document) {
  const versions = normalizedLyricsVersions(document);
  const advertisesGame = versions.includes("game");
  const projection = document?.gameProjection;
  const errors = [];

  if (!advertisesGame && projection !== undefined) {
    errors.push("Game 投影存在，但 availableVersions 未声明 game");
  }
  if (advertisesGame && !record(projection)) {
    errors.push("availableVersions 声明了 game，但缺少 Game 行投影");
  }
  if (!record(projection)) return { ok: errors.length === 0, lines: [], lineIds: [], errors };
  if (!["tagged_full_and_game", "untagged_uncut_identity"].includes(projection.reasonCode)) {
    errors.push("Game 投影包含不允许公开的版本判定原因");
  }
  if (!Array.isArray(projection.lineIds) || projection.lineIds.length === 0) {
    errors.push("Game 投影必须至少引用一条 Full 行 ID");
    return { ok: false, lines: [], lineIds: [], errors };
  }

  const fullLines = Array.isArray(document?.lines) ? document.lines : [];
  const positions = new Map();
  const duplicateFullLineIds = new Set();
  for (let index = 0; index < fullLines.length; index++) {
    const id = fullLines[index]?.id;
    if (typeof id !== "string" || !id) continue;
    if (positions.has(id)) duplicateFullLineIds.add(id);
    else positions.set(id, index);
  }
  if (duplicateFullLineIds.size > 0) {
    for (const lineId of duplicateFullLineIds) errors.push(`Full 歌词包含重复行 ID ${lineId}`);
    return { ok: false, lines: [], lineIds: [...projection.lineIds], errors };
  }
  const seen = new Set();
  const lines = [];
  let previousPosition = -1;
  for (const lineId of projection.lineIds) {
    if (typeof lineId !== "string" || !lineId) {
      errors.push("Game 投影包含无效的 Full 行 ID");
      continue;
    }
    if (seen.has(lineId)) {
      errors.push(`Game 投影重复引用 Full 行 ${lineId}`);
      continue;
    }
    seen.add(lineId);
    const position = positions.get(lineId);
    if (position === undefined) {
      errors.push(`Game 投影引用的 Full 行 ${lineId} 已不存在`);
      continue;
    }
    if (position <= previousPosition) {
      errors.push("Game 投影的行 ID 必须保持 Full 顺序");
      continue;
    }
    previousPosition = position;
    lines.push(fullLines[position]);
  }
  if (projection.reasonCode === "untagged_uncut_identity" && (
    projection.lineIds.length !== fullLines.length ||
    projection.lineIds.some((lineId, index) => lineId !== fullLines[index]?.id)
  )) {
    errors.push("untagged_uncut_identity 必须按原顺序引用全部 Full 行");
  }
  return { ok: errors.length === 0, lines, lineIds: [...projection.lineIds], errors };
}

export function projectGameLyricsLines(document, renditionKey) {
  if (!isRenditionLyricsDocument(document)) return legacyGameProjection(document);
  const rendition = selectedRendition(document, renditionKey);
  if (!rendition) return { ok: false, lines: [], lineIds: [], errors: ["peer rendition 必须按稳定 key 选择"] };
  const errors = renditionSaveProblems(rendition);
  if (!record(rendition.game) || !Array.isArray(rendition.game.lines)) {
    return { ok: errors.length === 0, lines: [], lineIds: [], errors };
  }
  const lineIds = rendition.relation?.kind === "exact_projection" && Array.isArray(rendition.relation.lineIds)
    ? [...rendition.relation.lineIds]
    : rendition.game.lines.map((line) => line?.id).filter((lineId) => typeof lineId === "string" && lineId);
  if (rendition.relation?.kind !== "exact_projection" || !record(rendition.full) || !Array.isArray(rendition.full.lines)) {
    return { ok: errors.length === 0, lines: [...rendition.game.lines], lineIds, errors };
  }
  const fullByID = new Map(rendition.full.lines.map((line) => [line?.id, line]));
  const lines = rendition.game.lines.map((line, index) => {
    const fullLine = fullByID.get(lineIds[index]);
    if (!fullLine) return { ...line };
    const projected = { ...line };
    for (const locale of ["zh-CN", "en-US"]) {
      if (Object.hasOwn(fullLine, locale)) projected[locale] = fullLine[locale];
      else delete projected[locale];
    }
    return projected;
  });
  return { ok: errors.length === 0, lines, lineIds, errors };
}

function validateRenditionSide(side, path, expectedKind, errors) {
  if (!record(side) || !record(side.version) || side.version.kind !== expectedKind ||
      typeof side.version.label !== "string" || !side.version.label.trim()) {
    errors.push(`${path} 版本标签与 rendition kind 不一致`);
    return;
  }
  if (!Array.isArray(side.lines) || side.lines.length === 0) {
    errors.push(`${path} 必须包含歌词行`);
    return;
  }
  const ids = new Set();
  const orders = new Set();
  for (let lineIndex = 0; lineIndex < side.lines.length; lineIndex++) {
    const line = side.lines[lineIndex];
    const linePath = `${path}.lines[${lineIndex}]`;
    if (!record(line) || typeof line.id !== "string" || !line.id || ids.has(line.id)) {
      errors.push(`${linePath}.id 必须稳定、非空且唯一`);
      continue;
    }
    ids.add(line.id);
    if (!Number.isSafeInteger(line.order) || line.order < 0 || orders.has(line.order)) {
      errors.push(`${linePath}.order 必须非负且唯一`);
    } else {
      orders.add(line.order);
    }
    if (typeof line.japanese !== "string" || !line.japanese) errors.push(`${linePath}.japanese 必须非空`);
    if (!Array.isArray(line.trailingPerformerIds) || line.trailingPerformerIds.some((id) => typeof id !== "string" || !id) ||
        new Set(line.trailingPerformerIds).size !== line.trailingPerformerIds.length) {
      errors.push(`${linePath}.trailingPerformerIds 必须是唯一的稳定字符串 ID`);
    }
    if (line["zh-CN"] !== undefined && typeof line["zh-CN"] !== "string") errors.push(`${linePath}.zh-CN 必须是字符串`);
    if (line["en-US"] !== undefined && typeof line["en-US"] !== "string") errors.push(`${linePath}.en-US 必须是字符串`);
    if (!Array.isArray(line.segments) || line.segments.length === 0) {
      errors.push(`${linePath}.segments 必须非空`);
      continue;
    }
    let japanese = "";
    for (let segmentIndex = 0; segmentIndex < line.segments.length; segmentIndex++) {
      const segment = line.segments[segmentIndex];
      const segmentPath = `${linePath}.segments[${segmentIndex}]`;
      if (!record(segment) || typeof segment.text !== "string" || !segment.text) {
        errors.push(`${segmentPath}.text 必须是非空字符串`);
        continue;
      }
      japanese += segment.text;
      if (!Array.isArray(segment.performerIds) || segment.performerIds.some((id) => typeof id !== "string" || !id) ||
          new Set(segment.performerIds).size !== segment.performerIds.length) {
        errors.push(`${segmentPath}.performerIds 必须是唯一的稳定字符串 ID`);
      }
      if (!Array.isArray(segment.ruby) || segment.ruby.length === 0 ||
          segment.ruby.some((span) => !record(span) || typeof span.text !== "string" || !span.text ||
            (span.reading !== undefined && (typeof span.reading !== "string" || !span.reading)))) {
        errors.push(`${segmentPath}.ruby 必须包含合法、非空的文字与可选注音`);
      } else {
        for (let rubyIndex = 0; rubyIndex < segment.ruby.length; rubyIndex++) {
          const span = segment.ruby[rubyIndex];
          if (span.reading !== undefined && (!rubyReadingBaseIsHan(span.text) || !rubyReadingIsKana(span.reading))) {
            errors.push(`${segmentPath}.ruby[${rubyIndex}] 仅允许纯非数字 Han 基底使用假名注音`);
          }
        }
        if (segment.ruby.map((span) => span.text).join("") !== segment.text) {
          errors.push(`${segmentPath}.ruby 必须按原顺序完整覆盖分段文字`);
        }
      }
    }
    if (typeof line.japanese === "string" && japanese !== line.japanese) {
      errors.push(`${linePath}.segments 必须完整拼接为日文原文`);
    }
  }
}

function renditionSaveProblems(rendition) {
  const errors = [];
  if (!record(rendition) || typeof rendition.key !== "string" || !STABLE_RENDITION_KEY.test(rendition.key)) {
    return ["rendition key 必须是稳定的小写 key"];
  }
  if (!["original", "sekai", "vocaloid", "alternate"].includes(rendition.kind)) {
    errors.push(`rendition ${rendition.key} kind 无效`);
  }
  const versions = canonicalVersions(rendition.availableVersions);
  if (versions.length === 0) errors.push(`rendition ${rendition.key} availableVersions 无效`);
  const hasFull = record(rendition.full);
  const hasGame = record(rendition.game);
  const actual = [...(hasFull ? ["full"] : []), ...(hasGame ? ["game"] : [])];
  if (JSON.stringify(versions) !== JSON.stringify(actual)) {
    errors.push(`rendition ${rendition.key} availableVersions 与 Full/Game 不一致`);
  }
  if (hasFull) validateRenditionSide(rendition.full, `rendition ${rendition.key}.full`, rendition.kind, errors);
  if (hasGame) validateRenditionSide(rendition.game, `rendition ${rendition.key}.game`, rendition.kind, errors);
  if (!record(rendition.relation)) {
    errors.push(`rendition ${rendition.key} 缺少 projection relation`);
  } else if (rendition.relation.kind === "none") {
    if (rendition.relation.fullRenditionKey !== undefined || rendition.relation.lineIds !== undefined) {
      errors.push(`rendition ${rendition.key} none relation 不能携带 Full 引用`);
    }
  } else if (rendition.relation.kind === "exact_projection") {
    if (!hasFull || !hasGame || rendition.relation.fullRenditionKey !== rendition.key || !Array.isArray(rendition.relation.lineIds) || rendition.relation.lineIds.length === 0) {
      errors.push(`rendition ${rendition.key} exact projection 必须只引用同一 stable key 的 Full`);
    } else {
      const positions = new Map(rendition.full.lines.map((line, index) => [line?.id, index]));
      const seen = new Set();
      let previous = -1;
      for (const lineId of rendition.relation.lineIds) {
        const position = positions.get(lineId);
        if (typeof lineId !== "string" || !lineId || seen.has(lineId) || position === undefined || position <= previous) {
          errors.push(`rendition ${rendition.key} exact projection 行 ID 无效、重复、跨 family 或失序`);
          break;
        }
        seen.add(lineId);
        previous = position;
      }
      if (rendition.game.lines.length !== rendition.relation.lineIds.length) {
        errors.push(`rendition ${rendition.key} Game 行数必须与 exact projection 引用数一致`);
      } else {
        for (let gameIndex = 0; gameIndex < rendition.game.lines.length; gameIndex++) {
          const gameLine = rendition.game.lines[gameIndex];
          const fullLine = rendition.full.lines.find((line) => line?.id === rendition.relation.lineIds[gameIndex]);
          if (!fullLine || gameLine?.japanese !== fullLine.japanese) {
            errors.push(`rendition ${rendition.key} exact projection Game 必须按 relation line ID 保留对应 Full 原文`);
            break;
          }
        }
      }
    }
  } else {
    errors.push(`rendition ${rendition.key} projection relation 无效`);
  }
  if (!Array.isArray(rendition.performers) || rendition.performers.some((performer) =>
    !record(performer) || typeof performer.performerId !== "string" || !performer.performerId ||
    typeof performer.name !== "string" || !performer.name)) {
    errors.push(`rendition ${rendition.key} performer registry 无效`);
  } else if (new Set(rendition.performers.map((performer) => performer.performerId)).size !== rendition.performers.length) {
    errors.push(`rendition ${rendition.key} performer registry 重复 stable ID`);
  } else {
    const performerIDs = new Set(rendition.performers.map((performer) => performer.performerId));
    for (const sideName of ["full", "game"]) {
      const lines = Array.isArray(rendition[sideName]?.lines) ? rendition[sideName].lines : [];
      for (const line of lines) {
        const ids = [
          ...(Array.isArray(line?.trailingPerformerIds) ? line.trailingPerformerIds : []),
          ...(Array.isArray(line?.segments) ? line.segments.flatMap((segment) => Array.isArray(segment?.performerIds) ? segment.performerIds : []) : []),
        ];
        if (ids.some((performerID) => !performerIDs.has(performerID))) {
          errors.push(`rendition ${rendition.key} ${sideName} 使用了未注册的 performer ID`);
          break;
        }
      }
    }
  }
  if (rendition.translationCredits !== undefined) {
    if (!record(rendition.translationCredits) || Object.keys(rendition.translationCredits).length === 0 ||
        !["translation", "proofreading"].some((field) => typeof rendition.translationCredits[field] === "string" && rendition.translationCredits[field] !== "")) {
      errors.push(`rendition ${rendition.key} translationCredits 必须省略或至少包含一个非空署名`);
    } else {
      for (const field of ["translation", "proofreading"]) {
        if (!Object.hasOwn(rendition.translationCredits, field)) continue;
        const credit = rendition.translationCredits[field];
        if (typeof credit !== "string" || credit !== credit.trim() ||
            new TextEncoder().encode(credit).length > MAX_RENDITION_CREDIT_BYTES) {
          errors.push(`rendition ${rendition.key} ${field} 署名必须首尾无空白且不超过 2048 bytes`);
        }
      }
    }
  }
  return errors;
}

export function lyricsVersionSaveProblems(document) {
  if (!isRenditionLyricsDocument(document)) {
    const errors = [];
    const versions = document?.availableVersions;
    if (versions !== undefined && !(Array.isArray(versions) && (
      versions.length === 1 && versions[0] === "full" ||
      versions.length === 2 && versions[0] === "full" && versions[1] === "game"
    ))) {
      errors.push('availableVersions 必须严格为 ["full"] 或 ["full", "game"]');
    }
    errors.push(...legacyGameProjection(document).errors);
    return Array.from(new Set(errors));
  }
  const errors = [];
  if (document.renditions.length === 0 || document.renditions.length > 16) {
    errors.push("peer renditions 必须包含 1-16 个 stable-key family");
  }
  const seen = new Set();
  for (const rendition of document.renditions) {
    if (record(rendition) && seen.has(rendition.key)) errors.push(`peer renditions 重复 stable key ${rendition.key}`);
    if (record(rendition)) seen.add(rendition.key);
    errors.push(...renditionSaveProblems(rendition));
  }
  return Array.from(new Set(errors));
}

export function referencedGameFullLineIds(document, renditionKey) {
  if (isRenditionLyricsDocument(document)) {
    const rendition = selectedRendition(document, renditionKey);
    return rendition?.relation?.kind === "exact_projection" && Array.isArray(rendition.relation.lineIds)
      ? rendition.relation.lineIds.filter((lineId) => typeof lineId === "string" && lineId)
      : [];
  }
  const projection = document?.gameProjection;
  return record(projection) && Array.isArray(projection.lineIds)
    ? projection.lineIds.filter((lineId) => typeof lineId === "string" && lineId)
    : [];
}

export function removedReferencedFullLineIds(document, renditionKey) {
  const rendition = selectedRendition(document, renditionKey);
  const lines = rendition ? rendition.full?.lines : document?.lines;
  const currentIds = new Set(Array.isArray(lines) ? lines.map((line) => line?.id) : []);
  return referencedGameFullLineIds(document, renditionKey).filter((lineId) => !currentIds.has(lineId));
}

function linesHavePerformerSegmentation(lines) {
  if (lines.length === 0) return true;
  for (const line of lines) {
    if (Array.isArray(line?.trailingPerformerIds) && line.trailingPerformerIds.length > 0) return true;
    if (!Array.isArray(line?.segments) || line.segments.length !== 1) return true;
    const segment = line.segments[0];
    if (!Array.isArray(segment?.performerIds) || segment.performerIds.length > 0) return true;
    const lineText = typeof line?.japanese === "string" ? line.japanese : line?.text;
    if (typeof lineText !== "string" || lineText === "" || segment?.text !== lineText) return true;
  }
  return false;
}

export function lyricsHasPerformerSegmentation(document, renditionKey, version = "full") {
  const rendition = selectedRendition(document, renditionKey);
  if (rendition) {
    const side = rendition[version];
    const lines = Array.isArray(side?.lines) ? side.lines : [];
    if (linesHavePerformerSegmentation(lines)) return true;
    if (Array.isArray(rendition.performers) && rendition.performers.length > 0) {
      const performerIDs = new Set(rendition.performers.map((performer) => performer?.performerId));
      return lines.some((line) => (Array.isArray(line?.trailingPerformerIds) && line.trailingPerformerIds.some((id) => performerIDs.has(id))) ||
        (Array.isArray(line?.segments) && line.segments.some((segment) => Array.isArray(segment?.performerIds) && segment.performerIds.some((id) => performerIDs.has(id)))));
    }
    return false;
  }
  if (isRenditionLyricsDocument(document)) return true;
  const provenance = record(document?.provenance) ? document.provenance : null;
  if (record(provenance?.performerSegmentation)) return true;
  if (Array.isArray(document?.performers) && document.performers.length > 0) return true;
  const lines = Array.isArray(document?.lines)
    ? document.lines
    : Array.isArray(document?.extractedLines) ? document.extractedLines : [];
  return linesHavePerformerSegmentation(lines);
}

const COMPONENTS = Object.freeze([
  ["fullText", "Full 原文"],
  ["performerSegmentation", "演唱者分段"],
  ["gameProjection", "Game 投影"],
  ["ruby", "Ruby 注音"],
  ["versionEvidence", "版本判定"],
]);

function v3ComponentLabel(component) {
  const suffix = typeof component === "string" ? component.split("/").pop() : "";
  return ({
    full_text: "Full 原文",
    full_performer_segmentation: "Full 演唱者分段",
    full_ruby: "Full Ruby 注音",
    game_text: "Game 原文",
    game_performer_segmentation: "Game 演唱者分段",
    game_ruby: "Game Ruby 注音",
    relation: "Game / Full relation",
    version: "版本判定",
  })[suffix] || component;
}

export function resolvedLyricsComponentProvenance(document, renditionKey) {
  const rendition = selectedRendition(document, renditionKey);
  if (rendition) {
    return Array.isArray(rendition.provenance) ? rendition.provenance.filter(record).map((attribution) => ({
      component: attribution.component,
      label: v3ComponentLabel(attribution.component),
      renditionKey: rendition.key,
      identity: {
        provider: attribution.provider,
        title: attribution.title,
        revisionId: attribution.revisionId,
        canonicalUrl: attribution.revisionUrl,
        section: "",
        renditionKey: rendition.key,
      },
    })) : [];
  }
  if (isRenditionLyricsDocument(document)) return [];
  const provenance = record(document?.provenance) ? document.provenance : {};
  const identities = Array.isArray(document?.fixedIdentities) ? document.fixedIdentities : [];
  const byKey = new Map(identities.filter(record).map((identity) => [identity.renditionKey, identity]));
  const rows = [];
  for (const [component, label] of COMPONENTS) {
    const reference = provenance[component];
    if (!record(reference) || typeof reference.renditionKey !== "string" || !reference.renditionKey) continue;
    rows.push({ component, label, renditionKey: reference.renditionKey, identity: byKey.get(reference.renditionKey) ?? null });
  }
  return rows;
}
