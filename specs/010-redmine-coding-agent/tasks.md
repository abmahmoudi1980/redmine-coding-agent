---
description: "Task list for RedmineCodingAgent implementation"
---

# Tasks: RedmineCodingAgent — Automated Issue Implementation Worker

**Input**: Design documents from `specs/010-redmine-coding-agent/`

**Prerequisites**: [plan.md](./plan.md) ✅, [spec.md](./spec.md) ✅, [research.md](./research.md) ✅, [data-model.md](./data-model.md) ✅, [contracts/](./contracts/) ✅, [quickstart.md](./quickstart.md) ✅

**Tests**: Included. Principle I (Independent Verification) and Principle III (Durable, Auditable Run State) in `.specify/memory/constitution.md` require independently verifiable behaviour, and `quickstart.md` §7 defines the fake-service harness these tasks build. Test tasks come first inside each story and must fail before the implementation task they cover.

**Organization**: Tasks are grouped by user story so each can be implemented, tested, and demoed on its own.

**Stack**: Go 1.24 (`CGO_ENABLED=0`) · SQLite (`modernc.org/sqlite`) · SvelteKit 2 + Svelte 5 embedded · Linux + Windows.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: can run in parallel (different files, no dependency on an incomplete task)
- **[Story]**: `US1`…`US6`, matching the priorities in `spec.md`
- Every task names the exact file or directory it touches

## Path Conventions

Single Go module at the repository root, with the SPA in `web/`:

- backend: `cmd/redmine-agent/`, `internal/…`
- frontend: `web/src/…`
- tests: `internal/**/**_test.go`, `tests/integration/`, `web/src/**/*.test.ts`, `web/e2e/`

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Compilable skeleton, build targets, and CI for both operating systems.

- [X] T001 Initialize the Go module at repo root in `go.mod` (`module github.com/<org>/redmine-coding-agent`, `go 1.24`) and create `cmd/redmine-agent/main.go` with a `serve` stub that prints the build version
- [X] T002 [P] Create the directory skeleton from plan.md §Project Structure: `internal/{api,config,domain,agent,runner,redmine,gitlab,git,validate,health,proc,store,logging,web}`, `internal/store/migrations/`, `tests/{integration,fixtures}/`, `web/src/{routes,lib}`
- [X] T003 [P] Add `golangci-lint` configuration in `.golangci.yml` (enable `errcheck`, `govet`, `staticcheck`, `gosec`, `bodyclose`, `revive`) and a `make lint` target
- [X] T004 [P] Add `.gitignore` and `.editorconfig` covering `bin/`, `data/`, `web/build/`, `web/.svelte-kit/`, `node_modules/`, and `.opencode/` credential paths
- [X] T005 [P] Scaffold the SvelteKit app in `web/`: SvelteKit 2 + Svelte 5 + TypeScript + Vite 7, `web/package.json` scripts `dev|build|check|test|e2e`
- [X] T006 [P] Configure `web/svelte.config.js` for `@sveltejs/adapter-static` with `fallback: 'index.html'`, and set `export const ssr = false` in `web/src/routes/+layout.ts`
- [X] T007 [P] Add `web/eslint.config.js`, `web/.prettierrc`, and `web/vitest.config.ts` for frontend lint/format/unit test runs
- [X] T008 Create the embed bridge: `internal/web/embed.go` with `//go:embed all:dist` plus a committed placeholder `internal/web/dist/index.html` so `go build ./...` succeeds before any frontend build
- [X] T009 [P] Write `config.example.yaml` exactly matching `quickstart.md` §3 (schema_version, http, data, redmine, gitlab, repositories[].checks, agent, concurrency)
- [X] T010 [P] Add a `Makefile` with `build`, `build-web`, `build-all` (four GOOS/GOARCH targets), `test`, `lint`, `fmt`, `run` targets; document the Windows `PowerShell` equivalents in comments
- [X] T011 [P] Add `.goreleaser.yaml` building `linux/{amd64,arm64}` and `windows/{amd64,arm64}` with `CGO_ENABLED=0`, `-trimpath`, and the frontend build hooked before `go build`
- [X] T012 [P] Add `Dockerfile` (multi-stage, `distroless/static`, non-root) and `.github/workflows/ci.yml` with a `ubuntu-latest` + `windows-latest` matrix running `go vet`, `go test ./...`, `npm run check`, `npm test`, and the four-target cross-compile check

**Checkpoint**: `go build ./...` and `go test ./...` pass on both OSes with no product code yet.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: The shared spine every story depends on — config, secrets, logging, domain states, storage, process containment, git, HTTP plumbing, and the test harness.

**⚠️ CRITICAL**: No user story work starts until this phase is complete.

### Configuration, secrets, redaction

- [X] T013 Implement strict config decoding in `internal/config/config.go`: typed structs for every `config.example.yaml` key, `yaml.Decoder` with `KnownFields(true)`, `schema_version` check, and a `$REDMINE_AGENT_CONFIG` fallback path
- [X] T014 [P] Implement secret indirection in `internal/config/secrets.go`: resolve `env:NAME` and `file:/path` lazily, return a typed `SecretRef` that can never be formatted into a string (`String()` returns the reference name only)
- [X] T015 [P] Implement `internal/config/validate.go`: required sections, non-empty `base_branch`, unique `(redmine_project, repository_path)`, at most one `is_default` per project, positive timeouts, `max_active_runs >= 1`, and an agent command that `exec.LookPath` can find
- [X] T016 Implement the redaction registry in `internal/logging/redact.go`: register resolved secret values, provide `Redact(string) string` and a wrapping `io.Writer` that scrubs on write; cover JSON, quoted, and URL-encoded forms
- [X] T017 Write `internal/config/config_test.go` and `internal/logging/redact_test.go`: unknown-key rejection, unresolved-reference error naming the reference only, and a test proving a registered secret never survives `%v`, `slog`, or writer output (FR-056, SC-013)

### Logging

- [X] T018 Implement `internal/logging/logger.go`: `slog` JSON handler on stderr, per-run file handler at `<log_dir>/<run_id>.log`, a `WithRun(runID, stage)` helper, and the redaction writer applied to both sinks
- [X] T019 Write `internal/logging/logger_test.go`: run-scoped fields present on every record, log file created and rotated per run, redacted values absent from file and stderr

### Domain model and state machine

- [X] T020 Implement `internal/domain/run.go`: `Run`, `Workspace`, `IssueSnapshot`, `AgentInvocation`, `ValidationResult`, `Report` structs mirroring `data-model.md` §4–§8, with `RunState`, `Stage`, and `Outcome` string enums
- [X] T021 Implement `internal/domain/state.go`: the transition table from `data-model.md` §9, `CanTransition(from, to)`, `IsTerminal(state)`, an `ErrInvalidTransition` error carrying both states, and the rule that `interrupted` is settable only through `MarkInterrupted`
- [X] T022 [P] Implement `internal/domain/errors.go`: `FailureCode` constants for every row of `data-model.md` §10, a `Failure{Code, Message, Stage, Remediation}` type, and mapping helpers from upstream errors to `connectivity_*`, `push_rejected`, and `merge_request_failed`
- [X] T023 Write `internal/domain/state_test.go`: every allowed edge accepted, every other edge rejected, no terminal state exits, `cancelled` requires a cancel request, and `validating → completed` allowed only when publishing is disabled

### Storage

- [X] T024 Implement `internal/store/store.go`: open `modernc.org/sqlite` at `data.database`, apply `journal_mode=WAL`, `busy_timeout=5000`, `foreign_keys=on`, `synchronous=NORMAL` on every connection, one writer connection plus a read pool, and a `Close` that checkpoints WAL
- [X] T025 Implement `internal/store/migrate.go` plus `internal/store/migrations/0001_init.sql` creating all tables, indexes, partial unique indexes, and `CHECK` constraints from `data-model.md` §2–§8 (including `ux_runs_active_issue` and `CHECK (outcome <> 'passed' OR exit_code = 0)`), applied in a transaction and recorded in `schema_migrations`
- [X] T026 [P] Implement `internal/store/runs.go`: create/read/update run, state+stage transition written in the same transaction as its `run_events` row, active-run lookup by issue, active-run count, and cursor-paginated listing
- [X] T027 [P] Implement `internal/store/events.go`: append with per-run `seq` allocation inside the caller's transaction, replay-after-seq, latest-N, and the redaction guard on `message`/`data`
- [X] T028 [P] Implement `internal/store/checks.go` and `internal/store/reports.go`: invocation insert/finish, validation-result insert (one row per check per attempt), check-attempt grouping, report upsert by `run_id` with `dedupe_key`, and configuration-profile insert/get-by-hash
- [X] T029 Write `internal/store/store_test.go`: partial unique index rejects a second active run for the same issue, `CHECK` rejects `passed` with a non-zero exit code or a missing exit code, terminal runs cannot be updated, event `seq` is monotonic under concurrent appends, and a reopen preserves all history

### HTTP plumbing

- [X] T030 Implement `internal/api/router.go`: stdlib `net/http` routes for every path in `contracts/openapi.yaml`, plus a middleware chain of panic recovery → request id → structured access log → redaction → handler, bound to `http.listen`
- [X] T031 [P] Implement `internal/api/errors.go`: `Problem` writer emitting `application/problem+json` with `type`, `title`, `status`, `code`, `detail`, optional `stage`/`candidates`/`activeRun`, and a `domain.Failure` → HTTP status mapper (400/404/409/429/502)
- [X] T032 [P] Implement `internal/api/static.go`: serve `internal/web/dist` with immutable cache headers for hashed assets and an SPA fallback to `index.html` for non-`/api` paths
- [X] T033 Implement `internal/api/contract_test.go`: a table test asserting every documented route exists with the documented method and that unknown routes return a problem+json 404 (guards drift from `contracts/openapi.yaml`)

### Process containment

- [X] T034 Implement `internal/proc/proc.go` and `internal/proc/proc_unix.go`: start a child with `Setpgid`, stream stdout/stderr to supplied writers, and on `ctx` cancellation send `SIGTERM` then `SIGKILL` to `-pgid` after a grace period
- [X] T035 Implement `internal/proc/proc_windows.go`: create a Job Object with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`, assign the child immediately, and call `TerminateJobObject` on cancellation with a `taskkill /T /F /PID` fallback
- [X] T036 Write `internal/proc/proc_test.go` behind a `testing.Short()` guard: a helper child that spawns a grandchild, cancellation kills both on the current OS, and the returned exit state distinguishes cancelled from exited

### Git operations

- [X] T037 Implement `internal/git/repo.go`: ensure/refresh a bare mirror under `<workspace_root>/../repos/<repository_id>.git`, `worktree add -b <branch> <workspace> <base_branch>`, `rev-parse HEAD`, `status --porcelain`, `diff --name-status`, `add -A`, `commit`, and `push origin <branch>` — with **no** merge, rebase, or force-push function anywhere in the package (FR-041)
- [X] T038 [P] Implement `internal/git/lock.go` and `internal/git/paths.go`: per-repository mutex keyed by `repository_id`, workspace path layout `<workspace_root>/<run_id>/`, and a guard that refuses a workspace path outside the configured root
- [X] T039 Write `internal/git/repo_test.go`: create a bare origin in `t.TempDir()`, mirror it, add two worktrees concurrently, assert branch isolation and that the origin tip is unchanged; assert `dirty` detection refuses cleanup when unpushed changes exist

### Agent boundary and test harness

- [X] T040 Implement `internal/agent/adapter.go` from `contracts/agent-adapter.md` §1–§3: `Adapter` interface, `Request`/`Result`/`Prompt`/`Summary`/`ActivityEvent` types, a name-keyed registry, and validation of A1–A3 preconditions (existing workspace inside the root, sandbox dirs created)
- [X] T041 Implement the integration harness in `tests/integration/harness.go`: `httptest` fakes for Redmine (`/issues/:id.json` with journals/attachments, note posting) and GitLab (`/projects/:id`, branches, merge requests), a temp bare origin, a real SQLite store, and a config builder pointing at the fakes
- [X] T042 Implement `tests/integration/fakeagent/main.go`: a controllable fake agent with modes `ok`, `no-changes`, `lies`, `blocked`, `crash`, `hang`, `escape-path`, `grandchild`, emitting the JSONL shape and the `## RESULT` block from `contracts/agent-adapter.md` §5–§6

**Checkpoint**: Foundation ready — every subsequent story plugs into storage, states, git, containment, and the fakes.

---

## Phase 3: User Story 1 — Review a Redmine issue before any code is written (P1) 🎯 MVP

**Goal**: The operator submits an issue number and sees the issue plus its resolved repository and base branch, with no side effects, then confirms or declines.

**Independent Test**: Submit a reachable issue number → subject, project, status, priority, description, repository, and base branch match Redmine and configuration. Submit a bad number → specific error, no run, no workspace. Decline → back to input, nothing created.

### Tests for User Story 1

- [X] T043 [P] [US1] Write `internal/redmine/client_test.go` against `httptest`: successful fetch with journals/relations/attachments, `404` → `issue_not_found`, `403` → `issue_inaccessible`, and a request that carries `X-Redmine-API-Key` but never logs it
- [X] T044 [P] [US1] Write `internal/runner/resolve_test.go`: single mapping resolves; two candidates with no default → `repository_ambiguous` with both candidates; zero mappings → `no_repository_mapping`; explicit `repositoryId` overrides an ambiguous project (FR-014/FR-015)
- [X] T045 [P] [US1] Write `tests/integration/preview_test.go`: preview creates no run row, no workspace directory, no branch; a second preview of the same issue is side-effect free
- [X] T046 [P] [US1] Write `web/e2e/preview.spec.ts` (Playwright, stub API): enter number → issue card and repository card render → Decline returns to the input with an empty state

### Implementation for User Story 1

- [X] T047 [P] [US1] Implement `internal/redmine/types.go` and `internal/redmine/client.go`: `Issue` with subject, description, project, status, tracker, priority, assignee, parent, relations, custom fields, journals, attachments; `GetIssue(ctx, id)` with `?include=journals,relations,children,attachments`; typed errors for not-found vs forbidden
- [X] T048 [P] [US1] Implement `internal/redmine/attachments.go`: download attachments into `<run_dir>/attachments/` when permitted, else record `available=false` with the reason (FR-007)
- [X] T049 [US1] Implement `internal/runner/snapshot.go`: build an `IssueSnapshot` from the Redmine payload, extract acceptance criteria by heading heuristic (`## Acceptance Criteria`, `AC:`, `معیار پذیرش`, `پذیرش`) setting `criteria_source`, compute `content_hash`, and select comments
- [X] T050 [US1] Implement `internal/runner/resolve.go`: resolve `repository_id` + `base_branch` strictly from the `repositories` table, returning `repository_ambiguous` with candidates or `no_repository_mapping` — no inference, no fallback branch (FR-014, FR-015)
- [X] T051 [P] [US1] Implement `internal/api/issues.go`: `GET /api/issues/{issueId}/preview` with `repositoryId` query support, plus `GET /api/repositories` returning mappings and checks
- [X] T052 [US1] Add issue-number validation to `internal/api/validate.go`: positive integer, rejects blank/non-numeric with `400 invalid_issue_id` **before** any Redmine call (FR-002)
- [X] T053 [US1] Wire the preview route to emit `warnings` for missing acceptance criteria and unavailable attachments, and to surface `candidates` on ambiguity
- [X] T054 [P] [US1] Implement `web/src/lib/api.ts` request helpers and TS types generated from `contracts/openapi.yaml` (`openapi-typescript`), covering `previewIssue`, `listRepositories`, and the `Problem` shape
- [X] T055 [P] [US1] Implement `web/src/lib/components/IssueCard.svelte` (subject, project, status, priority, assignee, description, acceptance criteria with source badge, comments, attachments) and `RepositoryPicker.svelte` (candidates, base branch, disabled state until chosen)
- [X] T056 [US1] Implement `web/src/routes/+page.svelte`: issue input with inline validation, preview request, loading and error states rendered from `Problem.detail`, and confirmation controls that stay inert until US2 wires them
- [X] T057 [US1] Implement the Decline path in `web/src/routes/+page.svelte` and `+page.server.ts`-free store reset: returns to input, clears the preview and any candidate selection, leaves no client-side artifact
- [X] T058 [US1] Add `GET /api/health` minimal handler in `internal/api/health.go` returning database readiness only (deep probes arrive in US6), so the SPA header can show worker liveness

**Checkpoint**: US1 is demoable on its own — the operator can inspect any reachable issue and is never at risk of starting work by accident.

---

## Phase 4: User Story 2 — Implement the issue in an isolated workspace with independent verification (P1)

**Goal**: After confirmation, the worker provisions an isolated worktree, runs the agent under policy, then inspects the diff and executes the configured checks itself; a commit exists only when every required check passed.

**Independent Test**: Run a small issue end to end → dedicated branch in a dedicated workspace containing only that issue's changes, every check recorded with its real exit code, and a commit present only because checks passed. Failing checks → no commit, no push.

### Tests for User Story 2

- [X] T059 [P] [US2] Write `internal/agent/prompt_test.go`: every issue-derived block is fenced as untrusted, restrictions appear before issue content, no secret value can appear in the rendered prompt, and the `## RESULT` block is the last section (FR-006, FR-023)
- [X] T060 [P] [US2] Write `internal/agent/summary_test.go`: `status: blocked` with blockers → `Blocked`; `status: done` → `Succeeded`; malformed/missing block → empty `ReportedStatus` and no failure
- [X] T061 [P] [US2] Write `internal/validate/runner_test.go`: exit 0 → `passed`, non-zero → `failed`, timeout → `failed` with a timeout reason, skipped-after-cancel → `not_executed` with reason, output truncated to 64 KiB, and secrets absent from the excerpt
- [X] T062 [P] [US2] Write `tests/integration/pipeline_test.go` covering: happy path commits; a failing required check blocks the commit; `no-changes` mode creates no commit; `blocked` mode ends `blocked`; `escape-path` mode ends `policy_violation`; required check that never runs counts as failure (FR-032, FR-035, FR-037)
- [X] T063 [P] [US2] Write `internal/agent/opencode_test.go` with a golden JSONL transcript pinning the parser's extraction and proving unknown event shapes are skipped rather than fatal

### Implementation for User Story 2

- [X] T064 [US2] Implement `internal/api/runs.go` create handler: `POST /api/runs` with `confirm: true` required, `409 active_run_exists` carrying the blocking run id + stage, `429 concurrency_limit`, `201` with `Location` (FR-008, FR-010, FR-011)
- [X] T065 [US2] Implement `internal/runner/runner.go`: the state-machine driver that owns a `context`, acquires the concurrency slot inside a `BEGIN IMMEDIATE` transaction, persists `stage` + `run_event` per transition, and maps terminal outcomes to `domain.Failure`
- [X] T066 [US2] Implement `internal/runner/prepare.go`: connectivity probes for Redmine credential, GitLab reachability, repository fetch access, and agent presence via `Adapter.Probe`; any failure stops before touching the filesystem with `connectivity_*`/`agent_unavailable` (FR-016, FR-017)
- [X] T067 [US2] Implement workspace creation in `internal/runner/prepare.go` using `internal/git`: mirror refresh under the repository mutex, `worktree add -b <branch> <workspace> <base_branch>`, record `base_revision`, insert the `workspaces` row, and refuse any path outside `workspace_root` (FR-018–FR-020)
- [X] T068 [US2] Implement `internal/agent/prompt.go`: fixed precedence preamble, fenced untrusted blocks for subject/description/criteria/comments/attachments, repository instruction discovery (AGENTS.md/README conventions), the required validation commands, operating restrictions, and the `## RESULT` schema (FR-023, FR-024, FR-025)
- [X] T069 [US2] Implement `internal/agent/sandbox.go`: per-run `sandbox/` with redirected `HOME`/`XDG_*`/`TMPDIR`, an environment allowlist builder, and the read-only agent policy file that denies shell outside the workspace, network fetches, absolute-path writes, and data-dir reads (R9)
- [X] T070 [US2] Implement `internal/agent/opencode.go`: compose the verified `opencode run --standalone --format json --model … --agent … --title …` invocation, set `cmd.Dir` to the workspace, start it through `internal/proc`, capture redacted stdout/stderr to `transcript-attempt-<n>.jsonl`/`.err`, and map exit/timeout/cancel to `Status`
- [X] T071 [US2] Implement `internal/agent/transcript.go`: tolerant JSONL parsing into `ActivityEvent`s plus `policy_violation` detection from permission-denied events, never failing the run on unknown shapes
- [X] T072 [US2] Implement `internal/agent/summary.go`: parse the `## RESULT` block into `Summary`, surface `blocked` as `agent_blocked`, and store `checks_run` verbatim as a claim only (FR-025, FR-030)
- [X] T073 [US2] Implement `internal/validate/runner.go`: resolve `command_linux`/`command_windows`, execute via `sh -c` / `cmd.exe /c` through `internal/proc` with the per-check timeout and a scrubbed environment, capture exit code + duration + last 64 KiB, and classify `passed|failed|not_executed` (FR-031–FR-035)
- [X] T074 [US2] Implement the repair loop in `internal/runner/validate.go`: on required-check failure and while `attempt_no <= max_repairs`, re-invoke the adapter with the failure excerpt as `kind: repair`, insert a new invocation and a fresh result set, and stop at the limit with `validation_failed` (FR-036, FR-037)
- [X] T075 [US2] Implement no-change handling in `internal/runner/validate.go`: empty `status --porcelain` → terminal `failed` with `no_changes`, no commit, no push (FR-032)
- [X] T076 [US2] Implement commit creation in `internal/runner/validate.go`: only after all required checks pass, message referencing the Redmine issue and subject, `commit_sha` persisted; no commit path is reachable otherwise (FR-038)
- [X] T077 [US2] Implement `GET /api/runs/{runId}` in `internal/api/runs.go` returning the `RunDetail` shape: issue, workspace facts, invocations (exit code, timeout, `reported_status`), per-check results, commit, latest 200 timeline events
- [X] T078 [P] [US2] Implement `web/src/routes/runs/[id]/+page.svelte` static detail view: state/stage header, branch and base revision, changed file list, invocation list with exit codes, and the check table (`command`, exit code, duration, `passed|failed|not_executed`, stdout excerpt) with required/optional distinction
- [X] T079 [P] [US2] Implement `web/src/lib/components/CheckTable.svelte` and `Timeline.svelte`, the latter rendering `run_events` severities without raw log noise
- [X] T080 [US2] Implement the confirmation → start handoff in `web/src/routes/+page.svelte`: on Confirm call `POST /api/runs`, navigate to `/runs/{id}`, and render `409`/`429` problems inline with the blocking run link (completes US1's inert controls)
- [X] T081 [US2] Add agent/timeout/repair settings resolution to `internal/runner/runner.go` (model, profile, `timeout_minutes`, `max_repairs`) and record `agent_config_id` plus the configuration-profile hash on the run

**Checkpoint**: US1 + US2 deliver the product's core promise — verified implementation, or an honest failure with evidence.

---

## Phase 5: User Story 3 — Publish the implementation for human review (P2)

**Goal**: A validated run is pushed and opened as a merge request against the configured base branch, and the original issue receives a completion report. Nothing is merged.

**Independent Test**: From a passing run, a merge request exists targeting the base branch with every required description section, its source branch matches the reported branch, and the issue carries a comment with the same links.

### Tests for User Story 3

- [X] T082 [P] [US3] Write `internal/gitlab/client_test.go`: token sent as `PRIVATE-TOKEN`, project path URL-encoded, MR lookup by `source_branch`+`target_branch` reuses an existing MR, and no merge endpoint is ever called
- [X] T083 [P] [US3] Write `internal/runner/report_test.go`: the rendered report contains terminal status, change summary, branch, commit, MR link, validation results, blockers, and duration; the `dedupe_key` marker is stable for the same outcome and changes when the commit changes (FR-044, FR-045)
- [X] T084 [P] [US3] Write `tests/integration/publish_test.go`: push happens only after a passing validation; publishing disabled ⇒ `completed` with `skipped`; MR creation reuses rather than duplicates; a rejected push keeps the local commit and reports `push_rejected`; a failed comment keeps the report body (FR-039, FR-042, FR-043, FR-047)

### Implementation for User Story 3

- [X] T085 [P] [US3] Implement `internal/gitlab/client.go`: `GET /projects/:id`, `POST /projects/:id/merge_requests`, `GET /projects/:id/merge_requests?source_branch&target_branch&state=opened`, and `PUT` for reuse — deliberately no merge or branch-delete methods
- [X] T086 [US3] Implement `internal/runner/publish.go`: transition to `publishing`, push the branch only when validation passed and no cancel is requested, record `pushed_at`, and on push rejection preserve the commit locally while ending `failed`/`push_rejected` (FR-039)
- [X] T087 [US3] Implement MR creation/reuse in `internal/runner/publish.go`: build the description with the fixed section order (issue reference and subject, implementation summary, notable changed files, validation commands and results, known limitations, manual follow-up), target `base_branch`, and reuse an existing open MR for the same branches (FR-040, FR-043)
- [X] T088 [US3] Implement publish policy gating: `publish_enabled`/`merge_request_enabled` (global and per repository) cause `skipped` steps, not failures, and `validating → completed` when nothing needs publishing (FR-042)
- [X] T089 [US3] Implement `internal/runner/report.go`: render the final report markdown from the run, commit, MR, and check results; persist it to `reports` even when delivery fails (FR-044, FR-047)
- [X] T090 [US3] Implement `internal/redmine/notes.go`: post the report as a journal note carrying the `dedupe_key` as an HTML-comment marker, skip when the marker already exists on the issue, and record `delivery_status`/`delivery_error` (FR-045, FR-046)
- [X] T091 [US3] Implement `GET /api/runs/{runId}/report` and `GET /api/runs/{runId}/checks` in `internal/api/runs.go`, returning the report and the per-attempt check grouping
- [X] T092 [P] [US3] Implement `web/src/lib/components/ReportCard.svelte`: markdown report with delivery state, MR link, commit link, and a visible warning when the Redmine comment failed
- [X] T093 [US3] Add the publish/report sections to `web/src/routes/runs/[id]/+page.svelte`: commit, branch, MR link, delivery status, and the "nothing was merged" assurance line (US3-AC4)

**Checkpoint**: A passing run now reaches a human reviewer with a complete, auditable paper trail.

---

## Phase 6: User Story 4 — Follow and cancel a run in progress (P2)

**Goal**: The operator watches stage, elapsed time, and a readable activity summary, and can stop a run safely.

**Independent Test**: Start a run, watch the stage advance and elapsed time update, cancel mid-implementation, and confirm the agent and its children stop, a cancelled terminal state is recorded, logs remain readable, and nothing was pushed.

### Tests for User Story 4

- [X] T094 [P] [US4] Write `internal/api/events_test.go`: SSE emits `id`/`event`/`data` per `contracts/openapi.yaml`, replays strictly after `Last-Event-ID`, sends a heartbeat, and closes after the terminal event
- [X] T095 [P] [US4] Write `internal/runner/cancel_test.go`: cancel during `implementing` yields `cancelled` with `cancel_requested_at`, the agent context is cancelled, and no publish/report step runs afterwards (US4-AC4)
- [X] T096 [P] [US4] Write `tests/integration/cancel_test.go` with fake-agent `hang` + `grandchild` modes: the whole tree is dead within 30 s, logs survive, `has_unpushed_changes` is recorded, and nothing is pushed (SC-009)
- [X] T097 [P] [US4] Write `web/e2e/run-live.spec.ts`: stage indicator advances, elapsed timer ticks, activity lines appear, and the Cancel button confirms then reflects the terminal state

### Implementation for User Story 4

- [X] T098 [US4] Implement `internal/api/events.go`: `GET /api/runs/{runId}/events` as `text/event-stream`, replay from `run_events` using `Last-Event-ID`, live subscription via an in-process hub, 15 s heartbeats, terminal-event close, and `Flusher` correctness under `http.Server`
- [X] T099 [US4] Implement the event hub in `internal/api/hub.go`: per-run subscriber registry with bounded buffers that drop the oldest events for a slow client without blocking the runner
- [X] T100 [US4] Implement cancellation: `POST /api/runs/{runId}/cancel` in `internal/api/runs.go` sets `cancel_requested_at`, cancels the runner context, returns `202`; idempotent for an already-cancelled run, `409 run_not_cancellable` when terminal (FR-012, FR-013)
- [X] T101 [US4] Implement `internal/runner/cancel.go`: cancellation checks at every stage boundary so publishing and reporting never begin after a cancel request, while already-completed side effects stay recorded and are never rolled back
- [X] T102 [US4] Implement activity summarisation in `internal/runner/activity.go`: coalesce agent tool events into readable lines (file reads/edits, commands, permission denials) and emit `activity` events without flooding the timeline
- [X] T103 [US4] Implement `GET /api/runs/{runId}/logs?tail=N` in `internal/api/runs.go`: tail the run log file, already redacted, `410` when pruned
- [X] T104 [P] [US4] Implement `web/src/lib/sse.ts`: `EventSource` wrapper with `Last-Event-ID` resume, reconnect backoff, typed event dispatch, and teardown on route destroy
- [X] T105 [US4] Convert `web/src/routes/runs/[id]/+page.svelte` to the live view: stage stepper, server-anchored elapsed timer, activity feed, live check results, cancel with confirmation, and a post-terminal "what happened / what to do next" panel (SC-011)
- [X] T106 [P] [US4] Implement `web/src/lib/components/LogViewer.svelte`: collapsible redacted log tail fetched on demand, with a clear "logs pruned" state

**Checkpoint**: Long autonomous work is observable and stoppable — safe to leave running unattended.

---

## Phase 7: User Story 5 — Audit, retry, and recover after a failure or restart (P2)

**Goal**: Failures and restarts leave a durable, stage-level, diagnosable record; retries are new linked attempts, never silent duplicates.

**Independent Test**: Interrupt a run, restart the worker, and confirm the run survives as `interrupted` with its last stage, that it cannot silently resume into a push, and that a retry is a new, linked attempt.

### Tests for User Story 5

- [X] T107 [P] [US5] Write `internal/runner/recover_test.go`: the startup sweep marks every non-terminal run `interrupted` with `interrupted_stage`, leaves terminal runs untouched, and never triggers publish/report
- [X] T108 [P] [US5] Write `internal/store/history_test.go`: cursor pagination is stable and gap-free, filters by state and issue work, and ordering is newest-first by `created_at`
- [X] T109 [P] [US5] Write `tests/integration/recovery_test.go`: hard-kill the orchestrator mid-implementation, restart, assert the interrupted record, then retry and assert `previous_run_id`, a suffixed branch, and a distinct workspace (SC-007, FR-052)
- [X] T110 [P] [US5] Write `tests/integration/double_start_test.go`: a second start for an active issue returns `409` with the blocking run id and stage, and exactly one workspace + one agent process exist (SC-008)

### Implementation for User Story 5

- [X] T111 [US5] Implement `internal/runner/recover.go`: on `serve` startup, inside one transaction, move every non-terminal run to `interrupted`, persist `interrupted_stage`, append a terminal `run_events` row, and log a summary; wire it before the listener accepts traffic (FR-051)
- [X] T112 [US5] Implement `GET /api/runs` in `internal/api/runs.go` with `state`, `issueId`, `limit`, and opaque `cursor` returning `RunPage` (FR-049)
- [X] T113 [US5] Implement `POST /api/runs/{runId}/retry` in `internal/api/runs.go`: refuse while the previous run is active (`409`), otherwise create a linked run; choose `-r<n>` branch suffix when the prior branch exists remotely or was pushed (FR-052, R18)
- [X] T114 [US5] Implement branch-supersession logic in `internal/git/repo.go`: detect an existing remote/local branch, reuse it only when the prior attempt was unpushed with the same base revision, and never force-push
- [X] T115 [US5] Implement `POST /api/runs` active-run rejection detail: return `activeRun{runId, stage, state}` in the problem body so the UI can link straight to the blocker (FR-010)
- [X] T116 [P] [US5] Implement `web/src/routes/runs/+page.svelte`: history table with state badges, issue filter, cursor pagination, and "retry" on terminal failed/blocked/interrupted rows
- [X] T117 [US5] Add retry lineage to `web/src/routes/runs/[id]/+page.svelte`: previous-attempt link, attempt number, and a banner distinguishing a retry from the original
- [X] T118 [US5] Implement stage-level failure guidance in `internal/runner/runner.go`: every `domain.Failure` carries `stage` and `remediation`, rendered in the run view so a failed run is diagnosable without logs (SC-011)

**Checkpoint**: The tool survives its first bad day without becoming untrustworthy.

---

## Phase 8: User Story 6 — Configure repositories, agents, and policies without code changes (P3)

**Goal**: An administrator changes mappings, branches, checks, agent, timeouts, and policies in configuration, restarts, and sees the change take effect — with no rebuild and no secret leakage.

**Independent Test**: Add a mapping, a base branch, and a validation command through configuration only, restart, and see them used on the next run. Inspect logs, prompts, and the database: no credential value appears. Health shows each dependency separately.

### Tests for User Story 6

- [X] T119 [P] [US6] Write `internal/config/reload_test.go`: a changed mapping/check/base branch/agent setting is applied on restart without recompiling, and a removed mapping stops resolving (SC-012)
- [X] T120 [P] [US6] Write `tests/integration/secret_scan_test.go`: run a full pipeline against the fakes, then scan the DB dump, all log files, transcripts, prompts, reports, and HTTP responses for the fixture secret values — zero matches (SC-013)
- [X] T121 [P] [US6] Write `internal/health/health_test.go`: each probe reports `ok`/`failing` independently, a failing dependency degrades the overall status without failing the request, and remediation text is present on failure (FR-058)
- [X] T122 [P] [US6] Write `web/e2e/health.spec.ts`: the health page lists every component with status and latency, and shows the remediation for a failing one

### Implementation for User Story 6

- [X] T123 [US6] Implement config → database seeding in `internal/store/seed.go`: upsert `repositories` and `repository_checks` from configuration, deactivate mappings removed from config without deleting history, and hash the effective non-secret config into `configuration_profiles` (FR-055)
- [X] T124 [US6] Implement `GET /api/config` in `internal/api/config.go`: return the redacted effective configuration, `contentHash`, `schemaVersion`, and `loadedAt`; the same document persisted for audit
- [X] T125 [US6] Implement `internal/health/health.go`: probes for database writability + migration state, Redmine (credential-authenticated read), GitLab reachability, repository fetch access, agent `--version`, and disk space, each with latency and remediation text
- [X] T126 [US6] Implement `GET /api/health?deep=true` in `internal/api/health.go`, returning `HealthReport` with per-component status and never a `5xx` for a degraded dependency
- [X] T127 [US6] Implement the `doctor` command in `cmd/redmine-agent/doctor.go`: the ten ordered checks from `contracts/cli.md`, `--deep`, `--all`, human and `--json` output, exit `4` on a dependency failure and `3` on configuration failure
- [X] T128 [US6] Implement `GET /api/meta` in `internal/api/meta.go` (version, commit, builtAt, os, arch, schemaVersion) fed by `-ldflags` build variables and registered in `Makefile`/`.goreleaser.yaml`
- [X] T129 [US6] Implement per-repository policy overrides end to end: `publish_enabled`, `merge_request_enabled`, check `required`, and per-check `timeout_seconds` read from the seeded tables and applied by the runner (FR-042)
- [X] T130 [P] [US6] Implement `web/src/routes/health/+page.svelte`: component grid with status, latency, detail, remediation, and a manual refresh; deep-probe toggle
- [X] T131 [US6] Implement `web/src/routes/settings/+page.svelte` (read-only): render `GET /api/config` as a redacted settings view with a "loaded at / hash" header and a note that changes require a restart

**Checkpoint**: An administrator can operate the tool with a text editor and a restart.

---

## Phase 9: Polish & Cross-Cutting Concerns

**Purpose**: Operator ergonomics, retention, documentation, and the cross-platform proof.

- [X] T132 Implement the headless `run` command in `cmd/redmine-agent/run.go`: `--issue`, `--repository`, `--yes`, `--wait`, `--format json`, terminal-state exit codes `4`–`9` per `contracts/cli.md`
- [X] T133 [P] Implement `cmd/redmine-agent/runs.go` (`list|show|checks|logs|report`), `cmd/redmine-agent/action.go` (`cancel|retry`), and `cmd/redmine-agent/version.go` with `--json` output matching the API documents
- [X] T134 Implement retention in `cmd/redmine-agent/prune.go` and `internal/runner/cleanup.go`: delete run directories and rows older than `--older-than`, refuse while `has_unpushed_changes = 1`, report skips, and never delete pushed branches or MRs (FR-053, FR-054)
- [X] T135 [P] Write `tests/integration/cli_test.go`: exit codes map to terminal states, `--json` emits one document per line, and `--yes` is required before any agent process starts
- [X] T136 Implement graceful shutdown in `cmd/redmine-agent/serve.go`: `SIGINT`/`SIGTERM` stops accepting runs, cancels the active run through `internal/proc`, drains the hub, checkpoints WAL, exits `0`
- [X] T137 [P] Add structured startup banner logging in `cmd/redmine-agent/serve.go` (version, listen address, schema version, workspace root, active-run limit) with the redaction writer applied
- [X] T138 [P] Write `README.md`: what it does and does not do (never merges), the five-minute setup, the `doctor` → preview → run path, and links to `quickstart.md`
- [X] T139 [P] Add deployment recipes in `deploy/`: `systemd/redmine-agent.service`, `windows/install-service.ps1` (`sc.exe create`), and `docker/compose.yaml`, each documenting the service-account and ACL expectations from R9
- [X] T140 Cross-platform hardening: verify workspace paths with spaces and non-ASCII characters on both OSes, confirm `command_windows`/`command_linux` selection, and add regression tests in `internal/git/paths_test.go` and `internal/validate/runner_test.go`
- [X] T141 [P] Performance pass: assert preview p95 ≤ 3 s against the fake Redmine (SC-001 budget), SPA first paint < 1 s from embedded assets, and binary size ≤ 40 MB stripped; record numbers in `tests/fixtures/perf-baseline.md`
- [X] T142 [P] Security review pass: audit every log/report/prompt/response write path for redaction coverage, confirm the agent policy file is read-only, and document the residual containment risk in `SECURITY.md`
- [X] T143 Run the full `quickstart.md` validation (Scenarios A–C plus the SC map) on Windows and Linux and record results in `specs/010-redmine-coding-agent/validation-log.md`
- [X] T144 Final quality gate: `go vet`, `golangci-lint`, `go test -race ./...` on Linux, `go test ./...` on Windows, `npm run check`, `npm test`, Playwright suite, and the four-target cross-compile — all green

---

## Dependencies & Execution Order

### Phase dependencies

- **Setup (Phase 1)**: no dependencies
- **Foundational (Phase 2)**: depends on Setup — **blocks every user story**
- **US1 (Phase 3)**: after Foundational — no story dependency
- **US2 (Phase 4)**: after Foundational **and US1** (a run starts from a confirmed preview; the issue snapshot and repository resolution come from US1)
- **US3 (Phase 5)**: after US2 (needs a validated run to publish)
- **US4 (Phase 6)**: after US2 (needs an active pipeline to stream and cancel); independent of US3
- **US5 (Phase 7)**: after US2 (needs runs to recover and retry); independent of US3 and US4
- **US6 (Phase 8)**: after Foundational; deep health probes are most useful after US2, and secret scanning (T120) needs the full pipeline
- **Polish (Phase 9)**: after all delivered stories

### Story dependency graph

```text
Setup ──▶ Foundational ──┬──▶ US1 ──▶ US2 ──┬──▶ US3 ──┐
                         │                  ├──▶ US4 ──┼──▶ Polish
                         │                  └──▶ US5 ──┤
                         └──────────────▶ US6 ─────────┘
```

### Within each story

Tests first (they must fail), then models, then services, then endpoints, then UI, then integration. A task that writes a test never shares a file with a task that writes product code.

### Parallel opportunities

- **Setup**: T003–T007, T009–T012 all touch separate files
- **Foundational**: T014/T015 (config), T022 (domain errors), T026–T028 (store files), T031/T032 (api files), T038 (git helpers) are independent; T016/T018 and T024/T025 are sequential within their area
- **US1**: T043/T044/T045/T046 (tests) together; T047/T048 (Redmine), T051 (api), T054–T055 (frontend) together
- **US2**: T059–T063 (tests) together; then T068/T069/T070 (agent) parallel with T073 (validate) and T077 (api), then T078/T079 (UI) together
- **US3**: T082–T084 together; T085/T086/T089/T090 split by file
- **US4**: T094–T097 together; T098/T102/T103 and T104/T106 split by file
- **US5**: T107–T110 together; T111/T112/T113/T116 split by file
- **US6**: T119–T122 together; T123/T125/T128/T130/T131 split by file
- **Polish**: T133, T135, T137, T139, T141, T142 are [P]

### Parallel example — US2

```text
# tests first, all independent files
T059 internal/agent/prompt_test.go
T060 internal/agent/summary_test.go
T061 internal/validate/runner_test.go
T062 tests/integration/pipeline_test.go
T063 internal/agent/opencode_test.go

# then implementation, split by package ownership
T068 internal/agent/prompt.go      T073 internal/validate/runner.go
T069 internal/agent/sandbox.go     T077 internal/api/runs.go
T070 internal/agent/opencode.go
```

---

## Implementation Strategy

### MVP first — US1 only

Setup → Foundational → US1 → **stop and validate**: preview any reachable issue, resolve the repository, decline cleanly, prove zero side effects (`T045`). This alone removes the manual lookup work and is demoable.

### Recommended delivery order (what the spec's priorities actually imply)

1. **Setup + Foundational** — the spine
2. **US1** — safe entry point (MVP demo)
3. **US2** — the core promise: isolated workspace + independent verification (first genuinely valuable release)
4. **US4** — observability and cancellation (safety before autonomy at scale)
5. **US3** — publish and report (value delivery to reviewers)
6. **US5** — recovery and retry (durability after the first bad day)
7. **US6** — configuration and health (operability)
8. **Polish** — retention, CLI completeness, both-OS proof

US4 before US3 is a deliberate deviation from priority number only: a run that cannot be cancelled should not be pushing branches yet. Both remain P2.

### Parallel team strategy

- After Foundational: **A** owns US1→US2 (pipeline), **B** owns US4 (transport + live UI), **C** owns US6 (config/health/doctor)
- After US2: **A** continues US3 (publish), **B** finishes US4, **C** takes US5 (recovery)
- Polish is shared; T143/T144 are the release gate and belong to whoever is releasing

### Testing gates per story

| Story | Must be green before the next story starts |
|---|---|
| US1 | T043–T046, `go test ./internal/redmine ./internal/runner` |
| US2 | T059–T063, `go test ./internal/agent ./internal/validate ./tests/integration` |
| US3 | T082–T084, MR description sections asserted in `publish_test.go` |
| US4 | T094–T097, 30 s cancellation assertion |
| US5 | T107–T110, restart and double-start assertions |
| US6 | T119–T122, secret scan zero-match |

---

## Notes

- `[P]` marks different files with no dependency on an incomplete task
- Every task cites a file path; no task is "implement the backend"
- FR references in task text are the acceptance hooks — if a task cannot be verified against its FR, the task is wrong
- Commit after each task or tight group; do not batch a story into one commit
- Stop at each checkpoint and validate the story independently before starting the next
- The constitution is ratified at v1.0.0 (2026-10-26): Principles I–VII are governing, and `plan.md`'s gate table maps to them
