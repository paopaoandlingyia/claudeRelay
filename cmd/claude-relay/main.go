package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/local/claude-relay/internal/cch"
	"github.com/local/claude-relay/internal/config"
	"github.com/local/claude-relay/internal/credential"
	"github.com/local/claude-relay/internal/proxy"
	"github.com/local/claude-relay/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "import":
		err = runImport(os.Args[2:])
	case "serve":
		err = runServe(os.Args[2:])
	case "sign-cch":
		err = runSignCCH(os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		usage()
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		slog.Error("command failed", "error", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  claude-relay import -from <cliproxy-credential.json> -alias <name> [-db data/claude-relay.db]")
	fmt.Fprintln(os.Stderr, "  claude-relay sign-cch -in <body.json> -out <signed-body.json>")
	fmt.Fprintln(os.Stderr, "  claude-relay serve [-config config.json]")
}

func runImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	from := fs.String("from", "", "source CLIProxyAPI Claude credential JSON")
	alias := fs.String("alias", "", "stable account alias used by routing and the private selection header")
	databasePath := fs.String("db", "data/claude-relay.db", "SQLite database")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == "" || *alias == "" {
		return fmt.Errorf("-from and -alias are required")
	}
	cred, err := credential.ReadImport(*from)
	if err != nil {
		return err
	}
	database, err := store.Open(*databasePath)
	if err != nil {
		return err
	}
	defer database.Close()
	account, err := database.ImportAccount(context.Background(), *alias, cred)
	if err != nil {
		return err
	}
	slog.Info("credential imported", "account", account.Alias, "database", *databasePath, "expires", cred.ExpiresAt)
	return nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := fs.String("config", "config.json", "configuration JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	database, err := store.Open(cfg.DatabaseFile)
	if err != nil {
		return err
	}
	defer database.Close()
	if err := migrateLegacyCredential(database, cfg.CredentialsFile); err != nil {
		return err
	}

	server, err := proxy.NewServer(cfg, database)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return server.Run(ctx)
}

func migrateLegacyCredential(database *store.Store, path string) error {
	count, err := database.AccountCount(context.Background())
	if err != nil || count != 0 || path == "" {
		return err
	}
	cred, err := credential.Load(path)
	if err != nil {
		return fmt.Errorf("load legacy credential for migration: %w", err)
	}
	if cred.IsExpired(time.Now()) {
		slog.Warn("access token appears expired; automatic refresh is not enabled", "expires", cred.ExpiresAt)
	}
	_, err = database.ImportAccount(context.Background(), "default", cred)
	if err == nil {
		slog.Info("migrated legacy credential into account database", "account", "default")
	}
	return err
}

func runSignCCH(args []string) error {
	fs := flag.NewFlagSet("sign-cch", flag.ContinueOnError)
	inputPath := fs.String("in", "", "captured Anthropic request body")
	outputPath := fs.String("out", "", "destination for the signed body")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *inputPath == "" || *outputPath == "" {
		return fmt.Errorf("-in and -out are required")
	}
	inputAbsolute, err := filepath.Abs(*inputPath)
	if err != nil {
		return fmt.Errorf("resolve input path: %w", err)
	}
	outputAbsolute, err := filepath.Abs(*outputPath)
	if err != nil {
		return fmt.Errorf("resolve output path: %w", err)
	}
	if inputAbsolute == outputAbsolute {
		return fmt.Errorf("input and output paths must differ")
	}

	body, err := os.ReadFile(inputAbsolute)
	if err != nil {
		return fmt.Errorf("read captured body: %w", err)
	}
	signed, info, err := cch.SignExisting(body)
	if err != nil {
		return err
	}
	if !info.Found {
		return fmt.Errorf("first system block is not a billing attribution block")
	}
	if err := os.MkdirAll(filepath.Dir(outputAbsolute), 0o700); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	if err := os.WriteFile(outputAbsolute, signed, 0o600); err != nil {
		return fmt.Errorf("write signed body: %w", err)
	}
	slog.Info("CCH signing complete", "changed", info.Changed, "output", outputAbsolute)
	return nil
}
