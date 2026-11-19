// The Evidence page's SHA-256 fallback (no crypto.subtle over plain HTTP) against node's own.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createHash, randomBytes } from "node:crypto";
import { sha256Hex } from "../src/lib/sha256.ts";

test("sha256Hex equals node's SHA-256 at the padding boundaries and for a larger input", () => {
  assert.equal(sha256Hex(new Uint8Array()), "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855");
  assert.equal(sha256Hex(new TextEncoder().encode("abc")), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
  for (const n of [1, 55, 56, 63, 64, 65, 119, 120, 1000, 1 << 20]) {
    const b = randomBytes(n);
    assert.equal(sha256Hex(new Uint8Array(b)), createHash("sha256").update(b).digest("hex"), `length ${n}`);
  }
});
