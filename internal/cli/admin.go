package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/alecthomas/kong"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rag-platform/ragctl/internal/cp/auth"
)

// AdminCmd groups platform-administration subcommands (FR-ADM-07: "CLI provides
// administrative operations", SPEC-02 §4).
type AdminCmd struct {
	Bootstrap AdminBootstrapCmd `cmd:"" help:"Create or promote a platform-admin user."`
}

// AdminBootstrapCmd creates (or promotes) a platform-admin user so an operator
// has login credentials for the admin UI (STORY-11.1 Task 6, FR-ADM-07,
// ADR-0074). Re-running it for an email that already exists promotes that user
// instead of failing (create-or-promote, idempotent) and never touches its
// password. A platform-admin user needs no tenant_members row — is_platform_admin
// alone grants the cross-tenant admin surface (SPEC-02 §4) — so this command
// does not create any tenant membership.
type AdminBootstrapCmd struct {
	Email string `help:"Email address of the platform-admin user." required:""`
	// PlatformAdmin defaults true: this command exists to grant the flag. Pass
	// --no-platform-admin to instead revoke it from an existing user.
	PlatformAdmin bool `help:"Grant (or, with --no-platform-admin, revoke) platform-admin." default:"true" negatable:""`
}

// Run resolves the control-plane URL (failing closed before any DB dial, like
// every other command that needs one), reads the password from
// RAGCTL_ADMIN_PASSWORD or stdin — never argv, which would leak via ps/shell
// history — and create-or-promotes the user. It prints only non-secret
// confirmation: the email, whether it was created or promoted, and the
// resulting platform_admin flag. The password and its hash are never echoed.
func (c *AdminBootstrapCmd) Run(k *kong.Context, g *Globals) error {
	if g.ControlPlaneURL == "" {
		return fmt.Errorf("admin bootstrap: no control-plane URL (set --control-plane-url or CONTROL_PLANE_URL)")
	}

	password, err := adminPasswordFromEnvOrStdin(os.Stdin, k.Stderr)
	if err != nil {
		return fmt.Errorf("admin bootstrap: %w", err)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, g.ControlPlaneURL)
	if err != nil {
		return fmt.Errorf("admin bootstrap: open control-plane pool: %w", err)
	}
	defer pool.Close()

	svc := auth.NewService(auth.FromPool(pool))
	created, err := svc.BootstrapAdmin(ctx, c.Email, password, c.PlatformAdmin)
	if err != nil {
		return fmt.Errorf("admin bootstrap: %w", err)
	}

	verb := "promoted existing user"
	if created {
		verb = "created new user"
	}
	_, werr := fmt.Fprintf(k.Stdout, "ragctl admin bootstrap: %s %q (platform_admin=%v)\n", verb, c.Email, c.PlatformAdmin)
	return werr
}

// adminPasswordFromEnvOrStdin resolves the bootstrap password: RAGCTL_ADMIN_PASSWORD
// when set, otherwise one line read from stdin (prompting on stderr only when
// stdin is a terminal — a pipe/redirect gets no prompt noise). The password is
// never accepted as a CLI argument, so it never appears in `ps` or shell history.
//
// ponytail: a real TTY sees its keystrokes echoed while typing the password
// (no golang.org/x/term dependency for hidden input). Ceiling: shoulder-surfing
// at an interactive prompt; upgrade path: add x/term and switch to
// term.ReadPassword when that's worth a new dependency. Env var / piped stdin
// (the CI and scripted paths) are unaffected either way.
func adminPasswordFromEnvOrStdin(stdin io.Reader, stderr io.Writer) (string, error) {
	if pw, ok := os.LookupEnv("RAGCTL_ADMIN_PASSWORD"); ok {
		return pw, nil
	}
	if f, ok := stdin.(*os.File); ok && isTerminal(f) {
		if _, err := fmt.Fprint(stderr, "password: "); err != nil {
			return "", fmt.Errorf("write password prompt: %w", err)
		}
	}
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read password from stdin: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// isTerminal reports whether f is a character device (a real TTY), as opposed
// to a pipe or redirected file.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
