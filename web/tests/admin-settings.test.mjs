import assert from "node:assert/strict";
import test from "node:test";

import { read } from "./source-surfaces.mjs";

const block = (source, start, end) => {
  const from = source.indexOf(start);
  assert.ok(from >= 0, `missing ${start}`);
  const to = source.indexOf(end, from);
  assert.ok(to > from, `missing end of ${start}`);
  return source.slice(from, to);
};

test("the admin upstream settings list every env-seeded upstream key the server accepts", async () => {
  const [modal, configSource, seed] = await Promise.all([
    read("src/components/AdminModal.tsx"), read("../server/internal/config/config.go"), read("../server/seed.go"),
  ]);
  const settingNames = new Map([...configSource.matchAll(/^\s*(Key\w+)\s*=\s*"([^"]+)"/gm)].map((m) => [m[1], m[2]]));
  const accepted = new Set([...block(configSource, "var settingKeys = map[string]bool{", "\n}").matchAll(/(Key\w+): true/g)]
    .map((m) => settingNames.get(m[1])));
  const seeded = [...seed.matchAll(/config\.(KeyUpstream\w+):\s*(?:os\.Getenv|envOr)\("UPSTREAM_/g)]
    .map((m) => settingNames.get(m[1]));
  const listed = [...block(modal, "const UPSTREAM_KEYS = [", "] as const;").matchAll(/\["([^"]+)", "([^"]+)"\]/g)];
  const listedKeys = listed.map((m) => m[1]);
  for (const [, key, label] of listed) assert.ok(label.trim(), `label of ${key}`);

  assert.ok(seeded.length >= 17, `seeded upstream keys: ${seeded.length}`);
  for (const key of seeded) assert.ok(listedKeys.includes(key), `admin modal is missing ${key}`);
  for (const key of listedKeys) assert.ok(accepted.has(key), `PUT /api/admin/settings rejects ${key}`);
  assert.equal(new Set(listedKeys).size, listedKeys.length);
});
