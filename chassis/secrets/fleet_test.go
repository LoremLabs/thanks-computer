package secrets

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// captureSyncer records what the Store publishes and runs the in-tx closure
// (returning a no-op outbox write) so the producer path is exercised exactly
// as in production minus the artifact store.
type captureSyncer struct {
	upserts []capturedPublish
	revokes []capturedPublish
}

type capturedPublish struct {
	tenantID   string
	versionRow map[string]any
	parentRow  map[string]any
}

func (c *captureSyncer) PublishSecretUpsert(_ context.Context, tenantID string, versionRow, parentRow map[string]any) (func(*sql.Tx) error, error) {
	c.upserts = append(c.upserts, capturedPublish{tenantID, versionRow, parentRow})
	return func(*sql.Tx) error { return nil }, nil
}

func (c *captureSyncer) PublishSecretRevoke(_ context.Context, tenantID string, parentRow map[string]any) (func(*sql.Tx) error, error) {
	c.revokes = append(c.revokes, capturedPublish{tenantID: tenantID, parentRow: parentRow})
	return func(*sql.Tx) error { return nil }, nil
}

// TestFleetRoundTripDecryptsUnderSharedKey is the load-bearing proof: a secret
// created on one node, shipped as JSON RowsArtifacts (blobs base64-wrapped),
// and applied to a SECOND empty DB decrypts there — but ONLY when both nodes
// share the same master key.
func TestFleetRoundTripDecryptsUnderSharedKey(t *testing.T) {
	ctx := context.Background()
	mk := newMockMK(t, 1) // the one shared fleet key

	// --- producer node ---
	cap := &captureSyncer{}
	producer := NewStore(newTestDB(t), mk)
	producer.SetSyncer(cap)

	const tenant = "tnt_round_trip"
	const want = "sk-super-secret-OPENAI-value\x00\xff\x10binary-ish"
	if _, err := producer.CreateSecret(ctx, tenant, nil, "OPENAI_KEY", "the key", "actor_1", []byte(want)); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	if len(cap.upserts) != 1 {
		t.Fatalf("expected 1 publish on create, got %d", len(cap.upserts))
	}
	pub := cap.upserts[0]
	if pub.versionRow == nil {
		t.Fatal("create must publish a version row")
	}
	// Blob columns must be base64-wrapped, never raw bytes (JSON would mangle).
	if _, ok := pub.versionRow["ciphertext"].(map[string]any)["$b64"]; !ok {
		t.Fatalf("ciphertext column not base64-wrapped: %T", pub.versionRow["ciphertext"])
	}

	// --- fleet transport: marshal + unmarshal both row artifacts ---
	versionRow := jsonRoundTrip(t, pub.versionRow)
	parentRow := jsonRoundTrip(t, pub.parentRow)

	// --- consumer node: fresh empty DB, SAME master key ---
	consumerDB := newTestDB(t)
	applyRowLikeApplier(t, consumerDB, "tenant_secret_versions", versionRow)
	applyRowLikeApplier(t, consumerDB, "tenant_secrets", parentRow)

	consumer := NewStore(consumerDB, mk)
	got, _, err := consumer.MaterializeSecretForOp(ctx, tenant, "", "OPENAI_KEY")
	if err != nil {
		t.Fatalf("consumer MaterializeSecretForOp: %v", err)
	}
	if string(got) != want {
		t.Fatalf("decrypted %q, want %q", got, want)
	}

	// --- a DIFFERENT master key must NOT decrypt (proves the shared-key need) ---
	otherDB := newTestDB(t)
	applyRowLikeApplier(t, otherDB, "tenant_secret_versions", jsonRoundTrip(t, pub.versionRow))
	applyRowLikeApplier(t, otherDB, "tenant_secrets", jsonRoundTrip(t, pub.parentRow))
	wrong := NewStore(otherDB, newMockMK(t, 1)) // same version, different random key
	if _, _, err := wrong.MaterializeSecretForOp(ctx, tenant, "", "OPENAI_KEY"); err == nil {
		t.Fatal("expected decryption failure under a different master key, got nil")
	}
}

// TestRevokePublishesRevokedRow checks the revoke path emits a parent row
// carrying revoked_at (the consumer's INSERT OR REPLACE flips it inactive).
func TestRevokePublishesRevokedRow(t *testing.T) {
	ctx := context.Background()
	cap := &captureSyncer{}
	s := NewStore(newTestDB(t), newMockMK(t, 1))
	s.SetSyncer(cap)

	const tenant = "tnt_revoke"
	if _, err := s.CreateSecret(ctx, tenant, nil, "TOKEN", "", "actor_1", []byte("v")); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	if err := s.RevokeSecret(ctx, tenant, nil, "TOKEN"); err != nil {
		t.Fatalf("RevokeSecret: %v", err)
	}
	if len(cap.revokes) != 1 {
		t.Fatalf("expected 1 revoke publish, got %d", len(cap.revokes))
	}
	if _, ok := cap.revokes[0].parentRow["revoked_at"]; !ok {
		t.Fatalf("revoke parent row missing revoked_at: %v", cap.revokes[0].parentRow)
	}
}

func TestNewInlineMasterKey(t *testing.T) {
	key := make([]byte, masterKeySize)
	for i := range key {
		key[i] = byte(i * 7)
	}
	mk, err := NewInlineMasterKey(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatalf("NewInlineMasterKey: %v", err)
	}
	if !bytes.Equal(mk.Key(), key) {
		t.Fatal("inline key bytes mismatch")
	}
	if mk.Version() != 1 {
		t.Fatalf("version = %d, want 1", mk.Version())
	}
	// Raw (no padding) URL encoding must also decode.
	if _, err := NewInlineMasterKey(base64.RawURLEncoding.EncodeToString(key)); err != nil {
		t.Fatalf("raw-url key: %v", err)
	}
	if _, err := NewInlineMasterKey(""); !errors.Is(err, ErrMasterKeyMissing) {
		t.Fatalf("empty: want ErrMasterKeyMissing, got %v", err)
	}
	if _, err := NewInlineMasterKey(base64.StdEncoding.EncodeToString([]byte("too-short"))); !errors.Is(err, ErrMasterKeyMalformed) {
		t.Fatalf("short: want ErrMasterKeyMalformed, got %v", err)
	}
	if _, err := NewInlineMasterKey("!!!not base64!!!"); !errors.Is(err, ErrMasterKeyMalformed) {
		t.Fatalf("garbage: want ErrMasterKeyMalformed, got %v", err)
	}
}

func jsonRoundTrip(t *testing.T, row map[string]any) map[string]any {
	t.Helper()
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal row: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal row: %v", err)
	}
	return out
}

// applyRowLikeApplier replicates chassis/controlapply.upsertRow + coerce: it
// decodes {"$b64":…} → []byte and integral float64 → int64, then INSERT OR
// REPLACEs. Kept in lock-step with controlapply.coerce (which has its own
// direct unit test); this test proves the producer→transport→consumer→decrypt
// chain end to end.
func applyRowLikeApplier(t *testing.T, db *sql.DB, table string, row map[string]any) {
	t.Helper()
	cols := make([]string, 0, len(row))
	for k := range row {
		cols = append(cols, k)
	}
	sort.Strings(cols)
	ph := make([]string, len(cols))
	args := make([]any, len(cols))
	for i, k := range cols {
		ph[i] = "?"
		args[i] = coerceLikeApplier(row[k])
	}
	q := fmt.Sprintf("INSERT OR REPLACE INTO %s (%s) VALUES (%s)",
		table, strings.Join(cols, ","), strings.Join(ph, ","))
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("apply row into %s: %v", table, err)
	}
}

func coerceLikeApplier(v any) any {
	switch t := v.(type) {
	case float64:
		if t == float64(int64(t)) {
			return int64(t)
		}
		return t
	case bool:
		if t {
			return 1
		}
		return 0
	case map[string]any:
		if len(t) == 1 {
			if enc, ok := t["$b64"].(string); ok {
				if raw, err := base64.StdEncoding.DecodeString(enc); err == nil {
					return raw
				}
			}
		}
		return v
	default:
		return v
	}
}
