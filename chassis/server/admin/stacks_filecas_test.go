package admin

import "testing"

func TestValidateStackFilePathFILES(t *testing.T) {
	valid := []string{
		// Tenant static assets — any extension under FILES/.
		"FILES/index.html",
		"FILES/assets/app.css",
		"FILES/_mail/welcome.html", // private (served filter is separate)
		"FILES/logo.png",
		"FILES/data.bin",
		// Existing rule/fixture shapes still pass.
		"100/route.txcl",
		"100/mock-request.json",
	}
	for _, p := range valid {
		if err := validateStackFilePath(p); err != nil {
			t.Errorf("validateStackFilePath(%q) = %v, want nil", p, err)
		}
	}

	invalid := []string{
		"logo.png",        // non-FILES, unsupported extension
		"image.jpg",       // non-FILES, unsupported extension
		"FILES",           // bare (no subpath) → not FILES/, unsupported
		"FILES/../secret", // traversal → not normalized
		"/FILES/x.html",   // absolute
		"100/random.json", // .json must be a mock-* fixture
	}
	for _, p := range invalid {
		if err := validateStackFilePath(p); err == nil {
			t.Errorf("validateStackFilePath(%q) = nil, want error", p)
		}
	}
}

func TestValidateStackFilePathStoreSeed(t *testing.T) {
	valid := []string{
		"VECTORS/books.jsonl",
		"VECTORS/rag-memory.jsonl",
		"KV/config.jsonl",
		"KV/seed.jsonl",
	}
	for _, p := range valid {
		if err := validateStackFilePath(p); err != nil {
			t.Errorf("validateStackFilePath(%q) = %v, want nil", p, err)
		}
	}

	invalid := []string{
		"VECTORS/books.json",         // wrong extension (.json, not .jsonl)
		"VECTORS/nested/books.jsonl", // packs may not nest — name must be one segment
		"VECTORS",                    // bare dir, no pack
		"VECTORS/",                   // empty name
		"KV/a/b.jsonl",               // nested
		"KV/data.txt",                // wrong extension
		"vectors/books.jsonl",        // case-sensitive: lower-case dir is not the channel
	}
	for _, p := range invalid {
		if err := validateStackFilePath(p); err == nil {
			t.Errorf("validateStackFilePath(%q) = nil, want error", p)
		}
	}
}
