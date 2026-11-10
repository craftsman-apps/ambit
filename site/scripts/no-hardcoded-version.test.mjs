import {test} from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync, readdirSync} from 'node:fs';

// The homepage version comes from CHANGELOG.md (docusaurus.config.ts customFields); site/src must not write one by hand.
const VERSION = /\bv\d+\.\d+\.\d+/g;
const src = new URL('../src/', import.meta.url);

export function hardCodedVersions(files) {
  const found = [];
  for (const [file, text] of Object.entries(files)) {
    text.split('\n').forEach((line, i) => {
      for (const m of line.matchAll(VERSION)) found.push(`${file}:${i + 1}: ${m[0]}`);
    });
  }
  return found;
}

test('no file under site/src writes a version like vX.Y.Z', () => {
  const files = Object.fromEntries(readdirSync(src, {recursive: true, withFileTypes: true})
    .filter(e => e.isFile())
    .map(e => {
      const path = `${e.parentPath}/${e.name}`;
      return [path.slice(src.pathname.length), readFileSync(path, 'utf8')];
    }));
  assert.deepEqual(hardCodedVersions(files), []);
});
test('a planted version is reported with file and line', () => {
  assert.deepEqual(hardCodedVersions({'pages/index.tsx': 'a\n<p>Alpha · v0.3.0 · Apache-2.0</p>'}), ['pages/index.tsx:2: v0.3.0']);
  assert.deepEqual(hardCodedVersions({'pages/index.tsx': '<p>Alpha · v{version} · Apache-2.0</p>'}), []);
});
