package secrets

import (
	"fmt"

	"github.com/tidwall/gjson"
)

// Ref is one parsed entry from `op.Meta.secrets`. Each Ref binds an
// outbound path (where the cleartext belongs) to the NAME of the
// secret to materialize and an optional format template.
//
// Example: a txcl rule with
//
//	WITH secrets.headers.authorization.secret = "STRIPE_API_KEY",
//	     secrets.headers.authorization.format = "Bearer {}"
//
// produces a single Ref:
//
//	Ref{Path: "headers.authorization", Secret: "STRIPE_API_KEY", Format: "Bearer {}"}
type Ref struct {
	// Path is the dotted path on the outbound request where the
	// (formatted) cleartext should be applied. Relative to the op's
	// outbound message — e.g. `headers.authorization` for an HTTP
	// header, `body.api_key` for a JSON body field.
	Path string
	// Secret is the NAME of the secret to look up in op.Secrets
	// (which is populated by the processor splice from
	// op.Meta.secrets's leaf NAMEs).
	Secret string
	// Format is the optional `{}`-template (see format.go); empty
	// string means "raw substitution — the cleartext is the value".
	Format string
}

// ParseRefs walks the `secrets` subtree of op.Meta (a JSON-string
// produced by the WITH-clause decoration pipeline) and returns one
// Ref per leaf path. Leaves are objects of the shape
//
//	{ "secret": "NAME", "format": "..{}.." }   // .format optional
//
// The walker descends through arbitrary nesting, treating any node
// that itself has a string `secret` field as a leaf (so
// `secrets.body.nested.api_key.{secret,format}` works the same as
// `secrets.headers.x.{secret,format}`).
//
// Returns (nil, nil) for an op with no `secrets` declaration —
// callers must NOT treat that as an error; it just means "no
// secrets to materialize for this op".
//
// Returns an error if any leaf is malformed: missing `secret`,
// wrong type, or a `format` template that fails ValidateFormat.
func ParseRefs(meta string) ([]Ref, error) {
	root := gjson.Get(meta, "secrets")
	if !root.Exists() {
		return nil, nil
	}
	if !root.IsObject() {
		return nil, fmt.Errorf("secrets: op.Meta.secrets must be an object, got %s", root.Type)
	}
	var out []Ref
	if err := walkRefs("", root, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// walkRefs recursively descends `node`, accumulating Refs at each
// leaf. A leaf is a node that has a string `secret` child; we treat
// it as opaque from there (siblings other than `secret`/`format` are
// reserved for future extension — we ignore unknown keys at the leaf
// rather than error, so adding new optional fields stays backward-
// compatible).
func walkRefs(pathPrefix string, node gjson.Result, out *[]Ref) error {
	if !node.IsObject() {
		// Stray scalar inside the tree — invalid shape. The operator
		// likely typed `secrets.X = "STRIPE_KEY"` (string) instead of
		// `secrets.X.secret = "STRIPE_KEY"`.
		return fmt.Errorf("secrets: bare value at %q (expected object with `secret`)", pathPrefix)
	}

	// A node is a leaf when it has a string `secret` child.
	if secretField := node.Get("secret"); secretField.Exists() {
		if pathPrefix == "" {
			// `secrets.secret = "X"` at the root — no path to apply
			// the cleartext to. Reject explicitly so the operator
			// gets a useful error instead of a silent no-op.
			return fmt.Errorf("secrets: leaf at the root of `secrets` has no path (must be e.g. `secrets.headers.x.secret = ...`)")
		}
		if secretField.Type != gjson.String {
			return fmt.Errorf("secrets: %q.secret must be a string, got %s", pathPrefix, secretField.Type)
		}
		name := secretField.String()
		if name == "" {
			return fmt.Errorf("secrets: %q.secret is empty", pathPrefix)
		}
		ref := Ref{Path: pathPrefix, Secret: name}

		if formatField := node.Get("format"); formatField.Exists() {
			if formatField.Type != gjson.String {
				return fmt.Errorf("secrets: %q.format must be a string, got %s", pathPrefix, formatField.Type)
			}
			ref.Format = formatField.String()
			if err := ValidateFormat(ref.Format); err != nil {
				return fmt.Errorf("secrets: %q.format: %w", pathPrefix, err)
			}
		}

		*out = append(*out, ref)
		return nil
	}

	// Not a leaf — descend.
	var iterErr error
	node.ForEach(func(key, value gjson.Result) bool {
		k := key.String()
		// Reserved leaf-only keys: if they appear here it's a malformed
		// declaration (e.g. `secrets.format = "Bearer {}"` at the top
		// level, no path attached).
		if k == "secret" || k == "format" {
			iterErr = fmt.Errorf("secrets: top-level %q key at %q (must be inside an object with `secret`)", k, pathPrefix)
			return false
		}
		child := pathPrefix
		if child == "" {
			child = k
		} else {
			child = child + "." + k
		}
		if err := walkRefs(child, value, out); err != nil {
			iterErr = err
			return false
		}
		return true
	})
	return iterErr
}

// DistinctNames returns the unique NAMEs across a list of Refs in
// stable order. Useful for the processor splice: materialize each
// distinct NAME once, even if the same name appears in multiple
// refs (e.g. the same key used in both a header and a body field).
func DistinctNames(refs []Ref) []string {
	if len(refs) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(refs))
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		if !seen[r.Secret] {
			seen[r.Secret] = true
			out = append(out, r.Secret)
		}
	}
	return out
}

// HasRefs reports whether op.Meta has any `secrets` declaration at
// all (without parsing it). Cheap pre-check for the processor
// splice's fast path.
func HasRefs(meta string) bool {
	if meta == "" {
		return false
	}
	return gjson.Get(meta, "secrets").Exists()
}

