import { op } from "@txco/op";

// Triage classifier (bundled compute). Reads the inbound message body and
// merges a coarse category + urgency into the envelope. Runs sandboxed: no
// filesystem, network, or ambient env. This is a deliberately simple
// keyword classifier — replace with a real model call via an external op if
// you need one.
export default op(({ input }) => {
  const body = String(input?._txc?.web?.req?.body ?? "").toLowerCase();
  const urgent = /can.?t|cannot|down|broken|urgent|asap|locked? ?out/.test(body);
  input.category = /log ?in|password|auth/.test(body) ? "auth" : "general";
  input.urgency = urgent ? "high" : "normal";
  return input;
});
