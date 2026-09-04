package apppass

import (
	"crypto/rand"
	_ "embed"
	"encoding/binary"
	"strings"
	"sync"
)

// bip39English is the BIP-39 English word list (2048 words, MIT-licensed
// with the BIP; bitcoin/bips bip-0039/english.txt). Chosen for passwords
// because every word is unambiguous in its first four letters, the list
// avoids offensive and look-alike words, and 2048 = 2^11 makes selection
// unbiased with a 16-bit draw. It is NOT used for seed phrases here — the
// words are a password alphabet, nothing more.
//
//go:embed bip39_english.txt
var bip39English string

var (
	wordListOnce sync.Once
	wordListVal  []string
)

// wordList parses the embedded list once. 2048 entries, lowercase ASCII.
func wordList() []string {
	wordListOnce.Do(func() {
		wordListVal = strings.Fields(bip39English)
	})
	return wordListVal
}

// WordPasswordDefaultWords is the default word count for
// GenerateWordPassword: 5 × 11 bits = 55 bits, enough that an offline
// attack on the argon2id hash is measured in millennia while the phrase
// stays typable on a phone. Four words (44 bits) is defensible only
// because the head throttles LOGIN; six (66 bits) costs one more word.
const WordPasswordDefaultWords = 5

// GenerateWordPassword returns n words from the BIP-39 list joined by
// hyphens ("river-galaxy-bamboo-orbit-velvet"). n < 1 uses the default.
// Each word is one unbiased 11-bit draw from crypto/rand: 65536 is a
// multiple of 2048, so the modulo introduces no bias. Returned to the
// caller exactly once by the account ops; only its hash is stored.
func GenerateWordPassword(n int) string {
	if n < 1 {
		n = WordPasswordDefaultWords
	}
	words := wordList()
	buf := make([]byte, 2*n)
	if _, err := rand.Read(buf); err != nil {
		panic("apppass: crypto/rand failed: " + err.Error())
	}
	out := make([]string, n)
	for i := range out {
		out[i] = words[int(binary.BigEndian.Uint16(buf[2*i:2*i+2]))%len(words)]
	}
	return strings.Join(out, "-")
}
