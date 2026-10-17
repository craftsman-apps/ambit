// Generates one blog post per released version in site/generated-releases/ (gitignored) from the root CHANGELOG.md.
// Runs before `npm run build` and `npm start`. "## Unreleased" is skipped; any other "## " heading needs "(YYYY-MM-DD)".
import {mkdirSync, readFileSync, rmSync, writeFileSync} from 'node:fs';
import {fileURLToPath} from 'node:url';

const rewriteLinks = (s) => s.replace(/\]\(docs\/([^)#\s]+)\.md(#[^)\s]*)?\)/g, (_, p, frag = '') => `](${p === 'index' ? '/docs' : `/docs/${p}`}${frag})`);

export function releasePages(md) {
  const pages = [];
  for (const section of md.split(/^(?=## )/m).filter((x) => x.startsWith('## '))) {
    const [heading, ...rest] = section.split('\n');
    const title = heading.slice(3).trim();
    if (title === 'Unreleased') continue;
    const m = title.match(/^(\S+) \((\d{4}-\d{2}-\d{2})\)$/);
    if (!m) throw new Error(`CHANGELOG.md: heading "## ${title}" must look like "## <version> (YYYY-MM-DD)"`);
    // Versions of one day keep the file order (newest first): the blog sorts by date only, so a second per position breaks the tie.
    const stamp = new Date(Date.parse(`${m[2]}T12:00:00Z`) - pages.length * 1000).toISOString();
    pages.push({version: m[1], date: m[2], file: `v${m[1]}.md`, content: `---\ntitle: v${m[1]}\nslug: v${m[1]}\ndate: ${stamp}\n---\n\n${rewriteLinks(rest.join('\n').trim())}\n`});
  }
  return pages;
}

export function writeReleases(md, dir) {
  const pages = releasePages(md);
  rmSync(dir, {recursive: true, force: true});
  mkdirSync(dir, {recursive: true});
  for (const p of pages) writeFileSync(new URL(p.file, dir), p.content);
  return pages;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  writeReleases(readFileSync(new URL('../../CHANGELOG.md', import.meta.url), 'utf8'), new URL('../generated-releases/', import.meta.url));
}
