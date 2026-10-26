CREATE TABLE configuration_profiles (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  content_hash TEXT NOT NULL UNIQUE,
  schema_version INTEGER NOT NULL,
  source_path TEXT NOT NULL,
  content TEXT NOT NULL,
  loaded_at TEXT NOT NULL
);

CREATE TABLE repositories (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  redmine_project TEXT NOT NULL,
  repository_path TEXT NOT NULL,
  display_name TEXT NOT NULL DEFAULT '',
  base_branch TEXT NOT NULL CHECK (length(trim(base_branch)) > 0),
  is_default INTEGER NOT NULL DEFAULT 0 CHECK (is_default IN (0, 1)),
  publish_enabled INTEGER NOT NULL DEFAULT 1 CHECK (publish_enabled IN (0, 1)),
  merge_request_enabled INTEGER NOT NULL DEFAULT 1 CHECK (merge_request_enabled IN (0, 1)),
  enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE (redmine_project, repository_path)
);

CREATE UNIQUE INDEX ux_repo_default ON repositories(redmine_project) WHERE is_default = 1;
CREATE INDEX ix_repo_project ON repositories(redmine_project);

CREATE TABLE repository_checks (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  repository_id INTEGER NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  command_linux TEXT NOT NULL,
  command_windows TEXT NOT NULL,
  required INTEGER NOT NULL DEFAULT 1 CHECK (required IN (0, 1)),
  timeout_seconds INTEGER NOT NULL DEFAULT 900 CHECK (timeout_seconds > 0),
  position INTEGER NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE (repository_id, name)
);

CREATE TABLE issue_snapshots (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  redmine_issue_id INTEGER NOT NULL,
  subject TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  acceptance_criteria TEXT NOT NULL DEFAULT '',
  criteria_source TEXT NOT NULL DEFAULT 'none' CHECK (criteria_source IN ('heading', 'none')),
  project_id TEXT NOT NULL,
  project_identifier TEXT NOT NULL,
  project_name TEXT NOT NULL,
  status_name TEXT NOT NULL DEFAULT '',
  tracker_name TEXT NOT NULL DEFAULT '',
  priority_name TEXT NOT NULL DEFAULT '',
  assignee_name TEXT NOT NULL DEFAULT '',
  parent_issue_id INTEGER,
  related_issue_ids TEXT NOT NULL DEFAULT '[]',
  custom_fields TEXT NOT NULL DEFAULT '[]',
  selected_comments TEXT NOT NULL DEFAULT '[]',
  attachments TEXT NOT NULL DEFAULT '[]',
  raw_json TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  fetched_at TEXT NOT NULL
);

CREATE INDEX ix_snapshots_issue ON issue_snapshots(redmine_issue_id);

CREATE TABLE runs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  public_id TEXT NOT NULL UNIQUE,
  issue_id INTEGER NOT NULL,
  issue_snapshot_id INTEGER NOT NULL REFERENCES issue_snapshots(id),
  repository_id INTEGER REFERENCES repositories(id),
  previous_run_id INTEGER REFERENCES runs(id),
  state TEXT NOT NULL CHECK (state IN ('queued', 'preparing', 'implementing', 'validating', 'repairing', 'publishing', 'reporting', 'completed', 'failed', 'blocked', 'cancelled', 'interrupted')),
  stage TEXT NOT NULL DEFAULT 'created',
  base_branch TEXT,
  base_revision TEXT,
  branch TEXT,
  commit_sha TEXT,
  pushed_at TEXT,
  agent_name TEXT,
  agent_model TEXT,
  agent_config_id TEXT,
  config_profile_id INTEGER REFERENCES configuration_profiles(id),
  requested_by TEXT,
  created_at TEXT NOT NULL,
  started_at TEXT,
  ended_at TEXT,
  cancel_requested_at TEXT,
  interrupted_stage TEXT,
  failure_code TEXT,
  failure_message TEXT,
  summary TEXT,
  CHECK ((state IN ('completed', 'failed', 'blocked', 'cancelled', 'interrupted')) = (ended_at IS NOT NULL)),
  CHECK (state <> 'cancelled' OR cancel_requested_at IS NOT NULL),
  CHECK (state <> 'interrupted' OR interrupted_stage IS NOT NULL)
);

CREATE UNIQUE INDEX ux_runs_active_issue ON runs(issue_id)
  WHERE state IN ('queued', 'preparing', 'implementing', 'repairing', 'validating', 'publishing', 'reporting');
CREATE INDEX ix_runs_created_at ON runs(created_at DESC);
CREATE INDEX ix_runs_state ON runs(state);
CREATE INDEX ix_runs_previous ON runs(previous_run_id);

CREATE TABLE workspaces (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id INTEGER NOT NULL UNIQUE REFERENCES runs(id) ON DELETE CASCADE,
  path TEXT NOT NULL,
  git_dir TEXT NOT NULL,
  branch TEXT NOT NULL,
  base_revision TEXT NOT NULL,
  created_at TEXT NOT NULL,
  cleaned_at TEXT,
  has_unpushed_changes INTEGER NOT NULL DEFAULT 0 CHECK (has_unpushed_changes IN (0, 1)),
  changed_paths TEXT NOT NULL DEFAULT '[]'
);

CREATE TABLE run_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  seq INTEGER NOT NULL,
  at TEXT NOT NULL,
  type TEXT NOT NULL CHECK (type IN ('state', 'stage', 'activity', 'check', 'agent', 'workspace', 'publish', 'report', 'warning', 'error')),
  severity TEXT NOT NULL DEFAULT 'info' CHECK (severity IN ('info', 'warn', 'error')),
  message TEXT NOT NULL,
  data TEXT NOT NULL DEFAULT '{}',
  UNIQUE (run_id, seq)
);

CREATE INDEX ix_run_events_run_seq ON run_events(run_id, seq);

CREATE TABLE agent_invocations (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  attempt_no INTEGER NOT NULL CHECK (attempt_no >= 1),
  kind TEXT NOT NULL CHECK (kind IN ('implement', 'repair')),
  adapter TEXT NOT NULL,
  model TEXT NOT NULL,
  config_id TEXT NOT NULL,
  prompt_path TEXT NOT NULL,
  prompt_sha256 TEXT NOT NULL,
  transcript_path TEXT NOT NULL,
  started_at TEXT,
  ended_at TEXT,
  exit_code INTEGER,
  timed_out INTEGER NOT NULL DEFAULT 0 CHECK (timed_out IN (0, 1)),
  status TEXT NOT NULL CHECK (status IN ('running', 'succeeded', 'failed', 'timed_out', 'cancelled', 'blocked')),
  reported_status TEXT,
  result_summary TEXT,
  UNIQUE (run_id, attempt_no)
);

CREATE INDEX ix_invocations_run ON agent_invocations(run_id, attempt_no);

CREATE TABLE validation_results (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  invocation_id INTEGER REFERENCES agent_invocations(id) ON DELETE SET NULL,
  check_name TEXT NOT NULL,
  command TEXT NOT NULL,
  required INTEGER NOT NULL CHECK (required IN (0, 1)),
  outcome TEXT NOT NULL CHECK (outcome IN ('passed', 'failed', 'not_executed')),
  exit_code INTEGER,
  duration_ms INTEGER,
  output_excerpt TEXT,
  reason TEXT,
  executed_at TEXT,
  CHECK (outcome = 'not_executed' OR exit_code IS NOT NULL),
  CHECK (outcome <> 'passed' OR exit_code = 0),
  CHECK (outcome <> 'not_executed' OR (reason IS NOT NULL AND length(trim(reason)) > 0)),
  UNIQUE (run_id, invocation_id, check_name)
);

CREATE INDEX ix_validation_run ON validation_results(run_id);

CREATE TABLE reports (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id INTEGER NOT NULL UNIQUE REFERENCES runs(id) ON DELETE CASCADE,
  body_md TEXT NOT NULL,
  commit_sha TEXT,
  merge_request_url TEXT,
  delivery_status TEXT NOT NULL CHECK (delivery_status IN ('pending', 'delivered', 'failed', 'skipped')),
  delivery_error TEXT,
  dedupe_key TEXT NOT NULL,
  created_at TEXT NOT NULL,
  delivered_at TEXT
);
