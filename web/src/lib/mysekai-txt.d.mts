import type { MysekaiDetail, MysekaiEdit } from "./mysekai-api";
export interface MysekaiImportRow extends MysekaiEdit { original: string; current: string; conflict: string }
export function mysekaiTemplate(detail: MysekaiDetail): string;
export function parseMysekaiTemplate(text: string, detail: MysekaiDetail): MysekaiImportRow[];
