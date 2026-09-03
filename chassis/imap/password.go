package imap

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. Sized for the IMAP login pattern — a desktop client
// opens 5–10 connections per wake, each with its own LOGIN — so the cost is
// kept at "tens of milliseconds, 16 MiB" per verify rather than the
// interactive-web-login maximum; the verified-login cache in the head
// (chassis/server/personality/imap) absorbs the fan-out on top of this.
const (
	argonTime    uint32 = 3
	argonMemory  uint32 = 16 * 1024 // KiB
	argonThreads uint8  = 1
	argonKeyLen  uint32 = 32
	argonSaltLen        = 16
)

var (
	// ErrBadHash is returned when a stored hash is not a PHC argon2id string
	// this package produced.
	ErrBadHash = errors.New("imap: malformed password hash")

	dummyOnce sync.Once
	dummyHash string
)

// HashPassword returns a PHC-formatted argon2id string
// ($argon2id$v=19$m=…,t=…,p=…$<salt>$<hash>, base64 without padding) for
// storage in imap_accounts.pw_hash.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("imap: salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// VerifyPassword reports whether password matches the PHC hash. The
// comparison is constant-time; parameters are read from the hash so a
// later cost bump verifies old rows unchanged. A malformed hash is an
// error (never a silent false-negative that hides a corrupt row).
func VerifyPassword(phc, password string) (bool, error) {
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, ErrBadHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, ErrBadHash
	}
	var mem, tm uint32
	var thr uint8
	for _, kv := range strings.Split(parts[3], ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return false, ErrBadHash
		}
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return false, ErrBadHash
		}
		switch k {
		case "m":
			mem = uint32(n)
		case "t":
			tm = uint32(n)
		case "p":
			thr = uint8(n)
		default:
			return false, ErrBadHash
		}
	}
	if mem == 0 || tm == 0 || thr == 0 {
		return false, ErrBadHash
	}
	enc := base64.RawStdEncoding
	salt, err := enc.DecodeString(parts[4])
	if err != nil {
		return false, ErrBadHash
	}
	want, err := enc.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false, ErrBadHash
	}
	got := argon2.IDKey([]byte(password), salt, tm, mem, thr, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// VerifyDummy burns one argon2id verify against a fixed hash so a LOGIN for
// an unknown username costs the same as one for a known username (no
// account-existence timing oracle). The result is always false.
func VerifyDummy(password string) bool {
	dummyOnce.Do(func() {
		h, err := HashPassword("txco-imap-dummy-" + generateSecret(8))
		if err == nil {
			dummyHash = h
		}
	})
	if dummyHash == "" {
		return false
	}
	ok, _ := VerifyPassword(dummyHash, password)
	return ok && false
}

// passwordAlphabet omits characters that read ambiguously when a password
// is shown once and typed into a mail client (0/O, 1/l/I).
const passwordAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// GeneratePassword returns a fresh random password in four-character groups
// ("xxxx-xxxx-xxxx-xxxx-xxxx-xxxx", ~116 bits) for the `password=""` path of
// txco://imap/account. It is returned to the caller exactly once and only
// its hash is stored.
func GeneratePassword() string {
	raw := generateSecret(24)
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		b.WriteByte(raw[i])
	}
	return b.String()
}

func generateSecret(n int) string {
	// Rejection sampling: 256 is not a multiple of the alphabet size, so a
	// plain modulo would bias the low characters.
	const limit = 256 - (256 % len(passwordAlphabet))
	out := make([]byte, 0, n)
	buf := make([]byte, n*2)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			panic("imap: crypto/rand unavailable: " + err.Error())
		}
		for _, c := range buf {
			if int(c) >= limit {
				continue
			}
			out = append(out, passwordAlphabet[int(c)%len(passwordAlphabet)])
			if len(out) == n {
				break
			}
		}
	}
	return string(out)
}
