package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/agent"
	"github.com/systemgroup/redmine-coding-agent/internal/api"
	"github.com/systemgroup/redmine-coding-agent/internal/config"
	"github.com/systemgroup/redmine-coding-agent/internal/git"
	"github.com/systemgroup/redmine-coding-agent/internal/gitlab"
	"github.com/systemgroup/redmine-coding-agent/internal/logging"
	"github.com/systemgroup/redmine-coding-agent/internal/redmine"
	"github.com/systemgroup/redmine-coding-agent/internal/runner"
	"github.com/systemgroup/redmine-coding-agent/internal/store"
	"github.com/systemgroup/redmine-coding-agent/internal/validate"
)

// runServe is the long-running worker. Startup order matters: configured
// secrets are registered with the redactor before anything can log them, and
// the recovery sweep freezes interrupted runs before the listener accepts
// traffic (FR-051, FR-056).
func runServe(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := flags.String("config", "", "path to config.yaml (defaults to $"+config.EnvConfigPath+" or config.yaml)")
	if err := flags.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	redactor := logging.NewRedactor()
	logger := logging.New(logging.Options{LogDir: cfg.Data.LogDir, Redactor: redactor})
	if err := registerSecrets(cfg, redactor); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.Data.Database, store.WithRedactor(redactor))
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			logger.Base().Error("store close failed", "error", err)
		}
	}()

	if _, err := st.SeedRepositories(ctx, cfg); err != nil {
		return fmt.Errorf("seed repositories from configuration: %w", err)
	}

	redmineClient, err := redmine.New(redmine.Options{
		BaseURL:  cfg.Redmine.BaseURL,
		APIKey:   cfg.Redmine.APIKey,
		Redactor: redactor,
		Logger:   logger.Base(),
	})
	if err != nil {
		return err
	}
	gitlabClient, err := gitlab.New(gitlab.Options{
		BaseURL:  cfg.GitLab.BaseURL,
		Token:    cfg.GitLab.Token,
		Redactor: redactor,
		Logger:   logger.Base(),
	})
	if err != nil {
		return err
	}

	registry := agent.NewRegistry()
	adapter, err := agent.NewOpenCode(agent.Config{
		Command:       cfg.Agent.Command,
		Model:         cfg.Agent.Model,
		AgentName:     cfg.Agent.Profile,
		WorkspaceRoot: cfg.Data.WorkspaceRoot,
		Redactor:      redactor,
	})
	if err != nil {
		return err
	}
	if err := registry.Register(adapter); err != nil {
		return err
	}

	pipeline, err := runner.New(runner.Options{
		Config:    cfg,
		Store:     st,
		Git:       git.NewManager("", cfg.Data.WorkspaceRoot),
		Adapters:  registry,
		Redmine:   redmineClient,
		GitLab:    gitlabClient,
		Logger:    logger,
		Validator: validate.New(validate.Options{Redactor: redactor}),
	})
	if err != nil {
		return err
	}

	if report, err := pipeline.RecoverInterrupted(ctx); err != nil {
		return err
	} else if len(report.Interrupted) > 0 {
		logger.Base().Warn("interrupted runs recovered before serving",
			"count", len(report.Interrupted))
	}

	handler := api.NewServer(api.Deps{
		Config:   cfg,
		Store:    st,
		Logger:   logger,
		Redactor: redactor,
		Redmine:  redmineClient,
		Runs:     pipeline,
		Build: api.BuildInfo{
			Version:       version,
			Commit:        commit,
			BuiltAt:       builtAt,
			OS:            runtime.GOOS,
			Arch:          runtime.GOARCH,
			SchemaVersion: cfg.SchemaVersion,
		},
	}).Handler()

	server := &http.Server{
		Addr:              cfg.HTTP.Listen,
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
		logger.Base().Info("shutdown requested", "listen", cfg.HTTP.Listen)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Base().Warn("http shutdown did not finish", "error", err)
		}
	}

	pipeline.Wait()
	return nil
}

// registerSecrets resolves every configured credential once and registers its
// value with the redactor, so a later log, report, prompt, or response can
// never carry it (FR-056, SC-013).
func registerSecrets(cfg *config.Config, redactor *logging.Redactor) error {
	if redactor == nil {
		return nil
	}
	for name, reference := range map[string]config.SecretRef{
		"redmine.api_key": cfg.Redmine.APIKey,
		"gitlab.token":    cfg.GitLab.Token,
	} {
		if !reference.IsSet() {
			continue
		}
		value, err := reference.Resolve()
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		redactor.Register(value)
	}
	return nil
}
