import {test} from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {spawnSync} from 'node:child_process';
import {fileURLToPath} from 'node:url';
import {manifest} from './manifest.mjs';

const sums = readFileSync(new URL('../npm/testdata/checksums.txt', import.meta.url), 'utf8');
const sumOf = archive => sums.split('\n').find(l => l.endsWith('  ' + archive)).split(/\s+/)[0];
const R = 'https://github.com/craftsman-apps/ambit/releases/download';

test('manifest pins both Windows archives of the release by their sha256', () => {
  const m = JSON.parse(manifest(sums, '0.5.0'));
  assert.deepEqual(m, {
    version: '0.5.0',
    description: 'Ambit CLI: install, check and connect Ambit, the AI gateway for companies',
    homepage: 'https://ambitproject.dev',
    license: 'Apache-2.0',
    architecture: {
      '64bit': {url: `${R}/v0.5.0/ambit_windows_amd64.zip`, hash: sumOf('ambit_windows_amd64.zip')},
      arm64: {url: `${R}/v0.5.0/ambit_windows_arm64.zip`, hash: sumOf('ambit_windows_arm64.zip')},
    },
    bin: 'ambit.exe',
    checkver: {github: 'https://github.com/craftsman-apps/ambit'},
    autoupdate: {architecture: {
      '64bit': {url: `${R}/v$version/ambit_windows_amd64.zip`},
      arm64: {url: `${R}/v$version/ambit_windows_arm64.zip`},
    }},
  });
});

test('manifest refuses a missing archive, a bad version and a prerelease', () => {
  const noArm = sums.split('\n').filter(l => !l.includes('ambit_windows_arm64')).join('\n');
  assert.throws(() => manifest(noArm, '0.5.0'), /no line for ambit_windows_arm64.zip/);
  assert.throws(() => manifest(sums, 'v0.5.0'), /not a release version/);
  assert.throws(() => manifest(sums, '0.5.0-rc.1'), /prerelease 0\.5\.0-rc\.1 is not published to/);
});

test('--dry-run prints the manifest and writes nothing', () => {
  const script = fileURLToPath(new URL('./manifest.mjs', import.meta.url));
  const fixture = fileURLToPath(new URL('../npm/testdata/checksums.txt', import.meta.url));
  const r = spawnSync(process.execPath, [script, '--checksums', fixture, '--version', '0.5.0', '--dry-run'], {encoding: 'utf8'});
  assert.equal(r.status, 0, r.stderr);
  assert.equal(JSON.parse(r.stdout).version, '0.5.0');
});
