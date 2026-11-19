// Writes the winget manifests of the ambit CLI (spec D2 8.3; package CraftsmanApps.Ambit) from the release's
// checksums.txt: the version, installer and default-locale files of microsoft/winget-pkgs, schema 1.9.0. The
// installer is the release zip with the portable ambit.exe inside; winget refuses an installer whose SHA-256
// differs. Used for the first submission (wingetcreate submit) and for `winget validate` in the release workflow;
// later versions go through Komac, which writes the same fields.
//   node scripts/winget/manifest.mjs --checksums rel/checksums.txt --version 0.5.0 --out winget/
import {mkdirSync, readFileSync, writeFileSync} from 'node:fs';
import {join} from 'node:path';
import {fileURLToPath} from 'node:url';
import {PRERELEASE_NEXT, parseChecksums, refusePrerelease} from '../npm/publish.mjs';
import {RELEASES} from '../homebrew/formula.mjs';

export const ID = 'CraftsmanApps.Ambit';
const SCHEMA = '1.9.0';
const ARCHES = [['x64', 'ambit_windows_amd64.zip'], ['arm64', 'ambit_windows_arm64.zip']];

// manifests returns {file name: content}.
export function manifests(checksumsText, version, {allowPrerelease = false} = {}) {
  if (!/^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/.test(version)) throw new Error(`not a release version: ${version}`);
  refusePrerelease(version, 'winget', allowPrerelease);
  const sums = parseChecksums(checksumsText);
  const head = `# yaml-language-server: $schema=https://aka.ms/winget-manifest.TYPE.${SCHEMA}.schema.json\n\nPackageIdentifier: ${ID}\nPackageVersion: ${version}\n`;
  const tail = type => `ManifestType: ${type}\nManifestVersion: ${SCHEMA}\n`;
  const installers = ARCHES.map(([arch, archive]) => {
    const sum = sums.get(archive);
    if (!sum) throw new Error(`checksums.txt has no line for ${archive}`);
    return `  - Architecture: ${arch}\n    InstallerUrl: ${RELEASES}/v${version}/${archive}\n    InstallerSha256: ${sum.toUpperCase()}\n`;
  }).join('');
  return {
    [`${ID}.yaml`]: head.replace('TYPE', 'version') + `DefaultLocale: en-US\n` + tail('version'),
    [`${ID}.installer.yaml`]: head.replace('TYPE', 'installer') +
      `InstallerType: zip\nNestedInstallerType: portable\nNestedInstallerFiles:\n  - RelativeFilePath: ambit.exe\n    PortableCommandAlias: ambit\n` +
      `Commands:\n  - ambit\nInstallers:\n${installers}` + tail('installer'),
    [`${ID}.locale.en-US.yaml`]: head.replace('TYPE', 'defaultLocale') +
      `PackageLocale: en-US\nPublisher: Craftsman Apps\nPublisherUrl: https://github.com/craftsman-apps\n` +
      `PackageName: Ambit\nPackageUrl: https://ambitproject.dev\nLicense: Apache-2.0\n` +
      `LicenseUrl: https://github.com/craftsman-apps/ambit/blob/main/LICENSE\n` +
      `ShortDescription: Ambit CLI - install, check and connect Ambit, the AI gateway for companies\n` +
      `Moniker: ambit\nTags:\n  - ai-gateway\n  - kubernetes\n  - mcp\n` +
      `ReleaseNotesUrl: https://github.com/craftsman-apps/ambit/releases/tag/v${version}\n` + tail('defaultLocale'),
  };
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const args = process.argv.slice(2);
  const opt = n => { const i = args.indexOf(n); return i < 0 ? undefined : args[i + 1]; };
  const [checksums, version, out] = [opt('--checksums'), opt('--version'), opt('--out')];
  if (!checksums || !version || !out) {
    console.error('usage: node scripts/winget/manifest.mjs --checksums <checksums.txt> --version <x.y.z> --out <directory>');
    process.exit(2);
  }
  try {
    const files = manifests(readFileSync(checksums, 'utf8'), version, {allowPrerelease: args.includes('--allow-prerelease')});
    mkdirSync(out, {recursive: true});
    for (const [name, text] of Object.entries(files)) writeFileSync(join(out, name), text);
    console.log(`wrote ${Object.keys(files).length} manifests to ${out}`);
  } catch (e) {
    console.error(`✗ ${e.message}`);
    if (/^prerelease /.test(e.message)) console.error(`  next: ${PRERELEASE_NEXT}`);
    process.exit(1);
  }
}
