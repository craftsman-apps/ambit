import {test} from 'node:test';
import assert from 'node:assert/strict';
import {mkdtempSync, mkdirSync, writeFileSync, readFileSync, existsSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {writeVercelOutput} from './vercel-output.mjs';

test('clean URLs: every .html file is served without its extension', () => {
  const b = mkdtempSync(join(tmpdir(), 'b-')); mkdirSync(join(b, 'docs', 'guides'), {recursive: true});
  for (const f of ['index.html', '404.html', 'docs.html', 'docs/quick-start.html', 'docs/guides/mcp-apps.html'])
    writeFileSync(join(b, f), 'x');
  const o = join(mkdtempSync(join(tmpdir(), 'o-')), 'output');
  writeVercelOutput(b, o);
  const c = JSON.parse(readFileSync(join(o, 'config.json'), 'utf8'));
  assert.deepEqual(c.overrides['docs/quick-start.html'], {path: 'docs/quick-start'});
  assert.deepEqual(c.overrides['docs.html'], {path: 'docs'});
  assert.deepEqual(c.overrides['docs/guides/mcp-apps.html'], {path: 'docs/guides/mcp-apps'});
  assert.equal(c.overrides['index.html'], undefined, 'the home page keeps index.html');
  assert.ok('cleanUrls' in c === false && 'trailingSlash' in c === false, 'vercel.json-only keys are not used');
  const slash = c.routes.find(r => r.status === 308);
  assert.ok(slash && slash.headers.Location === '/$1', 'a trailing slash redirects to the address without it');
});

test('copies the build and writes config v3 with headers', () => {
  const b = mkdtempSync(join(tmpdir(), 'b-')); mkdirSync(join(b, 'docs'));
  writeFileSync(join(b, 'index.html'), 'home'); writeFileSync(join(b, 'docs', 'providers.html'), 'p');
  const o = join(mkdtempSync(join(tmpdir(), 'o-')), 'output');
  writeVercelOutput(b, o);
  assert.ok(existsSync(join(o, 'static', 'docs', 'providers.html')));
  const c = JSON.parse(readFileSync(join(o, 'config.json'), 'utf8'));
  assert.equal(c.version, 3);
  const all = JSON.stringify(c.routes);
  for (const h of ['Content-Security-Policy', 'Strict-Transport-Security', 'X-Content-Type-Options',
                   'Referrer-Policy', 'Permissions-Policy']) assert.ok(all.includes(h), h);
  assert.ok(all.includes("default-src 'self'"));
  assert.ok(c.routes.some(r => r.handle === 'filesystem'), 'static files are served');
});
test('refuses an empty build directory', () => {
  const b = mkdtempSync(join(tmpdir(), 'b-'));
  assert.throws(() => writeVercelOutput(b, join(b, 'o')), /index.html/);
});
