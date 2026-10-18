// Release step: moves the lines of changelog/unreleased/*.md (file name order) into a new "## <version> (<date>)" section
// of CHANGELOG.md, directly after the "## Unreleased" section, and deletes those files. README.md stays.
// Usage: node site/scripts/changelog-assemble.mjs <version> <YYYY-MM-DD>
import {readdirSync, readFileSync, rmSync, writeFileSync} from 'node:fs';
import {fileURLToPath} from 'node:url';

export function assemble(version, date, changelogFile, dir) {
  if (!/^\d+\.\d+\.\d+\S*$/.test(version)) throw new Error(`version "${version}" must look like 0.2.4`);
  if (!/^\d{4}-\d{2}-\d{2}$/.test(date)) throw new Error(`date "${date}" must look like YYYY-MM-DD`);
  const md = readFileSync(changelogFile, 'utf8');
  if (md.split('\n').some((l) => l.startsWith(`## ${version} (`))) throw new Error(`CHANGELOG.md already has version ${version}`);
  const names = readdirSync(dir).filter((f) => f.endsWith('.md') && f !== 'README.md').sort();
  if (!names.length) throw new Error(`no fragment files in ${dir}`);
  const bodies = names.map((f) => {
    const text = readFileSync(new URL(f, dir), 'utf8').trim();
    if (!text) throw new Error(`fragment ${f} is empty`);
    return text;
  });
  const m = /^## Unreleased$/m.exec(md);
  if (!m) throw new Error('CHANGELOG.md has no "## Unreleased" section');
  const next = md.slice(m.index + m[0].length).search(/^## /m);
  const at = next < 0 ? md.length : m.index + m[0].length + next;
  const section = `## ${version} (${date})\n\n${bodies.join('\n')}\n\n`;
  writeFileSync(changelogFile, md.slice(0, at) + (next < 0 ? '\n' : '') + section + md.slice(at));
  for (const f of names) rmSync(new URL(f, dir));
  return names;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  try {
    const [version, date] = process.argv.slice(2);
    const names = assemble(version, date, new URL('../../CHANGELOG.md', import.meta.url), new URL('../../changelog/unreleased/', import.meta.url));
    console.log(`CHANGELOG.md: ${version} (${date}) from ${names.length} fragment(s): ${names.join(', ')}`);
  } catch (e) {
    console.error(`changelog-assemble: ${e.message}`);
    process.exit(1);
  }
}
