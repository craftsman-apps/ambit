import {test} from 'node:test';
import assert from 'node:assert/strict';
import {INSTALL_COMMANDS, missingInstallMethods} from './check-install-methods.mjs';

// One code block the way Docusaurus renders it: the command split into highlighted spans.
const block = cmd => `<pre class="prism-code"><code><div class="token-line">` +
  cmd.split(' ').map(w => `<span class="token plain">${w.replace(/&/g, '&amp;').replace(/</g, '&lt;')}</span>`).join(' ') +
  `</div></code></pre>`;
const page = cmds => `<html><body><h2>1. Install the CLI</h2>${cmds.map(block).join('<p>or</p>')}</body></html>`;

test('a page with every install command passes, also when highlighting splits the commands', () => {
  const cmds = Object.values(INSTALL_COMMANDS).map(c => c.endsWith('@') ? c + 'v0.2.0' : c);
  assert.deepEqual(missingInstallMethods(page(cmds)), []);
});

test('each missing install method is named', () => {
  const without = Object.entries(INSTALL_COMMANDS).filter(([m]) => m !== 'PowerShell' && m !== 'npm').map(([, c]) => c);
  assert.deepEqual(missingInstallMethods(page(without)), ['PowerShell', 'npm']);
});

test('the methods are the five release-1 channels that install the CLI', () => {
  assert.deepEqual(Object.keys(INSTALL_COMMANDS), ['Homebrew', 'install script', 'PowerShell', 'npm', 'go install']);
});
