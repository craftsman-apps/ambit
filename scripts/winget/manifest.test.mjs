import {test} from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {ID, manifests} from './manifest.mjs';

const sums = readFileSync(new URL('../npm/testdata/checksums.txt', import.meta.url), 'utf8');
const sumOf = archive => sums.split('\n').find(l => l.endsWith('  ' + archive)).split(/\s+/)[0];

test('three manifests: version, installer (zip with a portable ambit.exe, both architectures) and en-US locale', () => {
  const m = manifests(sums, '0.5.0');
  assert.deepEqual(Object.keys(m).sort(), [`${ID}.installer.yaml`, `${ID}.locale.en-US.yaml`, `${ID}.yaml`]);
  for (const text of Object.values(m)) {
    assert.match(text, /^PackageIdentifier: CraftsmanApps\.Ambit$/m);
    assert.match(text, /^PackageVersion: 0\.5\.0$/m);
    assert.match(text, /^ManifestVersion: 1\.9\.0$/m);
  }
  const inst = m[`${ID}.installer.yaml`];
  assert.match(inst, /^InstallerType: zip$/m);
  assert.match(inst, /^NestedInstallerType: portable$/m);
  assert.match(inst, /  - RelativeFilePath: ambit\.exe\n    PortableCommandAlias: ambit\n/);
  for (const [arch, archive] of [['x64', 'ambit_windows_amd64.zip'], ['arm64', 'ambit_windows_arm64.zip']]) {
    assert.ok(inst.includes(`  - Architecture: ${arch}\n    InstallerUrl: https://github.com/craftsman-apps/ambit/releases/download/v0.5.0/${archive}\n    InstallerSha256: ${sumOf(archive).toUpperCase()}\n`), arch);
  }
  assert.match(m[`${ID}.locale.en-US.yaml`], /^Moniker: ambit$/m);
  assert.match(m[`${ID}.locale.en-US.yaml`], /^License: Apache-2\.0$/m);
  assert.match(m[`${ID}.yaml`], /^DefaultLocale: en-US$/m);
});

test('manifests refuse a missing archive, a bad version and a prerelease', () => {
  const noX64 = sums.split('\n').filter(l => !l.includes('ambit_windows_amd64')).join('\n');
  assert.throws(() => manifests(noX64, '0.5.0'), /no line for ambit_windows_amd64.zip/);
  assert.throws(() => manifests(sums, '0.5.0\nPublisher: evil'), /not a release version/);
  assert.throws(() => manifests(sums, '0.5.0-rc.1'), /prerelease/);
});
