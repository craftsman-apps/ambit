import {test} from 'node:test';
import assert from 'node:assert/strict';
import {mkdtempSync, mkdirSync, writeFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {missingAddresses} from './check-readme-urls.mjs';

function build(files) {
  const dir = mkdtempSync(join(tmpdir(), 'site-'));
  for (const f of files) { mkdirSync(join(dir, f, '..'), {recursive: true}); writeFileSync(join(dir, f), 'x'); }
  return dir;
}
const readme = '[a](https://ambitproject.dev/docs/) [b](https://ambitproject.dev/docs/quick-start) ' +
  '<a href="https://ambitproject.dev/docs/guides/coding-tools">c</a> [d](https://ambitproject.dev/docs/quick-start#step)';

test('all present: flat .html files', () => {
  const dir = build(['docs.html', 'docs/quick-start.html', 'docs/guides/coding-tools.html']);
  assert.deepEqual(missingAddresses(readme, dir), []);
});
test('all present: directory index files', () => {
  const dir = build(['docs/index.html', 'docs/quick-start/index.html', 'docs/guides/coding-tools/index.html']);
  assert.deepEqual(missingAddresses(readme, dir), []);
});
test('one missing, fragments ignored, duplicates once', () => {
  const dir = build(['docs/index.html', 'docs/guides/coding-tools.html']);
  assert.deepEqual(missingAddresses(readme, dir), ['/docs/quick-start']);
});
test('allowed-missing pages are skipped, others still fail', () => {
  const dir = build(['docs.html']);
  assert.deepEqual(missingAddresses(readme, dir, ['/docs/quick-start']), ['/docs/guides/coding-tools']);
});
test('no addresses in README is an error, not a pass', () => {
  assert.throws(() => missingAddresses('no links here', build([])), /no ambitproject.dev\/docs address/);
});
