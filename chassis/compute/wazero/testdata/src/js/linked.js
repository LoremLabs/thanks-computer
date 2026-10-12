// linked.js — a dynamically linked JavaScript fixture: only this script's
// bytecode, importing the QuickJS engine from the vendored plugin
// (chassis/compute/javyplugin), the shape `txco` builds every op:// into.
// It counts its runs in a global and reports the count, so a reused runtime
// that let state through from an earlier call would answer runs > 1.
// {"spin": true} loops forever (the wall limit); {"throw": true} throws (a
// guest error). Compiled with:
// javy build linked.js -C dynamic -C plugin=../../../../javyplugin/plugin.wasm -C source=omitted -o linked.js.wasm
function readInput() {
  const chunks = [];
  let total = 0;
  const size = 1024;
  while (true) {
    const buf = new Uint8Array(size);
    const n = Javy.IO.readSync(0, buf);
    if (n === 0) break;
    chunks.push(buf.subarray(0, n));
    total += n;
  }
  const all = new Uint8Array(total);
  let off = 0;
  for (const c of chunks) { all.set(c, off); off += c.length; }
  if (total === 0) return {};
  return JSON.parse(new TextDecoder().decode(all));
}

function writeOutput(obj) {
  const bytes = new TextEncoder().encode(JSON.stringify(obj));
  Javy.IO.writeSync(1, new Uint8Array(bytes));
}

globalThis.runs = (globalThis.runs || 0) + 1;
const input = readInput();
if (input.throw) throw new Error("asked to throw");
if (input.spin) { for (;;) {} }
input.runs = globalThis.runs;
input.linked = true;
writeOutput(input);
