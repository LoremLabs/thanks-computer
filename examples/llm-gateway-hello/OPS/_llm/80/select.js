import { op } from "@txco/op";

// Extract the latest user question and (in keyword mode) retrieve the
// most relevant decision-log entries. Sandboxed: no filesystem, network,
// or ambient env. The emitted `_txc.llm.context` items are the stack's
// POLICY ("inject these"); the gateway enforces budget/dedup/guard and
// does the Anthropic serialization (the MECHANISM).
export default op(({ input }) => {
  // Latest user message, string or block-array content flattened —
  // exactly the extraction txcl's path grammar can't express.
  const msgs = Array.isArray(input?.request?.messages) ? input.request.messages : [];
  let question = "";
  for (let i = msgs.length - 1; i >= 0; i--) {
    const m = msgs[i];
    if (!m || m.role !== "user") continue;
    const c = m.content;
    if (typeof c === "string") question = c;
    else if (Array.isArray(c))
      // Skip harness chatter (Claude Code wraps reminders in
      // <system-reminder> blocks alongside the typed question) so the
      // scorer sees the user's words, not the tool roster.
      question = c
        .filter(
          (b) =>
            b && b.type === "text" && typeof b.text === "string" &&
            !b.text.trimStart().startsWith("<system-reminder>"),
        )
        .map((b) => b.text)
        .join("\n");
    if (question) break;
  }

  // Delta-only output: never return the whole envelope (arrays append
  // on merge — request.messages would double).
  const out = { _question: question };

  const mode = input?._retrieval?.mode ?? "keyword";
  const log = Array.isArray(input?._decisions) ? input._decisions : [];
  if (mode === "vector" || !question || log.length === 0) return out;

  const words = new Set((question.toLowerCase().match(/[a-z0-9]+/g) ?? []).filter((w) => w.length > 2));
  const scored = log
    .map((e) => {
      const hay = (
        String(e?.title ?? "") + " " +
        (Array.isArray(e?.tags) ? e.tags.join(" ") : "") + " " +
        String(e?.body ?? "")
      ).toLowerCase();
      let score = 0;
      for (const w of words) if (hay.includes(w)) score++;
      return { e, score };
    })
    .filter((s) => s.score > 0)
    .sort((a, b) => b.score - a.score)
    .slice(0, 2);

  if (scored.length) {
    out._txc = {
      llm: {
        context: scored.map(({ e }) => ({
          source: "kv:decisions/" + String(e?.id ?? "unknown"),
          title: String(e?.title ?? ""),
          content: String(e?.body ?? ""),
        })),
      },
    };
  }
  return out;
});
