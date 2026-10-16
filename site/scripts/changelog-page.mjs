// Generates docs/changelog.md (gitignored) from the root CHANGELOG.md. Runs before `npm run build` and `npm start`.
import {readFileSync, writeFileSync} from 'node:fs';
import {fileURLToPath} from 'node:url';

export function changelogPage(md) {
  const text = md
    .replace(/^## Unreleased\n[\s\S]*?(?=^## )/m, '')
    .replace(/\]\(docs\/([^)#\s]+)\.md(#[^)\s]*)?\)/g, (_, p, frag = '') => `](${p === 'index' ? '/docs' : `/docs/${p}`}${frag})`);
  return `---\ntitle: Changelog\n---\n\n${text}`;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const root = new URL('../../', import.meta.url);
  writeFileSync(new URL('docs/changelog.md', root), changelogPage(readFileSync(new URL('CHANGELOG.md', root), 'utf8')));
}
