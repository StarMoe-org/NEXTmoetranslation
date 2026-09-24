"use client";

import React, { useCallback, useEffect, useState } from "react";
import { useToast } from "@/app/providers";
import { Modal } from "@/components/Modal";
import { LyricsProviderTargets } from "@/components/admin/LyricsProviderTargets";
import {
  APIError, BackupStatus, ProjectionStatus, UpstreamStatus, User,
  checkUpstream, clearSession, createUser, deleteUser, getBackupStatus, getProjectionStatus, getSettings,
  getUpstreamStatus, getUsername, listUsers, publishProjection, pushBackup, restoreBackup,
  updateSettings, updateUser,
} from "@/lib/api";

type ShowFn = (msg: string, type?: "ok" | "err") => void;

// Settings keys grouped for the form (mirror the Go config constants).
const LLM_KEYS = [
  ["llm.type", "默认提供方 (gemini/openai)"],
  ["llm.gemini.key", "Gemini API Key"],
  ["llm.gemini.model", "Gemini 模型"],
  ["llm.openai.key", "OpenAI API Key"],
  ["llm.openai.base_url", "OpenAI Base URL"],
  ["llm.openai.model", "OpenAI 模型"],
  ["llm.request_timeout_ms", "单次请求超时 (ms)"],
  ["llm.max_retries", "失败重试次数 (0-5)"],
  ["translate.batch_size", "批大小"],
  ["translate.rate_delay_ms", "速率延迟 (ms)"],
] as const;

const UPSTREAM_KEYS = [
  ["upstream.repo", "上游仓库 (owner/repo)"],
  ["upstream.branch", "上游分支"],
  ["upstream.version_url", "版本检测 URL"],
  ["upstream.version_fallback_url", "版本检测备用 URL"],
  ["upstream.jp_masterdata_url", "JP Masterdata URL"],
  ["upstream.jp_masterdata_fallback_url", "JP Masterdata 备用 URL"],
  ["upstream.cn_masterdata_url", "CN Masterdata URL"],
  ["upstream.cn_masterdata_fallback_url", "CN Masterdata 备用 URL"],
  ["upstream.en_masterdata_url", "EN Masterdata URL"],
  ["upstream.en_masterdata_fallback_url", "EN Masterdata 备用 URL"],
  ["upstream.jp_assets_url", "JP 剧情资源 URL"],
  ["upstream.jp_assets_fallback_url", "JP 剧情资源备用 URL"],
  ["upstream.cn_assets_url", "CN 剧情资源 URL"],
  ["upstream.cn_assets_fallback_url", "CN 剧情资源备用 URL"],
  ["upstream.jp_scripts_url", "JP 卡牌/区域对话剧本 URL"],
  ["upstream.jp_scripts_fallback_url", "JP 卡牌剧本备用 URL"],
  ["upstream.cn_scripts_url", "CN 卡牌/区域对话剧本 URL"],
  ["upstream.en_scripts_url", "EN 卡牌/区域对话剧本 URL"],
  ["upstream.fetch_concurrency", "并发下载数 (1-12)"],
  ["scheduler.enabled", "启用自动检测 (true/false)"],
  ["side_story_backfill.enabled", "卡牌剧情/区域对话后台回填 (true/false)"],
] as const;

const BACKUP_KEYS = [
  ["backup.git.enabled", "启用 GitHub 备份 (true/false)"],
  ["backup.git.repo_url", "GitHub 仓库 URL (可含 token)"],
  ["backup.git.branch", "GitHub 备份分支"],
  ["backup.s3.enabled", "启用 S3 备份 (true/false)"],
  ["backup.s3.endpoint", "S3 Endpoint"],
  ["backup.s3.region", "S3 Region"],
  ["backup.s3.bucket", "S3 Bucket"],
  ["backup.s3.prefix", "S3 前缀"],
  ["backup.s3.access_key", "S3 Access Key"],
  ["backup.s3.secret_key", "S3 Secret Key"],
  ["backup.daily_hour", "每日备份时刻 (UTC 0-23)"],
] as const;

// Per-key help text rendered below the relevant input.
const SETTING_HINTS: Record<string, React.ReactNode> = {
  "llm.request_timeout_ms": <>默认 45000。每次请求到期后会自动取消，已完成批次仍会保留。</>,
  "llm.max_retries": <>默认 2，即首次请求失败后最多再尝试 2 次。</>,
  "upstream.version_fallback_url": <>可填写多个 URL，用逗号分隔；系统还会自动追加 GitHub Raw、Fastly、Gcore 和 jsDelivr 救援源。</>,
  "upstream.jp_assets_url": <>留空使用 <code>https://assets.unipjsk.com/ondemand</code>。旧 snowyassets 源持续返回 HTTP 525，不再作为默认源。</>,
  "upstream.jp_scripts_fallback_url": <>只用于卡牌剧情；区域对话剧本不在该源上。</>,
  "upstream.fetch_concurrency": <>留空默认 4；低内存实例建议 2-4。</>,
  "scheduler.enabled": <>旧版 CN 自动同步：检测到上游版本变化后同步 CN 内容，并对新活动剧情自动调用 AI。修改后重启服务生效。</>,
  "side_story_backfill.enabled": <>未设置时为 true（保存后只能填 true 或 false）。按间隔抓取卡牌剧情与区域对话剧本并导入官方 CN/EN 译文，不调用 AI，与“启用自动检测”无关；环境变量 SIDE_STORY_BACKFILL_ENABLED=false 时始终关闭。</>,
  "backup.git.repo_url": (
    <>
      私有仓库需要把访问令牌写进 URL，格式：
      <code>https://&lt;token&gt;@github.com/用户名/仓库名.git</code>
      。<br />
      令牌获取（GitHub）：头像 → Settings → Developer settings → Personal access
      tokens → <b>Fine-grained tokens</b> → Generate new token，选择目标仓库，
      Repository permissions 里把 <b>Contents</b> 设为 <b>Read and write</b>，生成后
      复制以 <code>github_pat_</code> 开头的字符串填入上面。<br />
      示例：<code>https://github_pat_xxx@github.com/yourname/moesekai-backup.git</code>
      （经典 token 以 <code>ghp_</code> 开头，用法相同）。公开仓库可不带 token。
    </>
  ),
};

export function AdminModal({ open, onClose, guardProducerMutation }: {
  open: boolean;
  onClose: () => void;
  guardProducerMutation: (label: string, action: () => Promise<void>) => void;
}) {
  const { show } = useToast();

  return (
    <Modal open={open} onClose={onClose} title="管理设置">
      <div className="modal-cards">
        <ProjectionCard show={show} />
        <UsersCard show={show} />
        <LyricsProviderTargets show={show} />
        <SettingsCard title="LLM 翻译" keys={LLM_KEYS} show={show} />
        <UpstreamCard show={show} guardProducerMutation={guardProducerMutation} />
        <SettingsCard title="上游更新检测" keys={UPSTREAM_KEYS} show={show} />
        <BackupCard show={show} guardProducerMutation={guardProducerMutation} />
        <SettingsCard title="备份配置" keys={BACKUP_KEYS} show={show} />
      </div>
    </Modal>
  );
}

// ---- Users ----

function UsersCard({ show }: { show: ShowFn }) {
  const [users, setUsers] = useState<User[]>([]);
  const [nu, setNu] = useState(""); const [np, setNp] = useState(""); const [nr, setNr] = useState<"admin" | "editor">("editor");

  const reload = useCallback(() => { listUsers().then(setUsers).catch((e) => show(e.message, "err")); }, [show]);
  useEffect(() => { reload(); }, [reload]);

  const add = async () => {
    try { await createUser(nu, np, nr); setNu(""); setNp(""); reload(); show("已创建用户", "ok"); }
    catch (e) { show(e instanceof Error ? e.message : "创建失败", "err"); }
  };
  const setRole = async (u: User, role: "admin" | "editor") => {
    try {
      await updateUser(u.username, { role });
      if (u.username === getUsername()) {
        await clearSession();
        show("当前账号角色已变化，请重新登录", "ok");
        return;
      }
      reload();
      show("已更新角色", "ok");
    } catch (e) { show(e instanceof Error ? e.message : "更新失败", "err"); }
  };
  const resetPw = async (u: User) => {
    const pw = prompt(`为 ${u.username} 设置新密码`);
    if (!pw) return;
    try { await updateUser(u.username, { password: pw }); show("已重置密码", "ok"); }
    catch (e) { show(e instanceof Error ? e.message : "重置失败", "err"); }
  };
  const remove = async (u: User) => {
    if (!confirm(`删除用户 ${u.username}？`)) return;
    try { await deleteUser(u.username); reload(); show("已删除", "ok"); }
    catch (e) { show(e instanceof Error ? e.message : "删除失败", "err"); }
  };

  return (
    <div className="card">
      <h3>用户管理</h3>
      <table className="data-table">
        <thead><tr><th>用户名</th><th>角色</th><th>操作</th></tr></thead>
        <tbody>
          {users.map((u) => (
            <tr key={u.id}>
              <td>{u.username}</td>
              <td>
                <select
                  aria-label={`${u.username} 的角色`}
                  value={u.role}
                  onChange={(e) => setRole(u, e.target.value as "admin" | "editor")}
                >
                  <option value="admin">管理员</option>
                  <option value="editor">校对员</option>
                </select>
              </td>
              <td style={{ display: "flex", gap: 6 }}>
                <button className="btn btn-ghost btn-sm" onClick={() => resetPw(u)}>重置密码</button>
                <button className="btn btn-ghost btn-sm" onClick={() => remove(u)}>删除</button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <div style={{ display: "flex", gap: 8, marginTop: 14, alignItems: "flex-end", flexWrap: "wrap" }}>
        <div className="form-row" style={{ margin: 0 }}><label htmlFor="new-username">用户名</label><input id="new-username" value={nu} onChange={(e) => setNu(e.target.value)} /></div>
        <div className="form-row" style={{ margin: 0 }}><label htmlFor="new-password">密码</label><input id="new-password" type="password" minLength={12} value={np} onChange={(e) => setNp(e.target.value)} /></div>
        <div className="form-row" style={{ margin: 0 }}><label htmlFor="new-role">角色</label>
          <select id="new-role" value={nr} onChange={(e) => setNr(e.target.value as "admin" | "editor")}>
            <option value="editor">校对员</option><option value="admin">管理员</option>
          </select>
        </div>
        <button className="btn btn-primary" onClick={add} disabled={!nu || !np}>添加用户</button>
      </div>
    </div>
  );
}

// ---- Generic settings card ----

function SettingsCard({ title, keys, show }: { title: string; keys: readonly (readonly [string, string])[]; show: ShowFn }) {
  const [values, setValues] = useState<Record<string, string>>({});
  const [hasMasterKey, setHasMasterKey] = useState(true);

  const reload = useCallback(() => {
    getSettings().then((r) => { setValues(r.settings); setHasMasterKey(r.hasMasterKey); }).catch((e) => show(e.message, "err"));
  }, [show]);
  useEffect(() => { reload(); }, [reload]);

  const saveAll = async () => {
    const patch: Record<string, string> = {};
    for (const [k] of keys) if (values[k] !== undefined) patch[k] = values[k];
    try { await updateSettings(patch); show("已保存", "ok"); reload(); }
    catch (e) { show(e instanceof Error ? e.message : "保存失败", "err"); }
  };

  return (
    <div className="card">
      <h3>{title}</h3>
      {!hasMasterKey && <p style={{ color: "var(--warn)", fontSize: 12, marginBottom: 10 }}>未配置 MOESEKAI_MASTER_KEY，密钥项无法保存</p>}
      {keys.map(([k, label]) => {
        const inputID = `setting-${k.replaceAll(".", "-")}`;
        return (
          <div className="form-row" key={k}>
            <label htmlFor={inputID}>{label}</label>
            <input
              id={inputID}
              type={k.includes("key") || k.includes("secret") ? "password" : "text"}
              value={values[k] ?? ""}
              onChange={(e) => setValues((p) => ({ ...p, [k]: e.target.value }))}
              placeholder={values[k] === "********" ? "（已设置，留空不变）" : ""}
            />
            {SETTING_HINTS[k] && <p className="form-hint">{SETTING_HINTS[k]}</p>}
          </div>
        );
      })}
      <button className="btn btn-primary" onClick={saveAll}>保存</button>
    </div>
  );
}

// ---- Upstream ----

function UpstreamCard({ show, guardProducerMutation }: { show: ShowFn; guardProducerMutation: (label: string, action: () => Promise<void>) => void }) {
  const [status, setStatus] = useState<UpstreamStatus | null>(null);
  const reload = useCallback(() => { getUpstreamStatus().then(setStatus).catch(() => {}); }, []);
  useEffect(() => { reload(); }, [reload]);

  const check = async (force: boolean) => {
    try { const s = await checkUpstream(force); setStatus(s); show(force ? "已强制同步" : "已检查", "ok"); }
    catch (e) { show(e instanceof Error ? e.message : "检查失败", "err"); }
  };

  return (
    <div className="card">
      <h3>上游更新状态</h3>
      {status && (
        <table className="data-table" style={{ marginBottom: 12 }}>
          <tbody>
            <tr><th>仓库</th><td>{status.repo}@{status.branch}</td></tr>
            <tr><th>检测源</th><td>{status.versionURL || "—"}</td></tr>
            {!!status.versionFallbackURLs?.length && <tr><th>备用检测源</th><td>{status.versionFallbackURLs.join(" · ")}</td></tr>}
            {status.lastSource && <tr><th>实际使用源</th><td>{status.lastSource}</td></tr>}
            <tr><th>当前 dataVersion</th><td>{status.lastDataVersion || "—"}</td></tr>
            <tr><th>上次检查</th><td>{status.lastCheck || "—"}</td></tr>
            <tr><th>上次成功</th><td>{status.lastSuccess || "—"}</td></tr>
            <tr><th>上次同步</th><td>{status.lastSync || "—"}</td></tr>
            {!!status.consecutiveFailures && <tr><th>连续失败</th><td>{status.consecutiveFailures}</td></tr>}
            {status.rateLimitedUntil && <tr><th>限流冷却</th><td>{status.rateLimitedUntil}</td></tr>}
            <tr><th>Git 镜像</th><td>{status.gitMirrorReady ? "就绪" : "未启用"}</td></tr>
            {status.lastError && <tr><th>错误</th><td style={{ color: "var(--err)" }}>{status.lastError}{status.lastErrorAt ? ` (${status.lastErrorAt})` : ""}</td></tr>}
          </tbody>
        </table>
      )}
      <div style={{ display: "flex", gap: 8 }}>
        <button className="btn btn-secondary" onClick={() => guardProducerMutation("检查上游更新", () => check(false))}>立即检查</button>
        <button className="btn btn-secondary" onClick={() => guardProducerMutation("强制同步上游", () => check(true))}>强制同步</button>
      </div>
    </div>
  );
}

// ---- Backup ----

function BackupCard({ show, guardProducerMutation }: { show: ShowFn; guardProducerMutation: (label: string, action: () => Promise<void>) => void }) {
  const [status, setStatus] = useState<BackupStatus | null>(null);
  const [busy, setBusy] = useState(false);
  const [restoreTarget, setRestoreTarget] = useState<"s3" | "git" | null>(null);
  const [restoreConfirmation, setRestoreConfirmation] = useState("");
  const reload = useCallback(() => { getBackupStatus().then(setStatus).catch(() => {}); }, []);
  useEffect(() => { reload(); }, [reload]);

  const doPush = async () => {
    setBusy(true);
    try { const r = await pushBackup(); show(`备份完成: ${JSON.stringify(r.results)}`, "ok"); reload(); }
    catch (e) {
      if (e instanceof APIError && e.results) show(`备份未全部完成: ${JSON.stringify(e.results)}`, "err");
      else show(e instanceof Error ? e.message : "备份失败", "err");
    }
    finally { setBusy(false); }
  };
  const performRestore = async (target: "s3" | "git", confirmation: string) => {
    setBusy(true);
    try {
      await restoreBackup(target, confirmation);
      reload();
      setRestoreTarget(null);
      setRestoreConfirmation("");
      show(`已从 ${target === "git" ? "GitHub" : "S3"} 恢复`, "ok");
    } catch (e) { show(e instanceof Error ? e.message : "恢复失败", "err"); }
    finally { setBusy(false); }
  };
  const requestRestore = (target: "s3" | "git") => {
    setRestoreTarget(target);
    setRestoreConfirmation("");
  };
  const confirmRestore = () => {
    if (!restoreTarget || restoreConfirmation !== `RESTORE:${restoreTarget}`) return;
    const target = restoreTarget;
    const confirmation = restoreConfirmation;
    guardProducerMutation(`从 ${target} 恢复`, () => performRestore(target, confirmation));
  };

  return (
    <div className="card">
      <h3>备份 / 恢复</h3>
      {status && (
        <table className="data-table" style={{ marginBottom: 12 }}>
          <tbody>
            <tr><th>S3 备份</th><td>{status.s3Enabled ? "已启用" : "未启用"} · 上次 {status.lastS3Backup || "—"}</td></tr>
            <tr><th>GitHub 备份</th><td>{status.gitEnabled ? "已启用" : "未启用"} · 上次 {status.lastGitBackup || "—"}</td></tr>
            <tr><th>每日时刻 (UTC)</th><td>{status.dailyHourUtc}:00</td></tr>
            <tr><th>上次恢复</th><td>{status.lastRestore || "—"}</td></tr>
            {status.lastError && <tr><th>错误</th><td style={{ color: "var(--err)" }}>{status.lastError}</td></tr>}
          </tbody>
        </table>
      )}
      <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
        <button className="btn btn-primary" onClick={doPush} disabled={busy}>立即备份</button>
        <button className="btn btn-secondary" onClick={() => requestRestore("git")} disabled={busy}>从 GitHub 恢复</button>
        <button className="btn btn-secondary" onClick={() => requestRestore("s3")} disabled={busy}>从 S3 恢复</button>
      </div>
      {restoreTarget && (
        <div className="restore-confirmation" role="alert">
          <strong>恢复会覆盖当前内容数据</strong>
          <p>先确认已完成最新备份且当前没有其他管理员操作。请输入 <code>RESTORE:{restoreTarget}</code> 才能继续从 {restoreTarget === "git" ? "GitHub" : "S3"} 恢复。</p>
          <label htmlFor="restore-confirmation-input">恢复确认文字</label>
          <input id="restore-confirmation-input" value={restoreConfirmation} onChange={(event) => setRestoreConfirmation(event.target.value)} autoComplete="off" disabled={busy} />
          <div className="dirty-guard-actions">
            <button className="btn btn-secondary" onClick={confirmRestore} disabled={busy || restoreConfirmation !== `RESTORE:${restoreTarget}`}>确认覆盖并恢复</button>
            <button className="btn btn-ghost" onClick={() => { setRestoreTarget(null); setRestoreConfirmation(""); }} disabled={busy}>取消</button>
          </div>
        </div>
      )}
    </div>
  );
}

// ---- Public Projections ----

function ProjectionCard({ show }: { show: ShowFn }) {
  const [status, setStatus] = useState<ProjectionStatus | null>(null);
  const [busy, setBusy] = useState(false);

  const reload = useCallback(() => {
    getProjectionStatus().then(setStatus).catch(() => setStatus(null));
  }, []);

  useEffect(() => { reload(); }, [reload]);

  const doPublish = async () => {
    setBusy(true);
    try {
      const next = await publishProjection();
      setStatus(next);
      show(`已触发全量公开文件发布 (generation ${next.generation})`, "ok");
    } catch (e) {
      show(e instanceof Error ? e.message : "发布失败", "err");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="card">
      <h3>公开文件发布 (Public Projections)</h3>
      <p style={{ fontSize: 13, color: "var(--text-secondary)", marginBottom: 12 }}>
        单条编辑会自动毫秒级增量更新；全量文件构建每 5 分钟自动防抖合并。您也可以随时在此处手动触发全量发布。
      </p>
      {status && (
        <table className="data-table" style={{ marginBottom: 12 }}>
          <tbody>
            <tr><th>当前版本 (Generation)</th><td>{status.generation}</td></tr>
            <tr><th>构建状态</th><td>{status.pending ? "正在构建/等待中…" : "已就绪"}</td></tr>
            <tr><th>上次成功时间</th><td>{status.lastSuccessAt || "—"}</td></tr>
            {status.lastError && <tr><th>错误</th><td style={{ color: "var(--err)" }}>{status.lastError}</td></tr>}
          </tbody>
        </table>
      )}
      <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
        <button className="btn btn-primary" onClick={doPublish} disabled={busy}>
          {busy ? "发布中…" : "立即全量发布"}
        </button>
        <button className="btn btn-secondary" onClick={reload} disabled={busy}>
          刷新状态
        </button>
      </div>
    </div>
  );
}
