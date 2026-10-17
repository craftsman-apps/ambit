import {readFileSync} from 'node:fs';
import {fileURLToPath} from 'node:url';

const root = new URL('../../', import.meta.url);
// The pages that name the version a reader installs. The first two must contain at least one match.
export const PAGES = ['docs/local-up.md', 'docs/install.md', 'docs/quick-start.md'];
const REQUIRED = ['docs/local-up.md', 'docs/install.md'];
const SEMVER = '\\d+\\.\\d+\\.\\d+(?:-[0-9A-Za-z.-]+)?';
// Only these three patterns are install versions. Kubernetes versions, image tags and digests are not.
const PATTERNS = [`releases/download/v(${SEMVER})`, `--version (${SEMVER})`, `cmd/ambit@v(${SEMVER})`];

// latestRelease is the first "## <version> (YYYY-MM-DD)" heading; "## Unreleased" does not match.
export function latestRelease(changelog) {
  const m = changelog.match(/^## (\d+\.\d+\.\d+\S*) \(\d{4}-\d{2}-\d{2}\)/m);
  if (!m) throw new Error('CHANGELOG.md has no "## <version> (YYYY-MM-DD)" heading');
  return m[1];
}

// installVersionProblems returns one message per problem. pages maps a file name to its text.
export function installVersionProblems(changelog, pages) {
  const latest = latestRelease(changelog);
  const problems = [];
  for (const [file, text] of Object.entries(pages)) {
    let found = 0;
    text.split('\n').forEach((line, i) => {
      for (const p of PATTERNS) {
        for (const m of line.matchAll(new RegExp(p, 'g'))) {
          found++;
          if (m[1] !== latest) problems.push(`${file}:${i + 1}: install version ${m[1]}, latest release is ${latest}`);
        }
      }
    });
    if (!found && REQUIRED.includes(file)) {
      problems.push(`${file}: no install version found (releases/download/v<X>, --version <X>); the check would be off`);
    }
  }
  return problems;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const read = f => readFileSync(new URL(f, root), 'utf8');
  const problems = installVersionProblems(read('CHANGELOG.md'), Object.fromEntries(PAGES.map(f => [f, read(f)])));
  if (problems.length) { console.error(problems.join('\n')); process.exit(1); }
  console.log(`install pages name release ${latestRelease(read('CHANGELOG.md'))}`);
}
