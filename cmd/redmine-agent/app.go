package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strings"

	"github.com/systemgroup/redmine-coding-agent/internal/agent"
	"github.com/systemgroup/redmine-coding-agent/internal/api"
	"github.com/systemgroup/redmine-coding-agent/internal/config"
	"github.com/systemgroup/redmine-coding-agent/internal/git"
	"github.com/systemgroup/redmine-coding-agent/internal/gitlab"
	"github.com/systemgroup/redmine-coding-agent/internal/health"
	"github.com/systemgroup/redmine-coding-agent/internal/logging"
	"github.com/systemgroup/redmine-coding-agent/internal/redmine"
	"github.com/systemgroup/redmine-coding-agent/internal/runner"
	"github.com/systemgroup/redmine-coding-agent/internal/store"
	"github.com/systemgroup/redmine-coding-agent/internal/validate"
)

// appOptions describes how much of the worker a command needs. Every front
// door (HTTP, CLI) is assembled from the same collaborators, so a CLI command
// can never bypass the validation or policy the server applies.
type appOptions struct {
	configPath string
	stderr     io.Writer
	logLevel   slog.Level
	seed       bool
	recover    bool
	eventSink  store.EventSink
	withRunner bool
	withHealth bool
}

// app is one assembled worker process.
type app struct {
	config   *config.Config
	redactor *logging.Redactor
	logger   *logging.Logger
	store    *store.Store
	redmine  *redmine.Client
	gitlab   *gitlab.Client
	git      *git.Manager
	adapters *agent.Registry
	health   *health.Checker
	runner   *runner.Runner
	seeded   store.SeedResult
}

// newApp is the single composition root. Secrets are registered with the
// redactor before any client can log them (FR-056, SC-013).
func newApp(ctx context.Context, opts appOptions) (*app, error) {
	cfg, err := config.Load(opts.configPath)
	if err != nil {
		return nil, &exitError{code: exitConfig, err: err}
	}

	redactor := logging.NewRedactor()
	logger := logging.New(logging.Options{
		LogDir:   cfg.Data.LogDir,
		Redactor: redactor,
		Stderr:   opts.stderr,
		Level:    opts.logLevel,
	})
	if err := registerSecrets(cfg, redactor); err != nil {
		return nil, &exitError{code: exitConfig, err: err}
	}

	st, err := store.Open(ctx, cfg.Data.Database, store.WithRedactor(redactor), store.WithEventSink(opts.eventSink))
	if err != nil {
		return nil, &exitError{code: exitInternal, err: fmt.Errorf("open database %s: %w", cfg.Data.Database, err)}
	}

	assembled := &app{
		config:   cfg,
		redactor: redactor,
		logger:   logger,
		store:    st,
	}

	cleanup := true
	defer func() {
		if cleanup {
			_ = st.Close()
		}
	}()

	if opts.seed {
		seeded, err := st.SeedConfiguration(ctx, cfg)
		if err != nil {
			return nil, &exitError{code: exitInternal, err: fmt.Errorf("seed repositories from configuration: %w", err)}
		}
		assembled.seeded = seeded
		if len(seeded.Deactivated) > 0 {
			logger.Base().Warn("repository mappings removed from configuration were deactivated",
				"repositories", seeded.Deactivated)
		}
	}

	redmineClient, err := redmine.New(redmine.Options{
		BaseURL:  cfg.Redmine.BaseURL,
		APIKey:   cfg.Redmine.APIKey,
		Redactor: redactor,
		Logger:   logger.Base(),
	})
	if err != nil {
		return nil, &exitError{code: exitConfig, err: err}
	}
	assembled.redmine = redmineClient

	// GitLab is optional: an all-local deployment runs without it, and the
	// pipeline never contacts a GitLab server for a local repository path.
	var gitlabClient *gitlab.Client
	if strings.TrimSpace(cfg.GitLab.BaseURL) != "" {
		gitlabClient, err = gitlab.New(gitlab.Options{
			BaseURL:  cfg.GitLab.BaseURL,
			Token:    cfg.GitLab.Token,
			Redactor: redactor,
			Logger:   logger.Base(),
		})
		if err != nil {
			return nil, &exitError{code: exitConfig, err: err}
		}
	}
	assembled.gitlab = gitlabClient

	registry := agent.NewRegistry()
	adapter, err := agent.NewOpenCode(agent.Config{
		Command:       cfg.Agent.Command,
		Model:         cfg.Agent.Model,
		AgentName:     cfg.Agent.Profile,
		WorkspaceRoot: cfg.Data.WorkspaceRoot,
		Redactor:      redactor,
	})
	if err != nil {
		return nil, &exitError{code: exitConfig, err: err}
	}
	if err := registry.Register(adapter); err != nil {
		return nil, &exitError{code: exitConfig, err: err}
	}
	assembled.adapters = registry

	gitManager := git.NewManager("", cfg.Data.WorkspaceRoot)
	if token, tokenErr := cfg.GitLab.Token.Resolve(); tokenErr == nil && token != "" {
		gitManager.SetHTTPAuth("oauth2", token)
	}
	assembled.git = gitManager

	if opts.withHealth {
		assembled.health = health.New(health.Deps{
			Store:    st,
			Config:   cfg,
			Redmine:  redmineClient,
			GitLab:   gitlabClient,
			Git:      gitManager,
			Adapters: registry,
			Redactor: redactor,
		})
	}

	if opts.withRunner {
		pipeline, err := runner.New(runner.Options{
			Config:    cfg,
			Store:     st,
			Git:       gitManager,
			Adapters:  registry,
			Redmine:   redmineClient,
			GitLab:    gitlabClient,
			Logger:    logger,
			Validator: validate.New(validate.Options{Redactor: redactor}),
		})
		if err != nil {
			return nil, &exitError{code: exitInternal, err: err}
		}
		assembled.runner = pipeline

		if opts.recover {
			report, err := pipeline.RecoverInterrupted(ctx)
			if err != nil {
				return nil, &exitError{code: exitInternal, err: fmt.Errorf("recover interrupted runs: %w", err)}
			}
			if len(report.Interrupted) > 0 {
				logger.Base().Warn("interrupted runs recovered before serving",
					"count", len(report.Interrupted))
			}
		}
	}

	cleanup = false
	return assembled, nil
}

// close settles the pipeline and closes the database, checkpointing WAL.
func (a *app) close() error {
	if a == nil {
		return nil
	}
	var closeErr error
	if a.runner != nil {
		a.runner.Wait()
	}
	if a.store != nil {
		closeErr = a.store.Close()
	}
	if a.logger != nil {
		if err := a.logger.Flush(); err != nil && closeErr == nil {
			closeErr = err
		}
	}
	return closeErr
}

// documents exposes the exact JSON shapes the HTTP API returns, so the CLI has
// one contract instead of two (contracts/cli.md §Machine output).
func (a *app) documents() *api.Server {
	return api.NewServer(api.Deps{
		Config:   a.config,
		Store:    a.store,
		Logger:   a.logger,
		Redactor: a.redactor,
		Redmine:  a.redmine,
		Runs:     a.runner,
		Health:   a.health,
		Build:    buildInfo(a.config),
	})
}

func buildInfo(cfg *config.Config) api.BuildInfo {
	schemaVersion := 0
	if cfg != nil {
		schemaVersion = cfg.SchemaVersion
	}
	return api.BuildInfo{
		Version:       version,
		Commit:        commit,
		BuiltAt:       builtAt,
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		SchemaVersion: schemaVersion,
	}
}

// registerSecrets resolves every configured credential once and registers its
// value with the redactor, so a later log, report, prompt, or response can
// never carry it (FR-056, SC-013).
func registerSecrets(cfg *config.Config, redactor *logging.Redactor) error {
	if redactor == nil || cfg == nil {
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

// parseLogLevel maps the documented --log-level values to slog levels.
func parseLogLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("--log-level must be one of debug, info, warn, error")
	}
}

// operatorName is recorded with every run the CLI starts.
func operatorName() string {
	for _, key := range []string{"REDMINE_AGENT_OPERATOR", "USER", "USERNAME"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return "cli"
}
