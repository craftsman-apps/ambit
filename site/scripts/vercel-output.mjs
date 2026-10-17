import {cpSync, existsSync, mkdirSync, readdirSync, writeFileSync} from 'node:fs';
import {join} from 'node:path';
import {fileURLToPath} from 'node:url';

const CSP = "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; font-src 'self' data:; connect-src 'self' https://us.i.posthog.com https://us-assets.i.posthog.com; frame-ancestors 'none'; base-uri 'self'";
const HEADERS = {
  'Content-Security-Policy': CSP,
  'Strict-Transport-Security': 'max-age=63072000; includeSubDomains; preload',
  'X-Content-Type-Options': 'nosniff',
  'Referrer-Policy': 'strict-origin-when-cross-origin',
  'Permissions-Policy': 'camera=(), microphone=(), geolocation=()',
};

// Build Output API v3 has no cleanUrls setting: each page file is given its address without ".html".
function cleanUrlOverrides(dir, base = '') {
  const out = {};
  for (const e of readdirSync(join(dir, base), {withFileTypes: true})) {
    const rel = base ? `${base}/${e.name}` : e.name;
    if (e.isDirectory()) Object.assign(out, cleanUrlOverrides(dir, rel));
    else if (rel.endsWith('.html') && rel !== 'index.html' && rel !== '404.html') out[rel] = {path: rel.slice(0, -5)};
  }
  return out;
}

// Two guide pages were merged into docs/guides/coding-tools; the old addresses go to the matching section.
const MOVED = {
  'codex-opencode-hermes': 'coding-tools#codex',
  'other-agents': 'coding-tools#other-agents',
};

export function writeVercelOutput(buildDir, outDir) {
  if (!existsSync(join(buildDir, 'index.html'))) throw new Error(`no index.html in ${buildDir}`);
  mkdirSync(outDir, {recursive: true});
  cpSync(buildDir, join(outDir, 'static'), {recursive: true});
  const config = {
    version: 3,
    routes: [
      ...Object.entries(MOVED).map(([from, to]) => ({src: `^/docs/guides/${from}/?$`, status: 308, headers: {Location: `/docs/guides/${to}`}})),
      {src: '^/(.+)/$', status: 308, headers: {Location: '/$1'}},
      {src: '/(.*)', headers: HEADERS, continue: true},
      {handle: 'filesystem'},
      {src: '/(.*)', status: 404, dest: '/404.html'},
    ],
    overrides: cleanUrlOverrides(buildDir),
  };
  writeFileSync(join(outDir, 'config.json'), JSON.stringify(config, null, 2));
}

if (process.argv[1] === fileURLToPath(import.meta.url)) writeVercelOutput(process.argv[2], process.argv[3]);
