/**
 * Typed API client for the moesekai v2 console backend.
 * All console calls hit /api/* (JWT, no-store). Public files are at /files/*.
 */

import {
  REFRESH_LOCK,
  clearSession,
  commitIdentitySession,
  commitRefreshedSession,
  ensureSessionMigrated,
  getSessionEnvelope,
  sameSessionIdentity,
  sameSessionVersion,
  validSession,
  validSessionRole,
  withSessionIdentityLock,
  type Session,
} from "./session";
import { collectCatalogPages } from "./catalog-pagination.mjs";
import {
  buildLyricsSavePayload,
  validateSongLyricsCheckpointResponse,
  validateSongLyricsMutationResponse,
} from "./lyrics-save.mjs";

export {
  clearSession,
  ensureSessionMigrated,
  getRole,
  getSessionEnvelope,
  getSessionEpoch,
  getSessionExpiresAt,
  getToken,
  getUsername,
  subscribeSessionChanged,
} from "./session";

// ---- Types (mirror the Go backend) ----

export interface FieldInfo {
  name: string;
  total: number;
  cnCount: number;
  humanCount: number;
  pinnedCount: number;
  llmCount: number;
  unknownCount: number;
}

export interface CategoryInfo {
  name: string;
  fields: FieldInfo[];
}

export interface TranslationEntry {
  key: string;
  text: string;
  source: string;
  ids?: string[];
  updatedAt?: number;
  speakerName?: string;
  japanese?: string;
  segmentId?: string;
  episodeNo?: string;
  entryType?: "title" | "talk";
  sourceHash?: string;
  revision?: number;
}

export interface EventStorySummary {
  eventId: number;
  eventName?: string;
  eventNameJapanese?: string;
  source: string;
  episodeCount: number;
  untranslatedCount: number;
  lastUpdated: number;
  allOfficialTagged?: boolean;
}

export interface EventAssociationIndex {
  categories: Record<string, Record<string, number[]>>;
}

export interface EventStoryEpisode {
  scenarioId: string;
  title: string;
  titleSource?: string;
  talkData: Record<string, string>;
  talkSources?: Record<string, string>;
  talkOrder?: string[];
  speakerNames?: Record<string, string>;
  segments?: EventStorySegment[];
}

export interface EventStorySegment {
  id: string;
  kind: "title" | "talk";
  position: number;
  japanese: string;
  sourceHash: string;
  text: string;
  source: string;
  revision?: number;
}

export interface EventStoryDetail {
  meta: { source: string; version: string; last_updated: number };
  episodes: Record<string, EventStoryEpisode>;
}

export interface EventScenarioSourceTalk {
  speaker: string;
  text: string;
  voices?: string[];
  volume?: number[];
  charIndex: number;
  chara2d?: number;
  talkDataIndex?: number;
}

export interface EventEpisodeScenarioSnapshot {
  scenarioId: string;
  fileName: string;
  sha256: string;
  parserVersion: number;
  rawJson: string;
  sourceTalks: EventScenarioSourceTalk[];
}

export interface EventEpisodeSnapshot {
  eventId: number;
  episodeNo: string;
  locale: Locale;
  revision: string;
  segments: EventStorySegment[];
  scenario: EventEpisodeScenarioSnapshot;
}

export interface TranslateStatus {
  translator: { running: boolean; lastRun?: string; lastMode?: string; lastError?: string; lastNote?: string };
  clients?: number;
}

export interface EditorGateStatus {
  version: number;
  instanceId: string;
  revision: number;
  generation: number;
  completedGeneration: number;
  running: boolean;
  lastRun: string;
}

export interface SongProvenance {
  musicId: number;
  source: string;
  revision: number;
  state: string;
  availableVersions: string[];
  updatedAt: string;
  hasDetail: boolean;
}

export interface LyricsProjectionSummary {
  totalSongs: number;
  bundleSongs: number;
  dbPublicationSongs: number;
  localizationSongs: number;
  degraded: boolean;
  degradedReason?: string;
  bundleReleaseId?: string;
}

export interface ProjectionStatus {
  generation: number;
  pending: boolean;
  lastSuccessAt?: string;
  lastError?: string;
  lyricsSummary?: LyricsProjectionSummary;
  song?: SongProvenance | null;
}

export interface LyricsCollaborationTicket {
  ticket: string;
  room: string;
  expiresAt: string;
}

export interface LoginResponse {
  token: string;
  username: string;
  role: "admin" | "editor";
  expiresAt: number;
}

export interface User {
  id: number;
  username: string;
  role: "admin" | "editor";
  createdAt: number;
}

export interface UpstreamStatus {
  enabled: boolean;
  repo?: string;
  branch?: string;
  versionURL?: string;
  versionFallbackURL?: string;
  versionFallbackURLs?: string[];
  lastSource?: string;
  lastCheck?: string;
  lastSuccess?: string;
  lastDataVersion?: string;
  changeDetectedAt?: string;
  lastSync?: string;
  lastError?: string;
  lastErrorAt?: string;
  consecutiveFailures?: number;
  rateLimitedUntil?: string;
  gitMirrorReady?: boolean;
}

export interface CNSyncResult {
  mode: string;
  categories: number;
  updatedEntries: number;
  eventStoryFiles: number;
  aiTranslationSkipped?: number;
  aiTranslationNote?: string;
  skipped?: string[];
  skippedDetails?: Record<string, string>;
}

export interface BackupStatus {
  running: boolean;
  s3Enabled: boolean;
  gitEnabled: boolean;
  lastBackup?: string;
  lastS3Backup?: string;
  lastGitBackup?: string;
  lastRestore?: string;
  lastError?: string;
  dailyHourUtc: number;
}

export type Locale = "ja-JP" | "zh-CN" | "en-US";

export interface LocalizedTitle {
  "ja-JP": string;
  "zh-CN"?: string;
  "en-US"?: string;
}

export interface RuntimeLyricsMetadata {
  releaseId: string;
  immutableOverlay: boolean;
  source?: "bundle" | "db_publication" | "localization_projection";
  state: "complete" | "game_only" | "satisfied_no_lyrics" | "incomplete";
  hasDetail: boolean;
  availableVersions: string[];
  revision: number;
  updatedAt: string;
  batchSha256?: string;
  rootSha256?: string;
}

export interface CatalogMusicItem {
  musicId: number;
  title: LocalizedTitle;
  jacketUrl?: string;
  isNewlyWrittenMusic: boolean;
  /** Editable/publishable SQLite state. This never describes the immutable embedded release. */
  lyricsStatus?: "draft" | "published" | "draft-published";
  /** Persisted SQLite availability state for reviewed songs without an editable text document. */
  lyricsAvailabilityState?: "satisfied_no_lyrics" | "ambiguous" | "missing" | "incomplete" | "failed";
  /**
   * What the public mirror currently serves for this song (embedded release overlaid by
   * database publications, minus withdrawals). Absent when nothing is served.
   */
  runtimeLyrics?: RuntimeLyricsMetadata;
  /** An operator withdrew this song from the public site. Omitted when false. */
  lyricsWithdrawn?: boolean;
  /** The song is a source-v3 document, whose saves go public unless withdrawn. Omitted when false. */
  lyricsSourceV3?: boolean;
}

export interface CatalogPerformerItem {
  performerId: number;
  name: LocalizedTitle;
}

export interface LyricRubySpan {
  text: string;
  reading?: string;
}

export type LyricsPerformerID = number | string;

export interface LyricSegment {
  text: string;
  performerIds: number[];
  ruby: LyricRubySpan[];
}

export interface LyricLine {
  id: string;
  order: number;
  japanese: string;
  "zh-CN"?: string;
  "en-US"?: string;
  stanzaBreakBefore?: boolean;
  segments: LyricSegment[];
  trailingPerformerIds?: number[];
}

/** The strict v3 line shape. String performer IDs keep rendition families independent from the legacy numeric catalog. */
export interface LyricsRenditionSegment {
  text: string;
  performerIds: string[];
  ruby: LyricRubySpan[];
}

export interface LyricsRenditionLine {
  id: string;
  order: number;
  japanese: string;
  "zh-CN"?: string;
  "en-US"?: string;
  stanzaBreakBefore?: boolean;
  segments: LyricsRenditionSegment[];
  trailingPerformerIds: string[];
}

export type LyricsEditorLine = LyricLine | LyricsRenditionLine;
export type LyricsEditorSegment = LyricSegment | LyricsRenditionSegment;

export type LyricsAvailableVersion = "full" | "game";

export interface LyricsGameProjection {
  reasonCode: "tagged_full_and_game" | "untagged_uncut_identity";
  lineIds: string[];
}

export type LyricsRenditionKind = "original" | "sekai" | "vocaloid" | "alternate";
export type LyricsRenditionRelationKind = "none" | "exact_projection";

export interface LyricsRenditionVersion {
  kind: LyricsRenditionKind;
  label: string;
}

export interface LyricsRenditionPerformer {
  performerId: string;
  name: string;
  color?: string;
}

export interface LyricsRenditionSide {
  version: LyricsRenditionVersion;
  lines: LyricsRenditionLine[];
}

export interface LyricsRenditionRelation {
  kind: LyricsRenditionRelationKind;
  fullRenditionKey?: string;
  lineIds?: string[];
}

export interface LyricsRenditionProvenance {
  component: string;
  provider: "vocaloid_fandom" | "moegirl" | "moegirl_public_exact" | "sekaipedia";
  title: string;
  revisionId: number;
  revisionUrl: string;
  licenseName: string;
  licenseUrl: string;
}

export interface LyricsRenditionTranslationCredits {
  translation?: string;
  proofreading?: string;
}

/**
 * The stable-key rendition editing model (REM). Full and Game are peers inside
 * one rendition; neither side is inferred from another rendition's equal text.
 */
export interface LyricsRendition {
  key: string;
  kind: LyricsRenditionKind;
  label: string;
  availableVersions: LyricsAvailableVersion[];
  performers: LyricsRenditionPerformer[];
  full?: LyricsRenditionSide;
  game?: LyricsRenditionSide;
  relation: LyricsRenditionRelation;
  sourceTabPaths: string[][];
  provenance: LyricsRenditionProvenance[];
  translationCredits?: LyricsRenditionTranslationCredits;
}

export interface TranslationEditionSummary {
  key: string;
  label: string;
}

export interface RenditionLyricsDocument {
  musicId: number;
  status: "draft" | "published" | "draft-published";
  revision: number;
  publishedRevision?: number;
  updatedAt: string;
  translationEditionKey: string;
  defaultTranslationEditionKey: string;
  translationEditions: TranslationEditionSummary[];
  /** Materialized source-v3 content for translationEditionKey only. */
  renditions: LyricsRendition[];
  /**
   * True (omitted otherwise) while the recovery import ledger owns the source document: saves that
   * change Japanese, ruby, segments, performers or line structure answer 422 source_drift.
   */
  recoveryLedgerOwned?: boolean;
}

export type LyricsTranslationEditionMutation =
  | { musicId: number; revision: number; operation: "create"; editionKey: string; label: string }
  | { musicId: number; revision: number; operation: "clone"; sourceEditionKey: string; editionKey: string; label: string }
  | { musicId: number; revision: number; operation: "rename"; editionKey: string; label: string }
  | { musicId: number; revision: number; operation: "set-default"; editionKey: string };

export type SongLyricsDocument = SongLyrics | RenditionLyricsDocument;

// ---- Whole-song lyrics document route (/editor/v1/lyrics/document) ----

/** One run of a line's ja markup; ja lines write ruby inline as {kanji|reading}. */
export interface LyricsDocumentSegment {
  ja: string;
  performerIds: number[];
}

export interface LyricsDocumentLine {
  ja: string;
  /** Text of the default translation edition. */
  zh?: string;
  /**
   * Text of declared non-default editions, keyed by edition; a missing key is an empty line in that
   * edition. Only on lines and on the gameLines of independent and only renditions.
   */
  zhEditions?: Record<string, string>;
  en?: string;
  performerIds?: number[];
  segments?: LyricsDocumentSegment[];
  stanzaBreakBefore?: boolean;
  inGame?: boolean;
}

export interface LyricsDocumentRendition {
  key: string;
  kind?: string;
  label?: string;
  performerIds?: number[];
  game?: "none" | "same" | "cut" | "independent" | "only";
  lines: LyricsDocumentLine[];
  gameLines?: LyricsDocumentLine[];
  /** Credits of the default translation edition for this rendition. */
  translationCredits?: LyricsRenditionTranslationCredits;
  /** Credits of declared non-default editions for this rendition, keyed by edition. */
  editionCredits?: Record<string, LyricsRenditionTranslationCredits>;
}

export interface LyricsDocumentRequest {
  musicId: number;
  expectedRevision?: number;
  source: { url: string; title?: string };
  /**
   * zh-CN translation editions; the first entry is the default edition, whose text is each line's zh.
   * Omitted: the one implicit edition main labelled 默认译本.
   */
  translationEditions?: TranslationEditionSummary[];
  translationCredit?: string;
  proofreadingCredit?: string;
  renditions: LyricsDocumentRendition[];
  dryRun?: boolean;
}

/** A 422 validation failure; line is zero-based and absent for rendition- or document-level issues. */
export interface LyricsDocumentIssue {
  rendition: string;
  side?: string;
  line?: number;
  field?: string;
  /** The translation edition key the issue is about. */
  edition?: string;
  message: string;
}

/** Something the served detail carries that the request format cannot; line is zero-based. */
export interface LyricsDocumentWarning {
  code: string;
  rendition?: string;
  side?: string;
  line?: number;
  message: string;
}

export interface LyricsDocumentExport {
  musicId: number;
  servedVersion: number;
  servedRevision: number;
  /** Which state was exported: what the site serves, or the database state when it serves nothing. */
  from: "served" | "database";
  /** document.expectedRevision is the revision PUT and takeover compare against. */
  document: LyricsDocumentRequest;
  warnings: LyricsDocumentWarning[];
}

export interface LyricsDocumentPublishResult {
  dryRun: boolean;
  musicId: number;
  revision: number;
  publicPath: string;
  /** The detail the site serves: public v3, or v4 when the song has several translation editions. */
  document: PublicLyricsV3Detail | { version: 4; [key: string]: unknown };
  changes: LyricsDocumentChanges;
}

export interface LyricsDocumentBeforeAfter<T> {
  before: T;
  after: T;
}

export interface LyricsDocumentRenditionSummary {
  key: string;
  game: string;
  lines: number;
  gameLines: number;
}

/** An added or removed line in the request format; performerIds and segments are resolved. */
export interface LyricsDocumentLineSummary {
  line: number;
  /** ja markup, ruby included. */
  ja: string;
  zh?: string;
  stanzaBreakBefore?: boolean;
  performerIds: number[];
  /** Only when the line has several segments or a segment's performers differ from the line's. */
  segments?: LyricsDocumentSegment[];
  /** Only on Full lines of a cut rendition. */
  inGame?: boolean;
  zhEditions?: Record<string, string>;
}

/** fields names ja, ruby, segments, performers, zh, zhEditions, stanzaBreakBefore and inGame. */
export interface LyricsDocumentLineChange {
  before: number;
  after: number;
  fields: string[];
  ja?: LyricsDocumentBeforeAfter<string>;
  zh?: LyricsDocumentBeforeAfter<string>;
  /** Changed keys of non-default editions only. */
  zhEditions?: Record<string, LyricsDocumentBeforeAfter<string>>;
  segments?: LyricsDocumentBeforeAfter<LyricsDocumentSegment[]>;
}

export interface LyricsDocumentSideChange {
  side: "full" | "game";
  linesBefore: number;
  linesAfter: number;
  addedCount: number;
  removedCount: number;
  changedCount: number;
  added?: LyricsDocumentLineSummary[];
  removed?: LyricsDocumentLineSummary[];
  changed?: LyricsDocumentLineChange[];
}

export interface LyricsDocumentRenditionChange {
  key: string;
  kind?: LyricsDocumentBeforeAfter<string>;
  label?: LyricsDocumentBeforeAfter<string>;
  game?: LyricsDocumentBeforeAfter<string>;
  translationCredits?: LyricsDocumentBeforeAfter<LyricsRenditionTranslationCredits>;
  /** Changed keys of non-default editions only. */
  editionCredits?: Record<string, LyricsDocumentBeforeAfter<LyricsRenditionTranslationCredits>>;
  sides?: LyricsDocumentSideChange[];
}

/** Bounded diff of a document request against what it replaces; empty entries are omitted. */
export interface LyricsDocumentChanges {
  against: "served" | "database" | "nothing";
  changed: boolean;
  truncated?: boolean;
  source?: { before: { url: string; title?: string } | null; after: { url: string; title?: string } };
  /** Present when the edition list or its order differs. */
  translationEditions?: LyricsDocumentBeforeAfter<TranslationEditionSummary[]>;
  renditionsAdded?: LyricsDocumentRenditionSummary[];
  renditionsRemoved?: LyricsDocumentRenditionSummary[];
  renditions?: LyricsDocumentRenditionChange[];
}

/** Takeover answers the publish result of the exported served song plus the export warnings. */
export interface LyricsDocumentTakeoverResult extends LyricsDocumentPublishResult {
  warnings: LyricsDocumentWarning[];
}

/** Public v3 detail envelope; kept separate from the authenticated editor envelope. */
export interface PublicLyricsV3Detail {
  version: 3;
  musicId: number;
  revision: number;
  updatedAt: string;
  state: "complete" | "game_only";
  renditions: LyricsRendition[];
}

export type LyricsRenditionProjectionStatus =
  | "full_only"
  | "game_only"
  | "exact_projection"
  | "independent_game"
  | "invalid";

export interface LyricsSourceIndexEvidenceRef {
  evidenceId: string;
  sha256: string;
}

export interface LyricsSourceFixedIdentity {
  provider: "vocaloid_fandom" | "moegirl" | "sekaipedia";
  origin: string;
  pageId: number;
  revisionId: number;
  sha1: string;
  title: string;
  canonicalUrl: string;
  fetchedAt: string;
  categories: string[];
  section: string;
  renditionKey: string;
  indexEvidenceRefs: LyricsSourceIndexEvidenceRef[];
}

export interface LyricsSourceComponentRef {
  renditionKey: string;
}

export interface LyricsSourceComponentProvenance {
  fullText: LyricsSourceComponentRef;
  performerSegmentation?: LyricsSourceComponentRef;
  gameProjection?: LyricsSourceComponentRef;
  ruby?: LyricsSourceComponentRef;
  versionEvidence: LyricsSourceComponentRef;
}

export interface SongLyrics {
  musicId: number;
  status: "draft" | "published" | "draft-published";
  revision: number;
  publishedRevision?: number;
  updatedAt: string;
  attribution: string;
  translationCredit: string;
  proofreadingCredit: string;
  sourceNote?: string;
  sourceUrl?: string;
  licenseNote?: string;
  sourcePageId?: number;
  sourceRevisionId?: number;
  sourceSha1?: string;
  sourceFetchedAt?: string;
  availableVersions?: LyricsAvailableVersion[];
  gameProjection?: LyricsGameProjection;
  reasonCode?: string;
  fixedIdentities?: LyricsSourceFixedIdentity[];
  provenance?: LyricsSourceComponentProvenance;
  lines: LyricLine[];
}

export interface LyricsSourceCandidate {
  pageId: number;
  title: string;
  canonicalUrl: string;
  revisionId: number;
  sha1: string;
  categories: string[];
}

export interface LyricsSourcePreview {
  canonicalUrl: string;
  pageId: number;
  revisionId: number;
  sha1: string;
  categories: string[];
  fetchedAt: string;
  lines: Array<{ japanese: string; stanzaBreakBefore?: boolean }>;
  structuredLines?: Array<{
    japanese: string;
    stanzaBreakBefore?: boolean;
    segments: Array<{ text: string; performerIds: string[]; ruby: LyricRubySpan[] }>;
    trailingPerformerIds: string[];
  }>;
  rubyGeneratorVersion?: string;
  importToken: string;
}

export type LyricsSourceReviewKind = "candidate_selection" | "artifact_review";
export type LyricsSourceReviewState = "pending" | "approved" | "rejected" | "superseded" | "cancelled";
export type LyricsSourceGateState = "not_applicable" | "pending" | "approved" | "rejected";
export type LyricsSourceReviewEvidenceGate = "identity" | "source_use" | "parse";
export type LyricsSourceReviewGate = LyricsSourceReviewEvidenceGate | "overall";
export interface LyricsSourceReviewSummary {
  reviewId: number;
  kind: LyricsSourceReviewKind;
  state: LyricsSourceReviewState;
  musicId: number;
  title: string;
  catalogFingerprint: string;
  reasonCode: string;
  identityGate: LyricsSourceGateState;
  sourceUseGate: LyricsSourceGateState;
  parseGate: LyricsSourceGateState;
  version: number;
  priority: number;
  createdAt: string;
  updatedAt: string;
}
export interface LyricsSourceReviewDecisionFact {
  decisionId: number;
  gate: LyricsSourceReviewGate | "candidate";
  decision: "approved" | "rejected" | "selected" | "excluded";
  selectedCandidate?: LyricsSourceCandidate;
  actor: string;
  note: string;
  expectedVersion: number;
  resultVersion: number;
  decidedAt: string;
}
export interface LyricsSourceRubySpan {
  text: string;
  reading?: string;
}
export interface LyricsSourceSegment {
  text: string;
  performerIds: string[];
  ruby: LyricsSourceRubySpan[];
}
export interface LyricsSourceExtractedLine {
  japanese: string;
  stanzaBreakBefore?: boolean;
  segments: LyricsSourceSegment[];
  trailingPerformerIds: string[];
}
export interface LyricsSourceReviewDetail {
  review: LyricsSourceReviewSummary;
  candidates: LyricsSourceCandidate[];
  artifact?: {
    sourceType: string; sourceOrigin: string; pageId: number; revisionId: number;
    pageTitle: string; canonicalRevisionUrl: string; mediaWikiSha1: string; categories: string[];
    firstFetchedAt: string;
  };
  analysis?: {
    matchingPolicyVersion: string; restrictionPolicyVersion: string; extractorVersion: string;
    matchOutcome: string; restrictionOutcome: string; extractionOutcome: string;
    matchingEvidence: Array<{ ruleId: string; gate: string; outcome: string; summary: string }>;
    restrictionRuleIds: string[];
    selectedVersion: { kind: "sekai" | "vocaloid" | "original"; label: string };
    performers: Array<{ performerId: string; name: string; color?: string }>;
    rubyGeneratorVersion: string;
    extractedLines: LyricsSourceExtractedLine[];
  };
  associations: Array<{ musicId: number; catalogFingerprint: string; kind: "full_target" | "game_size_evidence" }>;
  decisions: LyricsSourceReviewDecisionFact[];
}
export interface LyricsSourceReviewMutationResult {
  reviewId: number; state: LyricsSourceReviewState; identityGate: LyricsSourceGateState;
  sourceUseGate: LyricsSourceGateState; parseGate: LyricsSourceGateState; version: number; replayed: boolean;
}
export interface LyricsSourceReviewBatchDecisionItem {
  reviewId: number;
  expectedVersion: number;
}
export interface LyricsSourceReviewDecisionRequest {
  gate: "overall";
  decision: "approved" | "rejected";
  idempotencyKey: string;
  note: "";
  reviewId?: number;
  expectedVersion?: number;
  items?: LyricsSourceReviewBatchDecisionItem[];
}
export interface LyricsSourceReviewBatchMutationItem {
  reviewId: number;
  state: LyricsSourceReviewState;
  version: number;
}
export interface LyricsSourceReviewBatchDecisionResponse {
  items: LyricsSourceReviewBatchMutationItem[];
  replayed: boolean;
}

type LyricsSourceReviewMutationExpectation =
  | { kind: "overall"; reviewId: number; expectedVersion: number; decision: "approved" | "rejected" }
  | { kind: "candidate"; reviewId: number; expectedVersion: number; exclude: boolean };

function isStrictLyricsSourceReviewMutationResponse(
  value: unknown,
  expected: LyricsSourceReviewMutationExpectation,
): value is LyricsSourceReviewMutationResult {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const response = value as Record<string, unknown>;
  const keys = ["reviewId", "state", "identityGate", "sourceUseGate", "parseGate", "version", "replayed"];
  if (Object.keys(response).length !== keys.length || !keys.every((key) => Object.hasOwn(response, key))) return false;
  const expectedState = expected.kind === "overall" ? expected.decision : expected.exclude ? "rejected" : "approved";
  const expectedGate = expected.kind === "overall" ? expected.decision : "not_applicable";
  return Number.isSafeInteger(response.reviewId) && response.reviewId === expected.reviewId &&
    Number.isSafeInteger(response.version) && response.version === expected.expectedVersion + 1 &&
    response.state === expectedState && response.identityGate === expectedGate &&
    response.sourceUseGate === expectedGate && response.parseGate === expectedGate &&
    typeof response.replayed === "boolean";
}

function invalidLyricsSourceReviewMutationResponse(): APIError {
  return new APIError(502, {
    error: "invalid_lyrics_source_review_response",
    details: ["审核响应未与审核编号、版本、状态和检查门逐字段关联"],
  });
}

function isEditorGateStatus(value: unknown): value is EditorGateStatus {
  if (!value || typeof value !== "object") return false;
  const status = value as Partial<EditorGateStatus>;
  return status.version === 1 && typeof status.instanceId === "string" && status.instanceId.length > 0 &&
    Number.isSafeInteger(status.revision) && Number(status.revision) >= 0 &&
    Number.isSafeInteger(status.generation) && Number(status.generation) >= 0 &&
    Number.isSafeInteger(status.completedGeneration) && Number(status.completedGeneration) >= 0 &&
    typeof status.running === "boolean" && typeof status.lastRun === "string";
}

interface APIErrorBody {
  error?: string;
  details?: string[];
  current?: SongLyricsDocument;
  results?: Record<string, string>;
  issues?: LyricsDocumentIssue[];
}

export class APIError extends Error {
  status: number;
  code: string;
  details: string[];
  current?: SongLyricsDocument;
  producerStatus?: EditorGateStatus;
  results?: Record<string, string>;
  issues: LyricsDocumentIssue[];

  constructor(status: number, body: APIErrorBody | EditorGateStatus) {
    const producerStatus = isEditorGateStatus(body) ? body : undefined;
    const contractBody = producerStatus ? undefined : body as APIErrorBody;
    super(producerStatus ? "producer_state_changed" : contractBody?.error || `HTTP ${status}`);
    this.name = "APIError";
    this.status = status;
    this.code = producerStatus ? "producer_state_changed" : contractBody?.error || "request_failed";
    this.details = producerStatus ? ["内容生产状态已变化，请完成重新校对后再保存"] : contractBody?.details || [];
    this.current = contractBody?.current;
    this.producerStatus = producerStatus;
    this.results = contractBody?.results;
    this.issues = Array.isArray(contractBody?.issues) ? contractBody.issues : [];
  }
}

// ---- Per-tab client identity ----

let clientID = "";
let loadedProducerState: { epoch: string; header: string } | null = null;
const PRODUCER_PROOF_INVALIDATED_EVENT = "moesekai-producer-proof-invalidated";

export function getClientID(): string {
  if (typeof window === "undefined") return "";
  if (!clientID) clientID = crypto.randomUUID();
  return clientID;
}

export function clearLoadedProducerState(): void {
  loadedProducerState = null;
}

export function acceptLoadedProducerState(status: EditorGateStatus): boolean {
  const envelope = getSessionEnvelope();
  if (!envelope?.session || status.version !== 1 || status.running ||
      !/^[A-Za-z0-9_-]+$/.test(status.instanceId) ||
      !Number.isSafeInteger(status.revision) || status.revision < 0 ||
      !Number.isSafeInteger(status.generation) || status.generation < 0 ||
      !Number.isSafeInteger(status.completedGeneration) || status.completedGeneration < 0 ||
      status.revision < status.generation || status.completedGeneration !== status.generation) {
    loadedProducerState = null;
    return false;
  }
  loadedProducerState = {
    epoch: envelope.epoch,
    header: `${status.instanceId}:${status.revision}:${status.completedGeneration}`,
  };
  return true;
}

function invalidateLoadedProducerState(): void {
  loadedProducerState = null;
  if (typeof window !== "undefined") window.dispatchEvent(new Event(PRODUCER_PROOF_INVALIDATED_EVENT));
}

export function subscribeProducerProofInvalidated(listener: () => void): () => void {
  if (typeof window === "undefined") return () => {};
  window.addEventListener(PRODUCER_PROOF_INVALIDATED_EVENT, listener);
  return () => window.removeEventListener(PRODUCER_PROOF_INVALIDATED_EVENT, listener);
}

// ---- Fetch helper ----

const API_BASE = process.env.NEXT_PUBLIC_API_BASE || "/api";

async function apiFetch<T>(path: string, options?: RequestInit, requireProducerProof = false): Promise<T> {
  await ensureSessionMigrated();
  const initiated = await withSessionIdentityLock("shared", () => {
    const envelope = getSessionEnvelope();
    const token = envelope?.session?.token;
    const loaded = loadedProducerState;
    const proof = loaded && loaded.epoch === envelope?.epoch ? loaded.header : "";
    if (requireProducerProof && !proof) {
      invalidateLoadedProducerState();
      throw new APIError(409, { error: "内容版本尚未完成校对，请重试" });
    }
    return {
      envelope,
      response: fetch(`${API_BASE}${path}`, {
        ...options,
        headers: {
          "Content-Type": "application/json",
          ...(token ? { Authorization: `Bearer ${token}` } : {}),
          ...options?.headers,
          ...(requireProducerProof && proof ? { "X-Moe-Loaded-Producer-State": proof } : {}),
        },
      }),
    };
  });
  const res = await initiated.response;
  if (res.status === 401) {
    if (initiated.envelope?.session && await clearSession(initiated.envelope) && typeof window !== "undefined") {
      window.location.reload();
    }
    throw new APIError(401, { error: "未授权" });
  }
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: res.statusText }));
    if (requireProducerProof && (res.status === 400 || res.status === 428 ||
        res.status === 409 && isEditorGateStatus(err))) invalidateLoadedProducerState();
    throw new APIError(res.status, err);
  }
  let body: T;
  if (res.status === 204) {
    body = undefined as T;
  } else {
    try {
      body = await res.json() as T;
    } catch {
      throw new APIError(502, { error: "invalid_json_response", details: ["成功响应必须包含有效的 JSON"] });
    }
  }
  const current = await withSessionIdentityLock("shared", getSessionEnvelope);
  if (initiated.envelope && !sameSessionIdentity(current, initiated.envelope)) {
    throw new APIError(409, { error: "会话已变化，请重试" });
  }
  return body;
}

// ---- Auth ----

async function authenticateAndCommit(path: "/auth/login" | "/auth/setup", username: string, password: string): Promise<LoginResponse> {
  await ensureSessionMigrated();
  const previous = await withSessionIdentityLock("shared", getSessionEnvelope);
  const response = await fetch(`${API_BASE}${path}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ username, password }),
  });
  const body = await response.json().catch(() => ({ error: response.statusText }));
  if (!response.ok) throw new APIError(response.status, body);
  if (!validSession(body) || body.expiresAt * 1000 <= Date.now()) {
    throw new APIError(502, { error: "登录响应包含无效会话" });
  }
  const committed = await commitIdentitySession(body, previous);
  if (!committed?.session) throw new APIError(409, { error: "登录期间会话已变化" });
  return { ...committed.session };
}

export const login = (username: string, password: string) =>
  authenticateAndCommit("/auth/login", username, password);
export const fetchMe = async () => {
  const principal = await apiFetch<{ username: unknown; role: unknown }>("/auth/me");
  if (typeof principal.username !== "string" || !principal.username || !validSessionRole(principal.role)) {
    throw new APIError(502, { error: "身份响应无效" });
  }
  return principal as { username: string; role: "admin" | "editor" };
};
export const refreshSession = async (): Promise<{ token: string; expiresAt: number }> => {
  await ensureSessionMigrated();
  const dispatched = await withSessionIdentityLock("shared", getSessionEnvelope);
  if (!dispatched?.session) throw new Error("会话信息不完整");
  const refresh = async () => {
    const current = await withSessionIdentityLock("shared", getSessionEnvelope);
    if (!current?.session) throw new Error("会话已结束");
    if (!sameSessionVersion(current, dispatched)) {
      return { token: current.session.token, expiresAt: current.session.expiresAt };
    }
    const response = await fetch(`${API_BASE}/auth/refresh`, {
      method: "POST",
      headers: { "Content-Type": "application/json", Authorization: `Bearer ${current.session.token}` },
    });
    const refreshed = await response.json().catch(() => ({ error: response.statusText }));
    if (!response.ok) {
      if (response.status === 401) await clearSession(current);
      throw new APIError(response.status, refreshed);
    }
    const token = typeof refreshed.token === "string" ? refreshed.token : "";
    const expiresAt = Number(refreshed.expiresAt || 0);
    if (!token || !Number.isSafeInteger(expiresAt) || expiresAt * 1000 <= Date.now()) {
      throw new APIError(502, { error: "刷新响应包含无效会话" });
    }
    // The refresh endpoint has already issued a valid successor. Commit it
    // before the follow-up identity probe so a transient /auth/me failure does
    // not leave the browser holding the now-superseded predecessor token.
    const successor: Session = { token, expiresAt, username: current.session.username, role: current.session.role };
    const committed = await commitRefreshedSession(successor, current);
    if (!committed?.session) {
      const winner = await withSessionIdentityLock("shared", getSessionEnvelope);
      if (!winner?.session) throw new APIError(409, { error: "刷新期间会话已变化" });
      return { token: winner.session.token, expiresAt: winner.session.expiresAt };
    }
    let meResponse: Response;
    try {
      meResponse = await fetch(`${API_BASE}/auth/me`, {
        headers: { "Content-Type": "application/json", Authorization: `Bearer ${token}` },
      });
    } catch {
      return { token: committed.session.token, expiresAt: committed.session.expiresAt };
    }
    if (meResponse.status === 429 || meResponse.status >= 500) {
      await meResponse.body?.cancel().catch(() => {});
      return { token: committed.session.token, expiresAt: committed.session.expiresAt };
    }
    const principal = await meResponse.json().catch(() => ({ error: meResponse.statusText }));
    if (!meResponse.ok || typeof principal.username !== "string" || !principal.username || !validSessionRole(principal.role)) {
      if (meResponse.status === 401) await clearSession(committed);
      throw new APIError(meResponse.ok ? 502 : meResponse.status, { error: "刷新后的身份响应无效" });
    }
    const candidate: Session = { token, expiresAt, username: principal.username, role: principal.role };
    const verified = await commitRefreshedSession(candidate, committed);
    if (!verified?.session) {
      const winner = await withSessionIdentityLock("shared", getSessionEnvelope);
      if (!winner?.session) throw new APIError(409, { error: "刷新期间会话已变化" });
      return { token: winner.session.token, expiresAt: winner.session.expiresAt };
    }
    return { token: verified.session.token, expiresAt: verified.session.expiresAt };
  };
  if (!navigator.locks?.request) throw new Error("会话刷新需要浏览器 Web Locks 支持");
  return navigator.locks.request(REFRESH_LOCK, { mode: "exclusive" }, refresh);
};

// First-run setup: when no users exist, the console registers the first admin.
export const getSetupStatus = () => apiFetch<{ needsSetup: boolean }>("/auth/setup-status");
export const setupAdmin = (username: string, password: string) =>
  authenticateAndCommit("/auth/setup", username, password);

// ---- Translations ----

function addLocale(params: URLSearchParams, locale?: Locale) {
  if (locale) params.set("locale", locale);
}

export const getCategories = (locale?: Locale) => {
  const p = new URLSearchParams();
  addLocale(p, locale);
  return apiFetch<CategoryInfo[]>(`/categories${p.size ? `?${p}` : ""}`);
};
export const getEditorGateStatus = () => apiFetch<EditorGateStatus>("/editor-gate/status");
export const getProjectionStatus = (musicId?: number) => {
  const p = musicId && musicId > 0 ? `?musicId=${musicId}` : "";
  return apiFetch<ProjectionStatus>(`/projection/status${p}`);
};
export const publishProjection = () => apiFetch<ProjectionStatus>("/projection/publish", { method: "POST" });
export const getEntries = (category: string, field: string, source?: string, locale?: Locale) => {
  const p = new URLSearchParams({ category, field });
  if (source) p.set("source", source);
  addLocale(p, locale);
  return apiFetch<TranslationEntry[]>(`/entries?${p}`);
};
export interface EntryMutationResponse {
  status: "ok" | "noop";
  category: string;
  field: string;
  key: string;
  text: string;
  source: string;
  locale?: Locale;
}

function validateEntryMutationResponse(
  value: unknown,
  expected: { category: string; field: string; key: string; text: string; source: string; locale?: Locale },
): value is EntryMutationResponse {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const response = value as Record<string, unknown>;
  const required = ["status", "category", "field", "key", "text", "source"];
  const allowed = new Set([...required, "locale"]);
  const keys = Object.keys(response);
  if (!required.every((name) => Object.hasOwn(response, name)) || keys.some((name) => !allowed.has(name))) return false;
  if (response.status !== "ok" && response.status !== "noop") return false;
  if (response.category !== expected.category || response.field !== expected.field || response.key !== expected.key ||
      response.text !== expected.text || response.source !== expected.source) return false;
  const responseHasLocale = Object.hasOwn(response, "locale");
  return expected.locale === undefined
    ? !responseHasLocale
    : responseHasLocale && response.locale === expected.locale;
}

export const updateEntry = async (
  category: string, field: string, key: string, text: string, source: string, locale?: Locale,
): Promise<EntryMutationResponse> => {
  const payload = { category, field, key, text, source, clientId: getClientID(), ...(locale ? { locale } : {}) };
  const response = await apiFetch<unknown>("/editor/v1/entry?response=correlated-v1", {
    method: "PUT",
    body: JSON.stringify(payload),
  }, true);
  if (!validateEntryMutationResponse(response, payload)) {
    throw new APIError(502, { error: "invalid_entry_response", details: ["词条保存响应未与提交内容逐字段关联"] });
  }
  return response;
};

// ---- Event stories ----

export const getEventAssociations = () => apiFetch<EventAssociationIndex>("/event-associations");

export const getEventStories = (locale?: Locale) => {
  const p = new URLSearchParams();
  addLocale(p, locale);
  return apiFetch<EventStorySummary[]>(`/event-stories${p.size ? `?${p}` : ""}`);
};
export const getEventStory = (eventId: number, locale?: Locale) => {
  const p = new URLSearchParams({ eventId: String(eventId) });
  addLocale(p, locale);
  return apiFetch<EventStoryDetail>(`/event-story?${p}`);
};
export const getEventEpisodeSnapshot = (eventId: number, episodeNo: string, locale: Locale) => {
  const p = new URLSearchParams({ eventId: String(eventId), episodeNo, locale });
  return apiFetch<EventEpisodeSnapshot>(`/event-story/episode-snapshot?${p}`);
};
export interface EventStoryUpdateResult {
  status: "ok";
  revision: number;
}

export const updateEventStoryLine = async (
  eventId: number, episodeNo: string, jpKey: string, cnText: string,
  source: string, entryType: "talk" | "title", locale: Locale,
  segmentId: string, sourceHash: string, revision: number,
): Promise<EventStoryUpdateResult> => {
  const response = await apiFetch<unknown>("/editor/v1/event-story/update", {
    method: "PUT",
    body: JSON.stringify({ eventId, episodeNo, jpKey, cnText, source, entryType, locale, segmentId, sourceHash,
      revision, clientId: getClientID() }),
  }, true);
  if (!response || typeof response !== "object" || (response as { status?: unknown }).status !== "ok" ||
      (response as { revision?: unknown }).revision !== revision + 1) {
    throw new APIError(502, { error: "invalid_event_story_response", details: ["剧情保存响应未返回下一修订号"] });
  }
  return response as EventStoryUpdateResult;
};
export const promoteEventStoryHuman = (eventId: number) =>
  apiFetch<{ status: string }>("/editor/v1/event-story/promote-human", {
    method: "POST", body: JSON.stringify({ eventId, clientId: getClientID() }),
  }, true);
export const retryEventStory = (eventId: number) =>
  apiFetch<Record<string, unknown>>("/event-story/retry", {
    method: "POST", body: JSON.stringify({ eventId, clientId: getClientID() }),
  });
export const reorderEventStory = (eventId: number) =>
  apiFetch<Record<string, unknown>>("/event-story/reorder", {
    method: "POST", body: JSON.stringify({ eventId, clientId: getClientID() }),
  });

// ---- Translation engine ----

export const getTranslateStatus = () => apiFetch<TranslateStatus>("/translate/status");
export const runCNSync = () => apiFetch<CNSyncResult>("/translate/cn-sync", { method: "POST" });
export const triggerAITranslate = (category: string, field: string, provider: "gemini" | "openai") =>
  apiFetch<Record<string, unknown>>("/translate/ai", { method: "POST", body: JSON.stringify({ category, field, provider }) });
export const triggerAITranslateAll = (provider: "gemini" | "openai") =>
  apiFetch<Record<string, unknown>>("/translate/ai-all", { method: "POST", body: JSON.stringify({ provider }) });
export const triggerAIStory = (eventId: number, provider: "gemini" | "openai") =>
  apiFetch<Record<string, unknown>>("/translate/ai-story", {
    method: "POST", body: JSON.stringify({ eventId, provider, clientId: getClientID() }),
  });

// ---- Admin ----

export const listUsers = () => apiFetch<User[]>("/admin/users");
export const createUser = (username: string, password: string, role: "admin" | "editor") =>
  apiFetch<User>("/admin/users", { method: "POST", body: JSON.stringify({ username, password, role }) });
export const updateUser = (username: string, patch: { password?: string; role?: "admin" | "editor" }) =>
  apiFetch<{ status: string }>("/admin/users", { method: "PUT", body: JSON.stringify({ username, ...patch }) });
export const deleteUser = (username: string) =>
  apiFetch<{ status: string }>(`/admin/users?username=${encodeURIComponent(username)}`, { method: "DELETE" });

export const getSettings = () => apiFetch<{ settings: Record<string, string>; hasMasterKey: boolean }>("/admin/settings");
export const updateSettings = (patch: Record<string, string>) =>
  apiFetch<{ status: string; applied: number }>("/admin/settings", { method: "PUT", body: JSON.stringify(patch) });

export interface LyricsProviderContributorAlias {
  catalogContributor: string;
  providerContributor: string;
}

export interface LyricsProviderTarget {
  musicId: number;
  pageTitle: string;
  resolvedPageTitle: string;
  aliases: LyricsProviderContributorAlias[];
  updatedAt: number;
  updatedBy: string;
}

export const getLyricsProviderTargets = () =>
  apiFetch<{ items: LyricsProviderTarget[] }>("/admin/lyrics-providers/sekaipedia/targets");
export const putLyricsProviderTarget = (
  musicId: number,
  target: { pageTitle: string; resolvedPageTitle?: string; aliases?: LyricsProviderContributorAlias[] },
) =>
  apiFetch<LyricsProviderTarget>(`/admin/lyrics-providers/sekaipedia/targets/${musicId}`, {
    method: "PUT", body: JSON.stringify(target),
  });
export const deleteLyricsProviderTarget = (musicId: number) =>
  apiFetch<{ musicId: number; deleted: boolean }>(`/admin/lyrics-providers/sekaipedia/targets/${musicId}`, {
    method: "DELETE",
  });

export const getUpstreamStatus = () => apiFetch<UpstreamStatus>("/admin/upstream");
export const checkUpstream = (force = false) =>
  apiFetch<UpstreamStatus>("/admin/upstream/check", { method: "POST", body: JSON.stringify({ force }) });

export const getLyricsSourceReviews = (filters: { kind?: LyricsSourceReviewKind; state?: LyricsSourceReviewState; gate?: LyricsSourceReviewGate; cursor?: string; limit?: number } = {}) => {
  const query = new URLSearchParams();
  if (filters.kind) query.set("kind", filters.kind);
  if (filters.state) query.set("state", filters.state);
  if (filters.gate) query.set("gate", filters.gate);
  if (filters.cursor) query.set("cursor", filters.cursor);
  if (filters.limit !== undefined) query.set("limit", String(filters.limit));
  return apiFetch<{ items: LyricsSourceReviewSummary[]; nextCursor?: string }>(`/admin/lyrics-source-reviews${query.size ? `?${query}` : ""}`);
};
export const getLyricsSourceReviewDetail = (reviewId: number) =>
  apiFetch<LyricsSourceReviewDetail>(`/admin/lyrics-source-reviews/detail?reviewId=${reviewId}`);
export function decideLyricsSourceReview(request: LyricsSourceReviewDecisionRequest & { reviewId: number; expectedVersion: number; items?: never }): Promise<LyricsSourceReviewMutationResult>;
export function decideLyricsSourceReview(request: LyricsSourceReviewDecisionRequest & { items: LyricsSourceReviewBatchDecisionItem[]; reviewId?: never; expectedVersion?: never }): Promise<LyricsSourceReviewBatchDecisionResponse>;
export async function decideLyricsSourceReview(request: LyricsSourceReviewDecisionRequest): Promise<LyricsSourceReviewMutationResult | LyricsSourceReviewBatchDecisionResponse> {
  const response = await apiFetch<unknown>("/admin/lyrics-source-reviews/decision", { method: "PUT", body: JSON.stringify(request) });
  if (Array.isArray(request.items)) return response as LyricsSourceReviewBatchDecisionResponse;
  const expected = { kind: "overall" as const, reviewId: request.reviewId ?? 0,
    expectedVersion: request.expectedVersion ?? 0, decision: request.decision };
  if (!isStrictLyricsSourceReviewMutationResponse(response, expected)) throw invalidLyricsSourceReviewMutationResponse();
  return response;
}
export const selectLyricsSourceCandidate = async (request: { reviewId: number; candidateIdentity?: LyricsSourceCandidate; exclude: boolean; expectedVersion: number; idempotencyKey: string; note: "" }): Promise<LyricsSourceReviewMutationResult> => {
  const response = await apiFetch<unknown>("/admin/lyrics-source-reviews/candidate-selection", { method: "PUT", body: JSON.stringify(request) });
  if (!isStrictLyricsSourceReviewMutationResponse(response, { kind: "candidate", reviewId: request.reviewId,
    expectedVersion: request.expectedVersion, exclude: request.exclude })) throw invalidLyricsSourceReviewMutationResponse();
  return response;
};

// Read-only upstream status available to any authenticated user (user settings).
export const getUpstreamStatusPublic = () => apiFetch<UpstreamStatus>("/upstream/status");

export const getBackupStatus = () => apiFetch<BackupStatus>("/backup/status");
export const pushBackup = () => apiFetch<{ status: string; results: Record<string, string> }>("/editor/v1/backup/push", { method: "POST" }, true);
export const restoreBackup = (target: "s3" | "git", confirmation: string) =>
  apiFetch<Record<string, unknown>>("/backup/restore", { method: "POST", body: JSON.stringify({ target, confirmation }) });

// ---- Lyrics ----

export const getCatalogMusic = async (query = "", newlyWritten = true): Promise<{ items: CatalogMusicItem[] }> => ({
  items: await collectCatalogPages(async (cursor: string) => {
    const p = new URLSearchParams({ newlyWritten: String(newlyWritten), limit: "100" });
    if (query.trim()) p.set("q", query.trim());
    if (cursor) p.set("cursor", cursor);
    return apiFetch<{ items: CatalogMusicItem[]; nextCursor?: string }>(`/catalog/music?${p}`);
  }) as CatalogMusicItem[],
});
export const getCatalogPerformers = () =>
  apiFetch<{ items: CatalogPerformerItem[] }>("/catalog/characters");
export const getLyrics = async (musicId: number, editionKey?: string): Promise<SongLyricsDocument> => {
  const params = new URLSearchParams({ musicId: String(musicId) });
  if (editionKey) params.set("translationEditionKey", editionKey);
  const response = await apiFetch<unknown>(`/lyrics/detail?${params}`);
  const validated = validateSongLyricsMutationResponse(response, { operation: "conflict", musicId, revision: 0 });
  if (!validated.ok) throw new APIError(502, { error: "invalid_lyrics_response", details: validated.details });
  const document = validated.value as SongLyricsDocument;
  if (editionKey && "translationEditionKey" in document && document.translationEditionKey !== editionKey) {
    throw new APIError(502, { error: "invalid_lyrics_response", details: ["response translationEditionKey does not match the request"] });
  }
  return document;
};

function isStrictLyricsCollaborationTicket(value: unknown): value is LyricsCollaborationTicket {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const ticket = value as Record<string, unknown>;
  const keys = Object.keys(ticket);
  return keys.length === 3 && keys.every((key) => ["ticket", "room", "expiresAt"].includes(key)) &&
    typeof ticket.ticket === "string" && ticket.ticket.length > 0 &&
    typeof ticket.room === "string" && /^[A-Za-z0-9._~-]+$/.test(ticket.room) &&
    typeof ticket.expiresAt === "string" && Number.isFinite(Date.parse(ticket.expiresAt));
}

export const issueLyricsCollabTicket = async (musicId: number, signal?: AbortSignal): Promise<LyricsCollaborationTicket> => {
  const response = await apiFetch<unknown>(`/editor/v1/lyrics/${musicId}/collab-ticket`, {
    method: "POST",
    body: JSON.stringify({}),
    signal,
  }, true);
  if (!isStrictLyricsCollaborationTicket(response)) {
    throw new APIError(502, { error: "invalid_lyrics_collaboration_ticket" });
  }
  return response;
};

type LyricsMutationExpectation =
  | { operation: "save"; musicId: number; revision: number; document: SongLyricsDocument; editionKey?: string; conflictEditionKey?: string }
  | { operation: "edition"; musicId: number; revision: number; editionKey: string; conflictEditionKey?: string }
  | { operation: "publish" | "unpublish"; musicId: number; revision: number; conflictEditionKey?: never };

async function lyricsMutation(path: string, options: RequestInit, expectation: LyricsMutationExpectation): Promise<SongLyricsDocument> {
  let response: unknown;
  try {
    response = await apiFetch<unknown>(path, options, true);
  } catch (reason) {
    if (reason instanceof APIError && reason.code === "invalid_json_response") {
      throw new APIError(502, { error: "invalid_lyrics_response", details: reason.details });
    }
    if (reason instanceof APIError && reason.status === 409 && reason.current) {
      const conflict = validateSongLyricsMutationResponse(reason.current, {
        operation: "conflict",
        musicId: expectation.musicId,
        revision: expectation.revision,
        ...("conflictEditionKey" in expectation && expectation.conflictEditionKey ? { editionKey: expectation.conflictEditionKey } : {}),
      });
      if (!conflict.ok) throw new APIError(502, { error: "invalid_lyrics_response", details: conflict.details });
      reason.current = conflict.value as SongLyricsDocument;
    }
    throw reason;
  }
  const validated = validateSongLyricsMutationResponse(response, expectation);
  if (!validated.ok) throw new APIError(502, { error: "invalid_lyrics_response", details: validated.details });
  return validated.value as SongLyricsDocument;
}

export const saveLyrics = (lyrics: SongLyricsDocument, sourceImportToken?: string) =>
  lyricsMutation("/editor/v1/lyrics/save", {
    method: "PUT",
    body: JSON.stringify(buildLyricsSavePayload(lyrics, sourceImportToken, getClientID())),
  }, {
    operation: "save", musicId: lyrics.musicId, revision: lyrics.revision, document: lyrics,
    ...("translationEditionKey" in lyrics ? {
      editionKey: lyrics.translationEditionKey,
      conflictEditionKey: lyrics.translationEditionKey,
    } : {}),
  });

export const mutateLyricsTranslationEdition = (mutation: LyricsTranslationEditionMutation) =>
  lyricsMutation("/editor/v1/lyrics/translation-editions", {
    method: "POST",
    body: JSON.stringify({ ...mutation, clientId: getClientID() }),
  }, { operation: "edition", musicId: mutation.musicId, revision: mutation.revision, editionKey: mutation.editionKey });

export const checkpointLyrics = async (musicId: number): Promise<SongLyricsDocument> => {
  let response: unknown;
  try {
    response = await apiFetch<unknown>(`/editor/v1/lyrics/${musicId}/checkpoint`, {
      method: "POST",
      body: JSON.stringify({ clientId: getClientID() }),
    }, true);
  } catch (reason) {
    if (reason instanceof APIError && reason.code === "invalid_json_response") {
      throw new APIError(502, { error: "invalid_lyrics_response", details: reason.details });
    }
    throw reason;
  }
  const validated = validateSongLyricsCheckpointResponse(response, musicId);
  if (!validated.ok) throw new APIError(502, { error: "invalid_lyrics_response", details: validated.details });
  return validated.value as SongLyricsDocument;
};
export const publishLyrics = (musicId: number, revision: number) =>
  lyricsMutation("/editor/v1/lyrics/publish", { method: "POST", body: JSON.stringify({ musicId, revision, clientId: getClientID() }) },
    { operation: "publish", musicId, revision });
export const unpublishLyrics = (musicId: number, revision: number) =>
  lyricsMutation("/editor/v1/lyrics/unpublish", { method: "POST", body: JSON.stringify({ musicId, revision, clientId: getClientID() }) },
    { operation: "unpublish", musicId, revision });
export const getLyricsDocumentExport = (musicId: number) =>
  apiFetch<LyricsDocumentExport>(`/editor/v1/lyrics/document?musicId=${musicId}`);

function isLyricsDocumentWarning(value: unknown): value is LyricsDocumentWarning {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const warning = value as Record<string, unknown>;
  return typeof warning.code === "string" && typeof warning.message === "string" &&
    ["rendition", "side"].every((key) => warning[key] === undefined || typeof warning[key] === "string") &&
    (warning.line === undefined || Number.isSafeInteger(warning.line));
}

/** Publishes the song exactly as the site serves it as an editable document; admin only. */
export const takeOverLyricsDocument = async (musicId: number, expectedRevision: number): Promise<LyricsDocumentTakeoverResult> => {
  const response = await apiFetch<unknown>("/editor/v1/lyrics/document/takeover", {
    method: "POST",
    body: JSON.stringify({ musicId, expectedRevision }),
  }, true);
  const result = response && typeof response === "object" && !Array.isArray(response)
    ? response as Record<string, unknown>
    : null;
  const warnings = result?.warnings ?? [];
  if (!result || result.musicId !== musicId || result.dryRun !== false ||
      !Number.isSafeInteger(result.revision) || Number(result.revision) <= 0 ||
      !Array.isArray(warnings) || !warnings.every(isLyricsDocumentWarning)) {
    throw new APIError(502, { error: "invalid_lyrics_response", details: ["转换响应没有确认这首歌已发布为可编辑文档"] });
  }
  return { ...result, warnings } as unknown as LyricsDocumentTakeoverResult;
};
export const searchLyricsSource = (musicId: number) =>
  apiFetch<{ items: LyricsSourceCandidate[] }>(`/lyrics/source/search?musicId=${musicId}`);
export const previewLyricsSource = (musicId: number, pageId: number, revisionId: number) =>
  apiFetch<LyricsSourcePreview>("/lyrics/source/preview", {
    method: "POST", body: JSON.stringify({ musicId, pageId, revisionId }),
  });
