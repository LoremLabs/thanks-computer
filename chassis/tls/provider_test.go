package tls

import (
	"context"
	"reflect"
	"testing"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/libdns"
)

type fakePub struct {
	presents [][2]string
	cleans   [][2]string
}

func (f *fakePub) Present(fqdn, val string) { f.presents = append(f.presents, [2]string{fqdn, val}) }
func (f *fakePub) CleanUp(fqdn, val string) { f.cleans = append(f.cleans, [2]string{fqdn, val}) }

// TestChallengeProvider verifies the libdns adapter reconstructs the
// absolute owner FQDN and routes Append/Delete to Present/CleanUp — the
// glue between certmagic's DNS-01 solver and the dns head's store.
func TestChallengeProvider(t *testing.T) {
	pub := &fakePub{}
	p := challengeProvider{pub: pub}
	ctx := context.Background()

	rec := libdns.RR{Type: "TXT", Name: "_acme-challenge", Data: "key-authz-token"}

	if _, err := p.AppendRecords(ctx, "ops.example.test.", []libdns.Record{rec}); err != nil {
		t.Fatalf("AppendRecords: %v", err)
	}
	want := [2]string{"_acme-challenge.ops.example.test.", "key-authz-token"}
	if len(pub.presents) != 1 || pub.presents[0] != want {
		t.Fatalf("Present got %v want %v", pub.presents, want)
	}

	if _, err := p.DeleteRecords(ctx, "ops.example.test.", []libdns.Record{rec}); err != nil {
		t.Fatalf("DeleteRecords: %v", err)
	}
	if len(pub.cleans) != 1 || pub.cleans[0] != want {
		t.Fatalf("CleanUp got %v want %v", pub.cleans, want)
	}

	// Zone without a trailing dot still yields a trailing-dot owner.
	pub2 := &fakePub{}
	p2 := challengeProvider{pub: pub2}
	if _, err := p2.AppendRecords(ctx, "ops.example.test", []libdns.Record{rec}); err != nil {
		t.Fatalf("AppendRecords (no dot): %v", err)
	}
	if pub2.presents[0][0] != "_acme-challenge.ops.example.test." {
		t.Fatalf("owner not normalized: %q", pub2.presents[0][0])
	}
}

func TestWildcardDomains(t *testing.T) {
	got := WildcardDomains([]string{"ops.example.com", "team.example.org."})
	want := []string{"ops.example.com", "*.ops.example.com", "team.example.org", "*.team.example.org"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WildcardDomains = %v, want %v", got, want)
	}
	if d := WildcardDomains([]string{"", "."}); len(d) != 0 {
		t.Fatalf("blank origins should yield none, got %v", d)
	}
}

func TestStorageForDSN(t *testing.T) {
	// Empty DSN → file storage at the given path.
	s, err := storageForDSN("", "/tmp/acme-test")
	if err != nil {
		t.Fatalf("storageForDSN file: %v", err)
	}
	fs, ok := s.(*certmagic.FileStorage)
	if !ok || fs.Path != "/tmp/acme-test" {
		t.Fatalf("want FileStorage{/tmp/acme-test}, got %#v", s)
	}

	// Registered scheme → the factory's backend.
	RegisterStorage("teststore", func(dsn string) (certmagic.Storage, error) {
		return &certmagic.FileStorage{Path: "FROM-FACTORY:" + dsn}, nil
	})
	s2, err := storageForDSN("teststore://creds@host/db", "/ignored")
	if err != nil {
		t.Fatalf("storageForDSN factory: %v", err)
	}
	fs2 := s2.(*certmagic.FileStorage)
	if fs2.Path != "FROM-FACTORY:teststore://creds@host/db" {
		t.Fatalf("factory not used: %q", fs2.Path)
	}

	// Unregistered scheme falls back to file storage (safe default).
	s3, _ := storageForDSN("unknown://x", "/fallback")
	if fs3, ok := s3.(*certmagic.FileStorage); !ok || fs3.Path != "/fallback" {
		t.Fatalf("unknown scheme should fall back to file, got %#v", s3)
	}
}

// TestNewManagerWiring constructs a Manager and asserts it produces a usable
// TLS config (GetCertificate set) without contacting any CA. CARootFile +
// storage selection are exercised; issuance itself is the Pebble smoke test.
func TestNewManagerWiring(t *testing.T) {
	m, err := NewManager(Options{
		Publisher:   &fakePub{},
		Email:       "ops@example.test",
		CA:          "https://localhost:14000/dir", // not contacted here
		StoragePath: t.TempDir(),
		Resolvers:   []string{"127.0.0.1:5354"},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	tc := m.TLSConfig()
	if tc == nil || tc.GetCertificate == nil {
		t.Fatalf("TLSConfig missing GetCertificate")
	}
	var hasH2 bool
	for _, p := range tc.NextProtos {
		if p == "h2" {
			hasH2 = true
		}
	}
	if !hasH2 {
		t.Fatalf("NextProtos missing h2: %v", tc.NextProtos)
	}

	// A missing CA root file is a construction error (caught before issuance).
	if _, err := NewManager(Options{Publisher: &fakePub{}, CARootFile: "/no/such/file.pem"}); err == nil {
		t.Fatal("expected error for missing CA root file")
	}
	// No publisher is a programming error.
	if _, err := NewManager(Options{}); err == nil {
		t.Fatal("expected error for nil publisher")
	}
}
