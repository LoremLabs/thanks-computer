// Package connection is a principal's identity at an outside service: Alice's
// GitHub account, a pony's own machine account, an app installation acting
// for a team. The chassis holds the credential and renews it; a stack names
// the connection and never sees a token (docs/todo-connections.md in the
// service overlay).
//
// Two declarations take part, both small YAML files a stack deploys, parsed
// at apply and activate like SANDBOXES/ and CAPS/:
//
//	OPS/<stack>/PROVIDERS/<name>.yaml     how authentication at the service
//	                                      works, where each credential may go,
//	                                      and which stacks may use it (one
//	                                      active stack per name in a tenant)
//	OPS/<stack>/CONNECTIONS/<provider>.yaml  how THIS stack uses an identity:
//	                                      the origins it calls, the header the
//	                                      token goes on
//
// A connection's identity is (tenant, principal, provider, label), never a
// stack: moving a provider's file to another stack changes no row.
//
// This file is the leaf layer: the reserved-path vocabulary, with no
// dependency on stores, so the CLI, the admin producer and the control-event
// applier can import it cheaply.
package connection

import (
	"regexp"
	"strings"
)

// Reserved top-level directories and the declaration extension.
const (
	ProviderDir = "PROVIDERS"
	ConsumerDir = "CONNECTIONS"
	DeclExt     = ".yaml"
)

// nameRe pins provider names: they are a connection's identity, appear in a
// WITH clause (`connection = "github"`), on a callback path and in error
// payloads, so lowercase identifiers only.
var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// ValidName reports whether s may name a provider.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// IsProviderPath reports whether a stack_files path lives under PROVIDERS/.
func IsProviderPath(p string) bool { return strings.HasPrefix(p, ProviderDir+"/") }

// IsConsumerPath reports whether a stack_files path lives under CONNECTIONS/.
func IsConsumerPath(p string) bool { return strings.HasPrefix(p, ConsumerDir+"/") }

// ProviderName returns the provider a declaration path belongs to:
// "PROVIDERS/github.yaml" → "github"; "" when the path is not a declaration
// (nested, wrong extension, bad name) — the shape validateStackFilePath
// enforces at upload.
func ProviderName(p string) string { return declName(ProviderDir, p) }

// ConsumerName returns the provider a consumer declaration names:
// "CONNECTIONS/github.yaml" → "github"; "" when it is not a declaration.
func ConsumerName(p string) string { return declName(ConsumerDir, p) }

func declName(dir, p string) string {
	if !strings.HasPrefix(p, dir+"/") {
		return ""
	}
	rest := p[len(dir)+1:]
	if rest == "" || strings.Contains(rest, "/") || !strings.HasSuffix(rest, DeclExt) {
		return ""
	}
	name := strings.TrimSuffix(rest, DeclExt)
	if !ValidName(name) {
		return ""
	}
	return name
}

// ProviderPath is the stack_files path of a provider's declaration.
func ProviderPath(name string) string { return ProviderDir + "/" + name + DeclExt }

// ConsumerPath is the stack_files path of a stack's consumer declaration for
// a provider.
func ConsumerPath(provider string) string { return ConsumerDir + "/" + provider + DeclExt }
