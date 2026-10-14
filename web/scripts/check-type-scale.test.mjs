// Fails if an arbitrary pixel font size (text-[13px]) is used. Sizes come from the tokens in src/index.css.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

const root = new URL("../src", import.meta.url).pathname;
const files = (dir) => readdirSync(dir, { withFileTypes: true }).flatMap((e) =>
  e.isDirectory() ? files(join(dir, e.name)) : /\.(tsx?|css)$/.test(e.name) ? [join(dir, e.name)] : []);

test("no arbitrary text-[Npx] classes in web/src", () => {
  const hits = files(root).flatMap((f) => readFileSync(f, "utf8").split("\n")
    .flatMap((line, i) => (/text-\[\d/.test(line) ? [`${f}:${i + 1}: ${line.trim()}`] : [])));
  assert.deepEqual(hits, [], `Use a type token (text-body, text-small, text-label, text-h2, text-h1):\n${hits.join("\n")}`);
});
