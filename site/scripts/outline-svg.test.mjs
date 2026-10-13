import test from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {fileURLToPath} from 'node:url';
import {outline} from './outline-svg.mjs';

const brand = fileURLToPath(new URL('../../docs/assets/brand/', import.meta.url));
const NAMES = ['ambit-why', 'ambit-why-dark', 'ambit-architecture', 'ambit-architecture-dark'];
const attr = (svg, re) => svg.match(re)?.[1];

for (const name of NAMES) {
  const source = readFileSync(`${brand}src/${name}.svg`, 'utf8');
  const built = readFileSync(`${brand}${name}.svg`, 'utf8');

  test(`${name}: outlined file has no <text> and keeps viewBox, role, aria-label and title`, () => {
    assert.ok(/<text[\s>]/.test(source), 'the source has text');
    assert.ok(!/<text[\s>]/.test(built), 'the outlined file has no <text>');
    for (const re of [/viewBox="([^"]+)"/, /role="([^"]+)"/, /aria-label="([^"]+)"/, /<title>([^<]+)<\/title>/]) {
      assert.ok(attr(source, re), `source has ${re}`);
      assert.equal(attr(built, re), attr(source, re));
    }
  });

  test(`${name}: outlined file is current (run npm run outline-svg)`, () => {
    assert.equal(built, outline(source));
  });

  test(`${name}: no broken coordinates`, () => {
    assert.doesNotMatch(built, /NaN|undefined/);
  });
}
