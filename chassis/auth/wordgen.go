package auth

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"strings"
)

// EightWordSecret returns a hyphen-joined 8-word secret drawn from
// effLongWords (the EFF "long" wordlist, hyphenless variant, 7772
// entries). ~103 bits of entropy.
//
// Use this for any credential that authenticates an unsigned-consume
// endpoint: the first-boot admin bootstrap secret, admin-issued
// invitation tokens, and any future "anyone-with-the-string-can-mint-
// creds" pattern. 32-bit shortcuts (4 words from a small list) are
// not enough — the consume endpoints are the most exposed HTTP
// surface, and TTL + burn-after-use are belt-and-suspenders, not a
// substitute for entropy.
func EightWordSecret() (string, error) {
	parts := make([]string, 8)
	for i := range parts {
		idx, err := randIndex(len(effLongWords))
		if err != nil {
			return "", fmt.Errorf("wordgen: %w", err)
		}
		parts[i] = effLongWords[idx]
	}
	return strings.Join(parts, "-"), nil
}

// randIndex returns a uniform int in [0, n) using crypto/rand with
// rejection sampling so the distribution stays uniform for any n
// (not just powers of two). Reads 8 bytes per attempt; for n ≤ 7776
// the rejection probability is well under 1 in a million, so the
// loop almost always exits in one iteration.
func randIndex(n int) (int, error) {
	if n <= 0 {
		return 0, fmt.Errorf("randIndex: n=%d", n)
	}
	const span = uint64(1) << 63 // half of uint64 range, gives plenty of headroom
	max := span - (span % uint64(n))
	var b [8]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			return 0, err
		}
		v := binary.BigEndian.Uint64(b[:]) >> 1 // mask off the top bit
		if v < max {
			return int(v % uint64(n)), nil
		}
	}
}
