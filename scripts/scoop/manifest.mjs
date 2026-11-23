// Writes the Scoop manifest of the ambit CLI (spec D2 8.2) from the release's checksums.txt, as
// scripts/homebrew/formula.mjs does for Homebrew: the job publishes from the verified, published checksums, not
// from a build. Scoop refuses a download whose SHA-256 differs from `hash`.
//   node scripts/scoop/manifest.mjs --checksums rel/checksums.txt --version 0.5.0 --out bucket/ambit.json [--dry-run]
// --dry-run prints the manifest instead of writing it.
import {mkdirSync, readFileSync, writeFileSync} from 'node:fs';
import {dirname} from 'node:path';
import {fileURLToPath} from 'node:url';
import {PRERELEASE_NEXT, parseChecksums, refusePrerelease} from '../npm/publish.mjs';
import {RELEASES} from '../homebrew/formula.mjs';

const ARCHES = [['64bit', 'ambit_windows_amd64.zip'], ['arm64', 'ambit_windows_arm64.zip']];

export function manifest(checksumsText, version, {allowPrerelease = false} = {}) {
  if (!/^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/.test(version)) throw new Error(`not a release version: ${version}`);
  refusePrerelease(version, 'Scoop', allowPrerelease);
  const sums = parseChecksums(checksumsText);
  const architecture = {}, autoupdate = {};
  for (const [arch, archive] of ARCHES) {
    const hash = sums.get(archive);
    if (!hash) throw new Error(`checksums.txt has no line for ${archive}`);
    architecture[arch] = {url: `${RELEASES}/v${version}/${archive}`, hash};
    autoupdate[arch] = {url: `${RELEASES}/v$version/${archive}`};
  }
  return JSON.stringify({
    version,
    description: 'Ambit CLI: install, check and connect Ambit, the AI gateway for companies',
    homepage: 'https://ambitproject.dev',
    license: 'Apache-2.0',
    architecture,
    bin: 'ambit.exe',
    checkver: {github: 'https://github.com/craftsman-apps/ambit'},
    autoupdate: {architecture: autoupdate},
  }, null, 2) + '\n';
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const args = process.argv.slice(2);
  const opt = n => { const i = args.indexOf(n); return i < 0 ? undefined : args[i + 1]; };
  const [checksums, version, out] = [opt('--checksums'), opt('--version'), opt('--out')];
  if (!checksums || !version || (!out && !args.includes('--dry-run'))) {
    console.error('usage: node scripts/scoop/manifest.mjs --checksums <checksums.txt> --version <x.y.z> (--out <file> | --dry-run)');
    process.exit(2);
  }
  try {
    const text = manifest(readFileSync(checksums, 'utf8'), version, {allowPrerelease: args.includes('--allow-prerelease')});
    if (args.includes('--dry-run')) {
      process.stdout.write(text);
    } else {
      mkdirSync(dirname(out), {recursive: true});
      writeFileSync(out, text);
      console.log(`wrote ${out}`);
    }
  } catch (e) {
    console.error(`✗ ${e.message}`);
    if (/^prerelease /.test(e.message)) console.error(`  next: ${PRERELEASE_NEXT}`);
    process.exit(1);
  }
}
