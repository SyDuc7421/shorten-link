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
)

const adminUsage = `usage: server admin grant <email>
       server admin revoke <email>`

// adminSetter is the part of AdminService the admin command needs.
type adminSetter interface {
	SetAdmin(ctx context.Context, email string, admin bool) (bool, error)
}

// adminMain runs "server admin ...": it needs only MySQL, not Redis, RabbitMQ
// or the JWT keys. Returns the process exit code.
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

	// SetAdmin touches neither the cache nor tokens, so they are nil here.
	svc := service.NewAdminService(repository.NewAdminMySQL(db), repository.NewUserMySQL(db), nil, nil)
	return runAdminCommand(ctx, args, svc, os.Stdout)
}

// runAdminCommand grants or revokes the admin role. Exit codes: 0 done (or
// nothing to do), 1 failed, 2 bad usage.
func runAdminCommand(ctx context.Context, args []string, svc adminSetter, out io.Writer) int {
	msg, code := adminCommand(ctx, args, svc)
	fmt.Fprintln(out, msg) //nolint:errcheck // nothing useful to do if stdout fails
	return code
}

func adminCommand(ctx context.Context, args []string, svc adminSetter) (string, int) {
	if len(args) != 2 || (args[0] != "grant" && args[0] != "revoke") {
		return adminUsage, 2
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
