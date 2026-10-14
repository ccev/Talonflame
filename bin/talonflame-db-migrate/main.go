package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/UnownHash/Fletchling/internal/talonconfig"
	"github.com/UnownHash/Fletchling/internal/talondb"
	"github.com/UnownHash/Fletchling/internal/talonmigration"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "talonflame-db-migrate:", err)
		os.Exit(1)
	}
}

func run() error {
	path := flag.String("f", "config.toml", "configuration file")
	mode := flag.String("mode", "all", "schema, backfill, or all")
	apply := flag.Bool("apply", false, "apply changes (default: read-only preview)")
	flag.Parse()
	if flag.NArg() != 0 || (*mode != "schema" && *mode != "backfill" && *mode != "all") {
		return fmt.Errorf("expected -mode schema|backfill|all and no positional arguments")
	}
	cfg, err := talonconfig.Load(*path)
	if err != nil {
		return err
	}
	logger := cfg.Logger()
	db, err := talondb.Open(cfg.Database)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return talonmigration.Run(ctx, conn, logger, talonmigration.Options{
		Mode: *mode, Apply: *apply, BatchSize: cfg.Tuning.BackfillBatchSize,
	})
}
