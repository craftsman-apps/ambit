// Writes the Homebrew formula of the ambit CLI (spec 6, Homebrew; ruling S6) from the release's checksums.txt.
// The formula installs the prebuilt binary from the GitHub release archive for the system, pinned by its sha256.
// Homebrew does not set the quarantine attribute on files a formula installs, so the formula needs no xattr call
// (ruling S8). GoReleaser's own formula publisher (`brews`) is deprecated in v2 and fails `goreleaser check`.
//   node scripts/homebrew/formula.mjs --checksums dist/checksums.txt --version 0.2.0 --out formula/ambit.rb
import {mkdirSync, readFileSync, writeFileSync} from 'node:fs';
import {dirname} from 'node:path';
import {fileURLToPath} from 'node:url';
import {parseChecksums} from '../npm/publish.mjs';

export const RELEASES = 'https://github.com/craftsman-apps/ambit/releases/download';

// Homebrew blocks per system, in the order they appear in the formula.
const SYSTEMS = [
  ['on_macos', [['on_arm', 'ambit_darwin_arm64.tar.gz'], ['on_intel', 'ambit_darwin_amd64.tar.gz']]],
  ['on_linux', [['on_arm', 'ambit_linux_arm64.tar.gz'], ['on_intel', 'ambit_linux_amd64.tar.gz']]],
];

export function formula(checksumsText, version) {
  if (!/^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/.test(version)) throw new Error(`not a release version: ${version}`);
  const sums = parseChecksums(checksumsText);
  const blocks = SYSTEMS.map(([os, cpus]) => `  ${os} do\n` + cpus.map(([cpu, archive]) => {
    const sum = sums.get(archive);
    if (!sum) throw new Error(`checksums.txt has no line for ${archive}`);
    return `    ${cpu} do\n      url "${RELEASES}/v${version}/${archive}"\n      sha256 "${sum}"\n    end\n`;
  }).join('') + '  end\n').join('\n');
  return `class Ambit < Formula
  desc "Install, check and validate Ambit, the open-source MCP and LLM gateway"
  homepage "https://ambitproject.dev"
  version "${version}"
  license "Apache-2.0"

${blocks}
  def install
    bin.install "ambit"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/ambit version --kubeconfig /dev/null")
  end
end
`;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const args = process.argv.slice(2);
  const opt = n => { const i = args.indexOf(n); return i < 0 ? undefined : args[i + 1]; };
  const [checksums, version, out] = [opt('--checksums'), opt('--version'), opt('--out')];
  if (!checksums || !version || !out) {
    console.error('usage: node scripts/homebrew/formula.mjs --checksums <checksums.txt> --version <x.y.z> --out <file>');
    process.exit(2);
  }
  mkdirSync(dirname(out), {recursive: true});
  writeFileSync(out, formula(readFileSync(checksums, 'utf8'), version));
  console.log(`wrote ${out}`);
}
