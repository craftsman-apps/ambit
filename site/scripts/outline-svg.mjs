// Turns every <text> in the diagram sources (docs/assets/brand/src/*.svg) into outlined paths and writes
// the result to docs/assets/brand/*.svg. Images cannot load web fonts, so outlined text is the only way
// the diagrams look the same in every browser and on GitHub. Run: npm run outline-svg
import {readFileSync, writeFileSync, readdirSync} from 'node:fs';
import {fileURLToPath} from 'node:url';
import {dirname, join} from 'node:path';
import opentype from 'opentype.js';

const here = dirname(fileURLToPath(import.meta.url));
const brand = join(here, '../../docs/assets/brand');
const fontFile = (pkg, name) => {
  const b = readFileSync(join(here, '..', 'node_modules/@fontsource', pkg, 'files', name));
  return opentype.parse(b.buffer.slice(b.byteOffset, b.byteOffset + b.byteLength));
};
const sans = fontFile('ibm-plex-sans', 'ibm-plex-sans-latin-400-normal.woff');
const sansBold = fontFile('ibm-plex-sans', 'ibm-plex-sans-latin-600-normal.woff');
const sansItalic = fontFile('ibm-plex-sans', 'ibm-plex-sans-latin-400-italic.woff');
const mono = fontFile('ibm-plex-mono', 'ibm-plex-mono-latin-400-normal.woff');

// The text classes of the diagram sources (see the <style> block of each source file).
const CLASSES = {
  t: {font: sansBold, size: 14, spacing: 0},
  s: {font: sans, size: 12, spacing: 0},
  m: {font: mono, size: 11, spacing: 0.06},
  l: {font: sansItalic, size: 11, spacing: 0},
  lx: {font: sansItalic, size: 11, spacing: 0},
};
// path.toPathData() of opentype.js 2.0.0 writes "NaN" for some coordinates, so the data is written here.
const num = (n) => String(Number(n.toFixed(2)));
const pathData = (path) => path.commands.map((c) => c.type + ['x1', 'y1', 'x2', 'y2', 'x', 'y'].filter((k) => k in c).map((k) => num(c[k])).join(' ')).join('');
// IBM Plex Sans in Fontsource has no glyph for these three characters, so they are drawn here, in em units
// (x to the right, y up from the baseline; stroke width .09 em).
const quad = ([x1, y1], [x2, y2], w = 0.09) => {
  const l = Math.hypot(x2 - x1, y2 - y1), nx = ((y2 - y1) / l) * (w / 2), ny = (-(x2 - x1) / l) * (w / 2);
  return [[x1 + nx, y1 + ny], [x2 + nx, y2 + ny], [x2 - nx, y2 - ny], [x1 - nx, y1 - ny]];
};
const SYMBOLS = {
  '✕': {width: 0.8, shapes: [quad([0.1, 0.1], [0.7, 0.7]), quad([0.1, 0.7], [0.7, 0.1])]},
  '✓': {width: 0.8, shapes: [quad([0.08, 0.34], [0.3, 0.12]), quad([0.3, 0.12], [0.72, 0.66])]},
  '→': {width: 1, shapes: [quad([0.05, 0.3], [0.8, 0.3]), [[0.95, 0.3], [0.68, 0.46], [0.68, 0.14]]]},
};
const symbolData = (ch, x, y, size) =>
  SYMBOLS[ch].shapes.map((pts) => 'M' + pts.map(([px, py]) => `${num(x + px * size)} ${num(y - py * size)}`).join('L')).join('');
const unescape = (s) => s.replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&quot;/g, '"').replace(/&amp;/g, '&');

export function outline(svg) {
  const out = svg.replace(/<text class="(\w+)" x="([\d.]+)" y="([\d.]+)">([^<]*)<\/text>/g, (_, cls, x, y, text) => {
    const c = CLASSES[cls];
    if (!c) throw new Error(`unknown text class "${cls}"`);
    // SVG collapses runs of spaces; the browser would draw the source text with single spaces.
    const parts = unescape(text).replace(/\s+/g, ' ').split(/([✕✓→])/).filter(Boolean);
    let px = Number(x), d = '';
    for (const part of parts) {
      if (part in SYMBOLS) {
        d += symbolData(part, px, Number(y), c.size);
        px += SYMBOLS[part].width * c.size;
      } else {
        const opts = {letterSpacing: c.spacing};
        d += pathData(c.font.getPath(part, px, Number(y), c.size, opts));
        px += c.font.getAdvanceWidth(part, c.size, opts);
      }
    }
    return `<path class="${cls} tx" d="${d}"/>`;
  });
  if (/<text[\s>]/.test(out)) throw new Error('a <text> element is not in the supported form');
  // The fill that the source set on `text` now applies to the paths.
  return out.replace(/\ntext\{font-family:[^;]*;fill:(#[0-9A-Fa-f]{6})\}/, '\n.tx{fill:$1}')
    .replace(/(\.m)\{font-family:[^;]*;/, '$1{').replace(/(\.[tslm]x?)\{font-size:[\d.]+px;(font-weight:\d+;)?(letter-spacing:[^;]*;)?(font-style:italic;?)?/g, '$1{')
    .replace(/\{;/g, '{').replace(/;\}/g, '}').replace(/\.\w+\{\}\n/g, '');
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  for (const f of readdirSync(join(brand, 'src')).filter((f) => f.endsWith('.svg'))) {
    writeFileSync(join(brand, f), outline(readFileSync(join(brand, 'src', f), 'utf8')));
    console.log(`outlined ${f}`);
  }
}
