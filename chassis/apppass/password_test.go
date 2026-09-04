package apppass

import (
	"strings"
	"testing"
)

func TestHashAndVerify(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=16384,t=3,p=1$") {
		t.Errorf("phc = %q", h)
	}
	ok, err := VerifyPassword(h, "correct horse")
	if err != nil || !ok {
		t.Errorf("verify ok=%v err=%v", ok, err)
	}
	ok, err = VerifyPassword(h, "wrong")
	if err != nil || ok {
		t.Errorf("wrong password ok=%v err=%v", ok, err)
	}
	h2, _ := HashPassword("correct horse")
	if h2 == h {
		t.Error("salts must differ")
	}
	for _, bad := range []string{"", "plain", "$argon2i$v=19$m=1,t=1,p=1$YQ$YQ", "$argon2id$v=19$m=0,t=3,p=1$YQ$YQ", "$argon2id$v=19$m=16384,t=3,p=1$!!$YQ"} {
		if _, err := VerifyPassword(bad, "x"); err != ErrBadHash {
			t.Errorf("VerifyPassword(%q) err = %v, want ErrBadHash", bad, err)
		}
	}
	if VerifyDummy("anything") {
		t.Error("dummy verify must never succeed")
	}
}

func TestGeneratePassword(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		p := GeneratePassword()
		if len(p) != 29 || strings.Count(p, "-") != 5 {
			t.Fatalf("shape = %q", p)
		}
		for _, r := range strings.ReplaceAll(p, "-", "") {
			if !strings.ContainsRune(passwordAlphabet, r) {
				t.Fatalf("char %q outside alphabet", r)
			}
		}
		if seen[p] {
			t.Fatal("duplicate password")
		}
		seen[p] = true
	}
}
