import {test} from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {PAGES, latestRelease, installVersionProblems} from './check-install-version.mjs';

const read = f => readFileSync(new URL(`../../${f}`, import.meta.url), 'utf8');
const CL = '# Changelog\n\n## Unreleased\n\n- x\n\n## 0.2.2 (2026-10-17)\n\n- y\n\n## 0.1.0 (2026-10-12)\n';
const good = 'R=https://github.com/o/r/releases/download/v0.2.2\nhelm install a --version 0.2.2\n';
const pages = (over = {}) => ({'docs/local-up.md': good, 'docs/install.md': good, 'docs/quick-start.md': 'go install x/cmd/ambit@v0.2.2', ...over});

test('the real CHANGELOG and pages agree', () => {
  assert.deepEqual(installVersionProblems(read('CHANGELOG.md'), Object.fromEntries(PAGES.map(f => [f, read(f)]))), []);
});
test('Unreleased is skipped when picking the latest release', () => {
  assert.equal(latestRelease(CL), '0.2.2');
});
test('an old version fails and names file, line and version', () => {
  const p = installVersionProblems(CL, pages({'docs/install.md': 'a\nb\nhelm install --version 0.1.0\n' + good}));
  assert.deepEqual(p, ['docs/install.md:3: install version 0.1.0, latest release is 0.2.2']);
});
test('each of the three patterns is checked', () => {
  const old = 'releases/download/v0.1.0 --version 0.1.0 cmd/ambit@v0.1.0';
  assert.equal(installVersionProblems(CL, pages({'docs/quick-start.md': old})).length, 3);
});
test('a required page with no match fails; quick-start may have none', () => {
  assert.equal(installVersionProblems(CL, pages({'docs/local-up.md': 'no versions here'})).length, 1);
  assert.deepEqual(installVersionProblems(CL, pages({'docs/quick-start.md': 'nothing'})), []);
});
test('--version <new version> and other numbers are ignored', () => {
  const text = good + 'helm upgrade --version <new version>\nKubernetes 1.30 image v0.1.0 sha256:0123\n';
  assert.deepEqual(installVersionProblems(CL, pages({'docs/install.md': text})), []);
});
