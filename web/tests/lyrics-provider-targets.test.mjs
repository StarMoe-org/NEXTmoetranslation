import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const read = (path) => readFile(new URL(`../${path}`, import.meta.url), "utf8");

test("the console reaches the Sekaipedia provider-target routes the server registers", async () => {
  const [api, routes] = await Promise.all([
    read("src/lib/api.ts"), read("../server/internal/api/routes.go"),
  ]);
  assert.match(api, /apiFetch<\{ items: LyricsProviderTarget\[\] \}>\("\/admin\/lyrics-providers\/sekaipedia\/targets"\)/);
  assert.match(api, /apiFetch<LyricsProviderTarget>\(`\/admin\/lyrics-providers\/sekaipedia\/targets\/\$\{musicId\}`, \{\s*method: "PUT"/);
  assert.match(api, /method: "DELETE"/);
  assert.match(api, /musicId: number;[\s\S]*pageTitle: string;[\s\S]*resolvedPageTitle: string;[\s\S]*aliases: LyricsProviderContributorAlias\[\];/);
  assert.match(routes, /mux\.HandleFunc\("\/api\/admin\/lyrics-providers\/sekaipedia\/targets", s\.auth\.RequireAdmin\(getOnly\(s\.handleLyricsProviderTargets\)\)\)/);
  assert.match(routes, /mux\.HandleFunc\("\/api\/admin\/lyrics-providers\/sekaipedia\/targets\/\{musicId\}", s\.auth\.RequireAdmin\(s\.handleLyricsProviderTarget\)\)/);
});

test("the admin dialog shows the provider-target section between users and LLM settings", async () => {
  const modal = await read("src/components/AdminModal.tsx");
  const users = modal.indexOf("<UsersCard show={show} />");
  const targets = modal.indexOf("<LyricsProviderTargets show={show} />");
  const llm = modal.indexOf('<SettingsCard title="LLM 翻译"');
  assert.ok(users >= 0 && targets > users && llm > targets, `section order users=${users} targets=${targets} llm=${llm}`);
  assert.match(modal, /import \{ LyricsProviderTargets \} from "@\/components\/admin\/LyricsProviderTargets";/);
});

test("the provider-target card lists the stored map, edits in place, and confirms before deleting", async () => {
  const card = await read("src/components/admin/LyricsProviderTargets.tsx");
  assert.match(card, /<h3>歌词来源映射（Sekaipedia）<\/h3>/);
  assert.match(card, /getLyricsProviderTargets\(\)\.then\(\(r\) => setTargets\(r\.items\)\)/);
  assert.match(card, /putLyricsProviderTarget\(id, \{/);
  assert.match(card, /if \(!confirm\(`删除乐曲 \$\{target\.musicId\} 的 Sekaipedia 映射「\$\{target\.pageTitle\}」？`\)\) return;\s*\n\s*setBusy\(true\);\s*\n\s*try \{ await deleteLyricsProviderTarget\(target\.musicId\)/);
  assert.match(card, /<table className="data-table">/);
  assert.match(card, /className="btn btn-primary"/);
  assert.match(card, /show\(errorMessage\(e, "保存失败"\), "err"\)/);
  // A 422 invalid_provider_target carries the validator's reason in details.
  assert.match(card, /if \(e instanceof APIError && e\.details\.length\) return e\.details\.join\("；"\);/);
  assert.doesNotMatch(card, /import .*\.css/);
});
