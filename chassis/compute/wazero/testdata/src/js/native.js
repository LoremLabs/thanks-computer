// native.js — a dynamically linked fixture compiled against the CURRENT
// plugin (chassis/compute/javyplugin/txco-javy-plugin-v1.wasm), the shape
// `txco` builds every op:// into today. It exercises the plugin's natives:
// for input {"msg": "...", "key": "..."} it answers the SHA-256 of msg and
// HMAC-SHA256(key, msg) as hex, computed by __txco_native, plus what it saw
// of that global, so the engine test can compare against Go's crypto.
// Compiled with:
// javy build native.js -C dynamic -C plugin=../../../../javyplugin/txco-javy-plugin-v1.wasm -C source=omitted -o native.js.wasm
function readInput() {
  const chunks = [];
  let total = 0;
  while (true) {
    const buf = new Uint8Array(1024);
    const n = Javy.IO.readSync(0, buf);
    if (n === 0) break;
    chunks.push(buf.subarray(0, n));
    total += n;
  }
  const all = new Uint8Array(total);
  let off = 0;
  for (const c of chunks) { all.set(c, off); off += c.length; }
  return total === 0 ? {} : JSON.parse(new TextDecoder().decode(all));
}

function hex(bytes) {
  let s = "";
  for (let i = 0; i < bytes.length; i++) s += bytes[i].toString(16).padStart(2, "0");
  return s;
}

const input = readInput();
const enc = new TextEncoder();
const n = globalThis.__txco_native;
const msg = enc.encode(input.msg || "");
const key = enc.encode(input.key || "");
// A view into a larger buffer: the natives must honour offset and length.
const padded = new Uint8Array(msg.length + 7);
padded.set(msg, 3);
const view = padded.subarray(3, 3 + msg.length);
let detached = null;
try {
  const ab = new ArrayBuffer(8);
  const u8 = new Uint8Array(ab);
  ab.transfer ? ab.transfer() : null; // detach where supported
  n.sha256(u8);
} catch (e) {
  detached = String(e);
}
let notBytes = null;
try { n.sha256("a string"); } catch (e) { notBytes = String(e); }
const out = {
  native: typeof n,
  version: n && n.version,
  sha: hex(n.sha256(msg)),
  shaView: hex(n.sha256(view)),
  mac: hex(n.hmacSha256(key, msg)),
  notBytes,
  detached,
};
Javy.IO.writeSync(1, enc.encode(JSON.stringify(out)));
