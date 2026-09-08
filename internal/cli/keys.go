package cli

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"

	"github.com/alecthomas/kong"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rag-platform/ragctl/internal/crypto"
)

// KeysCmd groups DEK key-management subcommands (STORY-10.4, SPEC-09 §2, NFR-SEC-03).
type KeysCmd struct {
	NewDEK    NewDEKCmd    `cmd:"" name:"new-dek" help:"Generate and KMS-wrap a fresh DEK for the next key version."`
	RotateDEK RotateDEKCmd `cmd:"" name:"rotate-dek" help:"Re-encrypt all stored secrets under the primary DEK version (zero-downtime rotation)."`
}

// NewDEKCmd generates a fresh 32-byte DEK, wraps it with the configured KMS, and writes
// the wrapped blob to --out — the next key version for a rotation. It never prints key
// material. Rotation procedure (zero downtime, SPEC-09 §2):
//  1. `keys new-dek --out dek.vN.age`
//  2. deploy the fleet with DEK_WRAPPED_PATH=dek.vN.age, DEK_KEY_VERSION=N, and the old
//     key as DEK_PREVIOUS=dek.v(N-1).age:(N-1) — a rolling restart, so every process now
//     seals under vN and decrypts either version.
//  3. `keys rotate-dek` re-encrypts all stored secrets to vN.
//  4. once complete, drop the old key from DEK_PREVIOUS.
type NewDEKCmd struct {
	Out string `help:"Path to write the wrapped DEK blob." required:""`
}

// Run generates and wraps a new DEK.
func (c *NewDEKCmd) Run(k *kong.Context, g *Globals) error {
	ctx := context.Background()
	kms, err := buildKMS(ctx, g.Secrets)
	if err != nil {
		return err
	}
	dek := make([]byte, crypto.KeySize)
	if _, err := rand.Read(dek); err != nil {
		return fmt.Errorf("keys new-dek: generate DEK: %w", err)
	}
	defer crypto.Zero(dek)
	wrapped, err := kms.Wrap(ctx, dek)
	if err != nil {
		return fmt.Errorf("keys new-dek: wrap DEK: %w", err)
	}
	if err := os.WriteFile(c.Out, wrapped, 0o600); err != nil {
		return fmt.Errorf("keys new-dek: write wrapped DEK: %w", err)
	}
	_, err = fmt.Fprintf(k.Stdout, "ragctl keys new-dek: wrote wrapped DEK to %s (set as DEK_WRAPPED_PATH + a new DEK_KEY_VERSION; keep the old key in DEK_PREVIOUS during rotation)\n", c.Out)
	return err
}

// RotateDEKCmd re-encrypts every stored secret under the keyring's primary version. It
// expects the fleet to already run with the new primary and the old key in DEK_PREVIOUS
// (see NewDEKCmd), so not-yet-migrated rows keep decrypting throughout — zero downtime.
// It is idempotent and resumable: rows already at the primary version are skipped, so a
// re-run after an interruption only finishes the remainder.
type RotateDEKCmd struct{}

// Run re-encrypts the control-plane secret columns.
func (c *RotateDEKCmd) Run(k *kong.Context, g *Globals) error {
	if g.ControlPlaneURL == "" {
		return fmt.Errorf("keys rotate-dek: no control-plane URL (set --control-plane-url or CONTROL_PLANE_URL)")
	}
	ctx := context.Background()
	keyring, err := LoadStartupKeyring(ctx, g.Secrets)
	if err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, g.ControlPlaneURL)
	if err != nil {
		return fmt.Errorf("keys rotate-dek: open control-plane pool: %w", err)
	}
	defer pool.Close()

	tenantPW, err := reencryptColumn(ctx, pool, keyring,
		`select tenant_id::text, password_enc from tenant_databases where password_enc is not null`,
		`update tenant_databases set password_enc = $2 where tenant_id::text = $1`)
	if err != nil {
		return fmt.Errorf("keys rotate-dek: tenant passwords: %w", err)
	}
	sourceCreds, err := reencryptColumn(ctx, pool, keyring,
		`select id::text, credentials_enc from sources where credentials_enc is not null`,
		`update sources set credentials_enc = $2 where id::text = $1`)
	if err != nil {
		return fmt.Errorf("keys rotate-dek: source credentials: %w", err)
	}
	_, err = fmt.Fprintf(k.Stdout,
		"ragctl keys rotate-dek: re-encrypted under v%d (tenant passwords: %d, source credentials: %d)\n",
		keyring.PrimaryVersion(), tenantPW, sourceCreds)
	return err
}

// reencryptColumn re-seals every non-null secret returned by selectSQL under the
// keyring's primary version, updating only the rows that changed (updateSQL takes
// $1=id::text, $2=new ciphertext). It reads all ids up front so the connection is free
// during the per-row re-encrypt+update. Returns the number of rows changed.
func reencryptColumn(ctx context.Context, pool *pgxpool.Pool, kr *crypto.Keyring, selectSQL, updateSQL string) (int, error) {
	type item struct {
		id  string
		enc []byte
	}
	rows, err := pool.Query(ctx, selectSQL)
	if err != nil {
		return 0, err
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.enc); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	changed := 0
	for _, it := range items {
		out, ch, err := kr.Reencrypt(it.enc)
		if err != nil {
			return changed, err
		}
		if !ch {
			continue // already at the primary version (idempotent/resumable)
		}
		if _, err := pool.Exec(ctx, updateSQL, it.id, out); err != nil {
			return changed, err
		}
		changed++
	}
	return changed, nil
}
