package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/local/claude-relay/internal/config"
	"github.com/local/claude-relay/internal/credential"
	"github.com/local/claude-relay/internal/proxy"
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
	fmt.Fprintln(os.Stderr, "  claude-relay import -from <cliproxy-credential.json> [-to data/credentials.json]")
	fmt.Fprintln(os.Stderr, "  claude-relay serve [-config config.json]")
}

func runImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	from := fs.String("from", "", "source CLIProxyAPI Claude credential JSON")
	to := fs.String("to", "data/credentials.json", "destination credential JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == "" {
		return fmt.Errorf("-from is required")
	}

	cred, err := credential.Import(*from, *to)
	if err != nil {
		return err
	}
	slog.Info("credential imported", "destination", *to, "expires", cred.ExpiresAt)
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
	cred, err := credential.Load(cfg.CredentialsFile)
	if err != nil {
		return err
	}
	if cred.IsExpired(time.Now()) {
		slog.Warn("access token appears expired; automatic refresh is not enabled in the baseline build", "expires", cred.ExpiresAt)
	}

	server, err := proxy.NewServer(cfg, cred)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return server.Run(ctx)
}
