package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/UnownHash/Fletchling/internal/talonconfig"
	"github.com/UnownHash/Fletchling/internal/talondb"
	"github.com/UnownHash/Fletchling/internal/talonflame"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "talonflame:", err)
		os.Exit(1)
	}
}

func run() error {
	path := flag.String("f", "config.toml", "configuration file")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
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
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	analyzer, err := talonflame.New(ctx, db, logger, cfg.Tuning.Analyzer())
	if err != nil {
		return err
	}
	defer analyzer.Close()
	service := talonflame.NewServer(analyzer)
	httpServer := &http.Server{Addr: cfg.HTTP.Addr, Handler: service.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: time.Minute}
	errors := make(chan error, 1)
	go func() { errors <- httpServer.ListenAndServe() }()
	workerCtx, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()
	workerErrors := make(chan error, 1)
	go func() { workerErrors <- service.Run(workerCtx) }()
	workerStopped := false
	logger.WithField("listen", cfg.HTTP.Addr).Info("Talonflame webhook receiver started")
	select {
	case err = <-errors:
	case err = <-workerErrors:
		workerStopped = true
	case <-ctx.Done():
	}
	shutdown, stop := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop()
	if shutdownErr := httpServer.Shutdown(shutdown); shutdownErr != nil {
		httpServer.Close()
		stopWorker()
		if !workerStopped {
			<-workerErrors
		}
		return shutdownErr
	}
	if !workerStopped {
		service.Drain()
		select {
		case workerErr := <-workerErrors:
			if err == nil {
				err = workerErr
			}
		case <-shutdown.Done():
			stopWorker()
			<-workerErrors
			return shutdown.Err()
		}
	}
	logger.Info("Talonflame receiver stopped; accepted webhook queue drained")
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
