// @txco/op/crypto — pure-JS crypto helpers. The sandbox has no host crypto, so
// these are self-contained implementations.
//
//   import { b64, sha256, hmac } from "@txco/op/crypto";

type Bytes = string | Uint8Array;

const _enc = new TextEncoder();

function toBytes(input: Bytes): Uint8Array {
  return typeof input === "string" ? _enc.encode(input) : input;
}

const HEX: string[] = [];
for (let i = 0; i < 256; i++) HEX.push((i < 16 ? "0" : "") + i.toString(16));

function toHex(bytes: Uint8Array): string {
  let s = "";
  for (let i = 0; i < bytes.length; i++) s += HEX[bytes[i]];
  return s;
}

// --- base64 ---

const B64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";

export const b64 = {
  encode(input: Bytes): string {
    const bytes = toBytes(input);
    let out = "";
    for (let i = 0; i < bytes.length; i += 3) {
      const a = bytes[i];
      const b = i + 1 < bytes.length ? bytes[i + 1] : 0;
      const c = i + 2 < bytes.length ? bytes[i + 2] : 0;
      const n = (a << 16) | (b << 8) | c;
      out += B64[(n >> 18) & 63] + B64[(n >> 12) & 63];
      out += i + 1 < bytes.length ? B64[(n >> 6) & 63] : "=";
      out += i + 2 < bytes.length ? B64[n & 63] : "=";
    }
    return out;
  },
  decode(s: string): Uint8Array {
    const clean = s.replace(/[^A-Za-z0-9+/]/g, "");
    const len = Math.floor((clean.length * 3) / 4);
    const out = new Uint8Array(len);
    let p = 0;
    for (let i = 0; i < clean.length; i += 4) {
      const a = B64.indexOf(clean[i]);
      const b = B64.indexOf(clean[i + 1]);
      const c = B64.indexOf(clean[i + 2]);
      const d = B64.indexOf(clean[i + 3]);
      const n = (a << 18) | (b << 12) | ((c & 63) << 6) | (d & 63);
      if (p < len) out[p++] = (n >> 16) & 0xff;
      if (c !== -1 && p < len) out[p++] = (n >> 8) & 0xff;
      if (d !== -1 && p < len) out[p++] = n & 0xff;
    }
    return out;
  },
};

// --- sha256 ---
//
// Tuned for QuickJS, which interprets op code: 32-bit integer math only
// (`| 0` throughout; an unsigned coercion mid-round drops QuickJS into
// doubles), rotations written inline, one reused Int32Array schedule,
// big-endian words read straight from the padded buffer, and hex from a lookup
// table. The earlier version (a data view per word, a helper call per rotation,
// unsigned coercions) cost ~1.7x more per byte, and an op that hashes a whole
// message body or a rendered prompt pays that out of its wall-clock budget.
// Verified against Go's crypto/sha256 and crypto/hmac in
// chassis/cli/op/crypto_sdk_test.go.

const K = new Int32Array([
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
]);

const W = new Int32Array(64);

function sha256Bytes(input: Bytes): Uint8Array {
  const msg = toBytes(input);
  const len = msg.length;
  // Padding: 0x80, zeros, then the bit length as a 64-bit big-endian integer.
  const total = Math.ceil((len + 9) / 64) * 64;
  const buf = new Uint8Array(total);
  buf.set(msg);
  buf[len] = 0x80;
  const hi = Math.floor(len / 0x20000000); // high 32 bits of len * 8
  const lo = (len * 8) >>> 0;              // low 32 bits of len * 8
  buf[total - 8] = hi >>> 24;
  buf[total - 7] = hi >>> 16;
  buf[total - 6] = hi >>> 8;
  buf[total - 5] = hi;
  buf[total - 4] = lo >>> 24;
  buf[total - 3] = lo >>> 16;
  buf[total - 2] = lo >>> 8;
  buf[total - 1] = lo;

  let h0 = 0x6a09e667 | 0, h1 = 0xbb67ae85 | 0, h2 = 0x3c6ef372 | 0, h3 = 0xa54ff53a | 0;
  let h4 = 0x510e527f | 0, h5 = 0x9b05688c | 0, h6 = 0x1f83d9ab | 0, h7 = 0x5be0cd19 | 0;

  for (let off = 0; off < total; off += 64) {
    for (let i = 0; i < 16; i++) {
      const j = off + (i << 2);
      W[i] = (buf[j] << 24) | (buf[j + 1] << 16) | (buf[j + 2] << 8) | buf[j + 3];
    }
    for (let i = 16; i < 64; i++) {
      const x = W[i - 15];
      const y = W[i - 2];
      const s0 = ((x >>> 7) | (x << 25)) ^ ((x >>> 18) | (x << 14)) ^ (x >>> 3);
      const s1 = ((y >>> 17) | (y << 15)) ^ ((y >>> 19) | (y << 13)) ^ (y >>> 10);
      W[i] = (W[i - 16] + s0 + W[i - 7] + s1) | 0;
    }
    let a = h0, b = h1, c = h2, d = h3, e = h4, f = h5, g = h6, h = h7;
    for (let i = 0; i < 64; i++) {
      const S1 = ((e >>> 6) | (e << 26)) ^ ((e >>> 11) | (e << 21)) ^ ((e >>> 25) | (e << 7));
      const t1 = (h + S1 + ((e & f) ^ (~e & g)) + K[i] + W[i]) | 0;
      const S0 = ((a >>> 2) | (a << 30)) ^ ((a >>> 13) | (a << 19)) ^ ((a >>> 22) | (a << 10));
      const t2 = (S0 + ((a & b) ^ (a & c) ^ (b & c))) | 0;
      h = g;
      g = f;
      f = e;
      e = (d + t1) | 0;
      d = c;
      c = b;
      b = a;
      a = (t1 + t2) | 0;
    }
    h0 = (h0 + a) | 0;
    h1 = (h1 + b) | 0;
    h2 = (h2 + c) | 0;
    h3 = (h3 + d) | 0;
    h4 = (h4 + e) | 0;
    h5 = (h5 + f) | 0;
    h6 = (h6 + g) | 0;
    h7 = (h7 + h) | 0;
  }

  const out = new Uint8Array(32);
  const hs = [h0, h1, h2, h3, h4, h5, h6, h7];
  for (let i = 0; i < 8; i++) {
    const v = hs[i];
    const j = i << 2;
    out[j] = v >>> 24;
    out[j + 1] = v >>> 16;
    out[j + 2] = v >>> 8;
    out[j + 3] = v;
  }
  return out;
}

/** SHA-256 of the input, returned as a lowercase hex string. */
export function sha256(input: Bytes): string {
  return toHex(sha256Bytes(input));
}

/** HMAC-SHA256(key, message), returned as a lowercase hex string. */
export function hmac(key: Bytes, message: Bytes): string {
  const blockSize = 64;
  let k = toBytes(key);
  if (k.length > blockSize) k = sha256Bytes(k);
  const padded = new Uint8Array(blockSize);
  padded.set(k);

  const ipad = new Uint8Array(blockSize);
  const opad = new Uint8Array(blockSize);
  for (let i = 0; i < blockSize; i++) {
    ipad[i] = padded[i] ^ 0x36;
    opad[i] = padded[i] ^ 0x5c;
  }

  const msg = toBytes(message);
  const inner = new Uint8Array(blockSize + msg.length);
  inner.set(ipad);
  inner.set(msg, blockSize);
  const innerHash = sha256Bytes(inner);

  const outer = new Uint8Array(blockSize + 32);
  outer.set(opad);
  outer.set(innerHash, blockSize);
  return toHex(sha256Bytes(outer));
}
