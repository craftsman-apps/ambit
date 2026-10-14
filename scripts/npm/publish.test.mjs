import {test} from 'node:test';
import assert from 'node:assert/strict';
import {createHash} from 'node:crypto';
import {existsSync, mkdtempSync, readFileSync, statSync, writeFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {TARGETS, build, parseChecksums, publish} from './publish.mjs';

// fakeRelease writes one fake archive per target and a checksums.txt that matches them.
function fakeRelease({corrupt} = {}) {
  const dir = mkdtempSync(join(tmpdir(), 'rel-'));
  const lines = [];
  for (const t of TARGETS) {
    const body = `archive ${t.archive}`;
    writeFileSync(join(dir, t.archive), body);
    const sum = createHash('sha256').update(corrupt === t.archive ? 'other' : body).digest('hex');
    lines.push(`${sum}  ${t.archive}`);
  }
  writeFileSync(join(dir, 'checksums.txt'), lines.join('\n') + '\n');
  return dir;
}

// The real extract needs tar and unzip; this one writes a recognisable file.
const fakeExtract = (archive, binary, dest) => writeFileSync(dest, `binary ${binary} from ${archive}`);

test('parseChecksums reads sha256sum and GoReleaser lines', () => {
  const s = parseChecksums(`${'a'.repeat(64)}  ambit_linux_amd64.tar.gz\n${'b'.repeat(64)} *ambit_windows_amd64.zip\nnot a line\n`);
  assert.equal(s.get('ambit_linux_amd64.tar.gz'), 'a'.repeat(64));
  assert.equal(s.get('ambit_windows_amd64.zip'), 'b'.repeat(64));
  assert.equal(s.size, 2);
});

test('build makes six platform packages and the main package', () => {
  const out = mkdtempSync(join(tmpdir(), 'npm-'));
  const dirs = build({dir: fakeRelease(), version: '0.2.0', out, extractFn: fakeExtract});
  assert.equal(dirs.length, 7);
  const linux = JSON.parse(readFileSync(join(out, 'ambit-linux-x64', 'package.json'), 'utf8'));
  assert.deepEqual([linux.name, linux.version, linux.os, linux.cpu], ['@craftsman-apps/ambit-linux-x64', '0.2.0', ['linux'], ['x64']]);
  assert.ok(existsSync(join(out, 'ambit-win32-arm64', 'bin', 'ambit.exe')));
  if (process.platform !== 'win32') assert.equal(statSync(join(out, 'ambit-darwin-arm64', 'bin', 'ambit')).mode & 0o111, 0o111);
  const main = JSON.parse(readFileSync(join(out, 'ambit', 'package.json'), 'utf8'));
  assert.equal(main.name, '@craftsman-apps/ambit');
  assert.equal(main.bin.ambit, 'bin/ambit.js');
  assert.equal(Object.keys(main.optionalDependencies).length, 6);
  assert.equal(main.optionalDependencies['@craftsman-apps/ambit-darwin-arm64'], '0.2.0');
  assert.equal(main.scripts, undefined, 'no postinstall: the binary comes from the platform package');
  assert.match(readFileSync(join(out, 'ambit', 'bin', 'ambit.js'), 'utf8'), /'@craftsman-apps\/ambit-' \+ process\.platform \+ '-' \+ process\.arch/);
  assert.equal(dirs.at(-1), join(out, 'ambit'), 'the main package is published last');
});

test('build refuses an archive whose checksum does not match', () => {
  const out = mkdtempSync(join(tmpdir(), 'npm-'));
  assert.throws(() => build({dir: fakeRelease({corrupt: 'ambit_linux_arm64.tar.gz'}), version: '0.2.0', out, extractFn: fakeExtract}),
    /checksum mismatch for ambit_linux_arm64.tar.gz/);
});

test('publish skips versions already on the registry and publishes the rest with provenance', () => {
  const out = mkdtempSync(join(tmpdir(), 'npm-'));
  const dirs = build({dir: fakeRelease(), version: '0.2.0', out, extractFn: fakeExtract});
  const calls = [];
  publish(dirs, (cmd, args) => {
    calls.push([cmd, ...args].join(' '));
    if (args[0] === 'view' && !args[1].startsWith('@craftsman-apps/ambit-linux-x64@')) throw new Error('E404');
  });
  const published = calls.filter(c => c.startsWith('npm publish'));
  assert.equal(published.length, 6);
  assert.ok(published.every(c => c.endsWith('--access public --provenance')));
  assert.ok(!published.some(c => c.includes('ambit-linux-x64')));
});

// The full release matrix (spec 6): every GoReleaser archive maps to one npm package whose os and cpu are what
// process.platform and process.arch return on that system, with ambit.exe on Windows.
const MATRIX = [
  ['ambit_darwin_amd64.tar.gz', 'ambit', '@craftsman-apps/ambit-darwin-x64', 'darwin', 'x64'],
  ['ambit_darwin_arm64.tar.gz', 'ambit', '@craftsman-apps/ambit-darwin-arm64', 'darwin', 'arm64'],
  ['ambit_linux_amd64.tar.gz', 'ambit', '@craftsman-apps/ambit-linux-x64', 'linux', 'x64'],
  ['ambit_linux_arm64.tar.gz', 'ambit', '@craftsman-apps/ambit-linux-arm64', 'linux', 'arm64'],
  ['ambit_windows_amd64.zip', 'ambit.exe', '@craftsman-apps/ambit-win32-x64', 'win32', 'x64'],
  ['ambit_windows_arm64.zip', 'ambit.exe', '@craftsman-apps/ambit-win32-arm64', 'win32', 'arm64'],
];

for (const [archive, binary, name, os, cpu] of MATRIX) {
  test(`release matrix: ${archive} -> ${name}`, () => {
    const t = TARGETS.find(x => x.archive === archive);
    assert.ok(t, `no target for ${archive}`);
    assert.deepEqual([t.binary, t.name, t.os, t.cpu], [binary, name, os, cpu]);
  });
}

// testdata/checksums.txt is the file GoReleaser v2.18.3 wrote for a snapshot of .goreleaser.yaml (2026-10-14):
// the real line format, and exactly the six archive names, so a change of name_template fails here.
test('release matrix: GoReleaser checksums.txt names exactly the six archives', () => {
  const text = readFileSync(new URL('./testdata/checksums.txt', import.meta.url), 'utf8');
  const sums = parseChecksums(text);
  assert.equal(text.trim().split('\n').length, 6, 'one line per archive, nothing else');
  assert.deepEqual([...sums.keys()].sort(), MATRIX.map(r => r[0]).sort());
  assert.equal(TARGETS.length, MATRIX.length);
});
