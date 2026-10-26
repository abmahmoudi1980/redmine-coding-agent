// Package config loads, validates, and redacts the operator's YAML file.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// EnvConfigPath names the environment variable used when --config is absent.
const EnvConfigPath = "REDMINE_AGENT_CONFIG"

// DefaultPath is the last-resort configuration location.
const DefaultPath = "config.yaml"

// CurrentSchemaVersion is the only schema this build understands.
const CurrentSchemaVersion = 1

type Config struct {
	SchemaVersion int          `yaml:"schema_version" json:"schemaVersion"`
	HTTP          HTTP         `yaml:"http" json:"http"`
	Data          Data         `yaml:"data" json:"data"`
	Redmine       Redmine      `yaml:"redmine" json:"redmine"`
	GitLab        GitLab       `yaml:"gitlab" json:"gitlab"`
	Repositories  []Repository `yaml:"repositories" json:"repositories"`
	Agent         Agent        `yaml:"agent" json:"agent"`
	Concurrency   Concurrency  `yaml:"concurrency" json:"concurrency"`

	// SourcePath and LoadedAt are runtime facts, not configuration.
	SourcePath string    `yaml:"-" json:"-"`
	LoadedAt   time.Time `yaml:"-" json:"-"`
}

type HTTP struct {
	Listen string `yaml:"listen" json:"listen"`
}

type Data struct {
	Database      string `yaml:"database" json:"database"`
	WorkspaceRoot string `yaml:"workspace_root" json:"workspaceRoot"`
	LogDir        string `yaml:"log_dir" json:"logDir"`
	RetentionDays int    `yaml:"retention_days" json:"retentionDays"`
}

type Redmine struct {
	BaseURL         string    `yaml:"base_url" json:"baseUrl"`
	APIKey          SecretRef `yaml:"api_key" json:"apiKey"`
	CommentOnFinish bool      `yaml:"comment_on_finish" json:"commentOnFinish"`
	UpdateStatus    bool      `yaml:"update_status" json:"updateStatus"`
}

type GitLab struct {
	BaseURL             string    `yaml:"base_url" json:"baseUrl"`
	Token               SecretRef `yaml:"token" json:"token"`
	PushEnabled         bool      `yaml:"push_enabled" json:"pushEnabled"`
	MergeRequestEnabled bool      `yaml:"merge_request_enabled" json:"mergeRequestEnabled"`
}

type Repository struct {
	RedmineProject      string  `yaml:"redmine_project" json:"redmineProject"`
	RepositoryPath      string  `yaml:"repository_path" json:"repositoryPath"`
	DisplayName         string  `yaml:"display_name" json:"displayName"`
	BaseBranch          string  `yaml:"base_branch" json:"baseBranch"`
	IsDefault           bool    `yaml:"is_default" json:"isDefault"`
	PublishEnabled      *bool   `yaml:"publish_enabled" json:"publishEnabled,omitempty"`
	MergeRequestEnabled *bool   `yaml:"merge_request_enabled" json:"mergeRequestEnabled,omitempty"`
	Enabled             *bool   `yaml:"enabled" json:"enabled,omitempty"`
	Checks              []Check `yaml:"checks" json:"checks"`
}

// PublishEnabledFor resolves the per-repository override against the global policy.
func (r Repository) PublishEnabledFor(global bool) bool {
	if r.PublishEnabled == nil {
		return global
	}
	return *r.PublishEnabled
}

// MergeRequestEnabledFor resolves the per-repository override against the global policy.
func (r Repository) MergeRequestEnabledFor(global bool) bool {
	if r.MergeRequestEnabled == nil {
		return global
	}
	return *r.MergeRequestEnabled
}

// IsEnabled defaults to true when the key is absent.
func (r Repository) IsEnabled() bool {
	if r.Enabled == nil {
		return true
	}
	return *r.Enabled
}

type Check struct {
	Name           string `yaml:"name" json:"name"`
	CommandLinux   string `yaml:"command_linux" json:"commandLinux"`
	CommandWindows string `yaml:"command_windows" json:"commandWindows"`
	Required       *bool  `yaml:"required" json:"required,omitempty"`
	TimeoutSeconds int    `yaml:"timeout_seconds" json:"timeoutSeconds"`
}

// IsRequired defaults to true: silence must not weaken validation.
func (c Check) IsRequired() bool {
	if c.Required == nil {
		return true
	}
	return *c.Required
}

// EffectiveTimeoutSeconds defaults to 900 seconds.
func (c Check) EffectiveTimeoutSeconds() int {
	if c.TimeoutSeconds <= 0 {
		return 900
	}
	return c.TimeoutSeconds
}

type Agent struct {
	Adapter        string `yaml:"adapter" json:"adapter"`
	Command        string `yaml:"command" json:"command"`
	Model          string `yaml:"model" json:"model"`
	Profile        string `yaml:"profile" json:"profile"`
	TimeoutMinutes int    `yaml:"timeout_minutes" json:"timeoutMinutes"`
	MaxRepairs     int    `yaml:"max_repairs" json:"maxRepairs"`
}

type Concurrency struct {
	MaxActiveRuns int `yaml:"max_active_runs" json:"maxActiveRuns"`
}

// Load reads, strictly decodes, defaults, and validates a configuration file.
// An empty path falls back to $REDMINE_AGENT_CONFIG and then ./config.yaml.
func Load(path string) (*Config, error) {
	if path == "" {
		path = os.Getenv(EnvConfigPath)
	}
	if path == "" {
		path = DefaultPath
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("config: resolve %s: %w", path, err)
	}
	// #nosec G703 -- the path is the operator-supplied --config (or the documented default).
	f, err := os.Open(abs)
	if err != nil {
		return nil, fmt.Errorf("config: open %s: %w", abs, err)
	}
	defer f.Close()

	cfg, err := Decode(f)
	if err != nil {
		return nil, fmt.Errorf("config: %s: %w", abs, err)
	}
	cfg.SourcePath = abs
	cfg.LoadedAt = time.Now().UTC()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: %s: %w", abs, err)
	}
	return cfg, nil
}

// Decode strict-decodes one YAML document. Unknown keys are an error.
func Decode(r io.Reader) (*Config, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, fmt.Errorf("decode: %w", err)
		}
		return nil, errors.New("decode: exactly one YAML document is allowed")
	}

	if cfg.SchemaVersion != CurrentSchemaVersion {
		return nil, fmt.Errorf("schema_version %d is not supported (want %d)", cfg.SchemaVersion, CurrentSchemaVersion)
	}
	cfg.applyDefaults()
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Agent.Command == "" {
		c.Agent.Command = "opencode"
	}
	if c.HTTP.Listen == "" {
		c.HTTP.Listen = "127.0.0.1:8080"
	}
	if c.Agent.TimeoutMinutes <= 0 {
		c.Agent.TimeoutMinutes = 45
	}
	if c.Concurrency.MaxActiveRuns <= 0 {
		c.Concurrency.MaxActiveRuns = 1
	}
	if c.Data.RetentionDays <= 0 {
		c.Data.RetentionDays = 7
	}
	for i := range c.Repositories {
		for j := range c.Repositories[i].Checks {
			if c.Repositories[i].Checks[j].TimeoutSeconds <= 0 {
				c.Repositories[i].Checks[j].TimeoutSeconds = 900
			}
		}
	}
}

// RedactedDocument returns the configuration as a JSON-safe value where every
// secret is only its reference. Used by /api/config and configuration_profiles.
func (c *Config) RedactedDocument() map[string]any {
	doc := map[string]any{
		"schemaVersion": c.SchemaVersion,
		"http":          c.HTTP,
		"data":          c.Data,
		"redmine": map[string]any{
			"baseUrl":         c.Redmine.BaseURL,
			"apiKey":          c.Redmine.APIKey.Reference(),
			"commentOnFinish": c.Redmine.CommentOnFinish,
			"updateStatus":    c.Redmine.UpdateStatus,
		},
		"gitlab": map[string]any{
			"baseUrl":             c.GitLab.BaseURL,
			"token":               c.GitLab.Token.Reference(),
			"pushEnabled":         c.GitLab.PushEnabled,
			"mergeRequestEnabled": c.GitLab.MergeRequestEnabled,
		},
		"repositories": c.Repositories,
		"agent":        c.Agent,
		"concurrency":  c.Concurrency,
	}
	return doc
}

// ContentHash identifies the effective non-secret configuration. Two runs with
// the same hash were produced by the same settings (FR-048).
func (c *Config) ContentHash() string {
	encoded, err := json.Marshal(c.RedactedDocument())
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
