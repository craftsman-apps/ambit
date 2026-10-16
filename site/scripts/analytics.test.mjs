import {test} from 'node:test';
import assert from 'node:assert/strict';
import {isProduction} from '../src/analytics.mjs';

test('only the production host sends events', () => {
  assert.equal(isProduction('ambitproject.dev'), true);
  for (const h of ['localhost', '127.0.0.1', 'ambit-git-main-craftsman.vercel.app', 'www.ambitproject.dev', 'ambitproject.dev.evil.example', '']) {
    assert.equal(isProduction(h), false, h);
  }
});
