# RedmineCodingAgent

Turns a Redmine issue number into a **verified, reviewable GitLab merge request** — without ever merging.

The worker fetches one issue, provisions an isolated git worktree, hands the issue to a coding agent under a least-privilege policy, and then **inspects the diff and runs every configured check itself**. A commit exists only because the worker's own checks passed; the branch is pushed and a merge request is opened, and a completion report is written back to the issue. Approval and merging stay with a human.

One Go binary (no CGO), Svelte SPA embedded, SQLite for history, identical behaviour on Linux and Windows.

---

## Status

Phases 1–5 of the plan are implemented. The pipeline is complete and proven end to end by the test suite; the long-running HTTP server and the operator CLI are still to come (Phase 9).

| Capability | State |
|---|---|
| Issue preview, repository resolution, no side effects (US1) | **Implemented** |
| Isolated worktree, agent invocation, independent verification, repair loop, commit (US2) | **Implemented** |
| Push, merge-request create/reuse, run report, Redmine comment (US3) | **Implemented** |
| Live progress (SSE) and cancel API (US4) | Not yet — endpoints answer `501` |
| Run history, retry, restart recovery (US5) | Not yet |
| Config/health admin API, `doctor` (US6) | Partial — `/api/health` is a liveness check only |
| `serve` wiring, `run`/`runs`/`prune` CLI, graceful shutdown (Polish) | Pending — `serve` prints a stub |

> **Consequence:** you cannot drive the product from a browser yet, because nothing serves the SPA. Every implemented behaviour is nevertheless proven end to end by `go test`, against `httptest` fakes for Redmine and GitLab, a real SQLite database, a real git mirror + worktree, and a controllable fake agent. That suite is the way to test the app today — see [Testing](#testing).

---

## How a run works

```text
preview (no side effects)
  issue ──▶ repository + base branch from config     ← ambiguous ⇒ blocked, never guessed
    │
    ▼  operator confirms
queued ──▶ preparing ──▶ implementing ──▶ validating ──┬──▶ repairing ──▶ validating
             │             │                │          │
             │             │                │          └── repair limit reached ⇒ failed
             │             │                └── required check fails ⇒ no commit
             │             └── agent runs in a dedicated worktree, sandboxed HOME/TMP
             └── connectivity: Redmine, GitLab, repository fetch, agent presence
                                     │
                          all required checks passed
                                     ▼
                       commit ──▶ publishing ──▶ reporting ──▶ completed
                                    │             │
                                    │             └── report stored, comment posted once
                                    └── push branch, create/reuse MR (never merge)
```

Failure paths are honest: a push rejection keeps the commit local, a failed comment keeps the report body, and a failed required check produces no commit at all.

---

## Requirements

| Need | For | Check |
|---|---|---|
| Go 1.24+ | build + all tests | `go version` |
| git 2.30+ | the pipeline and every integration test | `git --version` |
| Node 22+ / npm 10+ | frontend build, type check, browser tests | `node --version` |
| Chromium (via Playwright) | browser tests only | `npx --prefix web playwright install chromium` |

A real run would additionally need a Redmine API key, a GitLab token, and an authenticated `opencode` CLI — not required for anything below.

---

## Build

```bash
# frontend first (build-time only; the output is embedded into the binary)
cd web && npm ci && npm run build && cd ..

# backend, statically linked
CGO_ENABLED=0 go build -trimpath -o bin/redmine-agent ./cmd/redmine-agent
```

PowerShell (no `make` needed):

```powershell
pushd web; npm ci; npm run build; popd
$env:CGO_ENABLED=0
go build -trimpath -o bin\redmine-agent.exe .\cmd\redmine-agent
```

Or use the `Makefile`: `make build` (frontend + backend), `make build-all` (Linux and Windows, amd64 and arm64), `make test`, `make lint`.

If a build asks for `gcc`, a CGO-dependent package slipped in: that breaks the portability guarantee and must be fixed, not worked around.

---

## Configure

`config.yaml` (git-ignored) is read from `--config`, then `$REDMINE_AGENT_CONFIG`, then `./config.yaml`. Start from `config.example.yaml`; decoding is strict, so unknown keys are an error.

| Key | Meaning |
|---|---|
| `schema_version` | must be `1` |
| `http.listen` | loopback address the API binds |
| `data.database` / `data.workspace_root` / `data.log_dir` | SQLite file, worktree root, per-run log directory |
| `data.retention_days` | how long workspaces are kept |
| `redmine.base_url`, `redmine.api_key` | issue reads; `comment_on_finish` gates the report comment |
| `gitlab.base_url`, `gitlab.token` | project lookup and merge requests |
| `gitlab.push_enabled` / `gitlab.merge_request_enabled` | global publishing policy |
| `repositories[]` | `redmine_project` → `repository_path`, `base_branch`, `is_default`, optional `publish_enabled` / `merge_request_enabled`, and `checks[]` |
| `repositories[].checks[]` | `name`, `command_linux`, `command_windows`, `required` (default true), `timeout_seconds` |
| `agent` | `adapter`, `model`, `profile`, `timeout_minutes`, `max_repairs` |
| `concurrency.max_active_runs` | one active run by default |

**Secrets are never literal values.** Use `env:NAME` or `file:/path`; they are resolved at use time, never logged, and scrubbed from every log line, transcript, report, and response by a redaction registry.

---

## Running

```bash
go run ./cmd/redmine-agent version     # build version
go run ./cmd/redmine-agent help        # usage
go run ./cmd/redmine-agent serve       # currently prints a stub banner
```

The API surface that is already implemented (behind the `serve` wiring) matches `specs/010-redmine-coding-agent/contracts/openapi.yaml`: preview, repository list, create run, run detail, checks, report, liveness.

---

## Testing

Everything below runs offline. No Redmine, GitLab, agent CLI, or credentials are needed — the integration suite starts its own fakes.

### 1. The gate to run before anything else

```bash
# Linux / macOS
CGO_ENABLED=0 go test ./cmd/... ./internal/... ./tests/... -count=1

# Windows PowerShell
$env:CGO_ENABLED=0; go test ./cmd/... ./internal/... ./tests/... -count=1

# or
make test
```

**Expected:** every package prints `ok`; total runtime is roughly three minutes, most of it in `tests/integration` (it creates real git repositories and waits for real child processes).

> Use the explicit package list rather than `go test ./...`: `web/node_modules` contains third-party Go files that must never be compiled.

### 2. What each package proves

| Package | Proves |
|---|---|
| `internal/config` | strict decoding, unknown-key rejection, validation, secret references |
| `internal/logging` | redaction on every sink — a registered secret never survives `%v`, `slog`, or a file write |
| `internal/domain` | every allowed state transition, every rejected one, `cancelled` requires a cancel request |
| `internal/store` | partial unique index blocks a second active run, `CHECK` blocks a pass without exit 0, monotonic event sequence, history survives a reopen |
| `internal/git` | mirror + worktree isolation, push from a worktree, cleanup refused while work is unpushed |
| `internal/gitlab` | `PRIVATE-TOKEN`, URL-encoded project path, merge-request reuse, and that no merge endpoint is ever called |
| `internal/redmine` | issue fetch, typed not-found vs forbidden errors, journal notes, the duplicate-report marker |
| `internal/agent` | prompt fencing of untrusted issue text, `## RESULT` parsing, tolerant JSONL parsing |
| `internal/validate` | exit code → `passed`/`failed`, timeout, truncation, no secret in an excerpt |
| `internal/api` | every documented route exists with its documented method; unknown routes return problem+json |
| `internal/proc` | cancelling a process kills the whole tree, including grandchildren (slow test; skipped with `-short`) |
| `internal/runner` | report contents and dedupe key, repository resolution, issue snapshot extraction |

### 3. Integration scenarios (the real end-to-end proof)

Each test drives the public HTTP surface, a real pipeline, real git, and a real SQLite file.

| Test | What it proves |
|---|---|
| `TestPreviewCreatesNoRunWorkspaceOrBranch` | preview is side-effect free (US1) |
| `TestPreviewRejectsInvalidIssueNumberBeforeCallingRedmine` | bad input never reaches Redmine |
| `TestPreviewAmbiguousRepositoryListsCandidatesAndHonoursChoice` | ambiguity is surfaced, never guessed |
| `TestPipelineHappyPathCommitsAfterChecksPass` | commit only after the worker's own check passes |
| `TestPipelineFailingRequiredCheckBlocksTheCommit` | an agent that *claims* success changes nothing |
| `TestPipelineNoChangesCreatesNoCommit` | empty diff ⇒ `no_changes`, zero commits |
| `TestPipelineBlockedAgentEndsBlocked` | blocker question reaches the operator |
| `TestPipelineEscapeAttemptEndsPolicyViolation` | escaping the workspace fails the run |
| `TestPipelineRequiredCheckThatNeverRunsFails` | required + `not_executed` ⇒ failure (SC-005) |
| `TestCreateRunRequiresConfirmation` / `TestCreateRunRejectsASecondActiveRun` | no accidental start; one run per issue |
| `TestPublishPushesBranchAndOpensMergeRequest` | pushed branch, MR sections in order, comment with the same links, base branch untouched, no merge call |
| `TestPublishReusesAnExistingMergeRequest` | repeated publishing refreshes instead of duplicating |
| `TestPublishingDisabledCompletesWithSkippedPublish` | policy off ⇒ completed, nothing pushed, report says so |
| `TestMergeRequestDisabledStillPushesTheBranch` | push and MR are independently configurable |
| `TestRejectedPushKeepsTheCommitAndFails` | `push_rejected`, commit preserved, workspace marked unpushed |
| `TestFailedCommentKeepsTheReportBody` | Redmine failure never rolls back the work |
| `TestMergeRequestFailureKeepsThePushAndFails` | push survives an MR failure |

Run one scenario and watch it:

```bash
go test ./tests/integration -run TestPublishPushesBranchAndOpensMergeRequest -v
go test ./tests/integration -run TestPipeline -v      # the whole US2 group
```

The controllable fake agent is selected by putting `[fake-agent:<mode>]` in the issue description: `ok`, `no-changes`, `lies`, `blocked`, `crash`, `hang`, `escape-path`, `grandchild`.

### 4. Race detector, vet, lint

```bash
go vet ./cmd/... ./internal/... ./tests/...
golangci-lint run ./cmd/... ./internal/... ./tests/...    # errcheck, govet, staticcheck, gosec, bodyclose, revive
go test -race ./cmd/... ./internal/... ./tests/... -count=1   # Linux/CI leg, slower
```

### 5. Frontend

```bash
npm --prefix web run check    # svelte-check: types + template diagnostics, must be 0 errors
npm --prefix web test         # vitest (no unit files yet; passes with --passWithNoTests)
npm --prefix web run e2e      # Playwright against a stubbed API
```

The browser tests build the SPA, serve it on `127.0.0.1:4173`, and intercept the API in the browser, so no backend is required. First run only:

```bash
npx --prefix web playwright install chromium
```

### 6. Cross-platform build check

```bash
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o /dev/null ./cmd/redmine-agent
CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -o /dev/null ./cmd/redmine-agent
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o NUL        ./cmd/redmine-agent
CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build -o NUL        ./cmd/redmine-agent
```

Four successes with no C toolchain is the portability gate. CI (`.github/workflows/ci.yml`) runs this plus the full suite on `ubuntu-latest` and `windows-latest`.

### 7. What you cannot test yet

- Browsing the SPA against a live worker (`serve` is a stub).
- Cancelling a run, watching SSE progress, retrying, listing history, `doctor`, and the headless `run`/`runs`/`prune` commands.
- Anything against a real Redmine/GitLab. The fakes are the substitute until the server is wired.

---

## Project layout

```text
cmd/redmine-agent/          CLI entrypoint (serve, version, help)
internal/
  agent/                    adapter boundary, prompt fencing, sandbox, opencode, summary
  api/                      router, middleware, handlers, contract tests
  config/                   strict load, secrets, validation, redaction registry
  domain/                   run states, transitions, failure codes, entities
  git/                      mirror cache, worktrees, status/diff/commit/push
  gitlab/                   project + merge requests (no merge method exists)
  logging/                  slog JSON, per-run files, redaction writer
  proc/                     process containment (only build-tagged package)
  redmine/                  issue reads, attachments, journal notes
  runner/                   pipeline: prepare → implement → validate → publish → report
  store/                    SQLite: migrations, runs, events, checks, reports, seed
  validate/                 check execution and classification
  web/                      go:embed of the built SPA
web/                        SvelteKit 2 + Svelte 5 + TypeScript
tests/integration/          fakes + end-to-end scenarios; fakeagent/ is the controllable agent
specs/010-redmine-coding-agent/   spec, plan, data model, contracts, quickstart, tasks
```

---

## Safety guarantees (already enforced)

- **Never merges.** No merge or branch-delete operation exists in the GitLab client.
- **Never writes a protected branch.** Only a dedicated `agent/issue-<id>` branch is pushed.
- **Independent verification.** Validation results are written only by the worker; agent claims are stored as `reported_status` and never decide an outcome.
- **No commit without green checks**, and no `passed` row without exit code 0 — the latter is a database `CHECK`.
- **Secrets never leak.** Values resolve at use time, are registered with the redactor, and are scrubbed from logs, transcripts, prompts, reports, and HTTP bodies.
- **Work is never discarded silently.** Cleanup refuses while a workspace is dirty or holds unpushed commits.
- **One active run per issue**, enforced by a partial unique index, not just by code.

Containment of the agent is layered (denied operations, sandboxed `HOME`/`TMPDIR`, post-hoc detection), not an absolute sandbox: run the worker in a container on Linux or under a restricted service account on Windows. See `specs/010-redmine-coding-agent/research.md` §R9.

---

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `go test ./tests/integration` fails with `exec: "git": executable file not found` | install git and put it on `PATH` |
| Integration tests time out on a slow machine | they allow 90 s per run; close other heavy processes, or run one group with `-run` |
| `go test ./...` hits third-party Go files under `web/node_modules` | use the explicit package list shown above |
| `npm run e2e` cannot launch a browser | `npx --prefix web playwright install chromium` |
| `svelte-check` complains about `.svelte-kit` types | `npm --prefix web run check` runs `svelte-kit sync` first; run it from `web/` if invoked directly |
| Build asks for `gcc` | a CGO dependency slipped in: fix the dependency, do not set `CGO_ENABLED=1` |
| `serve` prints “stub listener not implemented yet” | expected; the server wiring is Phase 9 |

---

## Documentation

- `specs/010-redmine-coding-agent/spec.md` — requirements and acceptance scenarios
- `specs/010-redmine-coding-agent/plan.md` — architecture, gates, project structure
- `specs/010-redmine-coding-agent/data-model.md` — schema, state machine, invariants
- `specs/010-redmine-coding-agent/contracts/` — HTTP + SSE, CLI, and agent-adapter contracts
- `specs/010-redmine-coding-agent/quickstart.md` — operator validation scenarios
- `specs/010-redmine-coding-agent/tasks.md` — task breakdown and current progress
