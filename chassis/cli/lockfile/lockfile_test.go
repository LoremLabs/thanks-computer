package lockfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadMissing(t *testing.T) {
	f, err := Read(t.TempDir())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(f.Packages) != 0 {
		t.Errorf("expected empty, got %+v", f.Packages)
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	root := t.TempDir()
	prev := nowFn
	nowFn = func() string { return "2026-05-30T12:00:00Z" }
	defer func() { nowFn = prev }()

	f := &File{}
	f.Upsert(Entry{
		Ref: "dir:./x", Name: "support-basic", Version: "0.1.0",
		ExportedStack: "cse", InstalledAs: "support", Mode: "as-stack",
		ManifestHash: "abc", InstalledAt: Now(),
	})
	if err := Write(root, f); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, FileName)); err != nil {
		t.Errorf("lockfile not at root: %v", err)
	}

	got, err := Read(root)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got.Packages) != 1 {
		t.Fatalf("len=%d", len(got.Packages))
	}
	e := got.Packages[0]
	if e.Name != "support-basic" || e.ExportedStack != "cse" || e.InstalledAs != "support" || e.InstalledAt != "2026-05-30T12:00:00Z" {
		t.Errorf("roundtrip mismatch: %+v", e)
	}
}

func TestUpsertReplaceByStack(t *testing.T) {
	f := &File{}
	f.Upsert(Entry{Name: "a", Version: "1.0.0", InstalledAs: "support", Mode: "as-stack", ManifestHash: "h1"})
	f.Upsert(Entry{Name: "a", Version: "2.0.0", InstalledAs: "support", Mode: "as-stack", ManifestHash: "h2"})
	if len(f.Packages) != 1 {
		t.Fatalf("expected replace, got %d", len(f.Packages))
	}
	if f.Packages[0].Version != "2.0.0" || f.Packages[0].ManifestHash != "h2" {
		t.Errorf("replace failed: %+v", f.Packages[0])
	}
	f.Upsert(Entry{Name: "a", Version: "2.0.0", InstalledAs: "support2", Mode: "as-stack"})
	if len(f.Packages) != 2 {
		t.Fatalf("a distinct stack should add an entry, got %d", len(f.Packages))
	}
}

func TestFindStack(t *testing.T) {
	f := &File{}
	f.Upsert(Entry{Name: "a", Version: "1.0.0", InstalledAs: "support", Mode: "as-stack"})
	if f.FindStack("support") == nil {
		t.Error("expected to find support")
	}
	if f.FindStack("nope") != nil {
		t.Error("expected nil for nope")
	}
}

func TestVendorKeyDistinctFromStack(t *testing.T) {
	f := &File{}
	f.Upsert(Entry{Name: "a", Version: "1.0.0", Mode: "vendor-only"})
	f.Upsert(Entry{Name: "a", Version: "1.0.0", InstalledAs: "support", Mode: "as-stack"})
	if len(f.Packages) != 2 {
		t.Fatalf("vendor + stack should be 2 distinct entries, got %d", len(f.Packages))
	}
}

func TestRemove(t *testing.T) {
	f := &File{}
	f.Upsert(Entry{Name: "a", Version: "1.0.0", InstalledAs: "support", Mode: "as-stack"})
	f.Upsert(Entry{Name: "b", Version: "1.0.0", InstalledAs: "billing", Mode: "as-stack"})
	f.Upsert(Entry{Name: "a", Version: "1.0.0", Mode: "vendor-only"})

	if f.Remove("nope") {
		t.Error("Remove of an absent stack should report false")
	}
	if !f.Remove("support") {
		t.Error("Remove of an existing stack should report true")
	}
	if f.FindStack("support") != nil {
		t.Error("support should be gone after Remove")
	}
	if len(f.Packages) != 2 {
		t.Fatalf("expected 2 entries left (billing + vendor a), got %d", len(f.Packages))
	}
	// A vendor-only entry shares the name but has no InstalledAs — Remove must
	// not match it.
	if f.Remove("a") {
		t.Error("Remove must not match a vendor-only entry by name")
	}
}
