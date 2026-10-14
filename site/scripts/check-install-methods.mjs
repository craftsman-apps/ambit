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

// pageText is the text a reader sees: tags removed (code highlighting splits a command into spans) and the
// entities Docusaurus writes decoded.
export function pageText(html) {
  return html.replace(/<[^>]*>/g, '')
    .replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&quot;/g, '"').replace(/&#x27;|&#39;/g, "'").replace(/&amp;/g, '&');
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
