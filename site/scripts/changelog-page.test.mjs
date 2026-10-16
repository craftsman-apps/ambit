import {test} from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {changelogPage} from './changelog-page.mjs';

const body = (s) => s.replace(/^---\ntitle: Changelog\n---\n\n/, '');

test('Unreleased is removed and the next release section is kept intact', () => {
  const out = changelogPage('# Changelog\n\n## Unreleased\n\nAdd a line here.\n\n- draft\n\n## 0.2.1 (2026-10-16)\n\n- Fixed offboarding.\n\n## 0.2.0 (2026-10-16)\n\n- Old.\n');
  assert.ok(!out.includes('Unreleased') && !out.includes('draft'));
  assert.equal(body(out), '# Changelog\n\n## 0.2.1 (2026-10-16)\n\n- Fixed offboarding.\n\n## 0.2.0 (2026-10-16)\n\n- Old.\n');
});

test('a file without Unreleased passes through unchanged except links', () => {
  const md = '# Changelog\n\n## 0.1.0 (2026-10-12)\n\n- First [site](https://example.com).\n';
  assert.equal(body(changelogPage(md)), md);
});

test('repository-relative docs links are rewritten, fragments kept, URLs untouched', () => {
  const out = changelogPage('[a](docs/upgrading.md) [b](docs/guides/x.md) [c](docs/index.md#known-limitations) [d](docs/upgrading.md#to-the-release-with-audit-only-mode) [e](https://github.com/o/r/blob/main/docs/a.md)\n');
  assert.equal(body(out), '[a](/docs/upgrading) [b](/docs/guides/x) [c](/docs#known-limitations) [d](/docs/upgrading#to-the-release-with-audit-only-mode) [e](https://github.com/o/r/blob/main/docs/a.md)\n');
});

test('every released heading in CHANGELOG.md has a date', () => {
  const md = readFileSync(new URL('../../CHANGELOG.md', import.meta.url), 'utf8');
  const heads = [...md.matchAll(/^## (.+)$/gm)].map((m) => m[1]).filter((h) => h !== 'Unreleased');
  assert.ok(heads.length >= 3);
  for (const h of heads) assert.match(h, /^\S+ \(\d{4}-\d{2}-\d{2}\)$/);
});
