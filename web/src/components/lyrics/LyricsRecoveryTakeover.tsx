"use client";

import { Modal } from "@/components/Modal";
import {
  lyricsDocumentWarningChangesPublicPage, lyricsDocumentWarningText, lyricsRecoveryTakeoverBlocked, lyricsRecoveryTakeoverFailure,
  type SourceV3PublicState,
} from "@/components/lyrics/lyricsDocumentModel";
import type { LyricsRecoveryTakeover } from "@/components/lyrics/useLyricsRecoveryTakeover";
import type { LyricsDocumentWarning } from "@/lib/api";

export interface LyricsRecoveryLedgerNoticeProps {
  role: "admin" | "editor" | "";
  publicState: SourceV3PublicState | null;
  disabled: boolean;
  onConvert?: () => void;
}

/** Explains why a recovery-ledger song's source layer is read-only; admins get the conversion action. */
export function LyricsRecoveryLedgerNotice({ role, publicState, disabled, onConvert }: LyricsRecoveryLedgerNoticeProps) {
  const blocked = lyricsRecoveryTakeoverBlocked(publicState);
  return (
    <div className="lyrics-recovery-notice" role="note">
      <div>
        <strong>日文、注音、分段与演唱者暂时只读</strong>
        <span>这首歌的原文来自恢复记录，目前只能修改简中译文和署名。</span>
        <span>{role === "admin"
          ? "转为可编辑后，日文和注音要通过整曲文档接口修改（例如交给 agent 提交），本页仍只读；分段、演唱者和段落空行可以在本页调整。"
          : "如需修改原文，请联系管理员把这首歌转为可编辑。"}</span>
        {blocked && <span>{blocked.reason}{blocked.next}</span>}
      </div>
      {role === "admin" && <button type="button" className="btn btn-secondary btn-sm" onClick={onConvert} disabled={disabled || blocked != null}>转为可编辑</button>}
    </div>
  );
}

function WarningList({ warnings, className }: { warnings: LyricsDocumentWarning[]; className?: string }) {
  return <ul className={className}>{warnings.map((warning, index) => <li key={`${warning.code}:${index}`}>{lyricsDocumentWarningText(warning)}</li>)}</ul>;
}

export interface LyricsRecoveryTakeoverDialogProps {
  takeover: LyricsRecoveryTakeover;
  saveable: boolean;
  writeLocked: boolean;
}

export function LyricsRecoveryTakeoverDialog({ takeover, saveable, writeLocked }: LyricsRecoveryTakeoverDialogProps) {
  const { dialog, confirm, close, reload } = takeover;
  const submitting = dialog?.phase === "submitting";
  const failure = dialog?.phase === "failed" && dialog.error ? lyricsRecoveryTakeoverFailure(dialog.error) : null;
  // Export warnings describe a conversion; when the site serves nothing there is none to describe.
  const blocked = dialog?.notServed
    ? lyricsRecoveryTakeoverBlocked(dialog.warnings.some((warning) => warning.code === "withdrawn_republished") ? "withdrawn" : "not_served")
    : null;
  const warnings = dialog && !dialog.notServed ? dialog.warnings : [];
  const publicChanges = warnings.filter(lyricsDocumentWarningChangesPublicPage);
  const otherWarnings = warnings.filter((warning) => !lyricsDocumentWarningChangesPublicPage(warning));
  const canConfirm = dialog != null && dialog.expectedRevision != null && (!failure || failure.retry);
  return (
    <Modal open={dialog != null} onClose={close} title="转为可编辑文档" maxWidth={560} closeDisabled={submitting}>
      {dialog?.phase === "checking" && <p className="dirty-guard-copy" role="status" aria-live="polite">正在读取公开页面上的这首歌…</p>}
      {dialog?.phase === "done" && <>
        <p className="dirty-guard-copy">{publicChanges.length > 0 ? "已转为可编辑文档。公开页面上有以下变化：" : "已转为可编辑文档，公开页面保持不变。"}</p>
        {publicChanges.length > 0 && <WarningList warnings={publicChanges} className="lyrics-takeover-warnings" />}
        {otherWarnings.length > 0 && <>
          <p className="dirty-guard-copy">{publicChanges.length > 0 ? "另有以下差异，不改变公开页面的显示：" : "转换时有以下差异，请留意："}</p>
          <WarningList warnings={otherWarnings} className="lyrics-takeover-warnings" />
        </>}
        <div className="dirty-guard-actions"><button type="button" className="btn btn-primary" onClick={close}>知道了</button></div>
      </>}
      {dialog && (dialog.phase === "confirm" || dialog.phase === "submitting" || dialog.phase === "failed") && <div aria-busy={submitting}>
        {blocked ? <div className="lyrics-error" role="alert"><strong>{blocked.reason}</strong><span>{blocked.next}</span></div> : <>
          <p className="dirty-guard-copy">这首歌会按公开页面上的内容转为可编辑的歌词文档：</p>
          <ul className="lyrics-takeover-facts">
            <li>恢复记录会保留，不会删除；</li>
            <li>{publicChanges.length > 0 ? "公开页面上除下面列出的变化外，其余内容保持不变；" : "公开页面的内容保持不变；"}</li>
            <li>转换后日文和注音通过整曲文档接口修改（例如交给 agent 提交），本页仍只读；分段、演唱者和段落空行可以在本页调整。</li>
          </ul>
        </>}
        {saveable && <div className="lyrics-publication-check" role="alert"><strong>还有未保存的修改。</strong><span>转换会按公开页面重建这首歌并重置协作文档，这些修改会丢失；请先保存或放弃后再转换。</span></div>}
        {publicChanges.length > 0 && <div className="lyrics-publication-check" role="note">
          <strong>转换后公开页面上会有以下变化：</strong>
          <WarningList warnings={publicChanges} />
        </div>}
        {otherWarnings.length > 0 && <div className="lyrics-publication-check" role="note">
          <strong>转换时会有以下差异（不改变公开页面的显示）：</strong>
          <WarningList warnings={otherWarnings} />
        </div>}
        {submitting && <p className="dirty-guard-copy" role="status" aria-live="polite">正在转换，请等待服务器确认…</p>}
        {failure && <div className="lyrics-error" role="alert">
          <strong>{failure.summary}</strong>
          {failure.lines.map((line, index) => <span key={`${index}:${line}`}>{line}</span>)}
          {failure.reload && <button type="button" className="btn btn-secondary btn-sm" onClick={() => void reload()}>重新载入</button>}
        </div>}
        {canConfirm && !blocked && !submitting && writeLocked && <p className="dirty-guard-copy" role="status" aria-live="polite">编辑器正在同步内容版本或协作文档，暂时不能转换；同步完成后按钮会恢复可用。</p>}
        <div className="dirty-guard-actions">
          {canConfirm && <button type="button" className="btn btn-primary" onClick={() => void confirm()}
            disabled={submitting || writeLocked || saveable || dialog.notServed}>{dialog.phase === "failed" ? "重试转换" : "确认转换"}</button>}
          <button type="button" className="btn btn-ghost" onClick={close} disabled={submitting}>取消</button>
        </div>
      </div>}
    </Modal>
  );
}
