import {test} from 'node:test';
import assert from 'node:assert/strict';
import {mkdtempSync, writeFileSync, readFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {writeVercelOutput} from './vercel-output.mjs';
import {API_HOST} from '../src/analytics.mjs';

function csp() {
  const b = mkdtempSync(join(tmpdir(), 'b-')); writeFileSync(join(b, 'index.html'), 'x');
  const o = join(mkdtempSync(join(tmpdir(), 'o-')), 'output');
  writeVercelOutput(b, o);
  const c = JSON.parse(readFileSync(join(o, 'config.json'), 'utf8'));
  const value = c.routes.find(r => r.headers && r.headers['Content-Security-Policy']).headers['Content-Security-Policy'];
  return Object.fromEntries(value.split('; ').map(d => { const [k, ...v] = d.split(' '); return [k, v]; }));
}

test('CSP connect-src allows the PostHog hosts and still allows the site itself', () => {
  const connect = csp()['connect-src'];
  const assets = API_HOST.replace('://us.', '://us-assets.');   // posthog-js reads its remote config from here
  for (const origin of [API_HOST, assets]) assert.ok(connect.includes(origin), `connect-src has ${origin}`);
  assert.ok(connect.includes("'self'"));
});

test('CSP script-src gets no new origins (posthog-js loads no remote scripts)', () => {
  assert.deepEqual(csp()['script-src'], ["'self'", "'unsafe-inline'"]);
});

test('CSP font-src allows data: URLs (the built CSS inlines @fontsource fonts)', () => {
  const font = csp()['font-src'];
  assert.ok(font.includes("'self'"));
  assert.ok(font.includes('data:'));
});
