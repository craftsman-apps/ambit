// Builds and publishes the npm packages of the ambit CLI (spec 6, npm): one package per platform holding the
// binary, and @craftsman-apps/ambit, which lists them in optionalDependencies (the esbuild pattern: npm installs
// only the package whose os and cpu match; no postinstall download). The binaries come from the release archives,
// each checked against checksums.txt first.
//   node scripts/npm/publish.mjs --dir <archives and checksums.txt> --version 0.2.0 --out <dir> [--publish]
import {createHash} from 'node:crypto';
import {chmodSync, copyFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {execFileSync} from 'node:child_process';
import {fileURLToPath} from 'node:url';

export const SCOPE = '@craftsman-apps';
export const REPOSITORY = {type: 'git', url: 'git+https://github.com/craftsman-apps/ambit.git'};

// GoReleaser archive name -> npm os and cpu (process.platform and process.arch).
export const TARGETS = [
  {goos: 'darwin', goarch: 'amd64', os: 'darwin', cpu: 'x64'},
  {goos: 'darwin', goarch: 'arm64', os: 'darwin', cpu: 'arm64'},
  {goos: 'linux', goarch: 'amd64', os: 'linux', cpu: 'x64'},
  {goos: 'linux', goarch: 'arm64', os: 'linux', cpu: 'arm64'},
  {goos: 'windows', goarch: 'amd64', os: 'win32', cpu: 'x64'},
  {goos: 'windows', goarch: 'arm64', os: 'win32', cpu: 'arm64'},
].map(t => ({...t,
  archive: `ambit_${t.goos}_${t.goarch}.${t.goos === 'windows' ? 'zip' : 'tar.gz'}`,
  binary: t.goos === 'windows' ? 'ambit.exe' : 'ambit',
  name: `${SCOPE}/ambit-${t.os}-${t.cpu}`}));

// A version with a '-' (0.2.0-rc.1) is a prerelease: it gets a GitHub release only, never npm or Homebrew.
export const PRERELEASE_NEXT = 'pass --allow-prerelease only if you mean to publish a prerelease';
export function refusePrerelease(version, channel, allow) {
  if (version.includes('-') && !allow) throw new Error(`prerelease ${version} is not published to ${channel} — releases with a '-' in the version go to GitHub only`);
}

export function parseChecksums(text) {
  const sums = new Map();
  for (const line of text.split('\n')) {
    const m = line.trim().match(/^([0-9a-f]{64})\s+\*?(\S+)$/);
    if (m) sums.set(m[2], m[1]);
  }
  return sums;
}

export function verify(file, name, sums) {
  const want = sums.get(name);
  if (!want) throw new Error(`checksums.txt has no line for ${name}`);
  const got = createHash('sha256').update(readFileSync(file)).digest('hex');
  if (got !== want) throw new Error(`checksum mismatch for ${name}: got ${got}, want ${want}`);
}

// extract copies the binary out of a release archive into dest.
export function extract(archive, binary, dest) {
  const tmp = mkdtempSync(join(tmpdir(), 'ambit-npm-'));
  try {
    if (archive.endsWith('.zip')) execFileSync('unzip', ['-q', '-o', archive, binary, '-d', tmp]);
    else execFileSync('tar', ['-xzf', archive, '-C', tmp, binary]);
    copyFileSync(join(tmp, binary), dest);
  } finally {
    rmSync(tmp, {recursive: true, force: true});
  }
}

export const LAUNCHER = `#!/usr/bin/env node
// Runs the ambit binary from this platform's package, which npm installed through optionalDependencies.
'use strict';
const {spawnSync} = require('node:child_process');
const pkg = '${SCOPE}/ambit-' + process.platform + '-' + process.arch;
let bin;
try {
  bin = require.resolve(pkg + '/bin/ambit' + (process.platform === 'win32' ? '.exe' : ''));
} catch {
  console.error('✗ no ambit binary for ' + process.platform + ' ' + process.arch + ' — the package ' + pkg + ' is not installed');
  console.error('  next: npm install ' + pkg + ', or see https://ambitproject.dev/docs/quick-start');
  process.exit(1);
}
const r = spawnSync(bin, process.argv.slice(2), {stdio: 'inherit'});
if (r.error) {
  console.error('✗ cannot run ' + bin + ' — ' + r.error.message);
  process.exit(1);
}
process.exit(r.status === null ? 1 : r.status);
`;

const common = version => ({version, license: 'Apache-2.0', repository: REPOSITORY, homepage: 'https://ambitproject.dev'});

// build writes one directory per package into out and returns them in publish order (platform packages first).
export function build({dir, version, out, extractFn = extract, allowPrerelease = false}) {
  refusePrerelease(version, 'npm', allowPrerelease);
  const sums = parseChecksums(readFileSync(join(dir, 'checksums.txt'), 'utf8'));
  const dirs = [];
  for (const t of TARGETS) {
    const archive = join(dir, t.archive);
    verify(archive, t.archive, sums);
    const pkgDir = join(out, `ambit-${t.os}-${t.cpu}`);
    mkdirSync(join(pkgDir, 'bin'), {recursive: true});
    const bin = join(pkgDir, 'bin', t.binary);
    extractFn(archive, t.binary, bin);
    chmodSync(bin, 0o755);
    writeFileSync(join(pkgDir, 'package.json'), JSON.stringify({name: t.name, ...common(version),
      description: `The ambit CLI binary for ${t.os} ${t.cpu}. Install ${SCOPE}/ambit instead.`,
      os: [t.os], cpu: [t.cpu], files: ['bin']}, null, 2) + '\n');
    dirs.push(pkgDir);
  }
  const main = join(out, 'ambit');
  mkdirSync(join(main, 'bin'), {recursive: true});
  writeFileSync(join(main, 'bin', 'ambit.js'), LAUNCHER, {mode: 0o755});
  writeFileSync(join(main, 'package.json'), JSON.stringify({name: `${SCOPE}/ambit`, ...common(version),
    description: 'Install, check and validate Ambit, the open-source MCP and LLM gateway. npx @craftsman-apps/ambit local up',
    bin: {ambit: 'bin/ambit.js'}, files: ['bin'],
    optionalDependencies: Object.fromEntries(TARGETS.map(t => [t.name, version]))}, null, 2) + '\n');
  dirs.push(main);
  return dirs;
}

// publish publishes each package that is not on the registry yet, so a re-run of the job is safe.
export function publish(dirs, run = (cmd, args) => execFileSync(cmd, args, {stdio: 'inherit'})) {
  for (const d of dirs) {
    const {name, version} = JSON.parse(readFileSync(join(d, 'package.json'), 'utf8'));
    try {
      run('npm', ['view', `${name}@${version}`, 'version']);
      console.log(`${name}@${version} is already published`);
      continue;
    } catch { /* not published yet */ }
    run('npm', ['publish', d, '--access', 'public', '--provenance']);
  }
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const args = process.argv.slice(2);
  const opt = n => { const i = args.indexOf(n); return i < 0 ? undefined : args[i + 1]; };
  const [dir, version, out] = [opt('--dir'), opt('--version'), opt('--out')];
  if (!dir || !version || !out) {
    console.error('usage: node scripts/npm/publish.mjs --dir <dir> --version <x.y.z> --out <dir> [--publish]');
    process.exit(2);
  }
  try {
    const dirs = build({dir, version, out, allowPrerelease: args.includes('--allow-prerelease')});
    if (args.includes('--publish')) publish(dirs);
    else console.log('built (not published):\n' + dirs.join('\n'));
  } catch (e) {
    console.error(`✗ ${e.message}`);
    if (/^prerelease /.test(e.message)) console.error(`  next: ${PRERELEASE_NEXT}`);
    process.exit(1);
  }
}
