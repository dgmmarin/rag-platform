// Package cli defines the ragctl command grammar. Per ADR-0009 and SPEC-02 §7
// the CLI is a Kong grammar: every subcommand is a struct with a Run method and
// typed flags, and global flags resolve flag -> env -> config file.
//
// STORY-01.1 delivers the skeleton: the wiring is real, but command bodies are
// stubs that return ErrNotImplemented. Later stories replace the bodies.
package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/alecthomas/kong"

	"github.com/rag-platform/ragctl/internal/config"
)

// ErrNotImplemented is returned by stub subcommands. Callers can distinguish an
// unimplemented-but-wired command from a real failure with errors.Is.
var ErrNotImplemented = errors.New("not implemented")

// ErrHelpRequested is returned when Kong handled --help (or an empty invocation)
// and printed usage. Callers should treat it as a clean, successful exit.
var ErrHelpRequested = errors.New("help requested")

// CLI is the top-level Kong grammar for ragctl.
type CLI struct {
	// Global flags: flag -> env -> config file (Kong resolvers wire env/file).
	Config          kong.ConfigFlag `help:"Path to an optional config file." env:"RAGCTL_CONFIG"`
	LogLevel        string          `help:"Log level (debug, info, warn, error)." env:"LOG_LEVEL" default:"info"`
	ControlPlaneURL string          `help:"Control-plane database URL." env:"CONTROL_PLANE_URL"`

	Serve   ServeCmd   `cmd:"" help:"Start the HTTP API server."`
	Work    WorkCmd    `cmd:"" help:"Start the job worker."`
	Migrate MigrateCmd `cmd:"" help:"Apply database migrations."`
	Enroll  EnrollCmd  `cmd:"" help:"Enrol a new tenant."`
	Tenant  TenantCmd  `cmd:"" help:"Manage tenant lifecycle (suspend, resume, delete)."`
}

// New builds the Kong parser for the ragctl grammar, writing output to the
// given streams. Kong's exit hook records the requested status into *exited
// instead of calling os.Exit, so callers (and tests) stay in control of the
// process lifecycle. exited is set to a non-negative value when Kong handled
// --help or a usage error itself.
func New(cli *CLI, stdout, stderr io.Writer, exited *int) (*kong.Kong, error) {
	return kong.New(cli,
		kong.Name("ragctl"),
		kong.Description("Multi-tenant company-knowledge RAG platform control binary."),
		kong.Writers(stdout, stderr),
		kong.Exit(func(code int) { *exited = code }),
	)
}

// Run parses args and dispatches to the selected subcommand. It returns the
// subcommand's error (ErrNotImplemented for stubs), a parse error, or
// ErrHelpRequested when Kong printed usage. It never exits the process, so it
// is safe to call from tests.
func Run(args []string, stdout, stderr io.Writer) error {
	var cli CLI
	exited := -1
	parser, err := New(&cli, stdout, stderr, &exited)
	if err != nil {
		return err
	}
	ctx, err := parser.Parse(args)
	// Kong prints help/usage and invokes the exit hook rather than returning an
	// error. Honour the status it asked for: 0 => clean help, non-zero => usage
	// error.
	if exited == 0 {
		return ErrHelpRequested
	}
	if exited > 0 {
		return fmt.Errorf("usage error (exit %d)", exited)
	}
	if err != nil {
		return err
	}
	// Load secret-loading configuration once here (env with optional config-file
	// overlay) so command Run methods receive resolved settings rather than
	// reading the environment themselves (STORY-01.4, SPEC-09 §2). The Kong
	// ConfigFlag resolves --config -> RAGCTL_CONFIG; reuse that path for the
	// overlay so precedence stays flag -> env -> file (ADR-0009).
	cfg, err := config.Load(string(cli.Config))
	if err != nil {
		return err
	}
	// Bind resolved global values by type so command Run methods can receive the
	// control-plane URL (and future globals) without reading the top-level struct
	// directly (ADR-0009: flag -> env -> config file, resolved once here).
	// The --log-level global flag takes precedence over the config/env LOG_LEVEL
	// (flag -> env -> file, ADR-0009); Kong has already resolved cli.LogLevel from
	// flag/env with a default of info.
	obs := obsSettingsFromConfig(cfg)
	if cli.LogLevel != "" {
		obs.LogLevel = cli.LogLevel
	}

	return ctx.Run(&Globals{
		ControlPlaneURL: cli.ControlPlaneURL,
		ProvisionURL:    cfg.ProvisionURL,
		TenantDBHost:    cfg.TenantDBHost,
		TenantDBPort:    cfg.TenantDBPort,
		TenantDBSSLMode: cfg.TenantDBSSLMode,
		Secrets:         startupSecretsFromConfig(cfg),
		Obs:             obs,
	})
}

// Globals carries resolved global flag values into command Run methods via
// Kong's typed bindings.
type Globals struct {
	ControlPlaneURL string
	// ProvisionURL is the privileged (superuser) connection used by `enroll` to
	// create the tenant role/database/extensions (STORY-02.3, SPEC-01 §6). Empty
	// falls back to ControlPlaneURL.
	ProvisionURL string
	// TenantDBHost / TenantDBPort are recorded as a provisioned tenant's database
	// location; empty/zero reuses the provisioning connection's target.
	TenantDBHost string
	TenantDBPort int
	// TenantDBSSLMode is the sslmode recorded for a provisioned tenant's DB
	// connection (default "require").
	TenantDBSSLMode string
	// Secrets carries the resolved KMS/DEK configuration for commands that must
	// load the data-encryption key at startup (STORY-01.4).
	Secrets StartupSecrets
	// Obs carries the resolved observability settings (log level, tracing) for
	// long-running commands that serve traffic (STORY-01.6, SPEC-10).
	Obs ObsSettings
}
