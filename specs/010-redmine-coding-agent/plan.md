# Implementation Plan: RedmineCodingAgent — Automated Issue Implementation Worker

**Branch**: `010-redmine-coding-agent` (no VCS initialized in this folder; spec branch name used as-is) | **Date**: 2026-10-26 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `specs/010-redmine-coding-agent/spec.md`

**Operator stack directive**: multi-platform (Linux **and** Windows); backend **Go**; frontend **Svelte** (only if needed); database **SQLite**.

## Summary

Build a self-hosted worker that turns one Redmine issue number into a verified, reviewable merge request. The operator submits a number, confirms the retrieved issue plus the resolved repository and base branch, and the worker provisions an isolated git worktree, hands the issue to a coding agent (OpenCode first) under a least-privilege policy, then **independently** inspects the diff and executes the configured checks itself. Only work that passes every required check is committed, pushed, and opened as a merge request; nothing is ever merged, and a completion report is written back to Redmine.

Technical approach: one Go binary (no CGO) that embeds a Svelte SPA, persists run history in SQLite, drives `git` and `opencode` as subprocesses under OS-level process containment, and streams live progress to the browser over SSE. Adapters isolate the issue tracker, the VCS host, and the coding agent so each can be swapped by configuration.

## Technical Context

**Language/Version**: Go 1.24 (module `go 1.24`, `CGO_ENABLED=0`); TypeScript 5 + Node 24/npm 11 for the frontend build only (build-time dependency, not runtime)

**Primary Dependencies**:
- Backend: Go standard library (`net/http` with 1.22+ method/wildcard routing, `log/slog`, `embed`, `os/exec`); `modernc.org/sqlite` (pure-Go SQLite); `github.com/google/uuid` (v7 run IDs); `golang.org/x/sys/windows` (Windows Job Objects); `gopkg.in/yaml.v3` (config)
- Frontend: SvelteKit 2 + Svelte 5 + Vite 7 + `@sveltejs/adapter-static` (SSR off), Vitest, Playwright

**Storage**: SQLite in WAL mode (`modernc.org/sqlite`), embedded numbered SQL migrations, single-writer discipline; per-run artifacts on disk (transcripts, prompts, logs) referenced by absolute path from the DB

**Testing**: `go test ./...` (unit + `httptest` fakes for Redmine/GitLab + a fake-agent test binary + throwaway git repos in `t.TempDir()`); Vitest for frontend units; Playwright E2E against a stub API; CI matrix `ubuntu-latest` + `windows-latest` with `CGO_ENABLED=0`

**Target Platform**: Linux amd64/arm64 and Windows amd64/arm64 — one statically linked binary per OS, browser UI served from the same process

**Project Type**: single Go module, web application + CLI (backend orchestration, Svelte SPA, headless subcommands)

**Performance Goals**: issue preview within 10 s of submission (SC-001) — p95 ≤ 3 s for a reachable Redmine; embedded SPA first paint < 1 s; SSE event delivery ≤ 500 ms after state change; cancel-to-agent-exit ≤ 30 s (SC-009); binary ≤ 40 MB stripped

**Constraints**: no CGO and no OS-specific build beyond small build-tagged files; no credential value in any log, report, prompt, or DB row (FR-056); agent runs with least privilege (FR-029); one active run by default (FR-011); run history and stage survive restart with no auto-resume into push (FR-051); identical behaviour on Linux and Windows (FR-059 + operator directive)

**Scale/Scope**: one small operator team; default 1 concurrent run, configurable to ≤ 10; ~10k runs/year; 1 repository per run; 3–5 configured checks per repository; 4 UI screens; 1 agent adapter in v1

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

> **Status of the constitution**: ratified at v1.0.0 on 2026-10-26 (`.specify/memory/constitution.md`). Gates G1–G7 below were the source material for Principles I–VII and are now governing, not provisional. Principle mapping: G3→I, G4→II, G5→III, G2→IV, G1/G6→V/VI, G7→VII.

| # | Gate | Constitution principle | Result | Evidence |
|---|---|---|---|---|
| G1 | One portable binary: no CGO, no runtime other than git + the agent CLI | VI | PASS | pure-Go SQLite; SPA embedded via `go:embed`; git and agent invoked as subprocesses |
| G2 | Secret hygiene: no credential in logs, prompts, DB, or reports | IV | PASS | secrets only as `env:`/`file:` indirection resolved at use time; registry-driven redaction on every log/transcript/report write |
| G3 | Independent verification: the worker, never the agent, decides pass/fail | I | PASS | checks executed by the worker from trusted config; agent text is recorded as claims only, never as results |
| G4 | Human-in-the-loop publishing: never merge, never write a protected branch | II | PASS | only `git push <branch>` and MR creation are reachable; no merge or force-push operation exists in the VCS client |
| G5 | Durable, auditable state: every run recoverable with last stage | III | PASS | append-only `run_events`; startup sweep marks orphans `interrupted`; no resume into push |
| G6 | Portability parity: OS differences confined to build-tagged files | VI | PASS | `internal/proc` is the only package with `_unix.go`/`_windows.go` variants |
| G7 | Simplicity: stdlib first, every dependency justified | VII | PASS | 4 backend dependencies, each justified in `research.md` |

**Violations**: none. No Complexity Tracking entries required.

## Project Structure

### Documentation (this feature)

```text
specs/010-redmine-coding-agent/
├── plan.md              # This file
├── spec.md              # Feature specification (/speckit.specify)
├── research.md          # Phase 0: decisions, rationale, alternatives
├── data-model.md        # Phase 1: entities, DDL, state machine, invariants
├── quickstart.md        # Phase 1: runnable validation scenarios
├── contracts/
│   ├── openapi.yaml     # HTTP + SSE contract for the UI and headless clients
│   ├── cli.md           # CLI commands, flags, exit codes
│   └── agent-adapter.md # Internal Go contract that isolates coding agents (FR-028)
├── checklists/
│   └── requirements.md  # Existing requirements checklist
└── tasks.md             # Phase 2 output (/speckit.tasks — not created here)
```

### Source Code (repository root)

```text
cmd/
└── redmine-agent/
    └── main.go                 # CLI entrypoint: serve, run, runs, doctor, version

internal/
├── api/                        # HTTP router, handlers, middleware, SSE hub
│   ├── router.go
│   ├── runs.go                 # create/get/list/cancel/retry
│   ├── issues.go               # preview + repository resolution
│   ├── events.go               # SSE stream with Last-Event-ID replay
│   ├── health.go
│   └── redact.go               # response-level scrubbing backstop
├── config/                     # load, strict-validate, redact, secret resolution
│   ├── config.go
│   ├── secrets.go
│   └── validate.go
├── domain/                     # pure types, states, transitions, failure codes
│   ├── run.go
│   ├── state.go
│   └── errors.go
├── agent/                      # FR-028 isolation
│   ├── adapter.go              # Adapter interface + registry
│   ├── opencode.go             # opencode CLI adapter
│   ├── prompt.go               # prompt builder (untrusted-content fencing)
│   └── summary.go              # parse the required summary / BLOCKED marker
├── runner/                     # orchestration pipeline, one run at a time
│   ├── runner.go               # state machine driver + cancellation
│   ├── prepare.go              # connectivity, workspace
│   ├── implement.go            # agent invocation
│   ├── validate.go             # independent checks, repair loop
│   ├── publish.go              # commit, push, merge request
│   ├── report.go               # final report + Redmine comment
│   └── recover.go              # startup sweep of interrupted runs
├── redmine/                    # REST client: issue, journals, attachments, comment
├── gitlab/                     # REST client: project, fetch check, branch, MR
├── git/                        # mirror cache, worktree, status, diff, commit, push
├── validate/                   # command execution, classification, output capture
├── health/                     # dependency probes with latency
├── proc/                       # process containment (only build-tagged package)
│   ├── proc.go
│   ├── proc_unix.go            # setpgid + kill(-pgid)
│   └── proc_windows.go         # Job Object + TerminateJobObject
├── store/                      # SQLite: open/pragmas, migrations, repositories
│   ├── store.go
│   ├── migrate.go
│   ├── runs.go
│   ├── events.go
│   ├── checks.go
│   └── migrations/*.sql        # embedded
├── logging/                    # slog JSON, per-run log files, redaction writer
└── web/                        # go:embed of web/build + SPA fallback handler

web/                            # SvelteKit 2 + Svelte 5
├── src/
│   ├── routes/
│   │   ├── +layout.svelte
│   │   ├── +page.svelte            # issue input → preview → confirm
│   │   ├── runs/+page.svelte       # run history
│   │   ├── runs/[id]/+page.svelte  # live run view: stage, elapsed, checks, logs, cancel, retry
│   │   └── health/+page.svelte
│   └── lib/
│       ├── api.ts                  # typed client generated from contracts/openapi.yaml
│       ├── sse.ts                  # EventSource with Last-Event-ID handling
│       └── components/             # IssueCard, RepositoryPicker, CheckList, Timeline, LogViewer
├── svelte.config.js                # adapter-static, fallback index.html, ssr=false
└── package.json

tests/
├── integration/                    # fake Redmine/GitLab, fake agent, real git, real SQLite
│   ├── harness.go
│   ├── pipeline_test.go
│   ├── crash_recovery_test.go
│   └── testdata/
└── fixtures/                       # sample issue payloads, config files, transcripts

config.example.yaml
Dockerfile                          # Linux image (distroless/static), secondary target
.goreleaser.yaml                    # linux/amd64, linux/arm64, windows/amd64, windows/arm64
```

**Structure Decision**: Web-application layout (Option 2) collapsed into **one Go module plus one frontend package**, because the product ships as a single portable binary. `web/build` is produced by SvelteKit's static adapter and embedded at compile time, so runtime needs no Node. OS-specific behaviour is confined to `internal/proc`; every other package is platform-neutral. `tests/integration` sits outside `internal` so it can exercise the public HTTP surface end to end.

## Phase 0 — Research

Complete. All Technical Context unknowns are resolved in [`research.md`](./research.md), including:

- pure-Go SQLite for CGO-free cross-compilation, and the WAL/busy-timeout settings that make single-writer concurrency safe
- git **mirror + worktree** strategy per run (isolation without touching developer checkouts)
- `opencode run` non-interactive invocation, verified against the installed CLI (`--format json`, `--model`, `--agent`, `--standalone`; no `--dir`, so the working directory is set by the parent process)
- SSE over WebSocket for progress, with `Last-Event-ID` replay from `run_events`
- OS-level process containment: process groups on Unix, Job Objects on Windows, both triggered by one `context`
- layered agent containment and its residual risk, stated honestly
- SQLite-enforced per-issue uniqueness via a partial unique index

## Phase 1 — Design & Contracts

### Data model

[`data-model.md`](./data-model.md) defines eight tables (`repositories`, `repository_checks`, `issue_snapshots`, `runs`, `workspaces`, `run_events`, `agent_invocations`, `validation_results`, `reports`, `configuration_profiles`), the nine-state machine with every allowed transition, the database-level invariants that enforce FR-010/FR-011/FR-032/FR-043/FR-045, and the startup recovery sweep.

### Interfaces

| Contract | File | Consumers |
|---|---|---|
| HTTP + SSE API | [`contracts/openapi.yaml`](./contracts/openapi.yaml) | Svelte SPA, headless operators, tests |
| CLI | [`contracts/cli.md`](./contracts/cli.md) | operators, cron/CI, service managers |
| Agent adapter (Go) | [`contracts/agent-adapter.md`](./contracts/agent-adapter.md) | orchestration ↔ coding-agent boundary (FR-028) |

No external service is exposed beyond a localhost-bound HTTP listener; the outbound contracts (Redmine REST, GitLab v4 REST) are described in `research.md` and consumed through `internal/redmine` and `internal/gitlab`.

### Quickstart

[`quickstart.md`](./quickstart.md) carries the runnable validation path: build, configure, `doctor`, preview an issue, run to completion against a fake or real pair of services, cancel mid-run, restart for recovery, and the SC-by-SC verification map. It deliberately contains no implementation bodies.

### Agent context

`AGENTS.md` plan reference updated to this file.

## Post-Design Constitution Re-Check

*Re-evaluated after Phase 1 design.*

| # | Gate | Principle | Result | Note |
|---|---|---|---|---|
| G1 | Portable single binary | VI | PASS | design adds only `go:embed` assets; both OS targets stay CGO-free |
| G2 | Secret hygiene | IV | PASS | secret resolution confined to `internal/config`; `logging` and `api/redact` scrub on write; prompts built from non-secret config only |
| G3 | Independent verification | I | PASS | `runner/validate.go` is the sole writer of `validation_results`; adapter output never sets an outcome |
| G4 | Human-in-the-loop publishing | II | PASS | `gitlab` client exposes create/reuse only; no merge method exists to call by accident |
| G5 | Durable audit | III | PASS | `run_events` append-only + `recover.go` sweep + `stage` column on every transition |
| G6 | Portability parity | VI | PASS | `internal/proc` remains the only build-tagged package; SQLite driver is pure Go |
| G7 | Simplicity | VII | PASS | 4 backend dependencies; no ORM, no message queue, no reverse proxy |

**Result: PASS.** No violations, no Complexity Tracking entries. One design-time risk is recorded rather than hidden: full host-level sandboxing of the agent is a deployment concern (container on Linux, restricted service account on Windows), not something this binary can guarantee — see `research.md` §R9 and the residual-risk note in `quickstart.md`.

## Deferred (not planned here)

- Admin UI for editing configuration (v1 is file + restart, per FR-055)
- Per-user authentication/SSO (explicitly out of scope in the spec's Assumptions)
- Additional agent adapters (Codex CLI, Claude Code) — the interface exists, implementations do not
- Prometheus metrics, log shipping, HA/multi-worker deployment
