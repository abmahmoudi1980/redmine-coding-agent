package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/api"
)

// runServe is the long-running worker. Startup order matters: configured
// secrets are registered with the redactor before anything can log them, and
// the recovery sweep freezes interrupted runs before the listener accepts
// traffic (FR-051, FR-056).
func runServe(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	globals := registerGlobalFlags(flags)
	listen := flags.String("listen", "", "override http.listen from the configuration")
	if err := flags.Parse(args); err != nil {
		return exitWith(exitUsage, "serve: %v", err)
	}
	if len(flags.Args()) > 0 {
		return exitWith(exitUsage, "serve: unexpected argument %q", flags.Args()[0])
	}
	level, err := parseLogLevel(globals.logLevel)
	if err != nil {
		return exitWith(exitUsage, "serve: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	worker, err := newApp(ctx, appOptions{
		configPath: globals.configPath,
		stderr:     os.Stderr,
		logLevel:   level,
		seed:       true,
		recover:    true,
		withRunner: true,
		withHealth: true,
	})
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if closed {
			return
		}
		if closeErr := worker.close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "redmine-agent: close worker: %v\n", closeErr)
		}
	}()

	address := worker.config.HTTP.Listen
	if *listen != "" {
		address = *listen
	}
	logger := worker.logger
	// The startup banner is structured on purpose: an operator can grep one
	// line for the version, the port, and the limits this process is running
	// with, and the redaction writer is already on the sink (T137).
	logger.Base().Info("redmine-agent starting",
		"version", version,
		"commit", commit,
		"built_at", builtAt,
		"listen", address,
		"schema_version", worker.config.SchemaVersion,
		"database", worker.config.Data.Database,
		"workspace_root", worker.config.Data.WorkspaceRoot,
		"log_dir", worker.config.Data.LogDir,
		"max_active_runs", worker.config.Concurrency.MaxActiveRuns,
		"agent", worker.config.Agent.Adapter,
		"os", buildInfo(worker.config).OS,
		"arch", buildInfo(worker.config).Arch,
	)
	if host, _, splitErr := net.SplitHostPort(address); splitErr == nil {
		if ip := net.ParseIP(host); host != "" && (ip == nil || !ip.IsLoopback()) {
			logger.Base().Warn("the listener is not bound to loopback; there is no per-user login in v1",
				"listen", address)
		}
	}

	hub := api.NewHub()
	handler := api.NewServer(api.Deps{
		Config:   worker.config,
		Store:    worker.store,
		Logger:   logger,
		Redactor: worker.redactor,
		Redmine:  worker.redmine,
		Runs:     worker.runner,
		Hub:      hub,
		Health:   worker.health,
		Build:    buildInfo(worker.config),
	}).Handler()

	server := &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		logger.Base().Info("shutdown requested: draining the run pipeline", "listen", address)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Stop accepting new connections first (Shutdown closes the listeners
		// before it waits), then cancel the in-flight run through its process
		// group/Job Object, then end event streams, then checkpoint the WAL.
		serverDone := make(chan struct{})
		go func() {
			defer close(serverDone)
			if err := server.Shutdown(shutdownCtx); err != nil {
				logger.Base().Warn("http shutdown did not finish", "error", err)
			}
		}()
		if err := worker.runner.Shutdown(shutdownCtx); err != nil {
			logger.Base().Warn("the active run did not settle before the deadline", "error", err)
		}
		hub.Close()
		<-serverDone
	}

	if err := worker.close(); err != nil {
		logger.Base().Error("store close failed", "error", err)
	}
	closed = true
	logger.Base().Info("redmine-agent stopped cleanly", "version", version)
	return nil
}
