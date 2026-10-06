// A minimal Fetch handler: answers every request, sets two cookies.
export default {
  async fetch(request, ctx) {
    const url = new URL(request.url);
    const headers = new Headers({ "content-type": "text/plain; charset=utf-8" });
    headers.append("set-cookie", "a=1; Path=/");
    headers.append("set-cookie", "b=2; Path=/");
    if (request.method === "POST") {
      return new Response(`posted ${await request.text()} from ${ctx.client.ip}`, { status: 201, headers });
    }
    if (url.pathname === "/") return new Response("home", { headers });
    return new Response("not found", { status: 404, headers });
  },
};
