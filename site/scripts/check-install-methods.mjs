import {readFileSync} from 'node:fs';
import {fileURLToPath} from 'node:url';

// Every install method of the ambit CLI (spec 6, release-1 channels) with the command the quick start must show
// (ruling S5: one subsection per method instead of tabs, so each command is on the page as text).
export const INSTALL_COMMANDS = {
  Homebrew: 'brew install craftsman-apps/tap/ambit',
  'install script': 'curl -fsSL https://ambitproject.dev/install.sh | sh',
  PowerShell: 'irm https://ambitproject.dev/install.ps1 | iex',
  npm: 'npx @craftsman-apps/ambit local up',
  'go install': 'go install github.com/craftsman-apps/ambit/cmd/ambit@',
};

const ENTITIES = {'&lt;': '<', '&gt;': '>', '&quot;': '"', '&#x27;': "'", '&#39;': "'", '&amp;': '&'};

// pageText is the text a reader sees: tags skipped (code highlighting splits a command into spans) and the
// entities Docusaurus writes decoded in one pass, so "&amp;lt;" becomes "&lt;" and not "<". The tags are
// skipped with a scan, not a regex replace: a replace can leave a tag behind after one pass.
export function pageText(html) {
  let text = '';
  let inTag = false;
  for (const ch of html) {
    if (ch === '<') inTag = true;
    else if (ch === '>' && inTag) inTag = false;
    else if (!inTag) text += ch;
  }
  return text.replace(/&(?:lt|gt|quot|amp|#x27|#39);/g, e => ENTITIES[e]);
}

export function missingInstallMethods(html) {
  const text = pageText(html);
  return Object.entries(INSTALL_COMMANDS).filter(([, cmd]) => !text.includes(cmd)).map(([method]) => method);
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const [page] = process.argv.slice(2);
  const missing = missingInstallMethods(readFileSync(page, 'utf8'));
  if (missing.length) { console.error(`${page} has no command for: ${missing.join(', ')}`); process.exit(1); }
  console.log(`${page} shows every install method`);
}
