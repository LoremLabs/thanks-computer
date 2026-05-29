// transform.js — a JavaScript compute fixture for the wazero engine.
// ABI: read the whole JSON envelope from stdin (fd 0), set computed:true,
// write the JSON result to stdout (fd 1). Uses Javy's host IO. Compiled to a
// self-contained WASI module with: javy build transform.js -o transform.js.wasm
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

const input = readInput();
input.computed = true;
input.lang = "js";
writeOutput(input);
