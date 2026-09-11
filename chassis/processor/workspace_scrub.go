package processor

import (
	"bytes"

	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// scrubMinSecretLen is the shortest secret value the scrubber will replace.
// A workspace command's output is free text, so scrubbing is by VALUE — and
// a very short value ("1234", "ok") would shred unrelated output wherever it
// happened to occur. Real tokens are far longer; a shorter one is not
// scrubbed and the trace will show it. Document it, don't guess at it.
const scrubMinSecretLen = 8

// scrubRedacted is what a materialized secret's cleartext becomes in
// stdout, stderr and error text.
var scrubRedacted = []byte("[REDACTED]")

// scrubSecrets replaces every occurrence of every materialized secret's
// cleartext in out with [REDACTED]. Runs before the payload is built, so
// the envelope, the trace step, the continuation store and the logs all
// see the redacted form — nothing downstream ever handles the cleartext.
// Values shorter than scrubMinSecretLen are left alone (see above).
//
// A `format`-templated value ("Bearer {}") still contains the raw
// cleartext as a substring, so scrubbing the raw value covers it.
func scrubSecrets(out []byte, bag secrets.SecretBag) []byte {
	if len(out) == 0 || bag.Len() == 0 {
		return out
	}
	for _, name := range bag.Names() {
		v, ok := bag.Get(name)
		if !ok || len(v) < scrubMinSecretLen {
			continue
		}
		out = bytes.ReplaceAll(out, v, scrubRedacted)
	}
	return out
}
