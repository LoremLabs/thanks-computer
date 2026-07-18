import { op } from "@txco/op";

// Vector-mode shaper: search hits → @llm.context items. Delta-only
// return (never the whole envelope — arrays append on merge).
export default op(({ input }) => {
  const hits = Array.isArray(input?._hits) ? input._hits : [];
  const items = hits
    .slice(0, 2)
    .map((h) => ({
      source: "vector:decisions/" + String(h?.id ?? "unknown"),
      title: String(h?.metadata?.title ?? ""),
      content: String(h?.text ?? ""),
    }))
    .filter((i) => i.content !== "");
  return items.length ? { _txc: { llm: { context: items } } } : {};
});
