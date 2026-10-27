package connection

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// Material is what a row holds, sealed: the tokens. It never leaves this
// package except as a header value on a request (Service.Use) or a form
// field at the token destination (renew).
type Material struct {
	Access  string `json:"a,omitempty"`
	Refresh string `json:"r,omitempty"`
	// TokenType is the provider's word for the access token ("bearer");
	// kept for the record, the consumer's format decides the header.
	TokenType string `json:"t,omitempty"`
}

// sealed is the stored form: the tenant secrets' envelope (a data key per
// row, wrapped by the master key), as one base64 JSON column.
type sealed struct {
	KeyVersion int    `json:"k"`
	Nonce      string `json:"n"`
	Ciphertext string `json:"c"`
	WrappedDEK string `json:"w"`
	DEKNonce   string `json:"d"`
}

// aad binds the ciphertext to its row: a row copied onto another principal,
// provider or generation does not decrypt. No stack name is in it, so a
// provider moving between stacks touches no row.
func aad(tenantID, principalID, provider, label, id string, generation int64) ([]byte, []byte) {
	outer := strings.Join([]string{tenantID, principalID, provider, label, id, strconv.FormatInt(generation, 10)}, "\x00")
	inner := strings.Join([]string{tenantID, id, strconv.FormatInt(generation, 10)}, "\x00")
	return []byte(outer), []byte(inner)
}

func sealMaterial(mk secrets.MasterKeyProvider, m Material, tenantID, principalID, provider, label, id string, generation int64) (string, error) {
	plain, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	defer secrets.Zero(plain)
	outer, inner := aad(tenantID, principalID, provider, label, id, generation)
	es, err := secrets.Encrypt(mk, plain, outer, inner)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(sealed{
		KeyVersion: es.KeyVersion,
		Nonce:      base64.StdEncoding.EncodeToString(es.Nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(es.Ciphertext),
		WrappedDEK: base64.StdEncoding.EncodeToString(es.WrappedDEK),
		DEKNonce:   base64.StdEncoding.EncodeToString(es.DEKNonce),
	})
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func openMaterial(mk secrets.MasterKeyProvider, column, tenantID, principalID, provider, label, id string, generation int64) (Material, error) {
	var s sealed
	if err := json.Unmarshal([]byte(column), &s); err != nil {
		return Material{}, fmt.Errorf("connection: stored material: %w", err)
	}
	dec := func(v string) []byte {
		b, _ := base64.StdEncoding.DecodeString(v)
		return b
	}
	es := &secrets.EncryptedSecret{
		KeyVersion: s.KeyVersion, Nonce: dec(s.Nonce), Ciphertext: dec(s.Ciphertext),
		WrappedDEK: dec(s.WrappedDEK), DEKNonce: dec(s.DEKNonce),
	}
	outer, inner := aad(tenantID, principalID, provider, label, id, generation)
	plain, err := secrets.Decrypt(mk, es, outer, inner)
	if err != nil {
		return Material{}, fmt.Errorf("connection: open material: %w", err)
	}
	defer secrets.Zero(plain)
	var m Material
	if err := json.Unmarshal(plain, &m); err != nil {
		return Material{}, fmt.Errorf("connection: stored material: %w", err)
	}
	return m, nil
}
