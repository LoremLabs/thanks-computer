// The server entry: Nitro's app as a Fetch handler, in the Web ABI's server
// shape, `export default { fetch(request, ctx) }` with ctx = { client: { ip } }.
// Nitro bundles this file into server/index.mjs; the imports below are its
// own, resolved at bundle time.
import "#nitro-internal-pollyfills";
import { useNitroApp } from "nitropack/runtime";

interface Context {
  client?: { ip?: string };
}

const nitroApp = useNitroApp();

export default {
  async fetch(request: Request, ctx?: Context): Promise<Response> {
    const url = new URL(request.url);
    const method = request.method.toUpperCase();
    const body = method === "GET" || method === "HEAD" ? undefined : Buffer.from(await request.arrayBuffer());
    return nitroApp.localFetch(url.pathname + url.search, {
      host: url.hostname,
      protocol: url.protocol,
      method,
      headers: request.headers,
      body,
      // Nitro spreads _platform into event.context; h3's getRequestIP reads
      // context.clientAddress first.
      context: {
        _platform: { clientAddress: ctx?.client?.ip, thanksComputer: { client: ctx?.client } },
      },
    } as Parameters<typeof nitroApp.localFetch>[1]);
  },
};
