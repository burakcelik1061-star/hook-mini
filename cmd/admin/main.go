// Command admin is the operator's tool for the things the HTTP API
// deliberately cannot do: creating tenants and issuing or revoking API keys.
//
// There is no unauthenticated "create account" endpoint. Whoever can run this
// command (and so reach the database) is the operator.
//
//	admin bootstrap <tenant name> [key name]   create a tenant and its first key
//	admin create-tenant <name>
//	admin create-key <tenant id> <key name>
//	admin revoke-key <tenant id> <key id>
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/hook-mini/internal/config"
	"github.com/burakcelik1061-star/hook-mini/internal/database"
	"github.com/burakcelik1061-star/hook-mini/internal/lifecycle"
	"github.com/burakcelik1061-star/hook-mini/internal/logging"
	"github.com/burakcelik1061-star/hook-mini/internal/service"
	"github.com/burakcelik1061-star/hook-mini/internal/store"
)

// admin is the part of the service this command uses.
type admin interface {
	CreateTenant(ctx context.Context, name string) (store.Tenant, error)
	IssueAPIKey(ctx context.Context, tenantID uuid.UUID, name string) (store.ApiKey, string, error)
	RevokeAPIKey(ctx context.Context, tenantID, keyID uuid.UUID) error
}

const usage = `usage:
  admin bootstrap <tenant name> [key name]
  admin create-tenant <name>
  admin create-key <tenant id> <key name>
  admin revoke-key <tenant id> <key id>`

func main() {
	if err := realMain(os.Args[1:], os.Stdout); err != nil {
		var u usageError
		if errors.As(err, &u) {
			fmt.Fprintln(os.Stderr, "admin:", err)
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "admin:", err)
		os.Exit(1)
	}
}

func realMain(args []string, stdout io.Writer) error {
	// Validate the command line before touching the database, so a typo gives
	// a usage message instead of a connection error.
	if err := checkArgs(args); err != nil {
		return err
	}

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	log := logging.New(os.Stderr, "admin", cfg.LogFormat, cfg.LogLevel)

	ctx, stop := lifecycle.SignalContext()
	defer stop()

	pool, err := database.Connect(ctx, log, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	return run(ctx, args, stdout, service.New(pool, log))
}

type usageError string

func (e usageError) Error() string { return string(e) + "\n" + usage }

func checkArgs(args []string) error {
	if len(args) == 0 {
		return usageError("missing command")
	}
	need := map[string][2]int{ // command -> min, max arguments
		"bootstrap":     {1, 2},
		"create-tenant": {1, 1},
		"create-key":    {2, 2},
		"revoke-key":    {2, 2},
	}
	n, ok := need[args[0]]
	if !ok {
		return usageError(fmt.Sprintf("unknown command %q", args[0]))
	}
	if got := len(args) - 1; got < n[0] || got > n[1] {
		return usageError(fmt.Sprintf("wrong number of arguments for %s", args[0]))
	}
	return nil
}

func run(ctx context.Context, args []string, out io.Writer, svc admin) error {
	if err := checkArgs(args); err != nil {
		return err
	}
	cmd, rest := args[0], args[1:]

	switch cmd {
	case "bootstrap":
		keyName := "default"
		if len(rest) == 2 {
			keyName = rest[1]
		}
		t, err := svc.CreateTenant(ctx, rest[0])
		if err != nil {
			return err
		}
		return issue(ctx, out, svc, t.ID, keyName, t.Name)

	case "create-tenant":
		t, err := svc.CreateTenant(ctx, rest[0])
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "tenant_id: %s\nname:      %s\n", t.ID, t.Name)
		return nil

	case "create-key":
		id, err := uuid.Parse(rest[0])
		if err != nil {
			return usageError("tenant id is not a valid UUID")
		}
		return issue(ctx, out, svc, id, rest[1], "")

	case "revoke-key":
		tenantID, err := uuid.Parse(rest[0])
		if err != nil {
			return usageError("tenant id is not a valid UUID")
		}
		keyID, err := uuid.Parse(rest[1])
		if err != nil {
			return usageError("key id is not a valid UUID")
		}
		if err := svc.RevokeAPIKey(ctx, tenantID, keyID); err != nil {
			if errors.Is(err, service.ErrNotFound) {
				return errors.New("no active key with that id for that tenant")
			}
			return err
		}
		fmt.Fprintf(out, "revoked: %s\n", keyID)
		return nil
	}
	return usageError("unreachable")
}

func issue(ctx context.Context, out io.Writer, svc admin, tenantID uuid.UUID, keyName, tenantName string) error {
	key, full, err := svc.IssueAPIKey(ctx, tenantID, keyName)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			return errors.New("no such tenant")
		}
		return err
	}
	fmt.Fprintf(out, "tenant_id: %s\n", tenantID)
	if tenantName != "" {
		fmt.Fprintf(out, "name:      %s\n", tenantName)
	}
	fmt.Fprintf(out, "key_id:    %s\napi_key:   %s\n\n", key.ID, full)
	fmt.Fprintln(out, strings.TrimSpace(`
Store the API key now. Only its hash is kept, so it cannot be shown again.`))
	return nil
}
