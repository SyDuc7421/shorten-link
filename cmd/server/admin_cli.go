package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/april/shorten-link/config"
	"github.com/april/shorten-link/internal/repository"
	"github.com/april/shorten-link/internal/service"
	"github.com/april/shorten-link/pkg/idgen"
)

const adminUsage = `usage: server admin create <email>   (password from ADMIN_PASSWORD)
       server admin grant <email>
       server admin revoke <email>`

// adminSetter is the part of AdminService the admin command needs.
type adminSetter interface {
	SetAdmin(ctx context.Context, email string, admin bool) (bool, error)
	CreateAdmin(ctx context.Context, email, password string) (created bool, err error)
}

// cliAdmin binds an ID generator to AdminService.CreateAdmin for the CLI.
type cliAdmin struct {
	*service.AdminService
	gen *idgen.Generator
}

func (c cliAdmin) CreateAdmin(ctx context.Context, email, password string) (bool, error) {
	return c.AdminService.CreateAdmin(ctx, c.gen.NextID(), email, password)
}

// adminMain runs "server admin ...". It loads the full config (so every
// required variable, JWT key paths included, must be set, as in the server's
// own container) but connects only to MySQL. Returns the process exit code.
func adminMain(args []string) int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := cfg.OpenMySQL(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to connect MySQL: %v\n", err)
		return 1
	}
	defer db.Close()

	gen, err := idgen.NewGenerator(cfg.NodeID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create ID generator: %v\n", err)
		return 1
	}

	// SetAdmin and CreateAdmin touch neither the cache nor tokens, so they are nil here.
	svc := cliAdmin{service.NewAdminService(repository.NewAdminMySQL(db), repository.NewUserMySQL(db), nil, nil), gen}
	// The password comes from the environment, never from argv, so it does
	// not end up in shell history or the process list.
	return runAdminCommand(ctx, args, svc, os.Getenv("ADMIN_PASSWORD"), os.Stdout)
}

// runAdminCommand creates an admin, or grants or revokes the admin role.
// Exit codes: 0 done (or nothing to do), 1 failed, 2 bad usage.
func runAdminCommand(ctx context.Context, args []string, svc adminSetter, password string, out io.Writer) int {
	msg, code := adminCommand(ctx, args, svc, password)
	fmt.Fprintln(out, msg) //nolint:errcheck // nothing useful to do if stdout fails
	return code
}

func adminCommand(ctx context.Context, args []string, svc adminSetter, password string) (string, int) {
	if len(args) != 2 || (args[0] != "create" && args[0] != "grant" && args[0] != "revoke") {
		return adminUsage, 2
	}
	if args[0] == "create" {
		return createAdminCommand(ctx, svc, args[1], password)
	}
	grant, email := args[0] == "grant", args[1]

	changed, err := svc.SetAdmin(ctx, email, grant)
	switch {
	case errors.Is(err, service.ErrNotFound):
		return "no user with email " + email, 1
	case err != nil:
		return "failed: " + err.Error(), 1
	case grant && changed:
		return "granted admin to " + email, 0
	case grant:
		return email + " is already an admin", 0
	case changed:
		return "revoked admin from " + email, 0
	default:
		return email + " is not an admin", 0
	}
}

func createAdminCommand(ctx context.Context, svc adminSetter, email, password string) (string, int) {
	created, err := svc.CreateAdmin(ctx, email, password)
	switch {
	case errors.Is(err, service.ErrWeakPassword):
		return "ADMIN_PASSWORD must be 8-72 bytes to create a new account", 1
	case err != nil:
		return "failed: " + err.Error(), 1
	case created:
		return "created admin " + email, 0
	default:
		return email + " already exists: granted admin, password unchanged", 0
	}
}
