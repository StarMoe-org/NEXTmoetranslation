"use client";

import * as Y from "yjs";
import { WebsocketProvider } from "y-websocket";
import type { SongLyricsDocument } from "./api";

export const LYRICS_YJS_ROOT = "lyrics";
export const LYRICS_YJS_SCHEMA_VERSION = 1;
export const LYRICS_YJS_RESYNC_INTERVAL_MS = 15_000;
export const LYRICS_YJS_LOCAL_ORIGIN = Symbol("lyrics-local-edit");

const RECONNECT_DELAYS_MS = [500, 1_000, 2_000, 4_000, 8_000, 10_000] as const;
const STRUCTURED_ITEM_ID_KEY = "__yjsId";
const STRUCTURED_ITEM_GENERATION_KEY = "__yjsGeneration";
const STRUCTURED_ITEM_ORIGIN_KEY = "__yjsOrigin";
const STRUCTURED_ARRAY_KEYS = new Set(["segments", "ruby"]);
const STRUCTURED_INTERNAL_KEYS = new Set([
  STRUCTURED_ITEM_ID_KEY, STRUCTURED_ITEM_GENERATION_KEY, STRUCTURED_ITEM_ORIGIN_KEY,
]);
const structuralOperationCounters = new WeakMap<Y.Doc, number>();
let detachedStructuralOperationCounter = 0;
const TEXT_KEYS = new Set([
  "attribution", "translationCredit", "proofreadingCredit", "sourceNote", "licenseNote",
  "japanese", "zh-CN", "en-US", "label", "translation", "proofreading", "text", "reading", "name",
]);
const STABLE_ARRAY_ID_FIELDS: Record<string, readonly string[]> = {
  lines: ["id"],
  renditions: ["key"],
  performers: ["performerId"],
  indexEvidenceRefs: ["evidenceId"],
  fixedIdentities: ["origin", "pageId", "revisionId", "section", "renditionKey"],
  provenance: ["component", "provider", "revisionId", "revisionUrl"],
  segments: [STRUCTURED_ITEM_ID_KEY],
  ruby: [STRUCTURED_ITEM_ID_KEY],
};

type YContainer = Y.Map<unknown> | Y.Array<unknown>;
type YValue = Y.Text | Y.Map<unknown> | Y.Array<unknown> | string | number | boolean | null;

interface StructuredItemMetadata {
  id: string;
  generation: string;
  origin?: string;
}

class LyricsStructuralConflict extends Error {}

export type LyricsCollaborationStatus = "connecting" | "synced" | "reconnecting" | "offline" | "error";

export interface LyricsCollaborationPeer {
  clientId: string;
  username: string;
  color: string;
}

export interface LyricsCollaborationTicket {
  ticket: string;
  room: string;
  expiresAt: string;
}

export interface LyricsCollaborationSnapshot {
  document: SongLyricsDocument | null;
  status: LyricsCollaborationStatus;
  peers: LyricsCollaborationPeer[];
  synced: boolean;
  /**
   * This client holds edits in its undo history that no checkpoint has committed yet, or
   * retired history whose edits the stored document has not been shown to contain.
   */
  localChanges: boolean;
  error?: Error;
}

/** The newest undo item a checkpoint request covers; null when there was none. */
export type LyricsCheckpointBoundary = Y.UndoManager["undoStack"][number] | null;

export interface LyricsCollaborationOptions {
  musicId: number;
  clientId: string;
  username: string;
  color: string;
  issueTicket: (musicId: number, signal?: AbortSignal) => Promise<LyricsCollaborationTicket>;
  onSnapshot: (snapshot: LyricsCollaborationSnapshot) => void;
}

const permanentlyClosed = (event: CloseEvent | null): boolean =>
  event !== null && event.code >= 4400 && event.code < 4500;

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function shouldUseText(key: string | null, value: unknown): value is string {
  return typeof value === "string" && key !== null && TEXT_KEYS.has(key);
}

function createSharedValue(value: unknown, key: string | null): YValue {
  if (shouldUseText(key, value)) {
    const text = new Y.Text();
    if (value) text.insert(0, value);
    return text;
  }
  if (Array.isArray(value)) {
    const array = new Y.Array<unknown>();
    if (value.length > 0) array.insert(0, value.map((item) => createSharedValue(item, null)));
    return array;
  }
  if (isRecord(value)) {
    const map = new Y.Map<unknown>();
    for (const [childKey, childValue] of Object.entries(value)) {
      if (childValue !== undefined) map.set(childKey, createSharedValue(childValue, childKey));
    }
    return map;
  }
  if (value === null || typeof value === "number" || typeof value === "boolean" || typeof value === "string") return value;
  return null;
}

function internalString(value: unknown, key: string): string | null {
  const item = value instanceof Y.Map ? value.get(key) : isRecord(value) ? value[key] : undefined;
  return typeof item === "string" && item.length > 0 ? item : null;
}

function structuredItemMetadata(value: unknown): StructuredItemMetadata | null {
  const id = internalString(value, STRUCTURED_ITEM_ID_KEY);
  const generation = internalString(value, STRUCTURED_ITEM_GENERATION_KEY);
  if (!id || !generation) return null;
  const origin = internalString(value, STRUCTURED_ITEM_ORIGIN_KEY);
  return { id, generation, ...(origin ? { origin } : {}) };
}

function validateStructuredArray(value: Y.Array<unknown>, key: string): void {
  const ids = new Set<string>();
  const originGenerations = new Map<string, Set<string>>();
  let legacyItems = 0;
  let identifiedItems = 0;
  for (const item of value.toArray()) {
    if (!(item instanceof Y.Map)) throw new LyricsStructuralConflict(`invalid_${key}_item`);
    const metadata = structuredItemMetadata(item);
    const hasInternalMetadata = Array.from(STRUCTURED_INTERNAL_KEYS).some((internalKey) => item.has(internalKey));
    if (!metadata) {
      if (hasInternalMetadata) throw new LyricsStructuralConflict(`incomplete_${key}_identity`);
      legacyItems++;
      continue;
    }
    identifiedItems++;
    if (ids.has(metadata.id)) throw new LyricsStructuralConflict(`conflicting_${key}_identity`);
    ids.add(metadata.id);
    if (!metadata.origin) continue;
    const generations = originGenerations.get(metadata.origin) || new Set<string>();
    generations.add(metadata.generation);
    originGenerations.set(metadata.origin, generations);
  }
  if (legacyItems > 0 && identifiedItems > 0) throw new LyricsStructuralConflict(`mixed_${key}_identity`);
  if (Array.from(originGenerations.values()).some((generations) => generations.size > 1)) {
    throw new LyricsStructuralConflict(`concurrent_${key}_structure`);
  }
}

function materializeValue(value: unknown, key: string | null = null, validateStructures = false): unknown {
  if (value instanceof Y.Text) return value.toString();
  if (value instanceof Y.Array) {
    if (validateStructures && key !== null && STRUCTURED_ARRAY_KEYS.has(key)) validateStructuredArray(value, key);
    return value.toArray().map((item) => materializeValue(item, null, validateStructures));
  }
  if (value instanceof Y.Map) {
    const result: Record<string, unknown> = {};
    value.forEach((child, childKey) => {
      if (!STRUCTURED_INTERNAL_KEYS.has(childKey)) {
        result[childKey] = materializeValue(child, childKey, validateStructures);
      }
    });
    return result;
  }
  return value;
}

function structuredSeedMetadata(key: string, itemPath: string): StructuredItemMetadata {
  const id = `${key}:${itemPath}`;
  return { id, generation: `seed:${id}` };
}

function nextStructuralOperation(target: Y.Array<unknown>, key: string): string {
  const doc = target.doc;
  if (!doc) return `edit:${key}:detached:${++detachedStructuralOperationCounter}`;
  const sequence = (structuralOperationCounters.get(doc) || 0) + 1;
  structuralOperationCounters.set(doc, sequence);
  return `edit:${key}:${doc.clientID}:${sequence}`;
}

function withStructuredMetadata(value: unknown, metadata: StructuredItemMetadata): unknown {
  if (!isRecord(value)) return value;
  return {
    ...value,
    [STRUCTURED_ITEM_ID_KEY]: metadata.id,
    [STRUCTURED_ITEM_GENERATION_KEY]: metadata.generation,
    ...(metadata.origin ? { [STRUCTURED_ITEM_ORIGIN_KEY]: metadata.origin } : {}),
  };
}

function publicEqual(current: unknown, next: unknown): boolean {
  return JSON.stringify(materializeValue(current)) === JSON.stringify(next);
}

function prepareStructuredArray(target: Y.Array<unknown> | null, next: unknown[], key: string, path: string): unknown[] {
  if (!target) {
    return next.map((item, index) => withStructuredMetadata(
      prepareSharedValue(undefined, item, null, `${path}[${index}]`),
      structuredSeedMetadata(key, `${path}[${index}]`),
    ));
  }

  const current = target.toArray();
  const matches = new Array<number>(next.length).fill(-1);
  const exactMatches = new Set<number>();
  const usedCurrent = new Set<number>();

  for (let desiredIndex = 0; desiredIndex < next.length; desiredIndex++) {
    for (let currentIndex = 0; currentIndex < current.length; currentIndex++) {
      if (usedCurrent.has(currentIndex) || !publicEqual(current[currentIndex], next[desiredIndex])) continue;
      matches[desiredIndex] = currentIndex;
      exactMatches.add(desiredIndex);
      usedCurrent.add(currentIndex);
      break;
    }
  }

  const remainingCurrent = current.map((_, index) => index).filter((index) => !usedCurrent.has(index));
  for (let desiredIndex = 0; desiredIndex < next.length && remainingCurrent.length > 0; desiredIndex++) {
    if (matches[desiredIndex] >= 0) continue;
    const currentIndex = remainingCurrent.shift();
    if (currentIndex !== undefined) matches[desiredIndex] = currentIndex;
  }

  const changedSources = matches
    .map((currentIndex, desiredIndex) => ({ currentIndex, desiredIndex }))
    .filter(({ currentIndex, desiredIndex }) => currentIndex >= 0 && !exactMatches.has(desiredIndex));
  const newItems = matches.map((currentIndex, desiredIndex) => ({ currentIndex, desiredIndex }))
    .filter(({ currentIndex }) => currentIndex < 0);
  const operation = newItems.length > 0 ? nextStructuralOperation(target, key) : "";
  const derivedFrom = new Map<number, { sourceIndex: number; metadata: StructuredItemMetadata; origin: string }>();
  const sourceOrigins = new Map<number, string>();

  if (changedSources.length > 0 && operation) {
    for (const { desiredIndex } of newItems) {
      const source = changedSources.reduce((best, candidate) =>
        Math.abs(candidate.desiredIndex - desiredIndex) < Math.abs(best.desiredIndex - desiredIndex) ? candidate : best);
      const sourceMetadata = structuredItemMetadata(current[source.currentIndex])
        || structuredSeedMetadata(key, `${path}[${source.currentIndex}]`);
      const origin = JSON.stringify([sourceMetadata.id, sourceMetadata.generation]);
      derivedFrom.set(desiredIndex, { sourceIndex: source.desiredIndex, metadata: sourceMetadata, origin });
      sourceOrigins.set(source.desiredIndex, origin);
    }
  }

  return next.map((item, desiredIndex) => {
    const currentIndex = matches[desiredIndex];
    if (currentIndex >= 0) {
      const prepared = prepareSharedValue(current[currentIndex], item, null, `${path}[${desiredIndex}]`);
      const existing = structuredItemMetadata(current[currentIndex])
        || structuredSeedMetadata(key, `${path}[${currentIndex}]`);
      const sourceOrigin = sourceOrigins.get(desiredIndex);
      return withStructuredMetadata(prepared, sourceOrigin
        ? { ...existing, generation: operation, origin: sourceOrigin }
        : existing);
    }

    const prepared = prepareSharedValue(undefined, item, null, `${path}[${desiredIndex}]`);
    const source = derivedFrom.get(desiredIndex);
    if (source) {
      return withStructuredMetadata(prepared, {
        id: `${key}:${operation}:${desiredIndex}`,
        generation: operation,
        origin: source.origin,
      });
    }
    const insertionOperation = operation || nextStructuralOperation(target, key);
    return withStructuredMetadata(prepared, {
      id: `${key}:${insertionOperation}:${desiredIndex}`,
      generation: insertionOperation,
    });
  });
}

function prepareSharedValue(current: unknown, next: unknown, key: string | null, path: string): unknown {
  if (Array.isArray(next)) {
    const target = current instanceof Y.Array ? current : null;
    if (key !== null && STRUCTURED_ARRAY_KEYS.has(key)) return prepareStructuredArray(target, next, key, path);

    const currentValues = target?.toArray() || [];
    const usedCurrent = new Set<number>();
    const stableIdentityArray = key !== null && Object.hasOwn(STABLE_ARRAY_ID_FIELDS, key);
    return next.map((item, index) => {
      let currentIndex = -1;
      const identity = stableArrayIdentity(item, key);
      if (identity !== null) {
        currentIndex = currentValues.findIndex((candidate, candidateIndex) =>
          !usedCurrent.has(candidateIndex) && stableArrayIdentity(candidate, key) === identity);
      }
      if (!stableIdentityArray && currentIndex < 0 && index < currentValues.length && !usedCurrent.has(index)) currentIndex = index;
      if (currentIndex >= 0) usedCurrent.add(currentIndex);
      return prepareSharedValue(currentIndex >= 0 ? currentValues[currentIndex] : undefined, item, null, `${path}[${index}]`);
    });
  }
  if (isRecord(next)) {
    const target = current instanceof Y.Map ? current : null;
    const result: Record<string, unknown> = {};
    for (const [childKey, childValue] of Object.entries(next)) {
      if (childValue !== undefined) {
        result[childKey] = prepareSharedValue(target?.get(childKey), childValue, childKey, `${path}.${childKey}`);
      }
    }
    return result;
  }
  return next;
}

function syncText(target: Y.Text, next: string): void {
  const current = target.toString();
  if (current === next) return;
  let prefix = 0;
  const maxPrefix = Math.min(current.length, next.length);
  while (prefix < maxPrefix && current.charCodeAt(prefix) === next.charCodeAt(prefix)) prefix++;
  let suffix = 0;
  const maxSuffix = Math.min(current.length - prefix, next.length - prefix);
  while (suffix < maxSuffix && current.charCodeAt(current.length - suffix - 1) === next.charCodeAt(next.length - suffix - 1)) suffix++;
  const deleteLength = current.length - prefix - suffix;
  if (deleteLength > 0) target.delete(prefix, deleteLength);
  const inserted = next.slice(prefix, next.length - suffix);
  if (inserted) target.insert(prefix, inserted);
}

function sharedKindMatches(current: unknown, next: unknown, key: string | null): boolean {
  if (shouldUseText(key, next)) return current instanceof Y.Text;
  if (Array.isArray(next)) return current instanceof Y.Array;
  if (isRecord(next)) return current instanceof Y.Map;
  return !(current instanceof Y.AbstractType);
}

function syncMap(target: Y.Map<unknown>, next: Record<string, unknown>): void {
  for (const key of Array.from(target.keys())) {
    if (!Object.hasOwn(next, key) || next[key] === undefined) target.delete(key);
  }
  for (const [key, nextValue] of Object.entries(next)) {
    if (nextValue === undefined) continue;
    const current = target.get(key);
    if (!sharedKindMatches(current, nextValue, key)) {
      target.set(key, createSharedValue(nextValue, key));
      continue;
    }
    syncSharedValue(current, nextValue, key, target, key);
  }
}

function replaceArrayValue(target: Y.Array<unknown>, index: number, value: unknown): void {
  target.delete(index, 1);
  target.insert(index, [value]);
}

function identityPart(value: unknown, field: string): string | null {
  const part = value instanceof Y.Map ? materializeValue(value.get(field)) : isRecord(value) ? value[field] : undefined;
  return (typeof part === "string" && part.length > 0) || typeof part === "number"
    ? `${typeof part}:${String(part)}`
    : null;
}

function stableArrayIdentity(value: unknown, arrayKey: string | null): string | null {
  if (arrayKey === null) return null;
  const fields = STABLE_ARRAY_ID_FIELDS[arrayKey];
  if (!fields) return null;
  const parts = fields.map((field) => identityPart(value, field));
  return parts.every((part): part is string => part !== null) ? `${arrayKey}:${parts.join("|")}` : null;
}

function uniqueStableIdentities(values: unknown[], arrayKey: string | null): string[] | null {
  const identities = values.map((value) => stableArrayIdentity(value, arrayKey));
  if (identities.some((identity) => identity === null)) return null;
  const stable = identities as string[];
  return new Set(stable).size === stable.length ? stable : null;
}

function syncStableArray(target: Y.Array<unknown>, next: unknown[], arrayKey: string | null): boolean {
  const desiredIdentities = uniqueStableIdentities(next, arrayKey);
  const currentIdentities = uniqueStableIdentities(target.toArray(), arrayKey);
  if (!desiredIdentities || !currentIdentities) return false;

  const desired = new Set(desiredIdentities);
  for (let index = currentIdentities.length - 1; index >= 0; index--) {
    if (!desired.has(currentIdentities[index])) target.delete(index, 1);
  }

  for (let index = 0; index < next.length; index++) {
    const desiredIdentity = desiredIdentities[index];
    const current = target.get(index);
    if (stableArrayIdentity(current, arrayKey) === desiredIdentity) {
      syncSharedValue(current, next[index], null, target, index);
      continue;
    }

    let existingIndex = -1;
    for (let candidate = index + 1; candidate < target.length; candidate++) {
      if (stableArrayIdentity(target.get(candidate), arrayKey) === desiredIdentity) {
        existingIndex = candidate;
        break;
      }
    }
    if (existingIndex >= 0) target.delete(existingIndex, 1);
    target.insert(index, [createSharedValue(next[index], null)]);
  }
  if (target.length > next.length) target.delete(next.length, target.length - next.length);
  return true;
}

function materializedEqual(current: unknown, next: unknown): boolean {
  return JSON.stringify(materializeValue(current)) === JSON.stringify(next);
}

function syncArray(target: Y.Array<unknown>, next: unknown[], arrayKey: string | null): void {
  if (syncStableArray(target, next, arrayKey)) return;

  if (target.length !== next.length) {
    let prefix = 0;
    const commonLength = Math.min(target.length, next.length);
    while (prefix < commonLength && materializedEqual(target.get(prefix), next[prefix])) prefix++;
    let suffix = 0;
    while (suffix < commonLength - prefix &&
      materializedEqual(target.get(target.length - suffix - 1), next[next.length - suffix - 1])) suffix++;
    const currentMiddleLength = target.length - prefix - suffix;
    if (currentMiddleLength > 0) target.delete(prefix, currentMiddleLength);
    const nextMiddle = next.slice(prefix, next.length - suffix);
    if (nextMiddle.length > 0) target.insert(prefix, nextMiddle.map((value) => createSharedValue(value, null)));
    return;
  }

  const commonLength = Math.min(target.length, next.length);
  for (let index = 0; index < commonLength; index++) {
    const current = target.get(index);
    const nextValue = next[index];
    if (!sharedKindMatches(current, nextValue, null)) {
      replaceArrayValue(target, index, createSharedValue(nextValue, null));
      continue;
    }
    syncSharedValue(current, nextValue, null, target, index);
  }
  if (target.length > next.length) target.delete(next.length, target.length - next.length);
  if (next.length > target.length) target.insert(target.length, next.slice(target.length).map((value) => createSharedValue(value, null)));
}

function syncSharedValue(current: unknown, next: unknown, key: string | null, parent: YContainer, slot: string | number): void {
  if (current instanceof Y.Text && typeof next === "string") {
    syncText(current, next);
  } else if (current instanceof Y.Map && isRecord(next)) {
    syncMap(current, next);
  } else if (current instanceof Y.Array && Array.isArray(next)) {
    syncArray(current, next, key);
  } else if (!Object.is(current, next)) {
    const replacement = createSharedValue(next, key);
    if (parent instanceof Y.Map) parent.set(String(slot), replacement);
    else replaceArrayValue(parent, Number(slot), replacement);
  }
}

// Shared Y.Map keys materialize in insertion order, which follows whichever peer
// (or the server seed) first wrote each key, so document equality must ignore it.
export function canonicalLyricsJSON(value: unknown): string {
  return JSON.stringify(canonicalValue(value));
}

function canonicalValue(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(canonicalValue);
  if (!isRecord(value)) return value;
  const sorted: Record<string, unknown> = {};
  for (const key of Object.keys(value).sort()) sorted[key] = canonicalValue(value[key]);
  return sorted;
}

export function materializeLyricsDocument(root: Y.Map<unknown>): SongLyricsDocument | null {
  if (root.get("schemaVersion") !== LYRICS_YJS_SCHEMA_VERSION) return null;
  let result: unknown;
  try {
    result = materializeValue(root, null, true);
  } catch (error) {
    if (error instanceof LyricsStructuralConflict) return null;
    throw error;
  }
  if (!isRecord(result)) return null;
  const document = { ...result };
  delete document.schemaVersion;
  return Number.isSafeInteger(document.musicId) && Number(document.musicId) > 0
    ? document as unknown as SongLyricsDocument
    : null;
}

export function syncLyricsDocument(root: Y.Map<unknown>, document: SongLyricsDocument): void {
  const next = prepareSharedValue(
    root,
    { schemaVersion: LYRICS_YJS_SCHEMA_VERSION, ...document },
    null,
    LYRICS_YJS_ROOT,
  );
  if (isRecord(next)) syncMap(root, next);
}

function websocketServerURL(): string {
  const configured = process.env.NEXT_PUBLIC_API_BASE?.replace(/\/api\/?$/, "") || window.location.origin;
  const base = configured.startsWith("http://") || configured.startsWith("https://")
    ? configured
    : new URL(configured || "/", window.location.origin).origin;
  return `${base.replace(/^http:/, "ws:").replace(/^https:/, "wss:").replace(/\/$/, "")}/yjs/lyrics`;
}

function validTicket(ticket: LyricsCollaborationTicket): boolean {
  return Boolean(ticket && typeof ticket.ticket === "string" && ticket.ticket &&
    typeof ticket.room === "string" && ticket.room &&
    typeof ticket.expiresAt === "string" && Number.isFinite(Date.parse(ticket.expiresAt)));
}

function peerFromState(value: unknown): LyricsCollaborationPeer | null {
  if (!isRecord(value)) return null;
  const user = isRecord(value.user) ? value.user : value;
  if (typeof user.clientId !== "string" || !user.clientId || typeof user.username !== "string" || !user.username ||
      typeof user.color !== "string" || !/^#[0-9A-Fa-f]{6}$/.test(user.color)) return null;
  return { clientId: user.clientId, username: user.username, color: user.color.toUpperCase() };
}

/**
 * Saving is possible whenever the document differs from the saved baseline, whoever made
 * the difference: edits a collaborator left unsaved in the shared document included.
 */
export function lyricsDocumentSaveable(document: SongLyricsDocument | null, baseline: string): boolean {
  return document != null && canonicalLyricsJSON(document) !== baseline;
}

/**
 * While a collaboration owns the document only this client's own edits make it dirty;
 * remote checkpoints and collaborators' edits change the shared document without doing so.
 * Pass null for localChanges when no collaboration owns the document.
 */
export function lyricsDocumentDirty(
  document: SongLyricsDocument | null,
  baseline: string,
  localChanges: boolean | null,
): boolean {
  return localChanges !== false && lyricsDocumentSaveable(document, baseline);
}

/**
 * Identifies the stored state a document's authority envelope records. Checkpoints,
 * publications and server reseeds change it; editing the shared document never does.
 */
export function lyricsAuthorityEnvelopeKey(document: SongLyricsDocument): string {
  return JSON.stringify([document.revision, document.status, document.publishedRevision || 0, document.updatedAt]);
}

export class LyricsCollaboration {
  doc: Y.Doc;
  root: Y.Map<unknown>;
  undoManager: Y.UndoManager;

  private readonly options: LyricsCollaborationOptions;
  private provider: WebsocketProvider | null = null;
  private abortController: AbortController | null = null;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private reconnectAttempt = 0;
  private canonicalRoom = "";
  private destroyed = false;
  private synced = false;
  private completedInitialSync = false;
  private status: LyricsCollaborationStatus = "connecting";
  private lastError: Error | undefined;
  private peers: LyricsCollaborationPeer[] = [];
  private reportedLocalChanges = false;
  private retainedLocalChanges = false;

  constructor(options: LyricsCollaborationOptions) {
    this.options = options;
    this.doc = new Y.Doc();
    this.root = this.doc.getMap<unknown>(LYRICS_YJS_ROOT);
    this.undoManager = this.createUndoManager();
    this.root.observeDeep(this.handleDocumentChange);
    void this.connect();
  }

  updateDocument(document: SongLyricsDocument): boolean {
    if (this.destroyed || !this.synced || document.musicId !== this.options.musicId) return false;
    this.doc.transact(() => syncLyricsDocument(this.root, document), LYRICS_YJS_LOCAL_ORIGIN);
    return true;
  }

  replaceAuthoritative(document: SongLyricsDocument): boolean {
    if (this.destroyed || !this.synced || document.musicId !== this.options.musicId) return false;
    this.doc.transact(() => syncLyricsDocument(this.root, document), this.provider || "checkpoint");
    return true;
  }

  updateAuthoritativeEnvelope(document: SongLyricsDocument): boolean {
    if (this.destroyed || !this.synced || document.musicId !== this.options.musicId) return false;
    const envelope: Record<string, unknown> = {
      status: document.status,
      revision: document.revision,
      updatedAt: document.updatedAt,
      ...(document.publishedRevision === undefined ? {} : { publishedRevision: document.publishedRevision }),
    };
    this.doc.transact(() => {
      if (document.publishedRevision === undefined) this.root.delete("publishedRevision");
      for (const [key, value] of Object.entries(envelope)) {
        const current = this.root.get(key);
        if (!sharedKindMatches(current, value, key)) this.root.set(key, createSharedValue(value, key));
        else syncSharedValue(current, value, key, this.root, key);
      }
    }, this.provider || "authoritative-envelope");
    return true;
  }

  beginCheckpoint(): LyricsCheckpointBoundary {
    this.undoManager.stopCapturing();
    return this.undoManager.undoStack.at(-1) ?? null;
  }

  /**
   * Drops the undo history up to and including boundary (all of it when omitted). A boundary
   * that is no longer on the stack was already retired, so nothing newer is dropped.
   */
  checkpointCommitted(boundary?: LyricsCheckpointBoundary): void {
    const stack = this.undoManager.undoStack;
    stack.splice(0, boundary === undefined ? stack.length : boundary === null ? 0 : stack.indexOf(boundary) + 1);
    this.undoManager.redoStack.splice(0);
    this.undoManager.stopCapturing();
    this.handleUndoStackChange();
  }

  /**
   * The authority envelope moved, so undoing the history recorded so far would revert
   * stored content: drop it. The server snapshots the room before it broadcasts the
   * envelope, so edits in that history may still be unsaved; they stay reported as local
   * changes until settleRetainedLocalChanges().
   */
  retireLocalHistory(): void {
    this.retainedLocalChanges = this.hasLocalChanges();
    this.checkpointCommitted();
  }

  /** No retired edit is left unsaved: the shared document matched the stored one. */
  settleRetainedLocalChanges(): void {
    if (!this.retainedLocalChanges) return;
    this.retainedLocalChanges = false;
    this.handleUndoStackChange();
  }

  hasLocalChanges(): boolean {
    return this.undoManager.canUndo() || this.retainedLocalChanges;
  }

  // Retired edits have no undo history left; they stay in the shared document as
  // unsaved shared edits instead of being reverted together with others' content.
  discardLocalChanges(): void {
    if (this.destroyed || !this.synced) return;
    this.retainedLocalChanges = false;
    while (this.undoManager.canUndo()) this.undoManager.undo();
    this.undoManager.clear();
    this.handleUndoStackChange();
  }

  reconnectNow(): void {
    if (this.destroyed) return;
    if (this.reconnectTimer) clearTimeout(this.reconnectTimer);
    this.reconnectTimer = null;
    this.abortController?.abort();
    this.abortController = null;
    this.reconnectAttempt = 0;
    this.teardownProvider();
    void this.connect();
  }

  getSnapshot(): LyricsCollaborationSnapshot {
    return {
      document: materializeLyricsDocument(this.root),
      status: this.status,
      peers: this.peers,
      synced: this.synced,
      localChanges: this.hasLocalChanges(),
      ...(this.lastError ? { error: this.lastError } : {}),
    };
  }

  destroy(): void {
    if (this.destroyed) return;
    this.destroyed = true;
    if (this.reconnectTimer) clearTimeout(this.reconnectTimer);
    this.reconnectTimer = null;
    this.abortController?.abort();
    this.abortController = null;
    this.teardownProvider();
    this.root.unobserveDeep(this.handleDocumentChange);
    this.undoManager.destroy();
    this.doc.destroy();
  }

  private readonly handleDocumentChange = (): void => {
    if (this.destroyed) return;
    if (this.synced) {
      const document = materializeLyricsDocument(this.root);
      if (!document || document.musicId !== this.options.musicId) {
        this.synced = false;
        this.teardownProvider();
        this.setStatus("error", new Error("invalid_lyrics_collaboration_document"));
        return;
      }
    }
    this.emit();
  };

  // Undo stack items are recorded after the document observers run, so a local edit
  // is reported by a second snapshot once the stack changes.
  private readonly handleUndoStackChange = (): void => {
    if (!this.destroyed && this.hasLocalChanges() !== this.reportedLocalChanges) this.emit();
  };

  private createUndoManager(): Y.UndoManager {
    const undoManager = new Y.UndoManager(this.root, { trackedOrigins: new Set([LYRICS_YJS_LOCAL_ORIGIN]) });
    undoManager.on("stack-item-added", this.handleUndoStackChange);
    undoManager.on("stack-item-popped", this.handleUndoStackChange);
    undoManager.on("stack-cleared", this.handleUndoStackChange);
    return undoManager;
  }

  private emit(): void {
    const snapshot = this.getSnapshot();
    this.reportedLocalChanges = snapshot.localChanges;
    this.options.onSnapshot(snapshot);
  }

  private setStatus(status: LyricsCollaborationStatus, error?: Error): void {
    this.status = status;
    this.lastError = error;
    this.emit();
  }

  private collectPeers(provider: WebsocketProvider): void {
    const peers = new Map<string, LyricsCollaborationPeer>();
    for (const [client, state] of provider.awareness.getStates()) {
      if (client === this.doc.clientID) continue;
      const peer = peerFromState(state);
      if (peer && peer.clientId !== this.options.clientId) peers.set(peer.clientId, peer);
    }
    this.peers = Array.from(peers.values()).sort((left, right) => left.username.localeCompare(right.username));
    this.emit();
  }

  private scheduleReconnect(error?: Error, permanent = false): void {
    if (this.destroyed || this.reconnectTimer) return;
    this.synced = false;
    if (permanent) {
      this.setStatus("error", error);
      return;
    }
    const delay = RECONNECT_DELAYS_MS[Math.min(this.reconnectAttempt++, RECONNECT_DELAYS_MS.length - 1)];
    this.setStatus("reconnecting", error);
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      void this.connect();
    }, delay);
  }

  private teardownProvider(): void {
    const provider = this.provider;
    this.provider = null;
    if (!provider) return;
    provider.shouldConnect = false;
    provider.destroy();
    provider.awareness.destroy();
    this.peers = [];
  }

  private resetDocumentForRoom(room: string): void {
    this.teardownProvider();
    this.root.unobserveDeep(this.handleDocumentChange);
    this.undoManager.destroy();
    this.doc.destroy();
    this.doc = new Y.Doc();
    this.root = this.doc.getMap<unknown>(LYRICS_YJS_ROOT);
    this.undoManager = this.createUndoManager();
    this.root.observeDeep(this.handleDocumentChange);
    this.retainedLocalChanges = false;
    this.completedInitialSync = false;
    this.synced = false;
    this.peers = [];
    this.canonicalRoom = room;
    this.emit();
  }

  private async connect(): Promise<void> {
    if (this.destroyed || this.provider || this.abortController) return;
    this.synced = false;
    this.setStatus(this.reconnectAttempt > 0 ? "reconnecting" : "connecting");
    const controller = new AbortController();
    this.abortController = controller;
    try {
      const ticket = await this.options.issueTicket(this.options.musicId, controller.signal);
      if (this.destroyed || controller.signal.aborted) return;
      if (!validTicket(ticket)) throw new Error("invalid_lyrics_collaboration_ticket");
      if (this.canonicalRoom && ticket.room !== this.canonicalRoom) this.resetDocumentForRoom(ticket.room);
      else this.canonicalRoom = ticket.room;
      const provider = new WebsocketProvider(websocketServerURL(), String(this.options.musicId), this.doc, {
        connect: false,
        params: { ticket: ticket.ticket },
        disableBc: true,
        resyncInterval: LYRICS_YJS_RESYNC_INTERVAL_MS,
        shouldReconnect: () => false,
      });
      this.provider = provider;
      provider.awareness.setLocalState({
        clientId: this.options.clientId,
        username: this.options.username,
        color: this.options.color,
      });
      provider.awareness.on("change", () => this.collectPeers(provider));
      provider.on("sync", (state) => {
        if (this.provider !== provider || !state) return;
        const document = materializeLyricsDocument(this.root);
        if (!document || document.musicId !== this.options.musicId) {
          this.teardownProvider();
          this.scheduleReconnect(new Error("invalid_lyrics_collaboration_document"));
          return;
        }
        this.synced = true;
        this.reconnectAttempt = 0;
        if (!this.completedInitialSync) {
          this.completedInitialSync = true;
          this.undoManager.clear();
        }
        this.setStatus("synced");
      });
      provider.on("connection-error", () => {
        if (this.provider === provider) this.lastError = new Error("lyrics_collaboration_connection_error");
      });
      provider.on("connection-close", (event) => {
        if (this.provider !== provider || this.destroyed) return;
        provider.shouldConnect = false;
        const permanent = permanentlyClosed(event);
        const error = event && permanent
          ? new Error(event.reason || `lyrics_collaboration_closed_${event.code}`)
          : this.lastError;
        this.teardownProvider();
        this.scheduleReconnect(error, permanent);
      });
      provider.connect();
    } catch (reason) {
      if (!controller.signal.aborted && !this.destroyed) {
        this.scheduleReconnect(reason instanceof Error ? reason : new Error("lyrics_collaboration_connect_failed"));
      }
    } finally {
      if (this.abortController === controller) this.abortController = null;
    }
  }
}
