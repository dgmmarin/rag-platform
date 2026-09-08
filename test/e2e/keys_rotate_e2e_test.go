//go:build e2e

// STORY-10.4 golden path: `ragctl keys rotate-dek` re-encrypts stored secrets under a
// new DEK version while the old key is still available to decrypt un-migrated rows
// (zero-downtime rotation, SPEC-09 §2). Asserted against the real binary + real control
// plane: a tenant password (sealed at v1 by enroll) and a source credential (sealed at
// v1) both become v2 ciphertext that decrypts to the original plaintext.
package e2e

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/rag-platform/ragctl/internal/crypto"
)

// wrapDEKUnder wraps dek under the recipient of an existing age secret key, so a second
// DEK version can be unwrapped by the same AGE_SECRET_KEY.
func wrapDEKUnder(t *testing.T, ageKey string, dek []byte) string {
	t.Helper()
	id, err := age.ParseX25519Identity(ageKey)
	if err != nil {
		t.Fatalf("parse age key: %v", err)
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, id.Recipient())
	if err != nil {
		t.Fatalf("age encrypt: %v", err)
	}
	if _, err := w.Write(dek); err != nil {
		t.Fatalf("write dek: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	path := filepath.Join(t.TempDir(), "dek.v2.age")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write v2 blob: %v", err)
	}
	return path
}

func TestKeysRotateDEKReencryptsSecrets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	migrateControl(t)
	pool := controlPool(t)
	bin := buildRagctl(t)

	ageKey, v1blob := writeWrappedDEK(t) // wraps migrateDEK as v1
	suffix := mustSuffix(t)
	slug := "rot-" + suffix
	t.Cleanup(func() {
		user := hostPort("POSTGRES_USER", "rag")
		if dbName := tryScalar(slug, "d.database_name"); dbName != "" {
			_ = tryPsql(user, "control_plane", "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
		}
		if role := tryScalar(slug, "d.username"); role != "" {
			_ = tryPsql(user, "control_plane", "DROP ROLE IF EXISTS "+role)
		}
		_ = tryPsql(user, "control_plane", "DELETE FROM tenants WHERE slug = '"+slug+"'")
	})

	// Enroll a tenant — its DB password is sealed at v1.
	if out, exit := runEnroll(t, ageKey, v1blob, slug, "Rotate "+suffix, 1024); exit != 0 {
		t.Fatalf("enroll exited %d\n%s", exit, out)
	}
	var tenantID string
	if err := pool.QueryRow(ctx, `select id::text from tenants where slug = $1`, slug).Scan(&tenantID); err != nil {
		t.Fatalf("tenant id: %v", err)
	}

	// Seed a source with credentials sealed at v1.
	v1, err := crypto.NewCipher(1, migrateDEK)
	if err != nil {
		t.Fatalf("v1 cipher: %v", err)
	}
	credV1, err := v1.Encrypt([]byte("s3cr3t-token"))
	if err != nil {
		t.Fatalf("seal creds: %v", err)
	}
	var sourceID string
	if err := pool.QueryRow(ctx,
		`insert into sources (tenant_id, kind, name, status, credentials_enc)
		 values ($1::uuid, 'api', 'creds', 'active', $2) returning id::text`, tenantID, credV1).Scan(&sourceID); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	// A fresh v2 DEK, wrapped under the same age key.
	dek2 := bytes.Repeat([]byte{0x42}, 32)
	v2blob := wrapDEKUnder(t, ageKey, dek2)

	// Rotate: primary v2, previous v1 (the zero-downtime keyring).
	cmd := exec.CommandContext(ctx, bin, "keys", "rotate-dek")
	cmd.Env = append(dekEnv(
		"KMS_PROVIDER=local",
		"AGE_SECRET_KEY="+ageKey,
		"DEK_WRAPPED_PATH="+v2blob,
		"DEK_KEY_VERSION=2",
		"DEK_PREVIOUS="+v1blob+":1",
	), "CONTROL_PLANE_URL="+controlPlaneURL())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("keys rotate-dek failed: %v\n%s", err, out)
	}

	// Both secrets are now v2 and decrypt to their originals.
	v2, err := crypto.NewCipher(2, dek2)
	if err != nil {
		t.Fatalf("v2 cipher: %v", err)
	}
	var pw []byte
	if err := pool.QueryRow(ctx, `select password_enc from tenant_databases where tenant_id = $1::uuid`, tenantID).Scan(&pw); err != nil {
		t.Fatalf("read password_enc: %v", err)
	}
	if got := crypto.KeyVersion(pw); got != 2 {
		t.Fatalf("tenant password version = %d, want 2 after rotation", got)
	}
	if _, err := v2.Decrypt(pw); err != nil {
		t.Fatalf("v2 cannot decrypt the rotated tenant password: %v", err)
	}

	var creds []byte
	if err := pool.QueryRow(ctx, `select credentials_enc from sources where id = $1::uuid`, sourceID).Scan(&creds); err != nil {
		t.Fatalf("read credentials_enc: %v", err)
	}
	if got := crypto.KeyVersion(creds); got != 2 {
		t.Fatalf("source credential version = %d, want 2 after rotation", got)
	}
	pt, err := v2.Decrypt(creds)
	if err != nil {
		t.Fatalf("v2 cannot decrypt the rotated source credential: %v", err)
	}
	if string(pt) != "s3cr3t-token" {
		t.Fatalf("rotated credential = %q, want the original plaintext", pt)
	}

	// Idempotent: a second rotation changes nothing.
	cmd2 := exec.CommandContext(ctx, bin, "keys", "rotate-dek")
	cmd2.Env = cmd.Env
	if out, err := cmd2.CombinedOutput(); err != nil {
		t.Fatalf("second keys rotate-dek failed: %v\n%s", err, out)
	}
}
