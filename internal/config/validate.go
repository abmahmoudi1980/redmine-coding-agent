package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
)

// Validate checks every structural rule that can be checked without contacting
// an external system. Secret references are shape-checked, never resolved.
func (c *Config) Validate() error {
	var problems []error

	if c.SchemaVersion != CurrentSchemaVersion {
		problems = append(problems, fmt.Errorf("schema_version %d is not supported (want %d)", c.SchemaVersion, CurrentSchemaVersion))
	}

	if err := validateListen(c.HTTP.Listen); err != nil {
		problems = append(problems, fmt.Errorf("http.listen: %w", err))
	}

	if strings.TrimSpace(c.Data.Database) == "" {
		problems = append(problems, errors.New("data.database is required"))
	}
	if strings.TrimSpace(c.Data.WorkspaceRoot) == "" {
		problems = append(problems, errors.New("data.workspace_root is required"))
	}
	if strings.TrimSpace(c.Data.LogDir) == "" {
		problems = append(problems, errors.New("data.log_dir is required"))
	}
	if c.Data.RetentionDays < 0 {
		problems = append(problems, errors.New("data.retention_days must be zero or positive"))
	}

	if err := validateBaseURL("redmine.base_url", c.Redmine.BaseURL); err != nil {
		problems = append(problems, err)
	}
	if err := c.Redmine.APIKey.Validate(); err != nil {
		problems = append(problems, fmt.Errorf("redmine.api_key: %w", err))
	}

	if err := validateBaseURL("gitlab.base_url", c.GitLab.BaseURL); err != nil {
		problems = append(problems, err)
	}
	if err := c.GitLab.Token.Validate(); err != nil {
		problems = append(problems, fmt.Errorf("gitlab.token: %w", err))
	}

	if strings.TrimSpace(c.Agent.Adapter) == "" {
		problems = append(problems, errors.New("agent.adapter is required"))
	}
	if strings.TrimSpace(c.Agent.Command) == "" {
		problems = append(problems, errors.New("agent.command is required"))
	} else if _, err := exec.LookPath(c.Agent.Command); err != nil {
		problems = append(problems, fmt.Errorf("agent.command %q is not executable on PATH: %w", c.Agent.Command, err))
	}
	if strings.TrimSpace(c.Agent.Model) == "" {
		problems = append(problems, errors.New("agent.model is required"))
	}
	if c.Agent.TimeoutMinutes <= 0 {
		problems = append(problems, errors.New("agent.timeout_minutes must be positive"))
	}
	if c.Agent.MaxRepairs < 0 {
		problems = append(problems, errors.New("agent.max_repairs must be zero or positive"))
	}

	if c.Concurrency.MaxActiveRuns < 1 {
		problems = append(problems, errors.New("concurrency.max_active_runs must be at least 1"))
	}
	if c.Concurrency.MaxActiveRuns > 10 {
		problems = append(problems, errors.New("concurrency.max_active_runs must not exceed 10"))
	}

	problems = append(problems, c.validateRepositories()...)
	return errors.Join(problems...)
}

func (c *Config) validateRepositories() []error {
	var problems []error
	seen := make(map[string]struct{}, len(c.Repositories))
	defaults := make(map[string]int)

	for i, repo := range c.Repositories {
		prefix := fmt.Sprintf("repositories[%d]", i)
		project := strings.TrimSpace(repo.RedmineProject)
		path := strings.TrimSpace(repo.RepositoryPath)
		if project == "" {
			problems = append(problems, fmt.Errorf("%s.redmine_project is required", prefix))
		}
		if path == "" {
			problems = append(problems, fmt.Errorf("%s.repository_path is required", prefix))
		}
		if strings.Contains(path, "://") {
			problems = append(problems, fmt.Errorf("%s.repository_path must be a GitLab path like group/project, not a URL", prefix))
		}
		if strings.TrimSpace(repo.BaseBranch) == "" {
			problems = append(problems, fmt.Errorf("%s.base_branch is required and is never inferred", prefix))
		}

		key := project + "\x00" + path
		if project != "" && path != "" {
			if _, exists := seen[key]; exists {
				problems = append(problems, fmt.Errorf("%s duplicates the mapping %q -> %q", prefix, project, path))
			}
			seen[key] = struct{}{}
		}
		if repo.IsDefault && project != "" {
			defaults[project]++
			if defaults[project] > 1 {
				problems = append(problems, fmt.Errorf("%s is a second is_default repository for project %q", prefix, project))
			}
		}

		names := make(map[string]struct{}, len(repo.Checks))
		for j, check := range repo.Checks {
			checkPrefix := fmt.Sprintf("%s.checks[%d]", prefix, j)
			name := strings.TrimSpace(check.Name)
			if name == "" {
				problems = append(problems, fmt.Errorf("%s.name is required", checkPrefix))
			} else {
				if _, exists := names[name]; exists {
					problems = append(problems, fmt.Errorf("%s duplicates check name %q", checkPrefix, name))
				}
				names[name] = struct{}{}
			}
			if strings.TrimSpace(check.CommandLinux) == "" {
				problems = append(problems, fmt.Errorf("%s.command_linux is required", checkPrefix))
			}
			if strings.TrimSpace(check.CommandWindows) == "" {
				problems = append(problems, fmt.Errorf("%s.command_windows is required", checkPrefix))
			}
			if check.TimeoutSeconds < 0 {
				problems = append(problems, fmt.Errorf("%s.timeout_seconds must be positive", checkPrefix))
			}
		}
	}
	return problems
}

func validateBaseURL(key, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("%s is required", key)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s is not a valid URL: %w", key, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("%s must use http or https", key)
	}
	if parsed.Host == "" {
		return fmt.Errorf("%s must include a host", key)
	}
	return nil
}

func validateListen(listen string) error {
	if strings.TrimSpace(listen) == "" {
		return errors.New("is required")
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("must be host:port: %w", err)
	}
	_ = host
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return fmt.Errorf("port %q must be between 1 and 65535", port)
	}
	return nil
}
