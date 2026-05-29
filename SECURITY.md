# Security Policy

## Reporting a vulnerability

**Please do not open a public issue for security vulnerabilities.**

Report privately through one of:

- **GitHub** — use *Security → Report a vulnerability* (Private Vulnerability
  Reporting) on this repository. <!-- enable: Settings → Code security → Private vulnerability reporting -->
- **Email** — security@loremlabs.dev <!-- TODO: confirm/replace this address before going public -->

Please include:

- a description of the issue and its impact,
- steps to reproduce (a proof of concept if possible),
- affected version / commit, and
- any suggested remediation.

We aim to acknowledge reports within a few business days and will keep you
updated on remediation progress. Please give us a reasonable opportunity to
fix the issue before any public disclosure.

## Supported versions

This project is pre-1.0; security fixes land on the latest tagged release.
There is no long-term-support branch yet.

## Scope notes

- `txco` is an event chassis that executes operator-authored rules (txcl) and
  dispatches to operator-configured HTTP endpoints. Misconfiguration that
  causes the chassis to call attacker-controlled endpoints is an operator
  concern; SSRF defenses for outbound calls live in `chassis/egress/`.
- Secrets are envelope-encrypted at rest (`chassis/secrets/`); reports about
  key handling, decryption paths, or secret exposure in logs/traces are
  especially welcome.
