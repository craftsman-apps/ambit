import {existsSync, readFileSync} from 'node:fs';
import {join} from 'node:path';
import {fileURLToPath} from 'node:url';

export function missingAddresses(readmeText, buildDir, allowMissing = []) {
  // "/docs/" and "/docs" are the same page (trailingSlash: false); every other address loses a trailing slash too.
  const found = [...readmeText.matchAll(/https:\/\/ambitproject\.dev(\/docs[^\s)"'#]*)/g)].map(m => m[1].replace(/\/+$/, ''));
  if (found.length === 0) throw new Error('README has no ambitproject.dev/docs address');
  const paths = [...new Set(found)];
  return paths.filter(p => !allowMissing.includes(p) &&
    !(existsSync(join(buildDir, p + '.html')) || existsSync(join(buildDir, p, 'index.html'))));
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const [readme, buildDir, allow = ''] = process.argv.slice(2);
  const missing = missingAddresses(readFileSync(readme, 'utf8'), buildDir, allow.split(',').filter(Boolean));
  if (missing.length) { console.error('README addresses with no page:\n' + missing.join('\n')); process.exit(1); }
  console.log('all README docs addresses have a page');
}
