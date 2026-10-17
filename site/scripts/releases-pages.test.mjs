import {test} from 'node:test';
import assert from 'node:assert/strict';
import {existsSync, mkdtempSync, readdirSync, readFileSync, writeFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {pathToFileURL} from 'node:url';
import {releasePages, writeReleases} from './releases-pages.mjs';

const real = readFileSync(new URL('../../CHANGELOG.md', import.meta.url), 'utf8');

test('every dated version in CHANGELOG.md gives one file with the right slug, title and date', () => {
  const heads = [...real.matchAll(/^## (\S+) \((\d{4}-\d{2}-\d{2})\)$/gm)];
  const pages = releasePages(real);
  assert.ok(heads.length >= 3);
  assert.equal(pages.length, heads.length);
  heads.forEach(([, v, d], i) => {
    assert.equal(pages[i].file, `v${v}.md`);
    assert.ok(pages[i].content.startsWith(`---\ntitle: v${v}\nslug: v${v}\ndate: ${d}T`));
    assert.ok(!/authors|tags/.test(pages[i].content.split('---')[1]));
  });
});

test('Unreleased is not emitted and the next version is complete', () => {
  const md = '# Changelog\n\n## Unreleased\n\nAdd a line.\n\n- draft\n\n## 0.2.1 (2026-10-16)\n\n- First bullet.\n- Middle.\n\nUpgrade: run the command.\n\n- Last bullet.\n\n## 0.2.0 (2026-10-15)\n\n- Old.\n';
  const pages = releasePages(md);
  assert.deepEqual(pages.map((p) => p.version), ['0.2.1', '0.2.0']);
  assert.ok(!pages.some((p) => p.content.includes('Unreleased') || p.content.includes('draft')));
  assert.ok(pages[0].content.endsWith('\n\n- First bullet.\n- Middle.\n\nUpgrade: run the command.\n\n- Last bullet.\n'));
  assert.ok(!pages[0].content.includes('Old.'));
  const stamps = releasePages(real).map((p) => p.content.match(/^date: (.+)$/m)[1]);
  assert.deepEqual([...stamps].sort().reverse(), stamps, 'file order is date order, also for versions of one day');
  assert.equal(new Set(stamps).size, stamps.length);
});

test('a heading without a date throws and names the heading', () => {
  assert.throws(() => releasePages('## 0.3.0\n\n- x\n'), /"## 0\.3\.0"/);
});

test('docs links are rewritten with fragments kept; https links are unchanged', () => {
  const [p] = releasePages('## 1.0.0 (2026-01-02)\n\n[a](docs/upgrading.md) [b](docs/guides/x.md) [c](docs/index.md#known-limitations) [d](docs/upgrading.md#to-audit) [e](https://github.com/o/r/blob/main/docs/a.md)\n');
  assert.ok(p.content.endsWith('[a](/docs/upgrading) [b](/docs/guides/x) [c](/docs#known-limitations) [d](/docs/upgrading#to-audit) [e](https://github.com/o/r/blob/main/docs/a.md)\n'));
});

test('stale files in the output folder are removed', () => {
  const dir = mkdtempSync(join(tmpdir(), 'rel-'));
  writeFileSync(join(dir, 'v9.9.9.md'), 'stale');
  writeReleases('## 0.1.0 (2026-10-12)\n\n- x\n', pathToFileURL(dir + '/'));
  assert.deepEqual(readdirSync(dir), ['v0.1.0.md']);
  assert.ok(!existsSync(join(dir, 'v9.9.9.md')));
});
