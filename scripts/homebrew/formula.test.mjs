import {test} from 'node:test';
import assert from 'node:assert/strict';
import {existsSync, mkdtempSync, readFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {spawnSync} from 'node:child_process';
import {fileURLToPath} from 'node:url';
import {RELEASES, formula} from './formula.mjs';

// The checksums.txt GoReleaser v2.18.3 wrote for a snapshot (the npm script's test fixture).
const sums = readFileSync(new URL('../npm/testdata/checksums.txt', import.meta.url), 'utf8');
const sumOf = archive => sums.split('\n').find(l => l.endsWith('  ' + archive)).split(/\s+/)[0];

test('formula pins each macOS and Linux archive of the release by its sha256', () => {
  const f = formula(sums, '0.2.0');
  for (const archive of ['ambit_darwin_arm64.tar.gz', 'ambit_darwin_amd64.tar.gz', 'ambit_linux_arm64.tar.gz', 'ambit_linux_amd64.tar.gz']) {
    assert.ok(f.includes(`      url "${RELEASES}/v0.2.0/${archive}"\n      sha256 "${sumOf(archive)}"\n`), archive);
  }
  assert.equal(f.match(/^ *sha256 "[0-9a-f]{64}"$/gm).length, 4);
  assert.equal(f.match(/^ *url "/gm).length, 4);
  assert.ok(!f.includes('windows'), 'Homebrew has no Windows archive');
  assert.match(f, /^class Ambit < Formula$/m);
  assert.match(f, /^  version "0\.2\.0"$/m);
  assert.match(f, /bin\.install "ambit"/);
});

test('formula never removes the quarantine attribute and is not a cask', () => {
  const f = formula(sums, '0.2.0');
  assert.ok(!/xattr|quarantine|Cask|postflight/i.test(f), f);
});

test('formula refuses a checksums.txt without an archive it needs, and a version that is not a release', () => {
  const withoutLinuxArm = sums.split('\n').filter(l => !l.includes('ambit_linux_arm64')).join('\n');
  assert.throws(() => formula(withoutLinuxArm, '0.2.0'), /no line for ambit_linux_arm64.tar.gz/);
  assert.throws(() => formula(sums, 'v0.2.0'), /not a release version/);
  assert.throws(() => formula(sums, '0.2.0"; system "rm'), /not a release version/);
});

test('formula refuses a prerelease version unless allowed', () => {
  assert.throws(() => formula(sums, '0.2.0-rc.1'), /prerelease 0\.2\.0-rc\.1 is not published to/);
  assert.match(formula(sums, '0.2.0-rc.1', {allowPrerelease: true}), /^  version "0\.2\.0-rc\.1"$/m);
});

test('the command exits 1 on a prerelease and writes no file', () => {
  const dir = mkdtempSync(join(tmpdir(), 'brew-'));
  const out = join(dir, 'ambit.rb');
  const checksums = fileURLToPath(new URL('../npm/testdata/checksums.txt', import.meta.url));
  const run = (...extra) => spawnSync(process.execPath, [fileURLToPath(new URL('./formula.mjs', import.meta.url)), '--checksums', checksums, '--version', '0.2.0-rc.1', '--out', out, ...extra], {encoding: 'utf8'});
  const r = run();
  assert.equal(r.status, 1);
  assert.match(r.stderr, /✗ prerelease 0\.2\.0-rc\.1/);
  assert.match(r.stderr, /next: .*--allow-prerelease/);
  assert.equal(existsSync(out), false);
  assert.equal(run('--allow-prerelease').status, 0);
  assert.equal(existsSync(out), true);
});
