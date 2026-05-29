package client

// This file used to host the inline RFC 9421 signing path that called
// directly into yaronf/httpsign. The pluggable-signing-backends work
// moved both the canonicalizer and the dispatch into
// chassis/cli/signer so the same code paths serve raw-ed25519,
// ssh-agent, and future hardware backends.
//
// The `Auth` type that used to live here is gone; `Target.Auth` now
// holds a `signer.Signer` (the interface) instead of a struct.
// applyAuth in admin.go just delegates to `target.Auth.Sign(req, body)`.
