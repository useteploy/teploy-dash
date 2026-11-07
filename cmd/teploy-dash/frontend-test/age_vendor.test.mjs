// The vendored browser crypto, on what Dash really runs on: plain HTTP, where
// crypto.subtle is undefined. Fixtures come from the native age CLI, so this also
// proves the two agree. testdata/age/test-key.txt exists only for this test.
import { test, before } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const fixture = name => new Uint8Array(readFileSync(join(here, 'testdata/age', name)));
const keyFile = readFileSync(join(here, 'testdata/age/test-key.txt'), 'utf8');
let age;

before(() => {
  Object.defineProperty(globalThis.crypto, 'subtle', { value: undefined, configurable: true });
  assert.equal(globalThis.crypto.subtle, undefined);
  new Function(readFileSync(join(here, '../frontend/js/age.vendor.js'), 'utf8'))();
  age = globalThis.TeployAge;
});

test('generates a key and derives the same public half without crypto.subtle', async () => {
  const { identity, publicKey } = await age.generate();
  assert.match(identity, /^AGE-SECRET-KEY-1[0-9A-Z]+$/);
  assert.match(publicKey, /^age1[0-9a-z]{58}$/);
  assert.equal(await age.derivePublic(identity), publicKey);
});

test('the file parser skips comment lines and reads the notes', () => {
  const text = '# Teploy backup recovery key.\n\n# bucket: my-bucket\n# endpoint: https://s3.us-west-004.backblazeb2.com\n# base: teploy\nAGE-SECRET-KEY-1ABC\n';
  const parsed = age.parseIdentityFile(text);
  assert.equal(parsed.identity, 'AGE-SECRET-KEY-1ABC');
  assert.equal(parsed.notes.bucket, 'my-bucket');
  assert.equal(parsed.notes.base, 'teploy');
  assert.equal(age.parseIdentityFile('\r\n# c\r\nAGE-SECRET-KEY-1ABC\r\n').identity, 'AGE-SECRET-KEY-1ABC');
});

test('the file parser refuses what is not one key', () => {
  assert.throws(() => age.parseIdentityFile('# only comments\n'), /no recovery key/);
  assert.throws(() => age.parseIdentityFile('AGE-SECRET-KEY-1ABC\nAGE-SECRET-KEY-1DEF\n'), /more than one/);
  assert.throws(() => age.parseIdentityFile('hello\n'), /does not look like/);
});

test('reads a real age-keygen file and matches its public key', async () => {
  const { identity } = age.parseIdentityFile(keyFile);
  const expected = /public key: (age1[0-9a-z]+)/.exec(keyFile)[1];
  assert.equal(await age.derivePublic(identity), expected);
});

test('decrypts a whole file from the native age CLI', async () => {
  const { identity } = age.parseIdentityFile(keyFile);
  const out = await age.decryptStart(identity, fixture('whole.age'));
  assert.equal(out.length, 'hello backups\n'.length);
  assert.equal(out.truncated, false);
});

test('a truncated prefix gives its first chunk: 65536 bytes is the proof', async () => {
  const { identity } = age.parseIdentityFile(keyFile);
  const out = await age.decryptStart(identity, fixture('prefix-70k.age'));
  assert.equal(out.length, 65536);
  assert.equal(out.truncated, true);
});

test('a wrong key is refused before any byte comes out', async () => {
  const { identity } = await age.generate();
  await assert.rejects(() => age.decryptStart(identity, fixture('prefix-70k.age')));
  await assert.rejects(() => age.decryptStart(identity, fixture('whole.age')));
});

test('a prefix cut before the first chunk ends proves nothing', async () => {
  const { identity } = age.parseIdentityFile(keyFile);
  await assert.rejects(() => age.decryptStart(identity, fixture('prefix-70k.age').slice(0, 1000)));
});
