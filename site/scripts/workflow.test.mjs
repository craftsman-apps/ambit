import {test} from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
const site = readFileSync(new URL('../../.github/workflows/site.yml', import.meta.url), 'utf8');
const ci = readFileSync(new URL('../../.github/workflows/ci.yml', import.meta.url), 'utf8');

test('site workflow only for site files', () => {
  for (const p of ['"docs/**"', '"site/**"', '"README.md"', '".github/workflows/site.yml"']) assert.ok(site.includes(p), p);
  assert.equal((site.match(/paths:/g) || []).length, 2);
});
test('site job names cannot collide with the required check "build"', () => {
  assert.ok(/^  site-build:/m.test(site) && /^  site-deploy:/m.test(site));
  assert.ok(!/^  build:/m.test(site));
});
test('deploy never runs for fork pull requests', () => {
  assert.ok(site.includes("github.event.pull_request.head.repo.full_name == github.repository"));
});
test('ci.yml treats site/ and the site workflow as documentation-only', () => {
  assert.ok(ci.includes('^site/') && ci.includes('site\\.yml'));
});
test('README address check has no allow-list left', () => {
  const pkg = readFileSync(new URL('../package.json', import.meta.url), 'utf8');
  assert.ok(!/check-readme-urls\.mjs \.\.\/README\.md build \S/.test(pkg));
});
