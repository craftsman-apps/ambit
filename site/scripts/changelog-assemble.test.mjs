import {test} from 'node:test';
import assert from 'node:assert/strict';
import {existsSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {pathToFileURL} from 'node:url';
import {assemble} from './changelog-assemble.mjs';
import {releasePages} from './releases-pages.mjs';

const SAMPLE = '# Changelog\n\nIntro.\n\n## Unreleased\n\nEach pull request adds a file.\n\n## 0.2.3 (2026-10-18)\n\n- Old.\n';

function setup(files = {}) {
  const root = mkdtempSync(join(tmpdir(), 'assemble-'));
  const dir = join(root, 'unreleased');
  mkdirSync(dir);
  writeFileSync(join(dir, 'README.md'), 'readme\n');
  for (const [n, t] of Object.entries(files)) writeFileSync(join(dir, n), t);
  const file = join(root, 'CHANGELOG.md');
  writeFileSync(file, SAMPLE);
  return {file, dir: pathToFileURL(dir + '/'), read: () => readFileSync(file, 'utf8'), path: dir};
}

test('an empty directory fails and leaves CHANGELOG.md as it was', () => {
  const s = setup();
  assert.throws(() => assemble('0.2.4', '2026-10-20', s.file, s.dir), /no fragment files/);
  assert.equal(s.read(), SAMPLE);
});

test('fragments go into the new version in file name order, below Unreleased; the files are deleted, README stays', () => {
  const s = setup({'b-second.md': '- Second.\n', 'a-first.md': '- First.\n  More of first.\n', 'notes.txt': 'ignored'});
  assert.deepEqual(assemble('0.2.4', '2026-10-20', s.file, s.dir), ['a-first.md', 'b-second.md']);
  const md = s.read();
  assert.ok(md.includes('## Unreleased\n\nEach pull request adds a file.\n\n## 0.2.4 (2026-10-20)\n\n- First.\n  More of first.\n- Second.\n\n## 0.2.3 (2026-10-18)'));
  assert.ok(!existsSync(join(s.path, 'a-first.md')) && !existsSync(join(s.path, 'b-second.md')));
  assert.ok(existsSync(join(s.path, 'README.md')) && existsSync(join(s.path, 'notes.txt')));
  const pages = releasePages(md);
  assert.deepEqual(pages.map((p) => p.version), ['0.2.4', '0.2.3']);
  assert.ok(pages[0].content.endsWith('\n\n- First.\n  More of first.\n- Second.\n'));
});

test('a version that already exists is refused and nothing is deleted', () => {
  const s = setup({'a.md': '- A.\n'});
  assert.throws(() => assemble('0.2.3', '2026-10-20', s.file, s.dir), /already has version 0\.2\.3/);
  assert.ok(existsSync(join(s.path, 'a.md')));
  assert.equal(s.read(), SAMPLE);
});

test('an empty fragment is refused and nothing is deleted', () => {
  const s = setup({'a.md': '- A.\n', 'b.md': '\n  \n'});
  assert.throws(() => assemble('0.2.4', '2026-10-20', s.file, s.dir), /fragment b\.md is empty/);
  assert.ok(existsSync(join(s.path, 'a.md')));
  assert.equal(s.read(), SAMPLE);
});

test('a bad version or date is refused', () => {
  const s = setup({'a.md': '- A.\n'});
  assert.throws(() => assemble('v0.2.4', '2026-10-20', s.file, s.dir), /version/);
  assert.throws(() => assemble('0.2.4', '20 Oct', s.file, s.dir), /date/);
});

test('Unreleased as the last section still works', () => {
  const s = setup({'a.md': '- A.\n'});
  writeFileSync(s.file, '# Changelog\n\n## Unreleased\n\nText.\n');
  assemble('0.2.4', '2026-10-20', s.file, s.dir);
  assert.deepEqual(releasePages(s.read()).map((p) => p.version), ['0.2.4']);
});
