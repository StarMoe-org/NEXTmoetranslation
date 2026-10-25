// Metadata travels inside a TXT template. Editors change only text below each
// translation marker. Text never becomes executable content or line identity.
export function mysekaiTemplate(detail) {
  const meta = { schema: "moly-translation-txt/1", region: detail.region, locale: detail.locale, scriptKey: detail.script.key, sourceRevision: detail.sourceRevision };
  const out = ["# MYSEKAI 翻译模板：只编辑 <<<译文 与 >>>结束 之间的内容；留空不会删除译文。", "#! " + JSON.stringify(meta), ""];
  for (const script of [detail.script, ...detail.related]) for (const line of script.lines) {
    out.push("@@ " + JSON.stringify({ id: line.id, sourceHash: line.sourceHash, revision: detail.drafts[line.id]?.revision || 0 }));
    out.push("# " + (line.speaker || (line.kind === "tweet" ? "气泡" : "显示文字")));
    for (const text of line.text.split("\n")) out.push("# " + text);
    out.push("<<<译文", detail.drafts[line.id]?.text || "", ">>>结束", "");
  }
  return out.join("\n");
}

export function parseMysekaiTemplate(text, detail) {
  if (typeof text !== "string" || text.length > 4 * 1024 * 1024) throw new Error("模板不能超过 4 MB");
  const rows = text.replace(/^\uFEFF/, "").replace(/\r\n?/g, "\n").split("\n");
  const header = rows.filter((row) => row.startsWith("#! "));
  if (header.length !== 1) throw new Error("文件缺少有效模板信息，请重新下载模板");
  const meta = JSON.parse(header[0].slice(3));
  if (meta.schema !== "moly-translation-txt/1" || meta.region !== detail.region || meta.locale !== detail.locale || meta.scriptKey !== detail.script.key) throw new Error("模板的内容或语言与当前对话不同");
  const source = new Map([detail.script, ...detail.related].flatMap((s) => s.lines).map((l) => [l.id, l]));
  const seen = new Set(); const preview = [];
  for (let i = 0; i < rows.length; i++) {
    if (!rows[i].startsWith("@@ ")) continue;
    const binding = JSON.parse(rows[i].slice(3));
    if (!binding.id || seen.has(binding.id) || !Number.isSafeInteger(binding.revision) || binding.revision < 0) throw new Error("模板有重复或损坏的文字标识");
    seen.add(binding.id);
    while (++i < rows.length && rows[i].startsWith("# ")) { /* original context */ }
    if (rows[i] !== "<<<译文") throw new Error("缺少译文开始标记");
    const body = [];
    while (++i < rows.length && rows[i] !== ">>>结束") body.push(rows[i]);
    if (i === rows.length) throw new Error("缺少译文结束标记");
    const text = body.join("\n");
    if (!text.trim()) continue;
    if (text.length > 32768 || /[\u0000-\u0009\u000b-\u001f\u007f\u00ad\u202d\u202e]/u.test(text)) throw new Error("译文包含不支持的控制字符或过长");
    const line = source.get(binding.id); const current = detail.drafts[binding.id];
    const conflict = !line ? "原文已归档" : line.sourceHash !== binding.sourceHash ? "原文有更新，请重新校对" : (current?.revision || 0) !== binding.revision ? "已有更新的译文，请核对后在网页编辑" : "";
    if (!conflict && current?.text === text && current?.sourceHash === binding.sourceHash) continue;
    preview.push({ id: binding.id, sourceHash: binding.sourceHash, expectedRevision: binding.revision, text, original: line?.text || "", current: current?.text || "", conflict });
  }
  if (!seen.size) throw new Error("模板中没有文字条目");
  return preview;
}
