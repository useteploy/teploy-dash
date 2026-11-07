// Bundled into cmd/teploy-dash/frontend/js/age.vendor.js by build.sh. Exposes
// window.TeployAge: the only browser crypto Dash has. The private recovery key is
// created and used here and never leaves the browser.
//
// Dash runs on plain HTTP in many installs, where crypto.subtle is undefined and
// age-encryption would otherwise crash with a TypeError. forceWebCryptoOff makes it
// use its bundled pure-JS X25519. The package's exports expose only dist/index.js,
// hence the relative path through node_modules.
import { Decrypter, generateIdentity, identityToRecipient } from 'age-encryption';
import { forceWebCryptoOff } from './node_modules/age-encryption/dist/x25519.js';

forceWebCryptoOff(true);

const IDENTITY_RE = /^AGE-SECRET-KEY-1[0-9A-Z]+$/;

// parseIdentityFile reads a saved key file: comment lines (#) and blank lines are
// skipped, and exactly one identity is expected. Comment lines "# name: value" are
// returned as notes (bucket, endpoint and base live there).
function parseIdentityFile(text) {
  const notes = {};
  const identities = [];
  for (const raw of String(text).split(/\r?\n/)) {
    const line = raw.trim();
    if (line === '') continue;
    if (line.startsWith('#')) {
      const m = /^#\s*([A-Za-z ]+):\s*(.+)$/.exec(line);
      if (m) notes[m[1].trim().toLowerCase()] = m[2].trim();
      continue;
    }
    if (!IDENTITY_RE.test(line)) throw new Error('This file does not look like a recovery key.');
    identities.push(line);
  }
  if (identities.length !== 1) {
    throw new Error(identities.length === 0 ? 'This file holds no recovery key.' : 'This file holds more than one key.');
  }
  return { identity: identities[0], notes };
}

async function generate() {
  const identity = await generateIdentity();
  return { identity, publicKey: await identityToRecipient(identity) };
}

async function derivePublic(identity) {
  return identityToRecipient(identity);
}

// decryptStart decrypts the start of an age file, which may be cut off. age works in
// 64 KiB chunks, each with its own tag, so a truncated prefix yields its whole chunks
// and then an "invalid tag" error: the first chunk is the proof. A wrong key fails
// before any byte comes out.
async function decryptStart(identity, bytes) {
  const d = new Decrypter();
  d.addIdentity(identity);
  const input = new ReadableStream({
    start(c) { c.enqueue(bytes); c.close(); },
  });
  const reader = (await d.decrypt(input)).getReader();
  let length = 0;
  let truncated = false;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      length += value.length;
    }
  } catch (err) {
    if (length === 0) throw err;
    truncated = true;
  }
  if (length === 0) throw new Error('Decryption produced nothing.');
  return { length, truncated };
}

globalThis.TeployAge = { generate, derivePublic, decryptStart, parseIdentityFile };
