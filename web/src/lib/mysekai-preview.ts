import type { MysekaiDetail } from "./mysekai-api";

const hostOrigin = new URL(process.env.NEXT_PUBLIC_MOESEKAI_ORIGIN || "https://pjsk.moe").origin;
export function mysekaiPreviewContent(detail: MysekaiDetail): string | null {
  for (const content of detail.script.contents) {
    if (!["general", "fixture", "common"].includes(String(content.kind)) || !Number.isSafeInteger(content.id) || Number(content.id) <= 0) continue;
    return `talk:${content.kind}:${content.id}`;
  }
  return null;
}
export function openMysekaiPreview(detail: MysekaiDetail): () => void {
  const content = mysekaiPreviewContent(detail);
  if (!content) throw new Error("这类内容暂不支持舞台预览");
  const scripts = [detail.script, ...detail.related];
  const source = JSON.stringify({ schema: "moly-text-source/1", region: detail.region, revision: detail.sourceRevision, scripts });
  const entries = scripts.flatMap((s) => s.lines).flatMap((line) => {
    const d = detail.drafts[line.id];
    return d?.text.trim() && d.sourceHash === line.sourceHash ? [{ id: line.id, sourceHash: line.sourceHash, text: d.text, source: "human" }] : [];
  });
  const community = JSON.stringify({ schema: "moly-text-overlay/1", sourceRegion: detail.region, sourceRevision: detail.sourceRevision, locale: detail.locale, revision: detail.sourceRevision, entries });
  const encoder = new TextEncoder();
  if (encoder.encode(source).length > 2 * 1024 * 1024 || encoder.encode(community).length > 2 * 1024 * 1024) throw new Error("当前对话超过预览容量");
  const nonce = Array.from(crypto.getRandomValues(new Uint8Array(24)), (v) => v.toString(16).padStart(2, "0")).join("");
  const url = new URL("/mysekai/interactions/", hostOrigin);
  url.search = new URLSearchParams({ region: detail.region, content, translationPreview: nonce }).toString();
  const opened = window.open(url.href, "_blank");
  if (!opened) throw new Error("浏览器拦截了预览窗口，请允许弹出窗口后重试");
  const close = () => { window.removeEventListener("message", receive); clearTimeout(timer); };
  const receive = (event: MessageEvent) => {
    if (event.source !== opened || event.origin !== hostOrigin || event.data?.source !== "moly-preview" || event.data?.type !== "ready" || event.data?.nonce !== nonce) return;
    opened.postMessage({ source: "nexttrans-mysekai", type: "preview", nonce, region: detail.region, content, session: { source, official: "", community, locale: detail.locale } }, hostOrigin);
    close();
  };
  window.addEventListener("message", receive);
  const timer = setTimeout(close, 60_000);
  return close;
}
