import { apiFetch } from "./api";

export interface MysekaiLine { id: string; kind: "text" | "speaker" | "tweet"; step: number | null; text: string; sourceHash: string; speaker?: string; speakerUnit?: number; cues?: string[] }
export interface MysekaiScript { key: string; scriptHash: string; contents: Record<string, unknown>[]; lines: MysekaiLine[] }
export interface MysekaiDraft { id: string; sourceHash: string; text: string; revision: number; updatedAt: number; updatedBy: string }
export interface MysekaiDetail { region: string; locale: string; sourceRevision: string; script: MysekaiScript; related: MysekaiScript[]; drafts: Record<string, MysekaiDraft> }
export interface MysekaiEdit { id: string; sourceHash: string; text: string; expectedRevision: number }
export interface MysekaiChannel { region: string; locale: string; revision: number; publication: string }
export interface MysekaiSummary { key: string; contents: Record<string, unknown>[]; lines: number; translated: number; review: number }
export interface MysekaiPage { items: MysekaiSummary[]; sourceRevision: string; draftRevision: string; channel: MysekaiChannel; nextCursor?: string }
export interface MysekaiHistory { revision: number; publication: string; action: string; updatedAt: number; updatedBy: string }
const params = (value: Record<string, string>) => new URLSearchParams(value).toString();
export const getMysekaiList = (region: string, locale: string, q = "", state = "", cursor = "") => apiFetch<MysekaiPage>(`/mysekai/scripts?${params({ region, locale, q, state, cursor })}`);
export const getMysekaiDetail = (region: string, locale: string, key: string) => apiFetch<MysekaiDetail>(`/mysekai/script?${params({ region, locale, key })}`);
export const getMysekaiHistory = (region: string, locale: string) => apiFetch<MysekaiHistory[]>(`/mysekai/publications?${params({ region, locale })}`);
export const saveMysekai = (detail: MysekaiDetail, edits: MysekaiEdit[]) => apiFetch<MysekaiDetail>("/editor/v1/mysekai/drafts", { method: "PUT", body: JSON.stringify({ region: detail.region, locale: detail.locale, scriptKey: detail.script.key, sourceRevision: detail.sourceRevision, edits }) }, true);
export const importMysekai = (catalog: unknown, expectedRevision: string) => apiFetch<{ sourceRevision: string }>("/editor/v1/mysekai/source", { method: "POST", body: JSON.stringify({ catalog, expectedRevision }) }, true);
export const publishMysekai = (action: "publish" | "unpublish" | "rollback", channel: MysekaiChannel, sourceRevision: string, draftRevision: string, publication = "") => apiFetch<{ channel: MysekaiChannel; projectionReady: boolean }>(`/editor/v1/mysekai/publication/${action}`, { method: "POST", body: JSON.stringify({ region: channel.region, locale: channel.locale, expectedRevision: channel.revision, sourceRevision, draftRevision, publication }) }, true);
