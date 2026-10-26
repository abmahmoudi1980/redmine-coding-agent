# Phase 1 Data Model — RedmineCodingAgent

**Feature**: `010-redmine-coding-agent` | **Plan**: [plan.md](./plan.md) | **Date**: 2026-10-26

SQLite (WAL, `foreign_keys=on`), migrated by embedded numbered SQL. Timestamps are UTC RFC 3339 with milliseconds (`2026-10-26T09:41:07.123Z`). Money and floats do not appear; durations are integer milliseconds. Every table below is created by `0001_init.sql`; later changes are additive migrations.

---

## 1. Entity map

```text
repositories 1───n repository_checks
      │
      │ (resolved mapping for a run)
      ▼
runs 1───1 issue_snapshots          (immutable copy of the issue at start)
runs 1───1 workspaces               (per-run working copy facts)
runs 1───n run_events               (append-only timeline; drives SSE + audit)
runs 1───n agent_invocations        (attempt 1 = implementation, 2..n = repairs)
runs 1───n validation_results       (one row per configured check per attempt)
runs 1───0..1 reports               (final report + Redmine delivery state)
runs 0..1──1 runs                   (self-link: retry → previous attempt)
runs n───1 configuration_profiles   (effective non-secret config snapshot)
```

---

## 2. `repositories` — Repository Mapping

| Column | Type | Constraints | Notes |
|---|---|---|---|
| `id` | INTEGER | PK | internal |
| `redmine_project` | TEXT | NOT NULL | Redmine project **identifier** (slug), not the numeric id |
| `repository_path` | TEXT | NOT NULL | GitLab path with namespace, e.g. `group/sub/project` (URL-encoded when called) |
| `display_name` | TEXT | | shown in the picker |
| `base_branch` | TEXT | NOT NULL | never inferred from the repo default (spec Assumptions) |
| `is_default` | INTEGER | NOT NULL DEFAULT 0, CHECK IN (0,1) | partial unique below |
| `publish_enabled` | INTEGER | NOT NULL DEFAULT 1 | per-repo override of the global policy |
| `merge_request_enabled` | INTEGER | NOT NULL DEFAULT 1 | FR-042 |
| `enabled` | INTEGER | NOT NULL DEFAULT 1 | disable without deleting history |
| `created_at` / `updated_at` | TEXT | NOT NULL | |

**Rules**: `base_branch` must be non-empty. A project may map to many repositories, but at most one may be `is_default = 1` per project → `CREATE UNIQUE INDEX ux_repo_default ON repositories(redmine_project) WHERE is_default = 1;`. Resolution: default if exactly one candidate or a default exists; otherwise the run is **blocked** with the candidate list (FR-014, FR-015) — never guessed.

**Source of truth**: this table is seeded from `config.yaml` at startup (upsert by `redmine_project` + `repository_path`); configuration remains the operator's interface (FR-055), the table exists so history survives config edits and runs can reference a stable `repository_id`.

---

## 3. `repository_checks` — configured validation commands

| Column | Type | Constraints | Notes |
|---|---|---|---|
| `id` | INTEGER | PK | |
| `repository_id` | INTEGER | NOT NULL, FK → repositories ON DELETE CASCADE | |
| `name` | TEXT | NOT NULL | e.g. `unit-tests` |
| `command_linux` | TEXT | NOT NULL | executed via `sh -c` |
| `command_windows` | TEXT | NOT NULL | executed via `cmd.exe /c` |
| `required` | INTEGER | NOT NULL DEFAULT 1 | FR-035 |
| `timeout_seconds` | INTEGER | NOT NULL DEFAULT 900, CHECK > 0 | per-check timeout |
| `position` | INTEGER | NOT NULL | execution order |
| `created_at` | TEXT | NOT NULL | |

**Rules**: unique `(repository_id, name)`. Commands are **trusted configuration**, distinct from untrusted issue content (spec Assumptions). An empty command list is legal but means every run ends `validation_failed` with `no_checks_configured` when publishing is enabled — surfaced by `doctor` as a warning.

---

## 4. `issue_snapshots` — immutable issue copy

| Column | Type | Constraints | Notes |
|---|---|---|---|
| `id` | INTEGER | PK | |
| `redmine_issue_id` | INTEGER | NOT NULL | the number the operator typed |
| `subject` | TEXT | NOT NULL | |
| `description` | TEXT | NOT NULL DEFAULT '' | full raw text |
| `acceptance_criteria` | TEXT | | extracted by heading heuristic; may be empty |
| `criteria_source` | TEXT | NOT NULL DEFAULT 'none', CHECK IN ('heading','none') | provenance, so the UI can warn honestly |
| `project_id` / `project_identifier` / `project_name` | TEXT | NOT NULL | |
| `status_name` / `tracker_name` / `priority_name` | TEXT | | |
| `assignee_name` | TEXT | | |
| `parent_issue_id` | INTEGER | | |
| `related_issue_ids` | TEXT | NOT NULL DEFAULT '[]' | JSON array |
| `custom_fields` | TEXT | NOT NULL DEFAULT '[]' | JSON array of `{id,name,value}` |
| `selected_comments` | TEXT | NOT NULL DEFAULT '[]' | JSON array `{id,author,created_on,notes}` |
| `attachments` | TEXT | NOT NULL DEFAULT '[]' | JSON array `{id,filename,content_url,size,available}` — `available=false` when not retrieved (FR-007) |
| `raw_json` | TEXT | NOT NULL | exact API payload, for audit |
| `content_hash` | TEXT | NOT NULL | sha256 of `subject+description+selected_comments` |
| `fetched_at` | TEXT | NOT NULL | |

**Rules**: never updated after insert (reporting a retry takes a *new* snapshot). `acceptance_criteria` is only populated when a recognisable heading exists (`## Acceptance Criteria`, `پذیرش`, `معیار پذیرش`, `AC:`); otherwise `criteria_source='none'` and the operator sees the warning required by the spec's edge case. `raw_json` is stored because later issue edits must not rewrite history.

---

## 5. `runs` — one attempt at one issue

| Column | Type | Constraints | Notes |
|---|---|---|---|
| `id` | INTEGER | PK | |
| `public_id` | TEXT | NOT NULL UNIQUE | UUIDv7, shown to operators |
| `issue_id` | INTEGER | NOT NULL | Redmine number |
| `issue_snapshot_id` | INTEGER | NOT NULL, FK | |
| `repository_id` | INTEGER | FK → repositories | NULL only while resolving |
| `previous_run_id` | INTEGER | FK → runs(id) | retry lineage (FR-052) |
| `state` | TEXT | NOT NULL, CHECK in enum | see §9 |
| `stage` | TEXT | NOT NULL DEFAULT 'created' | last entered stage (FR-050) |
| `base_branch` | TEXT | | copied from mapping at start |
| `base_revision` | TEXT | | `git rev-parse` of the base at worktree creation (FR-019) |
| `branch` | TEXT | | branch created for this run |
| `commit_sha` | TEXT | | set only when a commit was created (FR-038) |
| `pushed_at` | TEXT | | non-null ⇒ push happened |
| `agent_name` | TEXT | | e.g. `opencode` |
| `agent_model` | TEXT | | `provider/model` |
| `agent_config_id` | TEXT | | hash of the effective agent policy |
| `config_profile_id` | INTEGER | FK → configuration_profiles | |
| `requested_by` | TEXT | | operator identity as supplied (header/CLI), no auth in v1 |
| `created_at` | TEXT | NOT NULL | |
| `started_at` | TEXT | | |
| `ended_at` | TEXT | | non-null iff terminal |
| `cancel_requested_at` | TEXT | | distinguishes "cancelled" from "failed" (FR-013) |
| `interrupted_stage` | TEXT | | set by the startup sweep only (FR-051) |
| `failure_code` | TEXT | | enum, see §10 |
| `failure_message` | TEXT | | redacted, human-readable |
| `summary` | TEXT | | short change summary from `agent_invocations` |

**Invariants**

1. `ux_runs_active_issue`: `CREATE UNIQUE INDEX ux_runs_active_issue ON runs(issue_id) WHERE state IN ('queued','preparing','implementing','repairing','validating','publishing','reporting');` → FR-010 at the storage layer.
2. `ck_runs_ended`: `CHECK ((state IN ('completed','failed','blocked','cancelled','interrupted')) = (ended_at IS NOT NULL));`
3. `CHECK (commit_sha IS NULL OR pushed_at IS NOT NULL OR state IN ('failed','blocked','cancelled','interrupted'));` — a commit may exist locally after a push rejection (spec edge case) while still being unpushed; this is expressed by `pushed_at` being null, not by hiding the commit.
4. `CHECK (state <> 'cancelled' OR cancel_requested_at IS NOT NULL);` — cancellation is always requested, never inferred.
5. No row may move out of a terminal state (enforced in code; `store` rejects the update).

---

## 6. `workspaces` — the isolated working copy

| Column | Type | Constraints | Notes |
|---|---|---|---|
| `id` | INTEGER | PK | |
| `run_id` | INTEGER | NOT NULL UNIQUE, FK ON DELETE CASCADE | |
| `path` | TEXT | NOT NULL | absolute path inside `workspace_root` |
| `git_dir` | TEXT | NOT NULL | worktree's `.git` pointer target |
| `branch` | TEXT | NOT NULL | |
| `base_revision` | TEXT | NOT NULL | duplicated from run for a self-contained record |
| `created_at` | TEXT | NOT NULL | |
| `cleaned_at` | TEXT | | retention cleanup (FR-053) |
| `has_unpushed_changes` | INTEGER | NOT NULL DEFAULT 0 | must be recorded **before** cleanup is allowed |
| `changed_paths` | TEXT | NOT NULL DEFAULT '[]' | JSON array from `git status --porcelain` |

**Rules**: cleanup is refused while `has_unpushed_changes = 1` and `runs.pushed_at IS NULL` (FR-053). The record survives cleanup so history explains what happened to the directory.

---

## 7. `run_events` — append-only timeline

| Column | Type | Constraints | Notes |
|---|---|---|---|
| `id` | INTEGER | PK AUTOINCREMENT | |
| `run_id` | INTEGER | NOT NULL, FK ON DELETE CASCADE | |
| `seq` | INTEGER | NOT NULL | per-run monotonic, `UNIQUE(run_id, seq)`; SSE `Last-Event-ID` |
| `at` | TEXT | NOT NULL | |
| `type` | TEXT | NOT NULL, CHECK in enum | `state`, `stage`, `activity`, `check`, `agent`, `workspace`, `publish`, `report`, `warning`, `error` |
| `severity` | TEXT | NOT NULL DEFAULT 'info' | `info`, `warn`, `error` |
| `message` | TEXT | NOT NULL | redacted, human-readable |
| `data` | TEXT | NOT NULL DEFAULT '{}' | JSON payload (never secrets) |

**Rules**: append-only — no UPDATE or DELETE in normal operation (rows vanish only with the run, via cascade). `seq` is allocated inside the same transaction as the state change it describes, so the timeline order always matches the state machine.

---

## 8. `agent_invocations`, `validation_results`, `reports`, `configuration_profiles`

### `agent_invocations`

| Column | Type | Notes |
|---|---|---|
| `id` | INTEGER PK | |
| `run_id` | INTEGER NOT NULL FK | |
| `attempt_no` | INTEGER NOT NULL | 1 = implementation, ≥2 = repair (FR-036) |
| `kind` | TEXT NOT NULL | `implement`, `repair` |
| `adapter` | TEXT NOT NULL | `opencode` |
| `model` | TEXT NOT NULL | |
| `config_id` | TEXT NOT NULL | hash of policy sent |
| `prompt_path` | TEXT NOT NULL | file on disk |
| `prompt_sha256` | TEXT NOT NULL | proves what was sent |
| `transcript_path` | TEXT NOT NULL | raw JSONL, durable reference (FR-027) |
| `started_at` / `ended_at` | TEXT | |
| `exit_code` | INTEGER | |
| `timed_out` | INTEGER NOT NULL DEFAULT 0 | FR-026 |
| `status` | TEXT NOT NULL | `succeeded`, `failed`, `timed_out`, `cancelled`, `blocked` |
| `reported_status` | TEXT | agent's `RESULT.status` claim — recorded, never trusted |
| `result_summary` | TEXT | redacted excerpt of the agent's summary |

Unique `(run_id, attempt_no)`.

### `validation_results`

| Column | Type | Notes |
|---|---|---|
| `id` | INTEGER PK | |
| `run_id` | INTEGER NOT NULL FK | |
| `invocation_id` | INTEGER FK | which attempt the checks followed |
| `check_name` | TEXT NOT NULL | from `repository_checks.name` |
| `command` | TEXT NOT NULL | exact command executed (FR-033) |
| `required` | INTEGER NOT NULL | |
| `outcome` | TEXT NOT NULL CHECK IN ('passed','failed','not_executed') | FR-034 |
| `exit_code` | INTEGER | non-null iff executed |
| `duration_ms` | INTEGER | |
| `output_excerpt` | TEXT | last 64 KiB, redacted |
| `reason` | TEXT | required when `outcome='not_executed'` (timeout, cancelled, not configured) |
| `executed_at` | TEXT | |

**Rules**: `CHECK (outcome = 'not_executed' OR exit_code IS NOT NULL)`; `CHECK (outcome <> 'passed' OR exit_code = 0)` — a pass without exit code 0 is unrepresentable (SC-005). Unique `(run_id, invocation_id, check_name)`.

### `reports`

| Column | Type | Notes |
|---|---|---|
| `id` | INTEGER PK | |
| `run_id` | INTEGER NOT NULL UNIQUE FK | FR-045 non-duplication starts here |
| `body_md` | TEXT NOT NULL | full report, redacted (FR-044) |
| `commit_sha` / `merge_request_url` | TEXT | |
| `delivery_status` | TEXT NOT NULL | `pending`, `delivered`, `failed`, `skipped` |
| `delivery_error` | TEXT | FR-047 keeps the body even when delivery fails |
| `dedupe_key` | TEXT NOT NULL | sha256(run, terminal state, commit, MR url) — used as an HTML-comment marker in the Redmine note |
| `created_at` / `delivered_at` | TEXT | |

### `configuration_profiles`

| Column | Type | Notes |
|---|---|---|
| `id` | INTEGER PK | |
| `content_hash` | TEXT NOT NULL UNIQUE | sha256 of the canonicalised non-secret config |
| `schema_version` | INTEGER NOT NULL | |
| `source_path` | TEXT NOT NULL | config file path (no values) |
| `content` | TEXT NOT NULL | JSON, **redacted** — secret *references* only |
| `loaded_at` | TEXT NOT NULL | |

**Rule**: only redacted content is ever written. The profile hash on a run is what makes "which settings produced this result" answerable after a config edit (FR-048).

---

## 9. Run state machine

**States**

| State | Meaning | Terminal |
|---|---|---|
| `queued` | accepted, waiting for a concurrency slot | no |
| `preparing` | connectivity probes + workspace creation | no |
| `implementing` | first agent invocation running | no |
| `validating` | worker executing configured checks | no |
| `repairing` | agent re-invoked with failure output | no |
| `publishing` | commit → push → merge request | no |
| `reporting` | final report write + Redmine comment | no |
| `completed` | validated, published as configured | **yes** |
| `failed` | stopped with a `failure_code` | **yes** |
| `blocked` | agent or configuration surfaced a blocker / ambiguity | **yes** |
| `cancelled` | operator cancelled (`cancel_requested_at` set) | **yes** |
| `interrupted` | found non-terminal at startup after a crash/restart | **yes** |

**Allowed transitions** (everything else is rejected by `domain.Transition`)

```text
queued        → preparing, cancelled, failed
preparing     → implementing, cancelled, failed, blocked
implementing  → validating, cancelled, failed, blocked
validating    → repairing, publishing, completed, failed, cancelled
repairing     → validating, failed, cancelled
publishing    → reporting, failed, cancelled
reporting     → completed, failed
*  (non-terminal) → interrupted        # startup sweep only
```

Notes that encode spec decisions:

- `validating → completed` exists only when publishing is disabled *and* nothing needed publishing (FR-042: skipped, not failed).
- Cancellation after validation stops further publishing/reporting beyond what already completed (US4-AC4): `publishing`/`reporting` may only move to `cancelled` *before* their side effect, and any already-created commit/MR is recorded, never rolled back.
- `interrupted` is only ever written by `runner/recover.go`, never by a running pipeline.
- No transition ever targets a base branch or triggers a merge — there is no such state.

**Stage vs state**: `stage` records the fine-grained step (`connectivity.redmine`, `workspace.created`, `agent.invoked:1`, `checks.running:unit-tests`, `commit.created`, `branch.pushed`, `mr.reused`, `comment.posted`). `state` is the coarse machine above. Both are written in the same transaction as the corresponding `run_events` row.

---

## 10. Failure codes (`runs.failure_code`)

| Code | Terminal state | Trigger |
|---|---|---|
| `issue_not_found` | failed | FR-005 |
| `issue_inaccessible` | failed | FR-005 |
| `issue_input_invalid` | failed (pre-run; normally a 4xx instead) | FR-002 |
| `no_repository_mapping` | blocked | spec edge case; FR-014 |
| `repository_ambiguous` | blocked | FR-015 |
| `config_invalid` | failed | startup/`doctor` |
| `connectivity_redmine` | failed | FR-016/FR-017 |
| `connectivity_gitlab` | failed | FR-016/FR-017 |
| `connectivity_repository` | failed | FR-016/FR-017 |
| `agent_unavailable` | failed | FR-016 |
| `workspace_failed` | failed | worktree creation error |
| `agent_error` | failed | non-zero exit, no output |
| `agent_timeout` | failed | FR-026 |
| `agent_blocked` | blocked | FR-030 |
| `policy_violation` | failed | attempted escape from the sandbox |
| `no_changes` | failed | FR-032 (no commit created) |
| `validation_failed` | failed | FR-037 |
| `no_checks_configured` | failed | publishing enabled with an empty check list |
| `push_rejected` | failed | branch protection/credentials; local commit preserved |
| `merge_request_failed` | failed | push succeeded, MR did not |
| `cancelled` | cancelled | FR-012/FR-013 |
| `interrupted` | interrupted | FR-051 |

---

## 11. Validation rules traced to requirements

| Rule | Enforced in |
|---|---|
| Issue number must be a positive integer before any network call | HTTP/CLI validator (FR-002) |
| One non-terminal run per issue | partial unique index (FR-010) |
| Global active-run limit, externally configurable | semaphore + transactional count (FR-011) |
| Repository resolved only from explicit mapping | `runner/prepare.go`, no fallback code path (FR-014) |
| Ambiguous mapping blocks the run and lists candidates | `preparing → blocked` + `failure_code='repository_ambiguous'` (FR-015) |
| Base revision recorded at workspace creation | `workspaces.base_revision` (FR-019) |
| Never reuse a dirty checkout; never discard local work | worktree isolation + `git status` guard (FR-020) |
| No commit without changes | `no_changes` terminal state (FR-032) |
| No pass without exit code 0 | table `CHECK` (SC-005) |
| Required + not executed ⇒ failure | `runner/validate.go` + `CHECK` on outcome (FR-035) |
| Repair attempts bounded and individually recorded | `agent_invocations.attempt_no`, config limit (FR-036) |
| Commit only after all required checks pass | `publishing` is unreachable otherwise (FR-038) |
| Push only after validation and not cancelled | transition guard (FR-039) |
| MR description has fixed sections | `runner/publish.go` renders the contract's template (FR-040) |
| Never merge / never write base branch | no such operation exists in `internal/gitlab` (FR-041) |
| Publishing/reporting independently skippable | `delivery_status='skipped'`, state `completed` (FR-042) |
| Repeated publishing is idempotent | MR lookup by branch + `reports` uniqueness (FR-043) |
| Report content retained when delivery fails | `reports.body_md` regardless of `delivery_status` (FR-047) |
| History survives restart | SQLite file + startup sweep (FR-049, FR-051) |
| Stage recorded before each transition | same-transaction `stage` + `run_event` (FR-050) |
| Retry links to previous attempt | `previous_run_id` + branch attempt suffix (FR-052) |
| No workspace removal while unpushed work exists | `workspaces.has_unpushed_changes` guard (FR-053) |
| No credential in any stored text | redaction at write boundaries (FR-056) |

---

## 12. Retention and cleanup

| Data | Retention | Mechanism |
|---|---|---|
| `runs`, `*_events`, checks, reports | until an administrator prunes (spec Assumptions) | `redmine-agent prune --older-than <dur>` (manual, not scheduled in v1) |
| Workspace directories | 7 days after `completed` (configurable) | `runner` cleanup pass, refused while `has_unpushed_changes=1` |
| Transcripts/prompts/logs | same as the run | deleted with the run's directory, paths never reused |
| Pushed branches, merge requests | never deleted automatically (FR-054) | no code path exists |
