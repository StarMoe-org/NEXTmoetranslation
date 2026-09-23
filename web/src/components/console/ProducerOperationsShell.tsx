import { useState, type RefObject } from "react";
import {
  Locale,
  promoteEventStoryHuman, publishProjection, reorderEventStory, retryEventStory, triggerAIStory,
} from "@/lib/api";
import { Modal } from "@/components/Modal";
import type { ContentConflict, ReconciliationReason, ShowToast } from "@/components/console/types";

export interface ProducerOperationsOptions {
  show: ShowToast;
  category: string;
  field: string;
  locale: Locale;
  writesLocked: boolean;
  writeFenceRef: RefObject<boolean>;
  contextGenerationRef: RefObject<number>;
  loadEntries: () => Promise<boolean>;
  reloadSidebar: () => Promise<boolean>;
}

export function useProducerOperations({
  show,
  category,
  field,
  locale,
  writesLocked,
  writeFenceRef,
  contextGenerationRef,
  loadEntries,
  reloadSidebar,
}: ProducerOperationsOptions) {
  const [busy, setBusy] = useState(false);
  const [publishing, setPublishing] = useState(false);

  const withBusy = async (fn: () => Promise<void>, producerOwnsFence = false) => {
    if (writeFenceRef.current && !producerOwnsFence) { show("实时连接校对完成前禁止写入", "err"); return; }
    if (busy) { show("已有任务在运行", "err"); return; }
    setBusy(true);
    try { await fn(); } finally { setBusy(false); }
  };

  const captureContext = () => ({
    generation: contextGenerationRef.current, category, field, locale,
  });
  const contextIsCurrent = (captured: ReturnType<typeof captureContext>) =>
    contextGenerationRef.current === captured.generation && category === captured.category &&
    field === captured.field && locale === captured.locale;

  // Per-story AI gap-fill: translate only the currently open event story.
  const doAIStory = () => withBusy(async () => {
    const captured = captureContext();
    const eventID = Number(captured.field);
    try {
      const r = await triggerAIStory(eventID, "openai") as { totalTranslated?: number; totalCandidates?: number };
      if (!contextIsCurrent(captured)) return;
      show(`AI 补充翻译完成: ${r.totalTranslated ?? 0}/${r.totalCandidates ?? 0}`, "ok");
      reloadSidebar(); loadEntries();
    } catch (e) {
      if (contextIsCurrent(captured)) show(e instanceof Error ? e.message : "AI 翻译失败", "err");
    }
  }, true);

  const promoteStory = () => withBusy(async () => {
    const captured = captureContext();
    try {
      await promoteEventStoryHuman(Number(captured.field));
      if (!contextIsCurrent(captured)) return;
      const [entriesLoaded, sidebarLoaded] = await Promise.all([loadEntries(), reloadSidebar()]);
      if (!contextIsCurrent(captured)) return;
      if (!entriesLoaded || !sidebarLoaded) {
        show("整篇标记人工已完成，但权威 revision 未完整重新载入", "err");
        return;
      }
      show("已整篇标记人工", "ok");
    } catch (reason) {
      if (contextIsCurrent(captured)) show(reason instanceof Error ? reason.message : "标记失败", "err");
    }
  }, true);

  const retryStory = () => withBusy(async () => {
    const captured = captureContext();
    try {
      await retryEventStory(Number(captured.field));
      if (!contextIsCurrent(captured)) return;
      loadEntries(); reloadSidebar(); show("已重新获取剧情", "ok");
    } catch (reason) {
      if (contextIsCurrent(captured)) show(reason instanceof Error ? reason.message : "重新获取失败", "err");
    }
  }, true);

  const reorderStory = () => withBusy(async () => {
    const captured = captureContext();
    try {
      await reorderEventStory(Number(captured.field));
      if (!contextIsCurrent(captured)) return;
      loadEntries(); show("已重排序对话", "ok");
    } catch (reason) {
      if (contextIsCurrent(captured)) show(reason instanceof Error ? reason.message : "重排序失败", "err");
    }
  }, true);

  const doPublish = async () => {
    if (publishing || writesLocked) return;
    setPublishing(true);
    try {
      const status = await publishProjection();
      show(`已触发全量公开文件发布 (generation ${status.generation})`, "ok");
    } catch (e) {
      show(e instanceof Error ? e.message : "发布失败", "err");
    } finally {
      setPublishing(false);
    }
  };

  return { busy, publishing, doPublish, doAIStory, promoteStory, retryStory, reorderStory };
}

export interface ProducerOperationsShellProps {
  pendingActionLabel: string;
  pendingActionBusy: boolean;
  saving: boolean;
  writesLocked: boolean;
  closePendingAction: () => void;
  continuePendingAction: (saveFirst: boolean) => Promise<void>;
  contentConflict: ContentConflict | null;
  exportConflictDraft: (conflict: ContentConflict) => void;
  resolveContentConflict: (conflict: ContentConflict) => void;
  reconcileContent: (reason: ReconciliationReason, draft?: string | null, detail?: string) => Promise<boolean>;
}

export function ProducerOperationsShell({
  pendingActionLabel,
  pendingActionBusy,
  saving,
  writesLocked,
  closePendingAction,
  continuePendingAction,
  contentConflict,
  exportConflictDraft,
  resolveContentConflict,
  reconcileContent,
}: ProducerOperationsShellProps) {
  return (
    <>
      <Modal open={pendingActionLabel !== ""} onClose={closePendingAction} title={pendingActionLabel || "处理未保存修改"} maxWidth={460} closeDisabled={pendingActionBusy || saving}>
        <div aria-busy={pendingActionBusy || saving}>
          {(pendingActionBusy || saving) && <p className="dirty-guard-copy" role="status" aria-live="polite">正在保存或放弃本地修改，请等待当前操作完成…</p>}
          <p className="dirty-guard-copy">当前内容有未保存修改。继续前请选择如何处理。</p>
          <div className="dirty-guard-actions">
            <button className="btn btn-primary" onClick={() => void continuePendingAction(true)} disabled={writesLocked || pendingActionBusy || saving}>保存并继续</button>
            <button className="btn btn-secondary" onClick={() => void continuePendingAction(false)} disabled={pendingActionBusy || saving}>放弃修改</button>
            <button className="btn btn-ghost" onClick={closePendingAction} disabled={pendingActionBusy || saving}>取消</button>
          </div>
        </div>
      </Modal>
      <Modal open={contentConflict != null} onClose={() => {}} title={contentConflict?.reason === "restore" ? "恢复数据与本地草稿冲突" : contentConflict?.reason === "remote" ? "协作者更新与本地草稿冲突" : "实时事件缺口校对"} maxWidth={560} dismissible={false}>
        {contentConflict && <>
          {contentConflict.detail && <p className="dirty-guard-copy">{contentConflict.detail}</p>}
          <p className="dirty-guard-copy">
            {contentConflict.reloadFailed
              ? "服务器权威数据尚未完整载入。写入保持锁定，请重试；本地草稿不会由此流程写回服务器。"
              : contentConflict.draft
                ? "服务器权威数据已重新载入。旧缓冲区仅可导出后手动合并，不能直接保存或覆盖恢复后的数据。"
                : "服务器权威数据已重新载入，可以继续。"}
          </p>
          <div className="dirty-guard-actions">
            {contentConflict.draft && <button className="btn btn-secondary" onClick={() => exportConflictDraft(contentConflict)}>仅导出旧草稿</button>}
            {contentConflict.reloadFailed ? (
              <button className="btn btn-primary" onClick={() => void reconcileContent(contentConflict.reason, contentConflict.draft, contentConflict.detail)}>重试权威载入</button>
            ) : <>
              {contentConflict.draft && <button className="btn btn-primary" onClick={() => { exportConflictDraft(contentConflict); resolveContentConflict(contentConflict); }}>导出后手动合并</button>}
              <button className="btn btn-ghost" onClick={() => resolveContentConflict(contentConflict)}>舍弃旧缓冲区并继续</button>
            </>}
          </div>
        </>}
      </Modal>
    </>
  );
}
